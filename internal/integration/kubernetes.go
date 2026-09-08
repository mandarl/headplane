package integration

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultServiceAccountRoot = "/var/run/secrets/kubernetes.io/serviceaccount"

type kubernetesIntegration struct {
	podName          string
	validateManifest bool
	logger           *slog.Logger

	// Test-injectable overrides (functional options). Production uses the
	// in-cluster service account and the standard env-based discovery.
	saRoot    string // serviceaccount dir, default defaultServiceAccountRoot
	procRoot  string // /proc root for the headscale scan, default "/proc"
	apiServer string // "host:port"; when empty, discovered from KUBERNETES_SERVICE_HOST/PORT

	pid int
}

// K8sOption customizes a kubernetesIntegration for tests.
type K8sOption func(*kubernetesIntegration)

// WithK8sProcRoot overrides the /proc root used by the headscale serve scan.
func WithK8sProcRoot(root string) K8sOption {
	return func(k *kubernetesIntegration) { k.procRoot = root }
}

// WithServiceAccountRoot overrides the serviceaccount directory
// (default "/var/run/secrets/kubernetes.io/serviceaccount").
func WithServiceAccountRoot(root string) K8sOption {
	return func(k *kubernetesIntegration) { k.saRoot = root }
}

// WithAPIServer overrides apiserver discovery with an explicit "host:port".
// When set, the KUBERNETES_SERVICE_HOST/PORT env vars are not consulted.
func WithAPIServer(addr string) K8sOption {
	return func(k *kubernetesIntegration) { k.apiServer = addr }
}

func newKubernetesIntegration(cfg *KubernetesConfig, logger *slog.Logger, opts ...K8sOption) *kubernetesIntegration {
	validate := cfg.ValidateManifest == nil || *cfg.ValidateManifest
	k := &kubernetesIntegration{
		podName:          cfg.PodName,
		validateManifest: validate,
		logger:           logger,
		saRoot:           defaultServiceAccountRoot,
		procRoot:         defaultProcRoot,
	}
	for _, opt := range opts {
		opt(k)
	}
	return k
}

func (k *kubernetesIntegration) Name() string { return "Kubernetes (k8s)" }

// discoverAPIServer returns the apiserver "host:port" from the standard
// KUBERNETES_SERVICE_HOST / KUBERNETES_SERVICE_PORT env vars (port defaults
// to 443), or the injected override when set.
func (k *kubernetesIntegration) discoverAPIServer() (string, error) {
	if k.apiServer != "" {
		return k.apiServer, nil
	}
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	if host == "" {
		return "", fmt.Errorf("KUBERNETES_SERVICE_HOST is not set")
	}
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if port == "" {
		port = "443"
	}
	return host + ":" + port, nil
}

// k8sPod mirrors the small subset of the Pod manifest we need.
type k8sPod struct {
	Spec *struct {
		ShareProcessNamespace *bool `json:"shareProcessNamespace"`
	} `json:"spec"`
}

// checkPodManifest reads the pod via the k8s REST API (no client library)
// and requires spec.shareProcessNamespace to be present and true.
func (k *kubernetesIntegration) checkPodManifest(namespace string) error {
	caPEM, err := os.ReadFile(filepath.Join(k.saRoot, "ca.crt"))
	if err != nil {
		return fmt.Errorf("failed to read serviceaccount ca.crt: %w", err)
	}
	tokenBytes, err := os.ReadFile(filepath.Join(k.saRoot, "token"))
	if err != nil {
		return fmt.Errorf("failed to read serviceaccount token: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("failed to parse serviceaccount ca.crt")
	}

	server, err := k.discoverAPIServer()
	if err != nil {
		return err
	}
	podURL := "https://" + server + "/api/v1/namespaces/" +
		url.PathEscape(namespace) + "/pods/" + url.PathEscape(k.podName)

	req, err := http.NewRequest(http.MethodGet, podURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tokenBytes)))

	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
	}
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to read pod info: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("failed to read pod info: %s %s", res.Status, string(detail))
	}

	var pod k8sPod
	if err := json.NewDecoder(res.Body).Decode(&pod); err != nil {
		return fmt.Errorf("failed to parse pod info: %w", err)
	}
	if pod.Spec == nil {
		return fmt.Errorf("missing spec in pod info for %s/%s", k.podName, namespace)
	}
	shared := pod.Spec.ShareProcessNamespace
	if shared == nil {
		return fmt.Errorf("pod does not have spec.shareProcessNamespace set")
	}
	if !*shared {
		return fmt.Errorf("pod has set but disabled spec.shareProcessNamespace")
	}
	return nil
}

func (k *kubernetesIntegration) IsAvailable() bool {
	if !linuxOnly() {
		k.logger.Error("Kubernetes is only available on Linux")
		return false
	}

	k.logger.Debug("Checking Kubernetes service account", "root", k.saRoot)
	entries, err := os.ReadDir(k.saRoot)
	if err != nil {
		k.logger.Error("Failed to access serviceaccount root", "root", k.saRoot, "error", err)
		return false
	}
	if len(entries) == 0 {
		k.logger.Error("Kubernetes service account not found", "root", k.saRoot)
		return false
	}

	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		present[filepath.Join(k.saRoot, e.Name())] = true
	}
	expected := []string{
		filepath.Join(k.saRoot, "ca.crt"),
		filepath.Join(k.saRoot, "token"),
		filepath.Join(k.saRoot, "namespace"),
	}
	for _, f := range expected {
		if !present[f] {
			k.logger.Error("Malformed Kubernetes service account", "missing", f)
			return false
		}
	}

	namespaceBytes, err := os.ReadFile(filepath.Join(k.saRoot, "namespace"))
	if err != nil {
		k.logger.Error("Failed to read serviceaccount namespace", "error", err)
		return false
	}
	namespace := strings.TrimSpace(string(namespaceBytes))

	if !k.validateManifest {
		k.logger.Warn("Skipping strict Pod status check")
	} else {
		if k.podName == "" {
			k.logger.Error("Missing POD_NAME variable")
			return false
		}
		if strings.TrimSpace(k.podName) == "" {
			k.logger.Error("Pod name is empty")
			return false
		}

		k.logger.Info("Checking pod in namespace", "pod", k.podName, "namespace", namespace)
		if err := k.checkPodManifest(namespace); err != nil {
			k.logger.Error("Failed to read pod info", "error", err)
			return false
		}
		k.logger.Info("Pod enabled shared processes", "pod", k.podName)
	}

	pid, err := findHeadscaleServe(k.procRoot)
	if err != nil {
		k.logger.Error("Failed to scan /proc", "error", err)
		return false
	}
	if pid == 0 {
		k.logger.Error("Could not find headscale serve process")
		return false
	}

	k.pid = pid
	k.logger.Info("Found headscale serve", "pid", pid)
	return true
}

func (k *kubernetesIntegration) OnConfigChange(health func() bool) error {
	if k.pid == 0 {
		return nil
	}
	return signalAndWaitHealthy(k.pid, health)
}
