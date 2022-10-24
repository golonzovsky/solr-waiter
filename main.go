package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	solrResource = schema.GroupVersionResource{Group: "solr.apache.org", Version: "v1beta1", Resource: "solrclouds"}
)

type solrStatus struct {
	ready int64
	total int64
}

type config struct {
	retryInterval   time.Duration
	timeoutInterval time.Duration
	namespace       string
}

func main() {
	if err := NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func NewRootCmd() *cobra.Command {
	conf := &config{}
	cmd := &cobra.Command{
		Use:               "solr-waiter [cluster name] -n namespace",
		Short:             "solr-waiter waits for solrcloud to be ready",
		Args:              cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		ValidArgsFunction: cobra.NoFileCompletions,
		Run: func(cmd *cobra.Command, args []string) {
			clusterName := args[0]
			doStuff(clusterName, *conf)
		},
	}
	cmd.Flags().DurationVar(&conf.retryInterval, "retry-interval", 3*time.Second, "retry interval. Default 3s")
	cmd.Flags().DurationVar(&conf.timeoutInterval, "timeout-interval", 10*time.Minute, "timeout interval. Default 10m")
	cmd.Flags().StringVarP(&conf.namespace, "namespace", "n", "", "namespace of the cluster")
	cmd.MarkFlagRequired("namespace")
	return cmd
}

func doStuff(clusterName string, conf config) error {
	// construct context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), conf.timeoutInterval)
	defer cancel()

	// interrupt handling
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	defer func() { signal.Stop(c) }()

	// construct k8s dynamic client
	client, err := constructClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to build k8s client: %s", err.Error())
		os.Exit(1)
	}

	// prepare retries and timeouts
	ticker := time.NewTicker(conf.retryInterval)
	defer ticker.Stop()
	timeoutTime := time.Now().Add(conf.timeoutInterval)

	// main polling loop
	for {
		status, err := getClusterStatus(ctx, client, clusterName, conf.namespace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to fetch status of %s/%s: %s\n", conf.namespace, clusterName, err.Error())
			os.Exit(1)
		}
		if status.ready == status.total {
			fmt.Printf("cluster %s is ready. Exiting.", clusterName)
			return nil
		}
		timeoutIn := timeoutTime.Sub(time.Now()).Truncate(time.Second)
		fmt.Printf("ready %d out of %d, retry in %s, timeout in ~%s\n",
			status.ready, status.total, conf.retryInterval, timeoutIn)

		select {
		case <-ticker.C:
			continue
		case <-c:
			cancel()
			return nil
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "cluster %s is not ready. Exiting with timeout.\n", clusterName)
			os.Exit(1)
		}
	}

}

func objectHandler(rcvCh chan<- *unstructured.Unstructured) *cache.ResourceEventHandlerFuncs {
	return &cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			rcvCh <- obj.(*unstructured.Unstructured)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			rcvCh <- newObj.(*unstructured.Unstructured)
		},
	}
}

func getClusterStatus(ctx context.Context, client dynamic.Interface, name, namespace string) (solrStatus, error) {
	res, err := client.Resource(solrResource).Namespace(namespace).Get(ctx, name, v1.GetOptions{})
	if err != nil {
		return solrStatus{}, err
	}

	ready, err := getStatusIntField(res, "readyReplicas")
	if err != nil {
		return solrStatus{}, err
	}

	replicas, err := getStatusIntField(res, "replicas")
	if err != nil {
		return solrStatus{}, err
	}

	return solrStatus{ready: ready, total: replicas}, nil
}

func getStatusIntField(res *unstructured.Unstructured, fieldName string) (int64, error) {
	fieldValue, ok, err := unstructured.NestedInt64(res.Object, "status", fieldName)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("readyReplicas type invalid")
	}
	return fieldValue, nil
}

func constructClient() (dynamic.Interface, error) {
	config, err := loadConfig()
	if err != nil {
		return nil, err
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func loadConfig() (*rest.Config, error) {
	kubeconfLocation := filepath.Join(homedir.HomeDir(), ".kube", "config")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfLocation)
	if err != nil {
		return rest.InClusterConfig()
	}
	return config, nil
}
