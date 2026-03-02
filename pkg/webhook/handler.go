// Package webhook implements a Kubernetes mutating admission webhook that
// co-locates KubeVirt virt-launcher pods with their Longhorn RWX share-manager
// pods on the same node by injecting nodeAffinity rules.
//
// Opt-in: only pods with the annotation
//
//	scheduler.virthorn-scheduler.io/co-schedule: "true"
//
// are processed. Live-migration target pods (kubevirt.io/migrationJobUID label)
// are always passed through unchanged.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
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

	// ShareManagerLabelKey is the Longhorn label key set on share-manager pods.
	// The actual label is longhorn.io/component=share-manager.
	// Note: Longhorn does NOT set app=longhorn-share-manager.
	ShareManagerLabelKey = "longhorn.io/component"

	// ShareManagerLabelValue is the value of the longhorn.io/component label on share-manager pods.
	ShareManagerLabelValue = "share-manager"

	// MigrationTargetLabel is the KubeVirt label set on virt-launcher pods that
	// are being created as the target of a live migration.
	MigrationTargetLabel = "kubevirt.io/migrationJobUID"

	// VirtLauncherLabel is the label key KubeVirt sets on all virt-launcher pods.
	// The actual label is kubevirt.io=virt-launcher (key="kubevirt.io", value="virt-launcher").
	VirtLauncherLabel = "kubevirt.io"

	// VirtLauncherLabelValue is the value of the kubevirt.io label on virt-launcher pods.
	VirtLauncherLabelValue = "virt-launcher"
)

// AffinityMode controls whether the injected nodeAffinity is hard (required)
// or soft (preferred / best-effort).
type AffinityMode string

const (
	// AffinityModeRequired uses requiredDuringSchedulingIgnoredDuringExecution.
	// The pod will only schedule on the co-location node; it stays Pending if
	// that node has resource pressure.
	AffinityModeRequired AffinityMode = "required"

	// AffinityModePreferred uses preferredDuringSchedulingIgnoredDuringExecution
	// with weight 100 (best-effort). The scheduler strongly prefers the
	// co-location node but will schedule elsewhere if it has resource pressure.
	AffinityModePreferred AffinityMode = "preferred"
)

// Handler handles mutating admission webhook requests.
type Handler struct {
	client       kubernetes.Interface
	affinityMode AffinityMode
}

// NewHandler creates a new webhook Handler.
// affinityMode controls whether co-location is enforced (required) or
// best-effort (preferred). Defaults to AffinityModePreferred if empty.
func NewHandler(client kubernetes.Interface, affinityMode AffinityMode) *Handler {
	if affinityMode == "" {
		affinityMode = AffinityModePreferred
	}
	return &Handler{client: client, affinityMode: affinityMode}
}

// Handle processes an AdmissionReview request and returns a response.
func (h *Handler) Handle(ctx context.Context, req *admissionv1.AdmissionRequest) *admissionv1.AdmissionResponse {
	if req.Kind.Kind != "Pod" {
		return allow(req.UID)
	}

	var pod corev1.Pod
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		klog.ErrorS(err, "webhook: failed to unmarshal pod", "uid", req.UID)
		return allowWithWarning(req.UID, fmt.Sprintf("failed to unmarshal pod: %v", err))
	}

	// Determine which type of pod this is.
	if isVirtLauncher(&pod) {
		return h.handleVirtLauncher(ctx, req, &pod)
	}
	if isShareManager(&pod) {
		return h.handleShareManager(ctx, req, &pod)
	}

	// Not a pod type we care about.
	return allow(req.UID)
}

