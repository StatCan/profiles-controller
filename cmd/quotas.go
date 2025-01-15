package cmd

import (
	"context"
	"strconv"
	"time"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

var quotaPrefixLabel = "quotas.statcan.gc.ca/"
var defaultResources = corev1.ResourceList{
	"requests.cpu": *resource.NewQuantity(16, resource.DecimalSI),
	"limits.cpu":   *resource.NewQuantity(16, resource.DecimalSI),

	// Memory
	"requests.memory": *resource.NewScaledQuantity(64, resource.Giga),
	"limits.memory":   *resource.NewScaledQuantity(64, resource.Giga),

	// Storage
	"requests.storage": *resource.NewScaledQuantity(1, resource.Tera),

	// GPU
	"requests.nvidia.com/gpu": *resource.NewQuantity(0, resource.DecimalSI),

	// Pods
	"pods": *resource.NewQuantity(20, resource.DecimalSI),

	// Services
	"services.nodeports":     *resource.NewQuantity(0, resource.DecimalSI),
	"services.loadbalancers": *resource.NewQuantity(0, resource.DecimalSI),
}

// Override the default resources from profile labels
func overrideResourceQuotas(profile *kubeflowv1.Profile) corev1.ResourceList {

	overrides := map[corev1.ResourceName]resource.Quantity{}

	// copy in the defaults
	for k, v := range defaultResources {
		overrides[k] = v
	}

	// Special case, quotas.statcan.gc.ca/gpu -> requests.nvidia.com/gpu
	if val, ok := profile.Labels[quotaPrefixLabel+"gpu"]; ok {
		numgpu, _ := strconv.Atoi(val)
		overrides["requests.nvidia.com/gpu"] = *resource.NewQuantity(int64(numgpu), resource.DecimalSI)
		klog.Infof("Overriding resource quota from label profile: %s, requests.nvidia.com/gpu: %d", profile.Name, int64(numgpu))
	}

	// Loop over again, searching profile labels for
	// quotas.statcan.gc.ca/{key}
	// We will not clobber requests.nvidia.com/gpu because
	// quotas.statcan.gc.ca/requests.nvidia.com/gpu is not a valid label.
	for key := range defaultResources {
		if overrideValue, ok := profile.Labels[quotaPrefixLabel+key.String()]; ok {
			overrides[key] = resource.MustParse(overrideValue)
			klog.Infof("Overriding resource quota from label profile %s, %s: %d", profile.Name, key.String(), overrides[key])
		}
	}

	return overrides
}

var quotasCmd = &cobra.Command{
	Use:   "quotas",
	Short: "Configure quotas resources",
	Long: `Configure quota resources for Kubeflow profiles.
	`,
	Run: func(cmd *cobra.Command, args []string) {
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()

		// Create Kubernetes config
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
		if err != nil {
			klog.Fatalf("error building kubeconfig: %v", err)
		}

		kubeClient, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
		}

		kubeflowClient, err := kubeflowclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("error building Kubeflow client: %v", err)
		}

		// Setup informers
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*(time.Duration(requeue_time)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(requeue_time)))

		resourceQuotaInformer := kubeInformerFactory.Core().V1().ResourceQuotas()
		resourceQuotaLister := resourceQuotaInformer.Lister()

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				// Generate quotas
				resourceQuotas := generateResourceQuotas(profile)

				// Delete resources no longer needed
				for _, resourceQuotaName := range []string{} {
					_, err := resourceQuotaLister.ResourceQuotas(profile.Name).Get(resourceQuotaName)
					if err == nil {
						klog.Infof("removing resource quota %s/%s", profile.Name, resourceQuotaName)
						err = kubeClient.CoreV1().ResourceQuotas(profile.Name).Delete(context.Background(), resourceQuotaName, metav1.DeleteOptions{})
						if err != nil {
							return err
						}
					}
				}

				// Create
				for _, resourceQuota := range resourceQuotas {
					currentResourceQuota, err := resourceQuotaLister.ResourceQuotas(resourceQuota.Namespace).Get(resourceQuota.Name)
					if errors.IsNotFound(err) {
						klog.Infof("creating resource quota %s/%s", resourceQuota.Namespace, resourceQuota.Name)
						currentResourceQuota, err = kubeClient.CoreV1().ResourceQuotas(resourceQuota.Namespace).Create(context.Background(), resourceQuota, metav1.CreateOptions{})
						if err != nil {
							return err
						}
					}

					if !equality.Semantic.DeepDerivative(resourceQuota.Spec, currentResourceQuota.Spec) {
						klog.Infof("updating resource quota %s/%s", resourceQuota.Namespace, resourceQuota.Name)
						currentResourceQuota.Spec = resourceQuota.Spec

						_, err = kubeClient.CoreV1().ResourceQuotas(resourceQuota.Namespace).Update(context.Background(), currentResourceQuota, metav1.UpdateOptions{})
						if err != nil {
							return err
						}
					}
				}

				return nil
			},
		)

		resourceQuotaInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newNP := new.(*corev1.ResourceQuota)
				oldNP := old.(*corev1.ResourceQuota)

				if newNP.ResourceVersion == oldNP.ResourceVersion {
					return
				}

				controller.HandleObject(new)
			},
			DeleteFunc: controller.HandleObject,
		})

		// Start informers
		kubeInformerFactory.Start(stopCh)
		kubeflowInformerFactory.Start(stopCh)

		// Wait for caches
		klog.Info("Waiting for informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, resourceQuotaInformer.Informer().GetController().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

// generateResourceQuotas generates resource quotas for the given profile.
func generateResourceQuotas(profile *kubeflowv1.Profile) []*corev1.ResourceQuota {

	overrideResourceQuotas(profile)
	quotas := []*corev1.ResourceQuota{}
	quotas = append(quotas, &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "quotas",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: overrideResourceQuotas(profile),
		},
	})

	return quotas
}

func init() {
	rootCmd.AddCommand(quotasCmd)
}
