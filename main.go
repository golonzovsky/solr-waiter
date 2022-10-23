package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var (
	solrResource    = schema.GroupVersionResource{Group: "solr.apache.org", Version: "v1beta1", Resource: "solrclouds"}
	retryInterval   = 3 * time.Second
	timeoutInterval = 10 * time.Minute
	clusterName     = "solr-test"
)

type solrStatus struct {
	ready int64
	total int64
}

func main() {
	// construct context with interrupt handling
	ctx, cancel := context.WithCancel(context.Background())
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	defer func() {
		signal.Stop(c)
		cancel()
	}()

	// construct k8s dynamic client
	client, err := constructClient()
	if err != nil {
		panic(err)
	}

	// prepare retries and timeouts
	ticker := time.NewTicker(retryInterval)
	defer ticker.Stop()
	timeoutC := time.After(timeoutInterval)
	timeoutTime := time.Now().Add(timeoutInterval)

	for {
		status, err := getClusterStatus(ctx, client, clusterName)
		if err != nil {
			panic(err)
		}
		if status.ready == status.total {
			fmt.Printf("cluster %s is ready. Exiting.", clusterName)
			return
		}
		timeoutIn := timeoutTime.Sub(time.Now()).Truncate(time.Second)
		fmt.Printf("ready %d out of %d, retry in %s, timeout in ~%s\n",
			status.ready, status.total, retryInterval, timeoutIn)

		select {
		case <-ticker.C:
			continue
		case <-timeoutC:
			fmt.Fprintf(os.Stderr, "cluster %s is not ready. Exiting with timeout.", clusterName)
			os.Exit(1)
		case <-c:
			cancel()
			return
		case <-ctx.Done():
		}
	}

}

func getClusterStatus(ctx context.Context, client dynamic.Interface, name string) (solrStatus, error) {
	res, err := client.Resource(solrResource).Namespace("sandbox").Get(ctx, name, v1.GetOptions{})
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
