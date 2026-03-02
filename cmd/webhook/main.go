// Command webhook is a Kubernetes mutating admission webhook server that
// co-locates KubeVirt virt-launcher pods with their Longhorn RWX share-manager
// pods on the same node by injecting nodeAffinity rules at pod creation time.
//
// Opt-in: set the annotation scheduler.virthorn-scheduler.io/co-schedule: "true"
// on the VirtualMachine spec.template.metadata.annotations. KubeVirt propagates
// this annotation to the virt-launcher pod automatically.
//
// Live migration target pods (kubevirt.io/migrationJobUID label) are always
// passed through unchanged — the KubeVirt migration controller handles placement.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"

	"github.com/michaeltrip/virthorn-scheduler/pkg/webhook"
)

var (
	port         = flag.Int("port", 8443, "HTTPS port for the webhook server")
	serviceName  = flag.String("service-name", "virthorn-webhook", "Kubernetes Service name for this webhook server (used for TLS SAN)")
	namespace    = flag.String("namespace", "kube-system", "Namespace this webhook server runs in (used for TLS SAN)")
	affinityMode = flag.String("affinity-mode", "preferred",
		`Affinity mode for co-location: "preferred" (best-effort, pod can schedule elsewhere if node has pressure) `+
			`or "required" (hard constraint, pod stays Pending if node has pressure)`)
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()

	klog.InfoS("virthorn-webhook starting",
		"port", *port,
		"serviceName", *serviceName,
		"namespace", *namespace,
	)

	// Build in-cluster Kubernetes client.
	cfg, err := rest.InClusterConfig()
	if err != nil {
		klog.ErrorS(err, "failed to build in-cluster config")
		os.Exit(1)
	}

	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.ErrorS(err, "failed to create Kubernetes client")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Bootstrap TLS: generate or load self-signed cert, patch caBundle in MutatingWebhookConfiguration.
	tlsCert, caPEM, err := webhook.EnsureTLS(ctx, client, *serviceName, *namespace)
	if err != nil {
		klog.ErrorS(err, "failed to ensure TLS certificate")
		os.Exit(1)
	}

	if err := webhook.PatchWebhookCABundle(ctx, client, caPEM); err != nil {
		klog.ErrorS(err, "failed to patch caBundle in MutatingWebhookConfiguration")
		os.Exit(1)
	}

	// Parse and validate affinity mode.
	mode := webhook.AffinityMode(*affinityMode)
	if mode != webhook.AffinityModePreferred && mode != webhook.AffinityModeRequired {
		klog.ErrorS(fmt.Errorf("invalid value %q", *affinityMode), "unknown --affinity-mode; must be \"preferred\" or \"required\"")
		os.Exit(1)
	}
	klog.InfoS("virthorn-webhook affinity mode", "mode", mode)

	// Build the webhook handler.
	handler := webhook.NewHandler(client, mode)

	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", makeMutateHandler(handler))
	mux.HandleFunc("/healthz", healthzHandler)

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{*tlsCert},
			MinVersion:   tls.VersionTLS13,
		},
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	// Start server in a goroutine.
	go func() {
		klog.InfoS("virthorn-webhook listening", "addr", srv.Addr)
		// ListenAndServeTLS with empty cert/key paths uses the TLSConfig.Certificates.
		if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			klog.ErrorS(err, "webhook server error")
			cancel()
		}
	}()

	// Wait for shutdown signal.
	<-ctx.Done()
	klog.InfoS("virthorn-webhook shutting down")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		klog.ErrorS(err, "error during webhook server shutdown")
	}
}

// makeMutateHandler returns an http.HandlerFunc that processes AdmissionReview requests.
func makeMutateHandler(h *webhook.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB limit
		if err != nil {
			klog.ErrorS(err, "webhook: failed to read request body")
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		var review admissionv1.AdmissionReview
		if err := json.Unmarshal(body, &review); err != nil {
			klog.ErrorS(err, "webhook: failed to unmarshal AdmissionReview")
			http.Error(w, "failed to unmarshal AdmissionReview", http.StatusBadRequest)
			return
		}

		if review.Request == nil {
			http.Error(w, "AdmissionReview.Request is nil", http.StatusBadRequest)
			return
		}

		response := h.Handle(r.Context(), review.Request)
		response.UID = review.Request.UID

		// Clear the request before sending back.
		review.Request = nil
		review.Response = response

		respBytes, err := json.Marshal(review)
		if err != nil {
			klog.ErrorS(err, "webhook: failed to marshal AdmissionReview response")
			// Fail open: return a plain allow response.
			fallback := admissionv1.AdmissionReview{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "admission.k8s.io/v1",
					Kind:       "AdmissionReview",
				},
				Response: &admissionv1.AdmissionResponse{
					UID:     response.UID,
					Allowed: true,
				},
			}
			fb, _ := json.Marshal(fallback)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(fb)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(respBytes)
	}
}

// healthzHandler responds to liveness/readiness probes.
func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}
