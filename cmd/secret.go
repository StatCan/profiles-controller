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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

var secretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Configure default secrets for profiles",
	Long:  `Configure default secrets for profiles`,
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

		// secretInformer := kubeInformerFactory.Core().V1().Secrets()
		// secretLister := secretInformer.Lister()

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				createArtifactorySecret(kubeClient, profile.Spec.Owner.Name)
				return nil
			},
		)

		// Start informers
		kubeInformerFactory.Start(stopCh)
		kubeflowInformerFactory.Start(stopCh)

		// Wait for caches
		// klog.Info("Waiting for informer caches to sync")
		// if ok := cache.WaitForCacheSync(stopCh, secretInformer.Informer().HasSynced); !ok {
		// 	klog.Fatalf("failed to wait for caches to sync")
		// }

		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

func createArtifactorySecret(client *kubernetes.Clientset, ns string) {
	_, err := client.CoreV1().Secrets(ns).Get(context.Background(), "artifactory-creds", metav1.GetOptions{})
	if err != nil {
		//Create the secret
		klog.Infof("Creating artifactory-secret")
		secret, err := client.CoreV1().Secrets("kubeflow").Get(context.Background(), "artifactory-creds", metav1.GetOptions{})
		if err != nil {
			// Now that we have the values for the keys put it into a secret in the namespace
			usersecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "artifactory-creds",
					Namespace: ns,
				},
				Data: map[string][]byte{
					// this [0] seems a bit suspect but we will see how it works for now I don't know
					// I don't think the S3 account will be used multiple times or anything
					"Username": []byte(secret.Data["Username"]),
					"Token":    []byte(secret.Data["Token"]),
				},
			}

			_, err = client.CoreV1().Secrets(ns).Create(context.Background(), usersecret, metav1.CreateOptions{})
			if err != nil {
				klog.Infof("An Error Occured while creating the secret %v", err)
			}
		}
	}
}

func init() {
	rootCmd.AddCommand(secretCmd)
}
