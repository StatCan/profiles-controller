package cmd

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	kubeflowclientset "github.com/StatCan/profiles-controller/pkg/generated/clientset/versioned"

	kubeflowinformers "github.com/StatCan/profiles-controller/pkg/generated/informers/externalversions"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth/azure"
	"k8s.io/klog"

	kubeflowv1 "github.com/StatCan/profiles-controller/pkg/apis/kubeflow/v1"
	"github.com/StatCan/profiles-controller/pkg/controllers/profiles"
	"github.com/StatCan/profiles-controller/pkg/signals"
	"github.com/spf13/cobra"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	// project packages
)

var clusterUrl string
var storageAccount string
var body *strings.Reader
var prefixSA string
var schemaName string
var catalogs = []string{"unclassified"}
var trinoSchema = &cobra.Command{
	Use:   "trino-schema",
	Short: "Create Trino schemas",
	Long:  `Create Trino schemas for AAW users`,
	Run: func(cmd *cobra.Command, args []string) {
		// Setup signals so we can shutdown cleanly
		stopCh := signals.SetupSignalHandler()
		// Create Kubernetes config
		cfg, err := clientcmd.BuildConfigFromFlags(apiserver, kubeconfig)
		if err != nil {
			klog.Fatalf("Error building kubeconfig: %v", err)
		}

		kubeflowClient, err := kubeflowclientset.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building Kubeflow client: %v", err)
		}

		kubeClient, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
		}
		// Setup Kubeflow informers
		kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Minute*(time.Duration(1)))
		kubeflowInformerFactory := kubeflowinformers.NewSharedInformerFactory(kubeflowClient, time.Minute*(time.Duration(1)))
		// Secret
		secretsInformer := kubeInformerFactory.Core().V1().Secrets()
		secretsLister := secretsInformer.Lister()

		// Setup controller
		controller := profiles.NewController(
			kubeflowInformerFactory.Kubeflow().V1().Profiles(),
			func(profile *kubeflowv1.Profile) error {
				var req *http.Request
				//Create a schema in each catalog for the profile
				for _, catalog := range catalogs {
					if cfg.Host == "https://10.131.0.1:443" { // Dev cluster internal ip address
						prefixSA = "aawdevcc00"
						trinoInstance(catalog, profile, "dev")
					} else {
						prefixSA = "aawprodcc00" // todo: replace with prod cluster internal ip address
						trinoInstance(catalog, profile, "prod")
					}
					//Fetch JWT Token from admin namespace from default Secret
					secret, _ := secretsLister.Secrets("rohan-katkar").List(labels.Everything())

					token := fetchToken(secret) // JWT token from rohan-katkar ns
					body = strings.NewReader("CREATE SCHEMA IF NOT EXISTS " + catalog + "." + schemaName + " WITH (location = 'wasbs://" + profile.Name + "@" + prefixSA + storageAccount + ".blob.core.windows.net/')")
					//body = strings.NewReader("Drop schema unclassified" + "." + schemaName)

					req, err = http.NewRequest("POST", clusterUrl, body)
					if err != nil {
						klog.Fatalf("error in creating POST request: %v", err)
					}
					// Utilise Trino request & response headers to set session user and catalog
					req.Header.Set("X-Trino-User", "Rohan Katkar")
					req.Header.Set("X-Trino-Catalog", catalog)
					req.Header.Set("X-Trino-Set-Catalog", catalog)
					req.Header.Set("Authorization", "Bearer "+token)
					// Initial curl request
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						klog.Fatalf("error sending and returning HTTP response  : %v", err)
					}
					if resp.StatusCode == http.StatusOK {
						nextURIResponse := nextUriCall(resp) // 1. Planning stage
						url := nextUriCall(nextURIResponse)  // 2. Executing stage
						nextUriCall(url)                     // 3. Finishing stage
					} else if resp.StatusCode == http.StatusServiceUnavailable {
						resp, _ := http.DefaultClient.Do(req)
						// re-try request
						nextURIResponse := nextUriCall(resp)
						nextUriCall(nextURIResponse)
					}
				}
				return nil
			},
		)

		// Start informers
		kubeInformerFactory.Start(stopCh)
		kubeflowInformerFactory.Start(stopCh)

		klog.Info("Waiting for configMap informer caches to sync")
		if ok := cache.WaitForCacheSync(stopCh, secretsInformer.Informer().HasSynced); !ok {
			klog.Fatalf("failed to wait for caches to sync")
		}
		// Run the controller
		if err = controller.Run(2, stopCh); err != nil {
			klog.Fatalf("error running controller: %v", err)
		}
	},
}

// Fetch JWT token from default-token secret
func fetchToken(secretList []*v1.Secret) string {
	for _, s := range secretList {
		if strings.Contains(s.ObjectMeta.Name, "default-token") {
			return string(s.Data["token"])
		}
	}
	return ""
}

// Logic to use appropriate trino instance prod/dev/unclass/protb, schema name and storage account
func trinoInstance(catalog string, profile *kubeflowv1.Profile, cluster string) {
	if cluster == "dev" {
		clusterUrl = "https://trino.aaw-dev.cloud.statcan.ca/v1/statement"
		schemaName = strings.Replace(profile.Name, "-", "", -1)
		storageAccount = "samgpremium"
	} else {
		clusterUrl = "https://trino.aaw.cloud.statcan.ca/v1/statement"
		schemaName = strings.Replace(profile.Name, "-", "", -1)
		storageAccount = "samgpremium"
	}
}

//Submit a GET request using the nextUri from the response of the POST request to retrieve query result
func nextUriCall(resp *http.Response) *http.Response {
	b, _ := ioutil.ReadAll(resp.Body)
	var jsonMap map[string]interface{}

	json.Unmarshal([]byte(b), &jsonMap)
	uri, ok := jsonMap["nextUri"].(string)
	if ok {
		fmt.Println(uri)
		r, err := http.NewRequest("GET", jsonMap["nextUri"].(string), nil)
		if err != nil {
			klog.Fatalf("error in creating GET request: %v", err)
		}
		response, err := http.DefaultClient.Do(r)
		if err != nil || response.StatusCode != http.StatusOK {
			klog.Fatalf("error in creating GET request: %v", err)
		}
		return response
	}
	return resp
}

func init() {
	rootCmd.AddCommand(trinoSchema)
}