// handleVirtLauncher processes a virt-launcher pod admission request.
// It injects nodeAffinity to co-locate the VM with its share-manager pod.
func (h *Handler) handleVirtLauncher(ctx context.Context, req *admissionv1.AdmissionRequest, pod *corev1.Pod) *admissionv1.AdmissionResponse {
	// Use pod.Namespace as the authoritative namespace — req.Namespace may be
	// empty for cluster-scoped webhook configurations.
	namespace := pod.Namespace
	if namespace == "" {
		namespace = req.Namespace
	}
	podKey := fmt.Sprintf("%s/%s", namespace, pod.Name)

	// Check opt-in annotation.
	if !isOptedIn(pod) {
		klog.V(5).InfoS("webhook/virt-launcher: not opted in, skipping", "pod", podKey)
		return allow(req.UID)
	}

	// Skip live-migration target pods — KubeVirt migration controller handles placement.
	if isMigrationTarget(pod) {
		klog.V(4).InfoS("webhook/virt-launcher: migration target pod, skipping",
			"pod", podKey,
			"migrationJobUID", pod.Labels[MigrationTargetLabel],
		)
		return allow(req.UID)
	}

	// Find the share-manager node for any RWX PVC on this pod.
	// Use the resolved namespace (pod.Namespace), not req.Namespace which is empty
	// for cluster-scoped webhook configurations.
	shareManagerNode, shareManagerPodName, err := h.findShareManagerForVirtLauncher(ctx, namespace, pod)
	if err != nil {
		klog.ErrorS(err, "webhook/virt-launcher: error looking up share-manager", "pod", podKey)
		// Fail open: allow the pod without affinity rather than blocking it.
		return allowWithWarning(req.UID, fmt.Sprintf("share-manager lookup failed: %v", err))
	}

	if shareManagerNode == "" {
		klog.V(4).InfoS("webhook/virt-launcher: no share-manager found yet, no affinity injected",
			"pod", podKey,
		)
		return allow(req.UID)
	}

	klog.V(4).InfoS("webhook/virt-launcher: injecting nodeAffinity to co-locate with share-manager",
		"pod", podKey,
		"shareManagerPod", shareManagerPodName,
		"shareManagerNode", shareManagerNode,
	)

	patch, err := buildAffinityPatch(pod, shareManagerNode, h.affinityMode)
	if err != nil {
		klog.ErrorS(err, "webhook/virt-launcher: failed to build affinity patch", "pod", podKey)
		return allowWithWarning(req.UID, fmt.Sprintf("failed to build affinity patch: %v", err))
	}

	return patchResponse(req.UID, patch)
}

// handleShareManager processes a share-manager pod admission request.
// It injects nodeAffinity to co-locate the share-manager with the opted-in virt-launcher pod.
func (h *Handler) handleShareManager(ctx context.Context, req *admissionv1.AdmissionRequest, pod *corev1.Pod) *admissionv1.AdmissionResponse {
	podKey := fmt.Sprintf("%s/%s", req.Namespace, pod.Name)

	// Extract the PV name from the share-manager pod name: share-manager-<pv-name>
	pvName := strings.TrimPrefix(pod.Name, ShareManagerPrefix)
	if pvName == pod.Name || pvName == "" {
		klog.V(5).InfoS("webhook/share-manager: could not extract PV name from pod name, skipping", "pod", podKey)
		return allow(req.UID)
	}

	// Find the opted-in virt-launcher pod that uses this PV.
	virtLauncherNode, virtLauncherPodName, err := h.findVirtLauncherForPV(ctx, pvName)
	if err != nil {
		klog.ErrorS(err, "webhook/share-manager: error looking up virt-launcher", "pod", podKey, "pvName", pvName)
		return allowWithWarning(req.UID, fmt.Sprintf("virt-launcher lookup failed: %v", err))
	}

	if virtLauncherNode == "" {
		klog.V(4).InfoS("webhook/share-manager: no opted-in virt-launcher found, no affinity injected",
			"pod", podKey,
			"pvName", pvName,
		)
		return allow(req.UID)
	}

	klog.V(4).InfoS("webhook/share-manager: injecting nodeAffinity to co-locate with virt-launcher",
		"pod", podKey,
		"pvName", pvName,
		"virtLauncherPod", virtLauncherPodName,
		"virtLauncherNode", virtLauncherNode,
	)

	patch, err := buildAffinityPatch(pod, virtLauncherNode, h.affinityMode)
	if err != nil {
		klog.ErrorS(err, "webhook/share-manager: failed to build affinity patch", "pod", podKey)
		return allowWithWarning(req.UID, fmt.Sprintf("failed to build affinity patch: %v", err))
	}

	return patchResponse(req.UID, patch)
}

