// Package longhorn_cosched implements a Kubernetes scheduling framework plugin
// that co-schedules KubeVirt VM pods with their Longhorn RWX share-manager pods
// on the same node.
package longhorn_cosched

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	// Name is the name of the plugin used in the plugin registry and configurations.
	Name = "LonghornCoSchedule"

	// AnnotationKey is the opt-in annotation that must be set on a pod to enable
	// co-scheduling with its Longhorn share-manager pod.
	AnnotationKey = "scheduler.virthorn-scheduler.io/co-schedule"

	// AnnotationValue is the value the annotation must have to opt in.
	AnnotationValue = "true"

	// LonghornNamespace is the namespace where Longhorn share-manager pods run.
	LonghornNamespace = "longhorn-system"

	// ShareManagerPrefix is the prefix used by Longhorn for share-manager pod names.
	// The full name is: share-manager-<pv-name>
	ShareManagerPrefix = "share-manager-"

	// MigrationTargetLabel is the KubeVirt label set on virt-launcher pods that
	// are being created as the target of a live migration. Its value is the UID
	// of the VirtualMachineInstanceMigration object. The plugin must not
	// constrain these pods — the migration subsystem handles node selection.
	MigrationTargetLabel = "kubevirt.io/migrationJobUID"
)

// Plugin implements the Filter and Score extension points of the Kubernetes
// Scheduling Framework to co-locate VM pods with their Longhorn share-manager pods.
type Plugin struct {
	handle    framework.Handle
	clientset kubernetes.Interface
	dynClient dynamic.Interface
}

var _ framework.FilterPlugin = &Plugin{}
var _ framework.ScorePlugin = &Plugin{}

// Name returns the name of the plugin.
func (p *Plugin) Name() string {
	return Name
}

// New creates a new instance of the LonghornCoSchedule plugin.
func New(_ context.Context, _ runtime.Object, h framework.Handle) (framework.Plugin, error) {
	clientset, err := kubernetes.NewForConfig(h.KubeConfig())
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(h.KubeConfig())
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	return &Plugin{
		handle:    h,
		clientset: clientset,
		dynClient: dynClient,
	}, nil
}

// isOptedIn returns true if the pod has the co-scheduling annotation set to "true".
func isOptedIn(pod *corev1.Pod) bool {
	if pod.Annotations == nil {
		return false
	}
	return pod.Annotations[AnnotationKey] == AnnotationValue
}

// isMigrationTarget returns true if the pod is a KubeVirt live-migration target
// pod. KubeVirt sets the label "kubevirt.io/migrationJobUID" to the UID of the
// VirtualMachineInstanceMigration object on the target virt-launcher pod.
// The plugin must be a no-op for these pods: the KubeVirt migration controller
// already selects the destination node via node affinity, and constraining it
// to the share-manager node would break live migration.
func isMigrationTarget(pod *corev1.Pod) bool {
	if pod.Labels == nil {
		return false
	}
	uid, ok := pod.Labels[MigrationTargetLabel]
	return ok && uid != ""
}
