/*
Script which deploys a K8s VPA for deployment/statefulset/daemonset resources across all namespaces.
If a VPA already exists which targets the resource then it is skipped.
*/
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	autoscaling "k8s.io/api/autoscaling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	verticalAutoscaling "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/apis/autoscaling.k8s.io/v1"
	verticalAutoscalingClientSet "k8s.io/autoscaler/vertical-pod-autoscaler/pkg/client/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// Random suffix applied to all created resources to avoid potential name clashes with source control managed resources
const vpaSuffix = "8dn39"

func main() {
	l, err := getLogger()
	if err != nil {
		panic(err)
	}

	var namespaces []string
	n := flag.String("namespaces", "", "comma separated list of namespaces to target")
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

	for _, namespace := range namespaces {
		l.Debug("Processing namespace", "namespace", namespace)

		resources, err := aggregateResourceNames(clientset, namespace, l)
		if err != nil {
			panic(err.Error())
		}

		for _, r := range resources {
			// Refresh VPAs list for namespace as one may be created by createVPA. This could be more efficient.
			vpas, err := vpaClient.AutoscalingV1().VerticalPodAutoscalers(namespace).List(context.TODO(), metav1.ListOptions{})
			if err != nil {
				panic(err.Error())
			}
			l.Debug("Found VPAs in namespace", "numVPAs", len(vpas.Items), "namespace", namespace)

			err = createVPA(namespace, r.apiGroup, r.resourceType, r.resourceName, vpas.Items, vpaClient, l)
			if err != nil {
				panic(err.Error())
			}
		}
	}
}

type resource struct {
	apiGroup     string
	resourceType string
	resourceName string
}

// aggregateResourceNames returns a slice containing deployments, statefulsets and daemonsets in a namespace, for later processing.
// If a resource is owned by another resource (has an owner reference) the parent resource details are returned instead, as this is required by the VPA.
func aggregateResourceNames(clientSet *kubernetes.Clientset, namespace string, l *slog.Logger) ([]resource, error) {
	results := make([]resource, 0)

	deployments, err := clientSet.AppsV1().Deployments(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return results, fmt.Errorf("error querying for deployents in %s namespace: %w", namespace, err)
	}
	l.Debug("Found deployments in namespace", "numDeployments", len(deployments.Items), "namespace", namespace)

	statefulsets, err := clientSet.AppsV1().StatefulSets(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return results, fmt.Errorf("error querying for statefulsets in %s namespace: %w", namespace, err)
	}
	l.Debug("Found statefulsets in namespace", "numStatefulsets", len(statefulsets.Items), "namespace", namespace)

	daemonsets, err := clientSet.AppsV1().DaemonSets(namespace).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return results, fmt.Errorf("error querying for daemonsets in %s namespace: %w", namespace, err)
	}
	l.Debug("Found daemonsets in namespace", "numDaemonsets", len(daemonsets.Items), "namespace", namespace)

	for _, d := range deployments.Items {
		// Check whether the resource is managed by a parent resource
		if found, r := checkOwnedBy(d.ObjectMeta); found {
			results = append(results, resource{resourceType: r.resourceType, resourceName: r.resourceName, apiGroup: r.apiGroup})
			l.Debug("resource owned by another controller", "childResource", d.Name, "parentType", r.resourceType, "parentName", r.resourceName, "parentAPIGroup", r.apiGroup)
			continue
		}
		results = append(results, resource{resourceType: "Deployment", resourceName: d.Name, apiGroup: "apps/v1"})
	}

	for _, s := range statefulsets.Items {
		// Check whether the resource is managed by a parent resource
		if found, r := checkOwnedBy(s.ObjectMeta); found {
			results = append(results, resource{resourceType: r.resourceType, resourceName: r.resourceName, apiGroup: r.apiGroup})
			l.Debug("resource owned by another controller", "childResource", s.Name, "parentType", r.resourceType, "parentName", r.resourceName, "parentAPIGroup", r.apiGroup)
			continue
		}
		results = append(results, resource{resourceType: "StatefulSet", resourceName: s.Name, apiGroup: "apps/v1"})
	}

	for _, d := range daemonsets.Items {
		// Check whether the resource is managed by a parent resource
		if found, r := checkOwnedBy(d.ObjectMeta); found {
			results = append(results, resource{resourceType: r.resourceType, resourceName: r.resourceName, apiGroup: r.apiGroup})
			l.Debug("resource owned by another controller", "childResource", d.Name, "parentType", r.resourceType, "parentName", r.resourceName, "parentAPIGroup", r.apiGroup)
			continue
		}
		results = append(results, resource{resourceType: "DaemonSet", resourceName: d.Name, apiGroup: "apps/v1"})
	}

	return results, nil
}

