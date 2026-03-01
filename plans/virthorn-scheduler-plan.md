# VirtHorn Scheduler — Architecture Plan

## Problem Statement

When KubeVirt VMs use **Longhorn RWX** volumes, Longhorn creates a `share-manager` pod that serves an NFS share for that PVC. By default, this share-manager pod is scheduled independently of the VM pod, meaning the VM's NFS traffic crosses node boundaries — adding latency and network overhead.

**Goal:** Co-schedule the VM pod and its Longhorn share-manager pod on the **same node**, using a custom Kubernetes Scheduling Framework plugin with an opt-in annotation.

---

## Solution Overview

Build a **custom Kubernetes scheduler** (named `virthorn-scheduler`) as a standalone binary that embeds the default `kube-scheduler` with an additional custom plugin registered. The plugin implements two extension points:

| Extension Point | Purpose |
|---|---|
| **Filter** | If a share-manager pod already exists for the VM's PVC, remove all nodes except the one where the share-manager runs |
| **Score** | If no share-manager pod exists yet, score nodes normally (plugin is a no-op); if one exists, give the share-manager's node the highest score |

The scheduler is **opt-in** via a pod annotation. Only pods with the annotation `virthorn-scheduler/co-schedule: "true"` are processed by the plugin logic.

---

## Architecture

```mermaid
graph TD
    A[KubeVirt VM Pod created] --> B{Has annotation\nvirthorn-scheduler/co-schedule: true?}
    B -- No --> C[Default scheduling logic]
    B -- Yes --> D[Plugin: inspect PVCs on the pod]
    D --> E{Longhorn Volume CRD\nspec.nodeID set?}
    E -- Yes\nwarm restart --> G[Filter: keep only spec.nodeID node\nScore: spec.nodeID node = 100]
    G --> H[VM scheduled on node-X\nLonghorn re-attaches engine to node-X]
    H --> I[share-manager starts on node-X ✅]
    E -- No\ncold start --> F[Filter/Score: no-op\nVM schedules freely on best node]
    F --> J[PostBind: PATCH Longhorn Volume spec.nodeID = bound-node]
    J --> K[Longhorn attaches engine to bound-node\nshare-manager starts on same node ✅]
```

---

## Key Design Decisions

### 1. Opt-in Annotation
```
virthorn-scheduler/co-schedule: "true"
```
Applied to the KubeVirt `VirtualMachineInstance` pod (the `virt-launcher` pod). KubeVirt propagates annotations from the VM spec to the virt-launcher pod.

### 2. Share-Manager Pod Discovery
Longhorn names share-manager pods with the pattern:
```
share-manager-<pvc-name>
```
in the `longhorn-system` namespace. The plugin will:
1. List all PVCs referenced by the pod being scheduled
2. For each PVC, look up `longhorn-system/share-manager-<pvc-name>`
3. If found and running, extract its `.spec.nodeName`

### 3. Plugin Extension Points

#### Filter Plugin (`LonghornCoScheduleFilter`)
- If annotation is absent → pass (allow all nodes)
- If annotation present and Longhorn Volume `spec.nodeID` is empty (cold start) → pass (allow all nodes)
- If annotation present and `spec.nodeID = node-X` (warm restart) → only pass node X, filter out all others

#### Score Plugin (`LonghornCoScheduleScore`)
- If annotation is absent → return score 0 (neutral)
- If annotation present and `spec.nodeID` is empty (cold start) → return score 0 (neutral)
- If annotation present and `spec.nodeID = node-X` (warm restart) → return score 100 for node X, 0 for all others

#### PostBind Plugin (`LonghornCoSchedulePostBind`)
- If annotation is absent → no-op
- If migration target pod → no-op
- If Longhorn Volume `spec.nodeID` is already set (warm restart) → no-op
- If Longhorn Volume `spec.nodeID` is empty (cold start) → PATCH `spec.nodeID = nodeName` on the Longhorn Volume CRD so Longhorn attaches the engine (and share-manager) to the same node as the virt-launcher

**Why PostBind instead of reading the ShareManager CRD?**
The ShareManager CRD (`sharemanagers.longhorn.io`) is created by Longhorn _at the same time_ as the share-manager pod — not before. It cannot provide earlier information than the pod itself. PostBind solves the cold-start problem by inverting the relationship: instead of asking "where will the share-manager go?", we tell Longhorn "attach the engine here" after we know where the VM landed.

### 4. Scheduler Configuration
The custom scheduler runs as a **separate scheduler** (not replacing `kube-scheduler`). VMs opt-in by setting `spec.schedulerName: virthorn-scheduler` in the VirtualMachine spec.

---

## Project Structure

```
virthorn-scheduler/
├── cmd/
│   └── scheduler/
│       └── main.go                  # Entry point, registers plugin
├── pkg/
│   └── plugins/
│       └── longhorn_cosched/
│           ├── plugin.go            # Plugin struct, Name(), registration
│           ├── filter.go            # Filter extension point
│           ├── score.go             # Score extension point
│           ├── sharemanager.go      # Logic to find share-manager pod
│           └── plugin_test.go       # Unit tests
├── manifests/
│   ├── rbac.yaml                    # ClusterRole + ClusterRoleBinding
│   ├── scheduler-config.yaml        # KubeSchedulerConfiguration
│   └── deployment.yaml              # Deployment for the scheduler
├── Dockerfile
├── go.mod
├── go.sum
├── LICENSE
└── README.md
```

