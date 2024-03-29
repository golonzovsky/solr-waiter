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
	solrResource = schema.GroupVersionResource{
		Group:    "solr.apache.org",
		Version:  "v1beta1",
		Resource: "solrclouds",
	}
)

type solrStatus struct {
	ready         int64
	total         int64
	upToDateNodes int64
}

func (s *solrStatus) isReady() bool {
	return s.ready == s.total && s.total == s.upToDateNodes
}

type config struct {
	timeoutInterval time.Duration
	initialDelay    time.Duration
	namespace       string
}

func main() {
	// interrupt handling
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	defer func() { signal.Stop(c) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-c: // first signal, cancel context
			cancel()
		case <-ctx.Done():
		}
		<-c // second signal, hard exit
		fmt.Fprintln(os.Stderr, "second interrupt, exiting")
		os.Exit(1)
	}()

	if err := NewRootCmd().ExecuteContext(ctx); err != nil {
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
		SilenceErrors:     true,
		RunE: func(cmd *cobra.Command, args []string) error {
			clusterName := args[0]
			cmd.SilenceUsage = true
			return doStuff(cmd.Context(), clusterName, *conf)
		},
	}
	cmd.Flags().DurationVar(&conf.timeoutInterval, "timeout", 10*time.Minute, "timeout interval. Set 0 to disable timeout")
	cmd.Flags().DurationVar(&conf.initialDelay, "initial-delay", 0*time.Second, "initial delay. Default 0 - no delay")
	cmd.Flags().StringVarP(&conf.namespace, "namespace", "n", "", "namespace of the cluster")
	cmd.MarkFlagRequired("namespace")
	return cmd
}

func doStuff(ctx context.Context, clusterName string, conf config) error {
	// construct k8s dynamic client here to fail fast on wrong config
	client, err := constructClient()
	if err != nil {
		return fmt.Errorf("unable to build k8s client: %s", err.Error())
	}

	// wait for initial delay
	if conf.initialDelay > 0 {
		fmt.Printf("waiting for %s before watch start\n", conf.initialDelay)
		waitCtx, cancel := watch.ContextWithOptionalTimeout(context.Background(), conf.initialDelay)
		defer cancel()
		// block wait here for initial delay or context cancel
		select {
		case <-ctx.Done():
			return fmt.Errorf("initial wait interrupted")
		case <-waitCtx.Done():
		}
		fmt.Printf("starting watcher for %s\n", clusterName)
	}

	// construct context with timeout
	ctx, cancel := watch.ContextWithOptionalTimeout(ctx, conf.timeoutInterval)
	defer cancel()

	// validate cluster exists
	_, err = client.Resource(solrResource).Namespace(conf.namespace).Get(ctx, clusterName, v1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to fetch cluster %s/%s: %s\n", conf.namespace, clusterName, err.Error())
	}

	// construct watcher
	informerReceiveObjectCh := make(chan *unstructured.Unstructured, 1)
	tweakListOpts := func(options *v1.ListOptions) {
		options.FieldSelector = fields.OneTermEqualSelector("metadata.name", clusterName).String()
	}
	target := dynamicinformer.NewFilteredDynamicSharedInformerFactory(client, 0, conf.namespace, tweakListOpts)
	target.ForResource(solrResource).Informer().AddEventHandler(republishToCh(informerReceiveObjectCh))
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
			printStatus(startTime, conf)
		case cluster := <-informerReceiveObjectCh:
			status, err := getClusterStatus(cluster)
			if err != nil {
				return fmt.Errorf("failed to fetch status of %s/%s: %s\n", conf.namespace, clusterName, err.Error())
			}
			if status.isReady() {
				printStatusMsg("cluster "+clusterName+" is ready. Exiting.", startTime)
				return nil
			}
			printStatusMsg(fmt.Sprintf("ready %d, total %d, upToDateNodes %d", status.ready, status.total, status.upToDateNodes), startTime)
		case <-ctx.Done():
			return fmt.Errorf("cluster %s is not ready. Exiting with timeout/interrupt", clusterName)
		}
	}

}

func printStatus(startTime time.Time, conf config) {
	timeoutIn := conf.timeoutInterval - calculateElapsed(startTime)
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

func republishToCh(rcvCh chan<- *unstructured.Unstructured) *cache.ResourceEventHandlerFuncs {
	return &cache.ResourceEventHandlerFuncs{
		// this is for "already ready" case
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
	upToDate, err := getStatusIntField(obj, "upToDateNodes")
	if err != nil {
		return solrStatus{}, err
	}
	return solrStatus{ready: ready, total: replicas, upToDateNodes: upToDate}, nil
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
