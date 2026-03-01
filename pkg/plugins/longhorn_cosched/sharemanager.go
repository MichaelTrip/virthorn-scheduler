package longhorn_cosched

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// longhornVolumeGVR is the GroupVersionResource for the Longhorn Volume CRD.
// spec.nodeID on this object is the authoritative node assignment for the
// volume engine — the share-manager pod always runs on this same node.
var longhornVolumeGVR = schema.GroupVersionResource{
	Group:    "longhorn.io",
	Version:  "v1beta2",
	Resource: "volumes",
}

// findShareManagerNode looks up the node where the Longhorn share-manager for
// any of the RWX PVCs referenced by the given pod will run.
//
// Resolution order:
//  1. Longhorn Volume CRD (spec.nodeID / status.currentNodeID) — the volume
//     engine node is the authoritative source; the share-manager pod always
//     runs co-located with the engine.
//  2. Share-manager pod directly — used as a fallback for non-standard setups
//     or when the Volume CRD doesn't have a node assignment yet.
func findShareManagerNode(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, pod *corev1.Pod) (string, error) {
	podKey := klog.KObj(pod)
	pvcNames := collectPVCNames(pod)

	// DIAGNOSTIC: log how many PVCs were found on the pod's volumes.
	klog.V(4).InfoS("LonghornCoSchedule/findShareManagerNode: collected PVCs from pod volumes",
		"pod", podKey,
		"pvcCount", len(pvcNames),
		"pvcNames", pvcNames,
	)

	if len(pvcNames) == 0 {
		klog.V(4).InfoS("LonghornCoSchedule/findShareManagerNode: no PVCs found on pod, plugin is a no-op",
			"pod", podKey,
		)
		return "", nil
	}

	for _, pvcName := range pvcNames {
		node, err := getShareManagerNodeForPVC(ctx, clientset, dynClient, pod.Namespace, pvcName)
		if err != nil {
			return "", err
		}
		if node != "" {
			klog.V(4).InfoS("LonghornCoSchedule/findShareManagerNode: resolved share-manager node",
				"pod", podKey,
				"pvcName", pvcName,
				"shareManagerNode", node,
			)
			return node, nil
		}
	}

	klog.V(4).InfoS("LonghornCoSchedule/findShareManagerNode: no share-manager node found for any PVC",
		"pod", podKey,
		"pvcNames", pvcNames,
	)
	return "", nil
}

// collectPVCNames returns all PVC names referenced by the pod's volumes.
func collectPVCNames(pod *corev1.Pod) []string {
	var names []string
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil {
			names = append(names, vol.PersistentVolumeClaim.ClaimName)
		}
	}
	return names
}