// checkOwnedBy returns true if the resource is managed by another resource, as well as the owner resource details.
func checkOwnedBy(m metav1.ObjectMeta) (bool, resource) {
	if len(m.OwnerReferences) == 0 {
		return false, resource{}
	}

	// Look for the controller. Only ever contains one.
	for _, ref := range m.OwnerReferences {
		if *ref.Controller {
			return true, resource{
				apiGroup:     ref.APIVersion,
				resourceType: ref.Kind,
				resourceName: ref.Name,
			}
		}
	}

	return false, resource{}
}

// createVPA creates a new VPA for a target object, if one does not already exist.
func createVPA(namespace, apiGroup, resourceType, resourceName string, vpas []verticalAutoscaling.VerticalPodAutoscaler, vpaClient *verticalAutoscalingClientSet.Clientset, l *slog.Logger) error {
	targetRef := autoscaling.CrossVersionObjectReference{
		APIVersion: apiGroup,
		Kind:       resourceType,
		Name:       resourceName,
	}

	// Skip if there is an existing VPA with the same config in this namespace
	if found, existingVPAName := containsVPATarget(&targetRef, vpas); found {
		l.Info("Found existing VPA. Skipping", "existingVPAName", existingVPAName, "resourceType", resourceType, "resourceName", resourceName)
		return nil
	}

	// Run in recommendation only mode
	var updateMode verticalAutoscaling.UpdateMode = "Off"

	vpa := verticalAutoscaling.VerticalPodAutoscaler{

		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-vpa-%s", resourceName, vpaSuffix),

			Labels: map[string]string{
				"source-control-managed": "false",
				"managed-by":             "vpa-recommendations-script",
			},
		},

		Spec: verticalAutoscaling.VerticalPodAutoscalerSpec{
			TargetRef: &targetRef,
			UpdatePolicy: &verticalAutoscaling.PodUpdatePolicy{
				UpdateMode: &updateMode,
			},
		},
	}

	_, err := vpaClient.AutoscalingV1().VerticalPodAutoscalers(namespace).Create(context.TODO(), &vpa, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("error creating VPA for %s/%s: %w", resourceType, resourceName, err)
	}
	l.Info("Created VPA", "vpaName", vpa.Name, "namespace", namespace)

	return nil
}

// containsVPATarget returns true, including the VPA name, if a VPA target (spec) is already defined in vpas.
func containsVPATarget(spec *autoscaling.CrossVersionObjectReference, vpas []verticalAutoscaling.VerticalPodAutoscaler) (bool, string) {
	found := false
	existingVPAName := ""

	for _, vpa := range vpas {
		if vpa.Spec.TargetRef.Name == spec.Name && vpa.Spec.TargetRef.Kind == spec.Kind && vpa.Spec.TargetRef.APIVersion == spec.APIVersion {
			found = true
			existingVPAName = vpa.Name
			break
		}
	}

	return found, existingVPAName
}

// getNamespaces returns all the namespaces in the cluster
func getNamespaces(client *kubernetes.Clientset) ([]string, error) {
	result := make([]string, 0)

	namespaces, err := client.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return result, err
	}

	for _, ns := range namespaces.Items {
		result = append(result, ns.Name)
	}

	return result, nil
}

// getLogger creates structured logger which defaults to info level (https://pkg.go.dev/log/slog#Level).
func getLogger() (*slog.Logger, error) {
	var logger *slog.Logger

	var logLevel = os.Getenv("LOG_LEVEL")
	if logLevel == "" {
		// Default to info level
		logLevel = "0"
	}
	level, err := strconv.Atoi(logLevel)
	if err != nil {
		return logger, fmt.Errorf("error parsing LOG_LEVEL: %w", err)
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.Level(level)})
	logger = slog.New(handler)

	return logger, nil
}
