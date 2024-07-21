# readme

Contains Go scripts for programmatically deploying [K8s VPAs](https://github.com/kubernetes/autoscaler/tree/master/vertical-pod-autoscaler) 
for every deployment/statefulset/daemonset resource in the
cluster and then extracting the recommendations from each into a CSV format. Designed to be used as part of a pod
rightsizing exercise whilst taking some of the toil away.

Scripts:

- `/manage-vpas`: deploys a VPA for every deployment/statefulset/daemonset resource. Skips if a VPA already exists for that resource