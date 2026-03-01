package longhorn_cosched

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
)

// PostBind implements the PostBindPlugin interface.
//
// It fires after the virt-launcher pod has been bound to a node. For opted-in
// pods whose Longhorn Volume CRD has an empty spec.nodeID (i.e. a brand-new
// volume that has never been attached before — "cold start"), PostBind writes
// the bound node name into spec.nodeID on the Longhorn Volume CRD.
//
// This solves the cold-start chicken-and-egg problem:
//   - At Filter/Score time, spec.nodeID is empty → the plugin cannot predict
//     which node Longhorn will pick, so the VM schedules freely.
//   - At PostBind time, the node is known → we write it into spec.nodeID so
//     Longhorn attaches the volume engine (and thus the share-manager) to that
//     same node.
//
// For warm restarts (spec.nodeID already set by Longhorn from a prior
// attachment), PostBind is a no-op — the Filter/Score extension points already
// constrained the pod to the correct node, and we must not overwrite a value
// that Longhorn manages.
func (p *Plugin) PostBind(ctx context.Context, _ *framework.CycleState, pod *corev1.Pod, nodeName string) {
	podKey := klog.KObj(pod)

	if !isOptedIn(pod) {
		return
	}

	if isMigrationTarget(pod) {
		klog.V(4).InfoS("LonghornCoSchedule/PostBind: migration target pod, skipping",
			"pod", podKey,
			"node", nodeName,
		)
		return
	}

	pvcNames := collectPVCNames(pod)
	if len(pvcNames) == 0 {
		return
	}

	for _, pvcName := range pvcNames {
		if err := p.pinLonghornVolumeToNode(ctx, pod, pvcName, nodeName); err != nil {
			// Log but do not fail — PostBind errors cannot be returned to the
			// scheduler as the binding has already been committed. The worst
			// case is that the share-manager lands on a different node for
			// this session (same as the pre-fix behaviour).
			klog.ErrorS(err, "LonghornCoSchedule/PostBind: failed to pin Longhorn volume to node",
				"pod", podKey,
				"pvcName", pvcName,
				"node", nodeName,
			)
		}
	}
}

// pinLonghornVolumeToNode sets spec.nodeID on the Longhorn Volume CRD for the
// given PVC to nodeName, but only when spec.nodeID is currently empty (cold
// start). If spec.nodeID is already set, this function is a no-op.
func (p *Plugin) pinLonghornVolumeToNode(ctx context.Context, pod *corev1.Pod, pvcName, nodeName string) error {
	podKey := klog.KObj(pod)

	// Resolve PVC → PV name.
	pvc, err := p.clientset.CoreV1().PersistentVolumeClaims(pod.Namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		klog.V(4).InfoS("LonghornCoSchedule/PostBind: PVC not found, skipping",
			"pod", podKey,
			"pvcName", pvcName,
			"error", err,
		)
		return nil
	}

	if !isRWX(pvc) {
		return nil // Not RWX — no share-manager, nothing to do.
	}

	pvName := pvc.Spec.VolumeName
	if pvName == "" {
		klog.V(4).InfoS("LonghornCoSchedule/PostBind: PVC not yet bound, skipping",
			"pod", podKey,
			"pvcName", pvcName,
		)
		return nil
	}

	// Read the current Longhorn Volume CRD.
	volObj, err := p.dynClient.Resource(longhornVolumeGVR).Namespace(LonghornNamespace).Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		klog.V(4).InfoS("LonghornCoSchedule/PostBind: Longhorn Volume CRD not found, skipping",
			"pod", podKey,
			"pvName", pvName,
			"error", err,
		)
		return nil
	}

	// Check whether spec.nodeID is already set.
	currentSpecNodeID := ""
	if spec, ok := volObj.Object["spec"].(map[string]interface{}); ok {
		currentSpecNodeID, _ = spec["nodeID"].(string)
	}

	if currentSpecNodeID != "" {
		// Warm restart — spec.nodeID was already set by a prior attachment.
		// Filter/Score already handled placement; do not overwrite.
		klog.V(4).InfoS("LonghornCoSchedule/PostBind: spec.nodeID already set, skipping patch",
			"pod", podKey,
			"pvName", pvName,
			"existingNodeID", currentSpecNodeID,
			"boundNode", nodeName,
		)
		return nil
	}

	// Cold start — spec.nodeID is empty. Patch it to the node the virt-launcher
	// was just bound to so Longhorn attaches the engine (and share-manager) there.
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"nodeID": nodeName,
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch for Longhorn Volume %q: %w", pvName, err)
	}

	_, err = p.dynClient.Resource(longhornVolumeGVR).Namespace(LonghornNamespace).Patch(
		ctx,
		pvName,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to patch Longhorn Volume %q spec.nodeID to %q: %w", pvName, nodeName, err)
	}

	klog.V(4).InfoS("LonghornCoSchedule/PostBind: patched Longhorn Volume spec.nodeID for cold-start co-location",
		"pod", podKey,
		"pvName", pvName,
		"node", nodeName,
	)
	return nil
}
