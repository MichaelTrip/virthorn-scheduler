package longhorn_cosched

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// Filter implements the FilterPlugin interface.
//
// If the pod has the co-scheduling annotation and a Longhorn share-manager pod
// is already running for one of its RWX PVCs, only the node where the
// share-manager is running will pass the filter. All other nodes are rejected
// with an Unschedulable status.
//
// If the pod does not have the annotation, is a migration target, or no
// share-manager pod is found, all nodes pass (the plugin is a no-op).
func (p *Plugin) Filter(ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	podKey := klog.KObj(pod)

	// DIAGNOSTIC: log scheduler name and opt-in annotation for every pod that enters Filter.
	klog.V(4).InfoS("LonghornCoSchedule/Filter: evaluating pod",
		"pod", podKey,
		"schedulerName", pod.Spec.SchedulerName,
		"annotations", pod.Annotations,
		"labels", pod.Labels,
	)

	if !isOptedIn(pod) {
		klog.V(4).InfoS("LonghornCoSchedule/Filter: pod not opted in, skipping",
			"pod", podKey,
			"schedulerName", pod.Spec.SchedulerName,
			"annotationKey", AnnotationKey,
			"annotationValue", func() string {
				if pod.Annotations != nil {
					return pod.Annotations[AnnotationKey]
				}
				return "<no annotations>"
			}(),
		)
		return nil
	}

	if isMigrationTarget(pod) {
		klog.V(4).InfoS("LonghornCoSchedule/Filter: migration target pod, skipping (KubeVirt migration controller handles placement)",
			"pod", podKey,
			"migrationJobUID", pod.Labels[MigrationTargetLabel],
		)
		return nil
	}

	node := nodeInfo.Node()
	if node == nil {
		return framework.NewStatus(framework.Error, "node not found")
	}

	shareManagerNode, err := findShareManagerNode(ctx, p.clientset, p.dynClient, pod)
	if err != nil {
		klog.ErrorS(err, "LonghornCoSchedule/Filter: error looking up share-manager", "pod", podKey)
		return framework.NewStatus(framework.Error, fmt.Sprintf("error looking up share-manager pod: %v", err))
	}

	// No share-manager found yet — allow all nodes (VM schedules freely).
	if shareManagerNode == "" {
		klog.V(4).InfoS("LonghornCoSchedule/Filter: no share-manager found, all nodes pass",
			"pod", podKey,
			"node", node.Name,
		)
		return nil
	}

	// Share-manager is running on a specific node — only allow that node.
	if node.Name != shareManagerNode {
		klog.V(4).InfoS("LonghornCoSchedule/Filter: node rejected (share-manager on different node)",
			"pod", podKey,
			"node", node.Name,
			"shareManagerNode", shareManagerNode,
		)
		return framework.NewStatus(
			framework.Unschedulable,
			fmt.Sprintf("node %q rejected: Longhorn share-manager pod is running on node %q", node.Name, shareManagerNode),
		)
	}

	klog.V(4).InfoS("LonghornCoSchedule/Filter: node accepted (share-manager co-located)",
		"pod", podKey,
		"node", node.Name,
		"shareManagerNode", shareManagerNode,
	)
	return nil
}
