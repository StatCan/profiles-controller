package cmd

import (
	"context"
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

var limitRangesCmd = &cobra.Command{
	Use:   "limitranges",
	Short: "Configure Limit Ranges",
	Long: `Configure Limit Ranges for Kubeflow profiles.
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

		LimitRangeInformer := kubeInformerFactory.Core().V1().LimitRanges()
		LimitRangeLister := LimitRangeInformer.Lister()

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				// Generate limit ranges
				LimitRanges := generateLimitRanges(profile)

				// Delete s no longer needed
				for _, LimitRangeName := range []string{} {
					_, err := LimitRangeLister.LimitRanges(profile.Name).Get(LimitRangeName)
					if err == nil {
						klog.Infof("removing limit range %s/%s", profile.Name, LimitRangeName)
						err = kubeClient.CoreV1().LimitRanges(profile.Name).Delete(context.Background(), LimitRangeName, metav1.DeleteOptions{})
						if err != nil {
							return err
						}
					}
				}

				// Create
				for _, LimitRange := range LimitRanges {
					currentLimitRange, err := LimitRangeLister.LimitRanges(LimitRange.Namespace).Get(LimitRange.Name)
					if errors.IsNotFound(err) {
						klog.Infof("creating limit range %s/%s", LimitRange.Namespace, LimitRange.Name)
						currentLimitRange, err = kubeClient.CoreV1().LimitRanges(LimitRange.Namespace).Create(context.Background(), LimitRange, metav1.CreateOptions{})
						if err != nil {
							return err
						}
					}

					if !equality.Semantic.DeepDerivative(LimitRange.Spec, currentLimitRange.Spec) {
						klog.Infof("updating limit range %s/%s", LimitRange.Namespace, LimitRange.Name)
						currentLimitRange.Spec = LimitRange.Spec

						_, err = kubeClient.CoreV1().LimitRanges(LimitRange.Namespace).Update(context.Background(), currentLimitRange, metav1.UpdateOptions{})
						if err != nil {
							return err
						}
					}
				}

				return nil
			},
		)

		LimitRangeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newNP := new.(*corev1.LimitRange)
				oldNP := old.(*corev1.LimitRange)

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
		if ok := cache.WaitForCacheSync(stopCh, LimitRangeInformer.Informer().GetController().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

// generateLimitRanges generates  limitranges for the given profile.
// TODO: Allow overrides in a namespace
func generateLimitRanges(profile *kubeflowv1.Profile) []*corev1.LimitRange {
	limitranges := []*corev1.LimitRange{}

	limitranges = append(limitranges, &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "default-container-limits",
			Namespace: profile.Name,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(profile, kubeflowv1.SchemeGroupVersion.WithKind("Profile")),
			},
		},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Default: corev1.ResourceList{
						"cpu":    *resource.NewScaledQuantity(1000, resource.Milli),
						"memory": *resource.NewScaledQuantity(20, resource.Giga),
					},
					DefaultRequest: corev1.ResourceList{
						"cpu":    *resource.NewScaledQuantity(50, resource.Milli),
						"memory": *resource.NewScaledQuantity(256, resource.Mega),
					},
					Type: "Container",
				},
			},
		},
	})

	return limitranges
}

func init() {
	rootCmd.AddCommand(limitRangesCmd)
}
