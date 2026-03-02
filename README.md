<div align="center">
  <img src="img/virthorn.png" alt="Virthorn Logo" width="320" />

  # virthorn-scheduler

  **A Kubernetes mutating admission webhook that co-locates KubeVirt VM pods with their Longhorn RWX share-manager pods — eliminating cross-node NFS traffic.**

  [![Release](https://github.com/michaeltrip/virthorn-scheduler/actions/workflows/build-release.yaml/badge.svg)](https://github.com/michaeltrip/virthorn-scheduler/actions/workflows/build-release.yaml)
  [![Go Report Card](https://goreportcard.com/badge/github.com/michaeltrip/virthorn-scheduler)](https://goreportcard.com/report/github.com/michaeltrip/virthorn-scheduler)
  [![Go version](https://img.shields.io/badge/Go-1.23+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
  [![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
  [![GHCR](https://img.shields.io/badge/container-ghcr.io-24292e?logo=github)](https://github.com/michaeltrip/virthorn-scheduler/pkgs/container/virthorn-scheduler)
  [![Kubernetes](https://img.shields.io/badge/Kubernetes-1.32-326CE5?logo=kubernetes&logoColor=white)](https://kubernetes.io/)
  [![KubeVirt](https://img.shields.io/badge/KubeVirt-compatible-blueviolet)](https://kubevirt.io/)
  [![Longhorn](https://img.shields.io/badge/Longhorn-RWX-orange)](https://longhorn.io/)
</div>

---

## Problem

When KubeVirt VMs use Longhorn RWX volumes, Longhorn creates a `share-manager` pod (NFS server) per PVC in the `longhorn-system` namespace. By default this pod is scheduled independently of the VM, causing NFS traffic to cross node boundaries — adding latency and network overhead.

## Solution

`virthorn-scheduler` is a **mutating admission webhook** that injects `nodeAffinity` rules into pods at creation time, co-locating:

- **virt-launcher pods** → pinned to the node where the share-manager is already running
- **share-manager pods** → pinned to the node where the opted-in virt-launcher is already running

Whichever pod is created second gets the affinity pointing to the first. The standard `kube-scheduler` then honors the injected affinity — no custom scheduler binary needed.

## How It Works

```
VM Pod created (virt-launcher)
  │
  ├─ No co-schedule annotation → pass through unchanged
  │
  ├─ migrationJobUID label set (live migration target)
  │    └─ pass through unchanged — KubeVirt migration controller handles placement
  │
  └─ annotation scheduler.virthorn-scheduler.io/co-schedule: "true"
       │
       ├─ Share-manager pod already running on node-X
       │    └─ Inject nodeAffinity: require kubernetes.io/hostname = node-X
       │       → VM scheduled on node-X ✅ co-located
       │
       └─ No share-manager pod yet
            └─ Pass through unchanged
               When share-manager is created later:
               └─ Webhook finds virt-launcher running on node-Y
                  Inject nodeAffinity: require kubernetes.io/hostname = node-Y
                  → Share-manager scheduled on node-Y ✅ co-located

```

### Bidirectional co-location

The webhook intercepts **both** pod types:

| Pod type | Trigger | Action |
|---|---|---|
| `virt-launcher` (any namespace) | `kubevirt.io=virt-launcher` label | Look up share-manager → inject nodeAffinity |
| `share-manager-*` (`longhorn-system`) | `longhorn.io/component=share-manager` label | Look up opted-in virt-launcher → inject nodeAffinity |

### Live migration

When `virtctl migrate` is used, KubeVirt creates a new **target virt-launcher pod** and sets the label `kubevirt.io/migrationJobUID` on it. The webhook detects this label and **passes the pod through unchanged** — the KubeVirt migration controller already selects the destination node via node affinity, and constraining it to the share-manager node would break migration.

## Installation

### 1. Build and push the image

```bash
docker build -t ghcr.io/<your-org>/virthorn-scheduler:latest .
docker push ghcr.io/<your-org>/virthorn-scheduler:latest
```

Update the `image:` field in [`manifests/webhook-deployment.yaml`](manifests/webhook-deployment.yaml) to match.

### 2. Apply the manifests

```bash
# Apply in order: RBAC first, then webhook config, then deployment
kubectl apply -f manifests/rbac.yaml
kubectl apply -f manifests/webhook.yaml
kubectl apply -f manifests/webhook-deployment.yaml
```

The webhook server will:
1. Generate a self-signed TLS certificate at startup
2. Store it in the `virthorn-webhook-tls` Secret in `kube-system`
3. Patch the `caBundle` field in the `MutatingWebhookConfiguration` automatically

### 3. Verify the webhook is running

```bash
kubectl -n kube-system get pods -l app=virthorn-webhook
kubectl -n kube-system logs -l app=virthorn-webhook
```

## Usage

### Opt-in a VirtualMachine

Add **only** the annotation to your `VirtualMachine` spec. No custom `schedulerName` is needed — the standard `kube-scheduler` handles scheduling after the webhook injects the affinity.

```yaml
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: my-vm
spec:
  template:
    metadata:
      annotations:
        scheduler.virthorn-scheduler.io/co-schedule: "true"   # opt-in to co-scheduling
    spec:
      # schedulerName is NOT needed — use the default kube-scheduler
      domain:
        # ... your VM spec ...
      volumes:
        - name: datavol
          persistentVolumeClaim:
            claimName: my-rwx-pvc                # must be a Longhorn RWX PVC
```

KubeVirt propagates annotations from the `VirtualMachine` template to the `virt-launcher` pod automatically. The webhook reads the annotation there.

### What gets injected

When the webhook fires, it injects a `nodeAffinity` rule into `spec.affinity`. The default mode is `preferred` (best-effort):

```yaml
spec:
  affinity:
    nodeAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          preference:
            matchExpressions:
              - key: kubernetes.io/hostname
                operator: In
                values:
                  - node-X   # the node where the share-manager is running
```

With `--affinity-mode=required` (hard constraint), the injected rule uses `requiredDuringSchedulingIgnoredDuringExecution` instead — the pod stays `Pending` if the target node has resource pressure.

Any existing `podAffinity` or `podAntiAffinity` rules on the pod are preserved. Only `nodeAffinity` is overwritten.

## Configuration

| Item | Value |
|---|---|
| Opt-in annotation key | `scheduler.virthorn-scheduler.io/co-schedule` |
| Opt-in annotation value | `true` |
| Migration target label | `kubevirt.io/migrationJobUID` |
| virt-launcher detection label | `kubevirt.io=virt-launcher` (key `kubevirt.io`, value `virt-launcher`) |
| Share-manager namespace | `longhorn-system` |
| Share-manager pod name pattern | `share-manager-<pv-name>` |
| Share-manager detection label | `longhorn.io/component=share-manager` |
| Affinity mode | `preferred` (best-effort, default) or `required` (hard constraint) — set via `--affinity-mode` flag |
| Webhook failure policy | `Ignore` (fail open — pods admitted normally if webhook is unavailable) |
| TLS | Self-signed cert generated at startup, stored in `kube-system/virthorn-webhook-tls` Secret |

## Debugging / Logging

The webhook emits structured log messages using `klog` at verbosity level **4** (`V(4)`). The default deployment ships with `--v=4`.

### Tailing the webhook logs

```bash
kubectl -n kube-system logs -l app=virthorn-webhook -f
```

### What gets logged

| Verbosity | Message |
|---|---|
| `V(4)` | Migration target pod detected — webhook skipped |
| `V(4)` | No share-manager found yet — no affinity injected |
| `V(4)` | Injecting nodeAffinity to co-locate with share-manager (includes node name) |
| `V(4)` | Injecting nodeAffinity to co-locate with virt-launcher (includes node name) |
| `V(4)` | No opted-in virt-launcher found — no affinity injected |
| `V(5)` | Pod not opted in — webhook skipped |
| `ErrorS` | API lookup failed (logged as warning; pod is admitted without affinity) |

### Example log output

**VM scheduled on share-manager node:**
```
webhook/virt-launcher: injecting nodeAffinity to co-locate with share-manager  pod=default/virt-launcher-my-vm-xxxxx shareManagerPod=share-manager-pvc-abc123 shareManagerNode=node-1
```

**Share-manager co-located with running VM:**
```
webhook/share-manager: injecting nodeAffinity to co-locate with virt-launcher  pod=longhorn-system/share-manager-pvc-abc123 pvName=pvc-abc123 virtLauncherPod=default/virt-launcher-my-vm-xxxxx virtLauncherNode=node-1
```

**Live migration target (webhook bypassed):**
```
webhook/virt-launcher: migration target pod, skipping  pod=default/virt-launcher-my-vm-yyyyy migrationJobUID=08b02237-4ab6-493b-a4e0-c90e5e940a47
```

**No share-manager yet (free scheduling):**
```
webhook/virt-launcher: no share-manager found yet, no affinity injected  pod=default/virt-launcher-my-vm-xxxxx
```

## Development

### Prerequisites

- Go 1.23+
- Access to a Kubernetes cluster with KubeVirt and Longhorn installed

### Build

```bash
go build -o virthorn-webhook ./cmd/webhook
```

### Test

```bash
go test ./pkg/...
```

### Project Structure

```
virthorn-scheduler/
├── cmd/webhook/main.go              # Entry point — HTTPS server, /mutate + /healthz
├── pkg/webhook/
│   ├── handler.go                   # Webhook logic — pod type detection, affinity injection
│   └── tls.go                       # Self-signed TLS bootstrap
├── manifests/
│   ├── rbac.yaml                    # ServiceAccount, ClusterRole, Role, bindings
│   ├── webhook.yaml                 # MutatingWebhookConfiguration + Service
│   └── webhook-deployment.yaml      # Deployment + ServiceAccount
└── Dockerfile
```

## License

See [LICENSE](LICENSE).