// findShareManagerForVirtLauncher finds the node where the share-manager pod
// for any RWX PVC on the given virt-launcher pod is running.
// Returns the node name, the share-manager pod name, and any error.
func (h *Handler) findShareManagerForVirtLauncher(ctx context.Context, namespace string, pod *corev1.Pod) (string, string, error) {
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}
		pvcName := vol.PersistentVolumeClaim.ClaimName

		pvc, err := h.client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
		if err != nil {
			klog.V(4).InfoS("webhook: PVC not found, skipping", "pvcName", pvcName, "namespace", namespace, "error", err)
			continue
		}

		if !isRWX(pvc) {
			klog.V(5).InfoS("webhook: PVC is not RWX, skipping", "pvcName", pvcName)
			continue
		}

		pvName := pvc.Spec.VolumeName
		if pvName == "" {
			klog.V(4).InfoS("webhook: PVC not yet bound, skipping", "pvcName", pvcName)
			continue
		}

		shareManagerName := ShareManagerPrefix + pvName
		smPod, err := h.client.CoreV1().Pods(LonghornNamespace).Get(ctx, shareManagerName, metav1.GetOptions{})
		if err != nil {
			klog.V(4).InfoS("webhook: share-manager pod not found (may not exist yet)",
				"shareManagerName", shareManagerName,
				"error", err,
			)
			continue
		}

		if smPod.Status.Phase == corev1.PodRunning && smPod.Spec.NodeName != "" {
			return smPod.Spec.NodeName, shareManagerName, nil
		}

		klog.V(4).InfoS("webhook: share-manager pod exists but not yet running",
			"shareManagerName", shareManagerName,
			"phase", smPod.Status.Phase,
			"nodeName", smPod.Spec.NodeName,
		)
	}

	return "", "", nil
}

// findVirtLauncherForPV finds the opted-in virt-launcher pod that uses the PVC
// bound to the given PV name, and returns the node it is scheduled on.
//
// To handle the race condition where the virt-launcher and share-manager are
// created simultaneously, this function retries with exponential backoff for up
// to 10 seconds when a matching virt-launcher pod exists but has not yet been
// assigned a node by the scheduler.
//
// Returns the node name, the virt-launcher pod name, and any error.
func (h *Handler) findVirtLauncherForPV(ctx context.Context, pvName string) (string, string, error) {
	// Find the PVC that is bound to this PV (search all namespaces).
	pvcList, err := h.client.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", "", fmt.Errorf("listing PVCs: %w", err)
	}

	var pvcName, pvcNamespace string
	for i := range pvcList.Items {
		pvc := &pvcList.Items[i]
		if pvc.Spec.VolumeName == pvName && isRWX(pvc) {
			pvcName = pvc.Name
			pvcNamespace = pvc.Namespace
			break
		}
	}

	if pvcName == "" {
		klog.V(4).InfoS("webhook: no RWX PVC found for PV", "pvName", pvName)
		return "", "", nil
	}

	// Retry loop: poll until the virt-launcher pod is scheduled (NodeName set)
	// or the context deadline is reached. This handles the race condition where
	// the share-manager webhook fires while the virt-launcher is still being
	// admitted/scheduled by the Kubernetes scheduler.
	//
	// Backoff: 200ms, 400ms, 800ms, 1.6s, 3.2s → max ~6s total, cap 10s.
	var (
		foundNode    string
		foundPodName string
	)
	backoff := wait.Backoff{
		Duration: 200 * time.Millisecond,
		Factor:   2.0,
		Jitter:   0.1,
		Steps:    6,
		Cap:      10 * time.Second,
	}
	_ = wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		// Find virt-launcher pods in that namespace that use this PVC.
		podList, err := h.client.CoreV1().Pods(pvcNamespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", VirtLauncherLabel, VirtLauncherLabelValue),
		})
		if err != nil {
			klog.V(4).InfoS("webhook: error listing pods, will retry",
				"namespace", pvcNamespace, "error", err)
			return false, nil // transient error, retry
		}

		for i := range podList.Items {
			p := &podList.Items[i]
			pKey := fmt.Sprintf("%s/%s", p.Namespace, p.Name)

			if !isVirtLauncher(p) || !isOptedIn(p) || isMigrationTarget(p) || !podUsesPVC(p, pvcName) {
				continue
			}

			if p.Spec.NodeName == "" {
				klog.V(4).InfoS("webhook: virt-launcher pod found but not yet scheduled, retrying",
					"pod", pKey,
				)
				return false, nil // pod exists but not scheduled yet — retry
			}

			foundNode = p.Spec.NodeName
			foundPodName = pKey
			return true, nil // done
		}

		// No matching pod found at all — don't retry, it won't appear.
		return true, nil
	})

	if foundNode == "" {
		klog.V(4).InfoS("webhook: no scheduled opted-in virt-launcher found for PVC",
			"pvName", pvName,
			"pvcName", pvcName,
			"pvcNamespace", pvcNamespace,
		)
	}
	return foundNode, foundPodName, nil
}

