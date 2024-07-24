/*
Script which queries the K8s VPA in every namespace (or scoped to a subset) and outputs the uncapped CPU and memory recommendations in CSV format.
The units are in K8s resource format to make it easier to copy into source control when updating services based on the recommendations.
*/

package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	verticalAutoscalingClientSet "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/client/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const resultsFile = "results.csv"

type containerConfig struct {
	namespace     string
	resourceType  string
	resourceName  string
	containerName string
	vpaName       string
	targetCPU     string
	targetMemory  string
}

func main() {
	l, err := getLogger()
	if err != nil {
		panic(err)
	}

	var namespaces []string
	n := flag.String("namespaces", "", "comma separated list of namespaces to query")
	flag.Parse()
	if *n != "" {
		namespaces = strings.Split(*n, ",")
		l.Info("Targeting specific namespaces", "namespaces", *n)
	}

	config, err := clientcmd.BuildConfigFromFlags("", filepath.Join(homedir.HomeDir(), ".kube", "config"))
	if err != nil {
		panic(err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	vpaClient, err := verticalAutoscalingClientSet.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	if len(namespaces) == 0 {
		namespaces, err = getNamespaces(clientset)
		if err != nil {
			panic(err.Error())
		}
	}

	results := make([]containerConfig, 0)

	for _, namespace := range namespaces {

		l.Debug("Processing namespace", "namespace", namespace)

		vpas, err := vpaClient.AutoscalingV1().VerticalPodAutoscalers(namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			panic(err.Error())
		}
		l.Debug("Found VPAs in namespace", "numVPAs", len(vpas.Items), "namespace", namespace)

		for _, vpa := range vpas.Items {
			for _, containerRecommendation := range vpa.Status.Recommendation.ContainerRecommendations {

				// Get uncapped memory recommendation and store in K8s format converted to MB
				t1 := containerRecommendation.UncappedTarget["memory"]
				memoryTargetBytes := t1.Value()
				memoryTargetMB := memoryTargetBytes / 1024 / 1024
				memoryTarget := fmt.Sprintf("%dMi", memoryTargetMB)

				// Get uncapped CPU recommendation. It's already in the correct K8s format
				t2 := containerRecommendation.UncappedTarget["cpu"]
				cpuTarget := t2.String()

				r := containerConfig{
					namespace:     namespace,
					resourceType:  vpa.Spec.TargetRef.Kind,
					resourceName:  vpa.Spec.TargetRef.Name,
					containerName: containerRecommendation.ContainerName,
					vpaName:       vpa.Name,
					targetCPU:     cpuTarget,
					targetMemory:  memoryTarget,
				}
				results = append(results, r)
			}
		}
	}

	l.Info("Container recommendation results", "count", len(results))

	err = writeResults(results)
	if err != nil {
		panic(err.Error())
	}
}

func writeResults(results []containerConfig) error {
	// csv package expects a slice of string slices. Each slice is a CSV row
	csvSource := make([][]string, 0, len(results))
	csvSource = append(csvSource, []string{"namespace", "resourceType", "resourceName", "containerName", "targetCPU", "targetMemory"})
	for _, r := range results {
		csvSource = append(csvSource, []string{r.namespace, r.resourceType, r.resourceName, r.containerName, r.targetCPU, r.targetMemory})
	}

	_ = os.Remove(resultsFile)
	f, err := os.Create(resultsFile)
	if err != nil {
		return fmt.Errorf("creating results file: %w", err)
	}

	w := csv.NewWriter(f)
	for _, record := range csvSource {
		if err := w.Write(record); err != nil {
			return fmt.Errorf("writing results to csv: %w", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("flushing csv writer: %w", err)
	}

	return nil
}

// getNamespaces returns all the namespaces in the cluster
func getNamespaces(client *kubernetes.Clientset) ([]string, error) {
	result := make([]string, 0)

	namespaces, err := client.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return result, fmt.Errorf("error listing namespaces: %v", err)
	}

	for _, ns := range namespaces.Items {
		result = append(result, ns.Name)
	}

	return result, nil
}

// getLogger creates a structured logger and defaults to error level (https://pkg.go.dev/log/slog#Level).
func getLogger() (*slog.Logger, error) {
	var logger *slog.Logger

	var logLevel = os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		// Default to info level
		logLevel = "0"
	}
	level, err := strconv.Atoi(logLevel)
	if err != nil {
		return logger, fmt.Errorf("error parsing LOG_LEVEL: %v", err)
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.Level(level)})
	logger = slog.New(handler)

	return logger, nil
}
