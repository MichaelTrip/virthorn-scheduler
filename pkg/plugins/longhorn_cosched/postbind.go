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

// PreBind implements the PreBindPlugin interface.
//
// It fires after the scheduler has selected a node but BEFORE the binding API
// call that assigns nodeName to the pod. This means kubelet has not yet seen
// the pod, so no VolumeAttachment has been triggered and Longhorn has not yet
// decided where to attach the volume engine.
//
// For opted-in pods whose Longhorn Volume CRD has an empty spec.nodeID
// (brand-new volume, "cold start"), PreBind writes the selected node name into
// spec.nodeID. Longhorn reads this field when it processes the subsequent
// VolumeAttachment and attaches the engine — and thus the share-manager pod —
// to that same node.
//
// For warm restarts (spec.nodeID already set by a prior attachment), PreBind
// is a no-op — Filter/Score already constrained the pod to the correct node.
//
// Why PreBind and not PostBind?
// PostBind fires AFTER the binding is committed: kubelet already knows the
// pod's nodeName and triggers a VolumeAttachment almost immediately. In
// testing, Longhorn resolved the attachment (and created the share-manager
// pod) within ~30ms of binding — faster than PostBind could write spec.nodeID.
// PreBind executes synchronously before the bind, guaranteeing our write lands
// before Longhorn processes any VolumeAttachment for the pod.
func (p *Plugin) PreBind(ctx context.Context, state *framework.CycleState, pod *corev1.Pod, nodeName string) *framework.Status {
	podKey := klog.KObj(pod)

	if !isOptedIn(pod) {
		return nil
	}

	if isMigrationTarget(pod) {
		klog.V(4).InfoS("LonghornCoSchedule/PreBind: migration target pod, skipping",
			"pod", podKey,
			"node", nodeName,
		)
		return nil
	}

	pvcNames := collectPVCNames(pod)
	if len(pvcNames) == 0 {
		return nil
	}

	for _, pvcName := range pvcNames {
		if err := p.pinLonghornVolumeToNode(ctx, pod, pvcName, nodeName); err != nil {
			klog.ErrorS(err, "LonghornCoSchedule/PreBind: failed to pin Longhorn volume to node",
				"pod", podKey,
				"pvcName", pvcName,
				"node", nodeName,
			)
			return framework.NewStatus(framework.Error,
				fmt.Sprintf("failed to pin Longhorn volume for PVC %q to node %q: %v", pvcName, nodeName, err))
		}
	}

	return nil
}

// pinLonghornVolumeToNode sets spec.nodeID on the Longhorn Volume CRD for the
// given PVC to nodeName, but only when spec.nodeID is currently empty (cold
// start). If spec.nodeID is already set, this function is a no-op.
func (p *Plugin) pinLonghornVolumeToNode(ctx context.Context, pod *corev1.Pod, pvcName, nodeName string) error {
	podKey := klog.KObj(pod)

	// Resolve PVC → PV name.
	pvc, err := p.clientset.CoreV1().PersistentVolumeClaims(pod.Namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		klog.V(4).InfoS("LonghornCoSchedule/PreBind: PVC not found, skipping",
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
		klog.V(4).InfoS("LonghornCoSchedule/PreBind: PVC not yet bound, skipping",
			"pod", podKey,
			"pvcName", pvcName,
		)
		return nil
	}

	// Read the current Longhorn Volume CRD.
	volObj, err := p.dynClient.Resource(longhornVolumeGVR).Namespace(LonghornNamespace).Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		klog.V(4).InfoS("LonghornCoSchedule/PreBind: Longhorn Volume CRD not found, skipping",
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
		klog.V(4).InfoS("LonghornCoSchedule/PreBind: spec.nodeID already set, skipping patch",
			"pod", podKey,
			"pvName", pvName,
			"existingNodeID", currentSpecNodeID,
			"selectedNode", nodeName,
		)
		return nil
	}

	// Cold start — spec.nodeID is empty. Patch it to the node the scheduler
	// has selected for the virt-launcher so Longhorn attaches the engine (and
	// share-manager) there. This write happens BEFORE the binding API call,
	// so Longhorn sees it before any VolumeAttachment is created.
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

	klog.V(4).InfoS("LonghornCoSchedule/PreBind: patched Longhorn Volume spec.nodeID for cold-start co-location",
		"pod", podKey,
		"pvName", pvName,
		"node", nodeName,
	)
	return nil
}