// getShareManagerNodeForPVC resolves the node for the share-manager of a
// specific PVC. It tries the Longhorn Volume CRD first (authoritative), then
// falls back to inspecting the share-manager pod directly.
func getShareManagerNodeForPVC(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, podNamespace, pvcName string) (string, error) {
	// Verify the PVC exists and is RWX.
	pvc, err := clientset.CoreV1().PersistentVolumeClaims(podNamespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err != nil {
		// DIAGNOSTIC: log PVC lookup failures (could be RBAC or namespace issue).
		klog.V(4).InfoS("LonghornCoSchedule/getShareManagerNodeForPVC: PVC not found or error, skipping",
			"pvcName", pvcName,
			"namespace", podNamespace,
			"error", err,
		)
		return "", nil // PVC not found — skip silently.
	}

	// DIAGNOSTIC: log access modes so we can see why isRWX might return false.
	klog.V(4).InfoS("LonghornCoSchedule/getShareManagerNodeForPVC: PVC found",
		"pvcName", pvcName,
		"pvName", pvc.Spec.VolumeName,
		"accessModes", pvc.Spec.AccessModes,
		"isRWX", isRWX(pvc),
		"phase", pvc.Status.Phase,
	)

	if !isRWX(pvc) {
		klog.V(4).InfoS("LonghornCoSchedule/getShareManagerNodeForPVC: PVC is not RWX, skipping",
			"pvcName", pvcName,
			"accessModes", pvc.Spec.AccessModes,
		)
		return "", nil // Not RWX — Longhorn won't create a share-manager.
	}

	pvName := pvc.Spec.VolumeName
	if pvName == "" {
		klog.V(4).InfoS("LonghornCoSchedule/getShareManagerNodeForPVC: PVC not yet bound, skipping",
			"pvcName", pvcName,
		)
		return "", nil // PVC not yet bound.
	}

	// --- Primary: query the Longhorn Volume CRD (spec.nodeID) ---
	// The Volume CRD is named after the PV (e.g. pvc-<uid>) and lives in
	// longhorn-system. spec.nodeID is set by Longhorn when the volume is pinned
	// to a node — the share-manager pod always runs co-located with the volume
	// engine on this node. This is the authoritative source.
	//
	// Note: the ShareManager CRD's ownerID is NOT used here because it reflects
	// the Longhorn controller manager node (which longhorn-manager pod controls
	// the object), not the node where the share-manager pod runs.
	if dynClient != nil {
		node, err := getLonghornVolumeNode(ctx, dynClient, pvName)
		if err != nil {
			// DIAGNOSTIC: log Volume CRD errors explicitly.
			klog.V(4).InfoS("LonghornCoSchedule/getShareManagerNodeForPVC: Volume CRD lookup failed, falling back to pod lookup",
				"pvName", pvName,
				"error", err,
			)
		} else if node != "" {
			return node, nil
		}
	}

	// --- Fallback: inspect the share-manager pod directly ---
	return getShareManagerNodeFromPod(ctx, clientset, pvName)
}

// getLonghornVolumeNode reads the Longhorn Volume CRD for the given PV name
// and returns the node where the volume engine will run (or is running).
//
// Resolution priority within this CRD:
//  1. spec.nodeID        — set by Longhorn when the volume has been attached before;
//     Longhorn re-attaches to this node on the next use.
//  2. status.currentNodeID — the node the engine is currently attached to
//     (same value as spec.nodeID when attached).
//  3. status.ownerID     — the Longhorn manager node responsible for this volume.
//     When the volume is detached and has never been attached
//     (spec.nodeID=""), the owning manager schedules the attach
//     and picks a node with a healthy replica. In practice the
//     manager attaches the volume to a node close to itself,
//     which is where the share-manager pod will run.
func getLonghornVolumeNode(ctx context.Context, dynClient dynamic.Interface, pvName string) (string, error) {
	obj, err := dynClient.Resource(longhornVolumeGVR).Namespace(LonghornNamespace).Get(ctx, pvName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	spec, hasSpec := obj.Object["spec"].(map[string]interface{})
	status, hasStatus := obj.Object["status"].(map[string]interface{})

	var specNodeID, currentNodeID, ownerID, volumeState string
	if hasSpec {
		specNodeID, _ = spec["nodeID"].(string)
	}
	if hasStatus {
		currentNodeID, _ = status["currentNodeID"].(string)
		ownerID, _ = status["ownerID"].(string)
		volumeState, _ = status["state"].(string)
	}

	// DIAGNOSTIC: log all relevant Volume CRD fields.
	klog.V(4).InfoS("LonghornCoSchedule/getLonghornVolumeNode: Longhorn Volume CRD status",
		"pvName", pvName,
		"spec.nodeID", specNodeID,
		"status.currentNodeID", currentNodeID,
		"status.ownerID", ownerID,
		"status.state", volumeState,
	)

	// 1. spec.nodeID: Longhorn persists the last-used attachment node here.
	//    This is set from the first attachment onward and is the most reliable
	//    predictor of where the volume engine (and share-manager pod) will run.
	if specNodeID != "" {
		klog.V(4).InfoS("LonghornCoSchedule/getLonghornVolumeNode: using spec.nodeID",
			"pvName", pvName,
			"node", specNodeID,
		)
		return specNodeID, nil
	}

	// 2. status.currentNodeID: node the engine is currently attached to.
	//    Identical to spec.nodeID when attached; use as belt-and-suspenders fallback.
	if currentNodeID != "" {
		klog.V(4).InfoS("LonghornCoSchedule/getLonghornVolumeNode: spec.nodeID empty, using status.currentNodeID",
			"pvName", pvName,
			"node", currentNodeID,
		)
		return currentNodeID, nil
	}

	// spec.nodeID and currentNodeID are both empty: the volume has never been
	// attached (cold start). Do NOT fall back to status.ownerID — that field
	// reflects the Longhorn controller manager node (which longhorn-manager
	// pod owns this Volume object), not the node where the engine will attach.
	// Using it as a predictor causes the virt-launcher to be pinned to the
	// wrong node.
	//
	// Cold-start co-location is handled by the PostBind extension point instead:
	// after the virt-launcher is bound to a node, PostBind writes that node into
	// spec.nodeID so Longhorn attaches the engine (and share-manager) there.
	klog.V(4).InfoS("LonghornCoSchedule/getLonghornVolumeNode: spec.nodeID and currentNodeID are empty (cold start), returning empty — PostBind will pin the volume",
		"pvName", pvName,
		"status.ownerID", ownerID,
		"status.state", volumeState,
	)
	return "", nil
}

// getShareManagerNodeFromPod looks up the share-manager pod for a PV and
// returns the node it is running on. Returns empty string if not found or
// not yet scheduled.
func getShareManagerNodeFromPod(ctx context.Context, clientset kubernetes.Interface, pvName string) (string, error) {
	shareManagerName := fmt.Sprintf("%s%s", ShareManagerPrefix, pvName)
	smPod, err := clientset.CoreV1().Pods(LonghornNamespace).Get(ctx, shareManagerName, metav1.GetOptions{})
	if err != nil {
		// DIAGNOSTIC: log pod lookup failures — could be RBAC or the pod simply not created yet.
		klog.V(4).InfoS("LonghornCoSchedule/getShareManagerNodeFromPod: share-manager pod not found (may not exist yet)",
			"shareManagerName", shareManagerName,
			"error", err,
		)
		return "", nil // Pod doesn't exist yet — that's fine.
	}

	// DIAGNOSTIC: log the pod's phase and node so we can see why it may not match.
	klog.V(4).InfoS("LonghornCoSchedule/getShareManagerNodeFromPod: share-manager pod found",
		"shareManagerName", shareManagerName,
		"phase", smPod.Status.Phase,
		"nodeName", smPod.Spec.NodeName,
	)

	if smPod.Status.Phase == corev1.PodRunning && smPod.Spec.NodeName != "" {
		return smPod.Spec.NodeName, nil
	}

	klog.V(4).InfoS("LonghornCoSchedule/getShareManagerNodeFromPod: share-manager pod not yet running on a node, returning empty",
		"shareManagerName", shareManagerName,
		"phase", smPod.Status.Phase,
		"nodeName", smPod.Spec.NodeName,
	)
	return "", nil
}

// isRWX returns true if the PVC has ReadWriteMany access mode.
func isRWX(pvc *corev1.PersistentVolumeClaim) bool {
	for _, mode := range pvc.Spec.AccessModes {
		if mode == corev1.ReadWriteMany {
			return true
		}
	}
	return false
}
