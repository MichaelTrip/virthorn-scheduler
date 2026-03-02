package webhook

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

const (
	// WebhookConfigName is the name of the MutatingWebhookConfiguration to patch.
	WebhookConfigName = "virthorn-webhook"

	// TLSSecretName is the name of the Secret that stores the TLS cert/key.
	TLSSecretName = "virthorn-webhook-tls"

	// TLSSecretNamespace is the namespace where the TLS Secret is stored.
	TLSSecretNamespace = "kube-system"

	// certValidity is how long the generated certificate is valid.
	certValidity = 10 * 365 * 24 * time.Hour // 10 years
)

// CertBundle holds the PEM-encoded CA cert, server cert, and server key.
type CertBundle struct {
	CACert     []byte
	ServerCert []byte
	ServerKey  []byte
}

// EnsureTLS ensures a TLS certificate exists for the webhook server.
// It loads an existing cert from the Secret if present and still valid,
// or generates a new self-signed cert and stores it in the Secret.
// Returns the parsed TLS certificate and the PEM-encoded CA cert.
func EnsureTLS(ctx context.Context, client kubernetes.Interface, serviceName, namespace string) (*tls.Certificate, []byte, error) {
	dnsNames := []string{
		serviceName,
		fmt.Sprintf("%s.%s", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc", serviceName, namespace),
		fmt.Sprintf("%s.%s.svc.cluster.local", serviceName, namespace),
	}

	// Try to load existing cert from Secret.
	secret, err := client.CoreV1().Secrets(TLSSecretNamespace).Get(ctx, TLSSecretName, metav1.GetOptions{})
	if err == nil {
		bundle, loadErr := loadFromSecret(secret)
		if loadErr == nil {
			tlsCert, parseErr := tls.X509KeyPair(bundle.ServerCert, bundle.ServerKey)
			if parseErr == nil {
				klog.V(4).InfoS("tls: loaded existing TLS cert from Secret", "secret", TLSSecretName)
				return &tlsCert, bundle.CACert, nil
			}
			klog.V(4).InfoS("tls: existing cert invalid, regenerating", "error", parseErr)
		} else {
			klog.V(4).InfoS("tls: could not load cert from Secret, regenerating", "error", loadErr)
		}
	} else if !errors.IsNotFound(err) {
		return nil, nil, fmt.Errorf("getting TLS secret: %w", err)
	}

	// Generate a new self-signed cert.
	klog.V(4).InfoS("tls: generating new self-signed TLS certificate", "dnsNames", dnsNames)
	bundle, err := generateSelfSignedCert(dnsNames)
	if err != nil {
		return nil, nil, fmt.Errorf("generating self-signed cert: %w", err)
	}

	// Store in Secret.
	if err := storeCertSecret(ctx, client, bundle); err != nil {
		return nil, nil, fmt.Errorf("storing TLS secret: %w", err)
	}

	tlsCert, err := tls.X509KeyPair(bundle.ServerCert, bundle.ServerKey)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing generated cert: %w", err)
	}

	return &tlsCert, bundle.CACert, nil
}

// PatchWebhookCABundle patches the caBundle field in all webhooks of the
// MutatingWebhookConfiguration with the given CA certificate PEM.
func PatchWebhookCABundle(ctx context.Context, client kubernetes.Interface, caPEM []byte) error {
	wc, err := client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(ctx, WebhookConfigName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting MutatingWebhookConfiguration %q: %w", WebhookConfigName, err)
	}

	type webhookPatch struct {
		Name     string `json:"name"`
		CABundle []byte `json:"caBundle"`
	}
	type configPatch struct {
		Webhooks []webhookPatch `json:"webhooks"`
	}

	p := configPatch{}
	for _, wh := range wc.Webhooks {
		p.Webhooks = append(p.Webhooks, webhookPatch{
			Name:     wh.Name,
			CABundle: caPEM,
		})
	}

	patchBytes, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("marshalling caBundle patch: %w", err)
	}

	_, err = client.AdmissionregistrationV1().MutatingWebhookConfigurations().Patch(
		ctx,
		WebhookConfigName,
		types.MergePatchType,
		patchBytes,
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("patching MutatingWebhookConfiguration caBundle: %w", err)
	}

	klog.V(4).InfoS("tls: patched caBundle in MutatingWebhookConfiguration", "name", WebhookConfigName)
	return nil
}

// generateSelfSignedCert generates a self-signed CA and server certificate.
func generateSelfSignedCert(dnsNames []string) (*CertBundle, error) {
	// Generate CA key.
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating CA key: %w", err)
	}

	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			Organization: []string{"virthorn-webhook"},
			CommonName:   "virthorn-webhook-ca",
		},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(certValidity),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}

	caCertDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("creating CA certificate: %w", err)
	}

	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, fmt.Errorf("parsing CA certificate: %w", err)
	}

	// Generate server key.
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating server key: %w", err)
	}

	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject: pkix.Name{
			Organization: []string{"virthorn-webhook"},
			CommonName:   dnsNames[0],
		},
		DNSNames:  dnsNames,
		NotBefore: time.Now().Add(-time.Minute),
		NotAfter:  time.Now().Add(certValidity),
		KeyUsage:  x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}

	serverCertDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("creating server certificate: %w", err)
	}

	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCertDER})
	serverCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCertDER})

	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		return nil, fmt.Errorf("marshalling server key: %w", err)
	}
	serverKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER})

	return &CertBundle{
		CACert:     caCertPEM,
		ServerCert: serverCertPEM,
		ServerKey:  serverKeyPEM,
	}, nil
}

// storeCertSecret creates or updates the TLS Secret with the given cert bundle.
func storeCertSecret(ctx context.Context, client kubernetes.Interface, bundle *CertBundle) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      TLSSecretName,
			Namespace: TLSSecretNamespace,
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       bundle.ServerCert,
			corev1.TLSPrivateKeyKey: bundle.ServerKey,
			"ca.crt":                bundle.CACert,
		},
	}

	_, err := client.CoreV1().Secrets(TLSSecretNamespace).Get(ctx, TLSSecretName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		_, err = client.CoreV1().Secrets(TLSSecretNamespace).Create(ctx, secret, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("creating TLS secret: %w", err)
		}
		klog.V(4).InfoS("tls: created TLS Secret", "secret", TLSSecretName)
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking TLS secret existence: %w", err)
	}

	_, err = client.CoreV1().Secrets(TLSSecretNamespace).Update(ctx, secret, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("updating TLS secret: %w", err)
	}
	klog.V(4).InfoS("tls: updated TLS Secret", "secret", TLSSecretName)
	return nil
}

// loadFromSecret extracts the CertBundle from a Secret.
func loadFromSecret(secret *corev1.Secret) (*CertBundle, error) {
	serverCert, ok := secret.Data[corev1.TLSCertKey]
	if !ok || len(serverCert) == 0 {
		return nil, fmt.Errorf("secret missing %s", corev1.TLSCertKey)
	}
	serverKey, ok := secret.Data[corev1.TLSPrivateKeyKey]
	if !ok || len(serverKey) == 0 {
		return nil, fmt.Errorf("secret missing %s", corev1.TLSPrivateKeyKey)
	}
	caCert, ok := secret.Data["ca.crt"]
	if !ok || len(caCert) == 0 {
		return nil, fmt.Errorf("secret missing ca.crt")
	}
	return &CertBundle{
		CACert:     caCert,
		ServerCert: serverCert,
		ServerKey:  serverKey,
	}, nil
}