---

## Go Module Dependencies

| Dependency | Purpose |
|---|---|
| `k8s.io/kubernetes` | Scheduler framework (via `k8s.io/kube-scheduler`) |
| `k8s.io/client-go` | Kubernetes API client |
| `k8s.io/api` | Kubernetes API types |
| `k8s.io/apimachinery` | API machinery utilities |
| `sigs.k8s.io/scheduler-plugins` | Reference for plugin patterns |

> **Note:** The recommended approach is to use `k8s.io/kube-scheduler` as a library (the same pattern used by `scheduler-plugins` upstream project).

---

## RBAC Requirements

The scheduler needs the following permissions beyond the default scheduler:

```yaml
# Additional rules needed:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch"]          # To find share-manager pods
- apiGroups: [""]
  resources: ["persistentvolumeclaims"]
  verbs: ["get", "list", "watch"]          # To inspect PVCs on the pod
- apiGroups: [""]
  resources: ["persistentvolumes"]
  verbs: ["get", "list", "watch"]          # To check storage class
```

---

## Scheduler Configuration (KubeSchedulerConfiguration)

```yaml
apiVersion: kubescheduler.config.k8s.io/v1
kind: KubeSchedulerConfiguration
profiles:
  - schedulerName: virthorn-scheduler
    plugins:
      filter:
        enabled:
          - name: LonghornCoSchedule
      score:
        enabled:
          - name: LonghornCoSchedule
      postBind:
        enabled:
          - name: LonghornCoSchedule
```

---

## VM Usage Example

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: my-vm
spec:
  template:
    metadata:
      annotations:
        virthorn-scheduler/co-schedule: "true"
    spec:
      schedulerName: virthorn-scheduler
      volumes:
        - name: datavol
          persistentVolumeClaim:
            claimName: my-rwx-pvc
```

---

## Sequence Diagrams

### Warm Restart (spec.nodeID already set by prior attachment)

```mermaid
sequenceDiagram
    participant K as kube-apiserver
    participant S as virthorn-scheduler
    participant P as Plugin: LonghornCoSchedule
    participant L as Longhorn Volume CRD

    K->>S: virt-launcher pod pending
    S->>P: RunFilterPlugins / RunScorePlugins
    P->>L: Read spec.nodeID → "node-X"
    P-->>S: Filter: only node-X / Score: node-X=100
    S->>K: Bind pod to node-X
    S->>P: PostBind(node-X)
    P->>L: spec.nodeID already = "node-X" → no-op
    note over L: Longhorn re-attaches engine to node-X
    note over L: share-manager starts on node-X ✅
```

### Cold Start (brand-new PVC, spec.nodeID = "")

```mermaid
sequenceDiagram
    participant K as kube-apiserver
    participant S as virthorn-scheduler
    participant P as Plugin: LonghornCoSchedule
    participant L as Longhorn Volume CRD

    K->>S: virt-launcher pod pending
    S->>P: RunFilterPlugins / RunScorePlugins
    P->>L: Read spec.nodeID → "" (empty)
    P-->>S: No constraint — all nodes pass, score 0
    S->>K: Bind pod to node-Y (best available)
    S->>P: PostBind(node-Y)
    P->>L: spec.nodeID is empty → PATCH spec.nodeID = "node-Y"
    note over L: Longhorn attaches engine to node-Y
    note over L: share-manager starts on node-Y ✅
```

---

## Implementation Steps

1. **Initialize Go module** — `go mod init github.com/yourusername/virthorn-scheduler`
2. **Plugin skeleton** — implement `framework.Plugin`, `framework.FilterPlugin`, `framework.ScorePlugin`, `framework.PostBindPlugin` interfaces
3. **Share-manager lookup** — read Longhorn Volume CRD `spec.nodeID` / `status.currentNodeID` from `longhorn-system`; fall back to the share-manager pod `spec.nodeName`
4. **Filter logic** — if `spec.nodeID` is set, return `framework.NewStatus(framework.Unschedulable)` for non-matching nodes; otherwise no-op
5. **Score logic** — return `framework.MaxNodeScore` for the `spec.nodeID` node; otherwise score 0
6. **PostBind logic** — if `spec.nodeID` is empty (cold start), PATCH it to the bound node on the Longhorn Volume CRD; if already set, no-op
7. **Main entrypoint** — use `scheduler/app` to build the scheduler binary with the plugin registered
8. **Dockerfile** — multi-stage build, distroless final image
9. **Manifests** — RBAC (including `patch`/`update` on `volumes.longhorn.io`), ConfigMap with scheduler config, Deployment
10. **Tests** — table-driven unit tests for filter, score, and postbind logic using fake clients
11. **README** — installation and usage docs
