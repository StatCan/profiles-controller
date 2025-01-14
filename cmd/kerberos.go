package cmd

import (
	"context"
	"reflect"
	"time"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"
	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	"github.com/StatCan/profiles-controller/pkg/signals"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

var kerberosCmd = &cobra.Command{
	Use:   "kerberos",
	Short: "Configure kerberos sidecar resources",
	Long:  "Configure kerberos sidecar resources",
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

		networkPolicyInformer := kubeInformerFactory.Networking().V1().NetworkPolicies()

		configMapInformer := kubeInformerFactory.Core().V1().ConfigMaps()

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				err := createKerberosConfigMap(profile.Namespace, kubeClient)
				if err != nil {
					return err
				}
				err = createKerberosNetworkPolicy(profile.Namespace, kubeClient)
				return err
			},
		)

		networkPolicyInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newNP := new.(*networkingv1.NetworkPolicy)
				oldNP := old.(*networkingv1.NetworkPolicy)

				if newNP.ResourceVersion == oldNP.ResourceVersion {
					return
				}

				controller.HandleObject(new)
			},
			DeleteFunc: controller.HandleObject,
		})

		configMapInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, new interface{}) {
				newCM := new.(*corev1.ConfigMap)
				oldCM := old.(*corev1.ConfigMap)

				if newCM.ResourceVersion == oldCM.ResourceVersion {
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
		if ok := cache.WaitForCacheSync(stopCh, networkPolicyInformer.Informer().HasSynced, configMapInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

func createKerberosConfigMap(namespace string, kubeClient *kubernetes.Clientset) error {
	// get the master configmap from the das namespace
	masterCM, err := kubeClient.CoreV1().ConfigMaps("das").Get(context.Background(), "kerberos-sidecar-config", metav1.GetOptions{})
	if err != nil {
		return err
	}

	// find the egress kerberos policy for the given namespace
	userCM, err := kubeClient.CoreV1().ConfigMaps(namespace).Get(context.Background(), masterCM.Name, metav1.GetOptions{})

	// if CM is not found, create it in the namespace
	if errors.IsNotFound(err) {
		klog.Infof("creating config map %s/%s", masterCM.Namespace, masterCM.Name)
		_, err = kubeClient.CoreV1().ConfigMaps(namespace).Create(context.Background(), masterCM, metav1.CreateOptions{})
		if err != nil {
			return err
		}

		return nil
	} else if err != nil {
		return err
	}

	// if CM is found, but not equal to the master CM, update the CM
	if !reflect.DeepEqual(masterCM.Data, userCM.Data) {
		klog.Infof("updating config map %s/%s", masterCM.Namespace, masterCM.Name)
		userCM.Data = masterCM.Data

		_, err = kubeClient.CoreV1().ConfigMaps(namespace).Update(context.Background(), userCM, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}

func createKerberosNetworkPolicy(namespace string, kubeClient *kubernetes.Clientset) error {
	// generate the policy for egress to kerberos
	policy := generateKerberosNetworkPolicy(namespace)

	// find the egress kerberos policy for the given namespace
	currentPolicy, err := kubeClient.NetworkingV1().NetworkPolicies(namespace).Get(context.Background(), policy.Name, metav1.GetOptions{})

	// if policy is not found, create it in the namespace
	if errors.IsNotFound(err) {
		klog.Infof("creating network policy %s/%s", policy.Namespace, policy.Name)
		_, err = kubeClient.NetworkingV1().NetworkPolicies(policy.Namespace).Create(context.Background(), &policy, metav1.CreateOptions{})
		if err != nil {
			return err
		}

		return nil
	} else if err != nil {
		return err
	}

	// if policy is found, but not equal to the generated policy, update the policy
	if !reflect.DeepEqual(policy.Spec, currentPolicy.Spec) {
		klog.Infof("updating network policy %s/%s", policy.Namespace, policy.Name)
		currentPolicy.Spec = policy.Spec

		_, err = kubeClient.NetworkingV1().NetworkPolicies(policy.Namespace).Update(context.Background(), currentPolicy, metav1.UpdateOptions{})
		if err != nil {
			return err
		}
	}

	return nil
}

func generateKerberosNetworkPolicy(namespace string) networkingv1.NetworkPolicy {
	portKDC := intstr.FromInt(88)
	protocolTCP := corev1.ProtocolTCP

	policy := networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "allow-egress-to-kerberos-kdc",
			Namespace: namespace,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "notebook-name",
						Operator: metav1.LabelSelectorOpExists,
					},
				},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &protocolTCP,
							Port:     &portKDC,
						},
					},
					To: []networkingv1.NetworkPolicyPeer{
						{
							IPBlock: &networkingv1.IPBlock{
								CIDR: "172.20.60.68/32",
							},
						},
						{
							IPBlock: &networkingv1.IPBlock{
								CIDR: "172.20.60.69/32",
							},
						},
					},
				},
			},
		},
	}

	return policy
}

func init() {
	rootCmd.AddCommand(kerberosCmd)
}
