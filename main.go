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
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/watch"
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
	timeoutInterval time.Duration
	initialDelay    time.Duration
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
	cmd.Flags().DurationVar(&conf.timeoutInterval, "timeout-interval", 10*time.Minute, "timeout interval. Set 0 to disable timeout. Default 10m")
	cmd.Flags().DurationVar(&conf.initialDelay, "initial-delay", 0*time.Second, "initial delay. Default 0s")
	cmd.Flags().StringVarP(&conf.namespace, "namespace", "n", "", "namespace of the cluster")
	cmd.MarkFlagRequired("namespace")
	return cmd
}

func doStuff(clusterName string, conf config) error {
	// construct context with timeout
	ctx, cancel := watch.ContextWithOptionalTimeout(context.Background(), conf.timeoutInterval)
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

	// construct watcher
	informerReceiveObjectCh := make(chan *unstructured.Unstructured, 1)
	tweakListOpts := func(options *v1.ListOptions) {
		options.FieldSelector = fields.OneTermEqualSelector("metadata.name", clusterName).String()
	}
	target := dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, 0, conf.namespace, tweakListOpts)
	target.ForResource(solrResource).Informer().AddEventHandler(resendEventToCh(informerReceiveObjectCh))
	target.Start(ctx.Done())
	if synced := target.WaitForCacheSync(ctx.Done()); !synced[solrResource] {
		return fmt.Errorf("informer for %s hasn't synced", solrResource)
	}

	// this is just for progress display, not used for polling
	startTime := time.Now()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	// main listener loop
	for {
		select {
		case <-ticker.C:
			printStatus(startTime, conf.timeoutInterval)
		case cluster := <-informerReceiveObjectCh:
			status, err := getClusterStatus(cluster)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to fetch status of %s/%s: %s\n", conf.namespace, clusterName, err.Error())
				os.Exit(1)
			}
			if status.ready == status.total {
				printStatusMsg("cluster "+clusterName+" is ready. Exiting.", startTime)
				return nil
			}
			printStatusMsg(fmt.Sprintf("ready %d out of %d", status.ready, status.total), startTime)
		case <-c:
			cancel()
			return nil
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "cluster %s is not ready. Exiting with timeout.\n", clusterName)
			os.Exit(1)
		}
	}

}

func printStatus(startTime time.Time, timeout time.Duration) {
	timeoutIn := timeout - calculateElapsed(startTime)
	if timeoutIn < 0 {
		printStatusMsg("still watching cluster", startTime)
	}
	printStatusMsg(fmt.Sprintf("still watching cluster (timeout in ~%s)", timeoutIn), startTime)
}

func printStatusMsg(msg string, startTime time.Time) {
	fmt.Printf("elapsed %s, %s\n", calculateElapsed(startTime), msg)
}

func calculateElapsed(startTime time.Time) time.Duration {
	return time.Now().Sub(startTime).Truncate(time.Second)
}

func resendEventToCh(rcvCh chan<- *unstructured.Unstructured) *cache.ResourceEventHandlerFuncs {
	return &cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			rcvCh <- obj.(*unstructured.Unstructured)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			rcvCh <- newObj.(*unstructured.Unstructured)
		},
	}
}

func getClusterStatus(obj *unstructured.Unstructured) (solrStatus, error) {
	ready, err := getStatusIntField(obj, "readyReplicas")
	if err != nil {
		return solrStatus{}, err
	}
	replicas, err := getStatusIntField(obj, "replicas")
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
	config, err := loadK8sConfig()
	if err != nil {
		return nil, err
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, err
	}
	return client, nil
}

func loadK8sConfig() (*rest.Config, error) {
	kubeconfLocation := filepath.Join(homedir.HomeDir(), ".kube", "config")
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfLocation)
	if err == nil {
		return config, nil
	}
	return rest.InClusterConfig()
}