// buildAffinityPatch builds a JSON patch (RFC 6902) that sets a nodeAffinity
// rule on the pod. The mode controls whether the affinity is hard (required)
// or soft (preferred / best-effort).
func buildAffinityPatch(pod *corev1.Pod, nodeName string, mode AffinityMode) ([]byte, error) {
	affinity := buildAffinity(pod.Spec.Affinity, nodeName, mode)

	affinityJSON, err := json.Marshal(affinity)
	if err != nil {
		return nil, fmt.Errorf("marshalling affinity: %w", err)
	}

	op := "replace"
	if pod.Spec.Affinity == nil {
		op = "add"
	}

	patch := []map[string]interface{}{
		{
			"op":    op,
			"path":  "/spec/affinity",
			"value": json.RawMessage(affinityJSON),
		},
	}

	return json.Marshal(patch)
}

// buildAffinity constructs a corev1.Affinity for the given node, preserving
// any existing pod/node affinity rules.
//
//   - AffinityModeRequired: requiredDuringSchedulingIgnoredDuringExecution —
//     the pod will only schedule on the target node; stays Pending if the node
//     has resource pressure.
//   - AffinityModePreferred (default): preferredDuringSchedulingIgnoredDuringExecution
//     with weight 100 — the scheduler strongly prefers the target node but will
//     schedule elsewhere if it has resource pressure (best-effort).
func buildAffinity(existing *corev1.Affinity, nodeName string, mode AffinityMode) *corev1.Affinity {
	var nodeAffinity *corev1.NodeAffinity

	if mode == AffinityModeRequired {
		nodeAffinity = &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{
					{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "kubernetes.io/hostname",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{nodeName},
							},
						},
					},
				},
			},
		}
	} else {
		// AffinityModePreferred (default): best-effort, weight=100.
		nodeAffinity = &corev1.NodeAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
				{
					Weight: 100,
					Preference: corev1.NodeSelectorTerm{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      "kubernetes.io/hostname",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{nodeName},
							},
						},
					},
				},
			},
		}
	}

	if existing == nil {
		return &corev1.Affinity{
			NodeAffinity: nodeAffinity,
		}
	}

	// Merge: preserve existing pod affinity/anti-affinity, override NodeAffinity.
	result := existing.DeepCopy()
	result.NodeAffinity = nodeAffinity
	return result
}

// isVirtLauncher returns true if the pod is a KubeVirt virt-launcher pod.
// KubeVirt sets the label kubevirt.io=virt-launcher on all virt-launcher pods.
func isVirtLauncher(pod *corev1.Pod) bool {
	if pod.Labels == nil {
		return false
	}
	return pod.Labels[VirtLauncherLabel] == VirtLauncherLabelValue
}

// isShareManager returns true if the pod is a Longhorn share-manager pod.
// Longhorn sets longhorn.io/component=share-manager on all share-manager pods.
// The pod name also always starts with "share-manager-<pv-name>" as a secondary check.
func isShareManager(pod *corev1.Pod) bool {
	if pod.Labels != nil && pod.Labels[ShareManagerLabelKey] == ShareManagerLabelValue {
		return true
	}
	// Fallback: match by name prefix in case the label is absent.
	return strings.HasPrefix(pod.Name, ShareManagerPrefix)
}

// isOptedIn returns true if the pod has the co-scheduling annotation set to "true".
func isOptedIn(pod *corev1.Pod) bool {
	if pod.Annotations == nil {
		return false
	}
	return pod.Annotations[AnnotationKey] == AnnotationValue
}

// isMigrationTarget returns true if the pod is a KubeVirt live-migration target pod.
func isMigrationTarget(pod *corev1.Pod) bool {
	if pod.Labels == nil {
		return false
	}
	uid, ok := pod.Labels[MigrationTargetLabel]
	return ok && uid != ""
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

// podUsesPVC returns true if the pod references the given PVC name.
func podUsesPVC(pod *corev1.Pod, pvcName string) bool {
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil && vol.PersistentVolumeClaim.ClaimName == pvcName {
			return true
		}
	}
	return false
}

// allow returns an AdmissionResponse that allows the request without modification.
func allow(uid types.UID) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		UID:     uid,
		Allowed: true,
	}
}

// allowWithWarning returns an AdmissionResponse that allows the request with a warning message.
func allowWithWarning(uid types.UID, warning string) *admissionv1.AdmissionResponse {
	return &admissionv1.AdmissionResponse{
		UID:      uid,
		Allowed:  true,
		Warnings: []string{warning},
	}
}

// patchResponse returns an AdmissionResponse with a JSON patch.
func patchResponse(uid types.UID, patch []byte) *admissionv1.AdmissionResponse {
	pt := admissionv1.PatchTypeJSONPatch
	return &admissionv1.AdmissionResponse{
		UID:       uid,
		Allowed:   true,
		Patch:     patch,
		PatchType: &pt,
	}
}
