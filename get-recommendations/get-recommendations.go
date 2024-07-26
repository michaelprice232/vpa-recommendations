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

	autoscaling "k8s.io/api/autoscaling/v2"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	verticalAutoscalingClientSet "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/client/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

const resultsFile = "results.csv"

type containerConfig struct {
	namespace       string
	resourceType    string
	resourceName    string
	containerName   string
	vpaName         string
	targetCPUStr    string
	targetMemoryStr string
	currentConfig   resourceDrift
	hasHPA          bool
}

type resourceDrift struct {
	currentCPUStr string
	currentMemStr string
	currentCPU    int64
	currentMem    int64
	cpuDiff       int64
	memDiff       int64
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

		// Get HPA targets for this namespace
		hasHPAMapping, err := hpaMappings(clientset, namespace)
		if err != nil {
			panic(err.Error())
		}

		vpas, err := vpaClient.AutoscalingV1().VerticalPodAutoscalers(namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			panic(err.Error())
		}
		l.Debug("Found VPAs in namespace", "numVPAs", len(vpas.Items), "namespace", namespace)

		for _, vpa := range vpas.Items {

			// Skip VPA if the target resource does not exist
			exists, err := resourceExists(vpa.Spec.TargetRef.Name, vpa.Spec.TargetRef.Kind, namespace, clientset)
			if err != nil {
				panic(err.Error())
			}
			if !exists {
				l.Info("target does not exist. Skipping", "namespace", namespace, "vpa", vpa.Name, "resourceType", vpa.Spec.TargetRef.Kind, "resourceName", vpa.Spec.TargetRef.Name)
				continue
			}

			for _, containerRecommendation := range vpa.Status.Recommendation.ContainerRecommendations {

				// Get uncapped memory recommendation and store in K8s format converted to MB
				t1 := containerRecommendation.UncappedTarget["memory"]
				memoryTargetBytes := t1.Value()
				memoryTargetMB := memoryTargetBytes / 1024 / 1024
				memoryTarget := fmt.Sprintf("%dMi", memoryTargetMB)

				// Get uncapped CPU recommendation. It's already in the correct K8s format
				t2 := containerRecommendation.UncappedTarget["cpu"]
				cpuTargetStr := t2.String()
				cpuTargetRaw := t2.MilliValue()

				// Get the current container resource config and calculate the diff from the recommendation
				resourceConfig, err := currentResourceConfig(vpa.Spec.TargetRef.Name, vpa.Spec.TargetRef.Kind, containerRecommendation.ContainerName, namespace, clientset, l)
				if err != nil {
					panic(err.Error())
				}

				r := containerConfig{
					namespace:       namespace,
					resourceType:    vpa.Spec.TargetRef.Kind,
					resourceName:    vpa.Spec.TargetRef.Name,
					containerName:   containerRecommendation.ContainerName,
					vpaName:         vpa.Name,
					targetCPUStr:    cpuTargetStr,
					targetMemoryStr: memoryTarget,
					currentConfig:   resourceConfig,
				}

				if resourceConfig.currentCPUStr != "NOT_SET" {
					r.currentConfig.cpuDiff = cpuTargetRaw - resourceConfig.currentCPU
				}

				if resourceConfig.currentMemStr != "NOT_SET" {
					r.currentConfig.memDiff = memoryTargetBytes - resourceConfig.currentMem
				}

				for _, hpa := range hasHPAMapping {
					if strings.ToLower(hpa.Kind) == strings.ToLower(r.resourceType) && strings.ToLower(hpa.Name) == strings.ToLower(r.resourceName) {
						r.hasHPA = true
					}
				}

				l.Debug("Container resourceConfig", "container", r.containerName, "currentCPURaw", resourceConfig.currentCPU, "currentMemoryRaw", resourceConfig.currentMem, "recommendedMemory", memoryTargetBytes, "recommendedCPU", cpuTargetRaw, "hasHPA", r.hasHPA)

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

// hpaMappings returns a slice containing the targets of every HPA in a namespace
func hpaMappings(clientset *kubernetes.Clientset, namespace string) ([]autoscaling.CrossVersionObjectReference, error) {
	hpas, err := clientset.AutoscalingV2().HorizontalPodAutoscalers(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("error getting HPAs: %v", err)
	}
	hasHPAMapping := make([]autoscaling.CrossVersionObjectReference, 0, len(hpas.Items))
	for _, hpa := range hpas.Items {
		hasHPAMapping = append(hasHPAMapping, hpa.Spec.ScaleTargetRef)
	}

	return hasHPAMapping, nil
}

func currentResourceConfig(resourceName, resourceType, containerName, namespace string, client *kubernetes.Clientset, logger *slog.Logger) (resourceDrift, error) {
	d := resourceDrift{}

	switch resourceType {
	case "Deployment":
		deployment, err := client.AppsV1().Deployments(namespace).Get(context.TODO(), resourceName, metav1.GetOptions{})
		if err != nil {
			return d, fmt.Errorf("error getting deployment %s/%s: %v", namespace, resourceName, err)
		}
		d = getContainerResourceConfig(deployment.Spec.Template.Spec.Containers, containerName, logger)

	case "StatefulSet":
		statefulset, err := client.AppsV1().StatefulSets(namespace).Get(context.TODO(), resourceName, metav1.GetOptions{})
		if err != nil {
			return d, fmt.Errorf("error getting statefuleset %s/%s: %v", namespace, resourceName, err)
		}
		d = getContainerResourceConfig(statefulset.Spec.Template.Spec.Containers, containerName, logger)

	case "DaemonSet":
		daemonset, err := client.AppsV1().DaemonSets(namespace).Get(context.TODO(), resourceName, metav1.GetOptions{})
		if err != nil {
			return d, fmt.Errorf("error getting daemonsets %s/%s: %v", namespace, resourceName, err)
		}
		d = getContainerResourceConfig(daemonset.Spec.Template.Spec.Containers, containerName, logger)
	}

	return d, nil
}

func getContainerResourceConfig(containers []v1.Container, containerName string, _ *slog.Logger) resourceDrift {
	d := resourceDrift{}

	for _, container := range containers {
		if strings.ToLower(container.Name) == strings.ToLower(containerName) {
			cpu := container.Resources.Requests.Cpu().MilliValue()
			if cpu == 0 {
				d.currentCPUStr = "NOT_SET"
			} else {
				d.currentCPUStr = fmt.Sprintf("%dm", cpu)
				d.currentCPU = container.Resources.Requests.Cpu().MilliValue()
			}

			mem := fmt.Sprintf("%dMi", container.Resources.Requests.Memory().Value()/1024/1024)
			if mem == "0Mi" {
				d.currentMemStr = "NOT_SET"
			} else {
				d.currentMemStr = mem
				d.currentMem = container.Resources.Requests.Memory().Value()
			}

			break
		}
	}

	return d
}

func resourceExists(resourceName, resourceType, namespace string, client *kubernetes.Clientset) (bool, error) {
	switch resourceType {
	case "Deployment":
		_, err := client.AppsV1().Deployments(namespace).Get(context.TODO(), resourceName, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("error getting deployment %s (%s): %v", resourceName, namespace, err)
		}

	case "StatefulSet":
		_, err := client.AppsV1().StatefulSets(namespace).Get(context.TODO(), resourceName, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("error getting statefuleset %s (%s): %v", resourceName, namespace, err)
		}

	case "DaemonSet":
		_, err := client.AppsV1().DaemonSets(namespace).Get(context.TODO(), resourceName, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			return false, nil
		} else if err != nil {
			return false, fmt.Errorf("error getting daemonset %s (%s): %v", resourceName, namespace, err)
		}
	}

	return true, nil
}

func writeResults(results []containerConfig) error {
	// csv package expects a slice of string slices. Each slice is a CSV row
	csvSource := make([][]string, 0, len(results))
	csvSource = append(csvSource, []string{"namespace", "resourceType", "resourceName", "containerName", "VPA Target CPU", "VPA Target Memory", "Current CPU Requests", "Current Memory Requests", "CPU Diff (VPA-Current)", "Memory Diff (VPA-Current)", "HPA Enabled"})
	for _, r := range results {
		csvSource = append(csvSource, []string{r.namespace, r.resourceType, r.resourceName, r.containerName, r.targetCPUStr, r.targetMemoryStr, r.currentConfig.currentCPUStr, r.currentConfig.currentMemStr, fmt.Sprintf("%d", r.currentConfig.cpuDiff), fmt.Sprintf("%d", r.currentConfig.memDiff), fmt.Sprintf("%t", r.hasHPA)})
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
