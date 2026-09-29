package integration

import (
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// k8sTestFixture builds a fake serviceaccount dir (ca.crt/token/namespace)
// and a fake /proc tree containing a headscale serve process, plus an
// httptest TLS server acting as the apiserver.
func k8sTestFixture(t *testing.T, podBody string) (saRoot, procRoot, apiServer string, mux *http.ServeMux) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("k8s integration is linux-only")
	}

	srv := httptest.NewTLSServer(nil)
	t.Cleanup(srv.Close)
	mux = http.NewServeMux()
	srv.Config.Handler = mux

	// Trust the test server: write its cert as the serviceaccount ca.crt.
	der := srv.Certificate().Raw
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	saRoot = t.TempDir()
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(saRoot, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("ca.crt", string(caPEM))
	write("token", "test-token")
	write("namespace", "testns\n")

	procRoot = fakeProc(t, map[string][2]string{
		"4242": {"headscale", "headscale\x00serve\x00"},
	})

	addr := srv.Listener.Addr().(*net.TCPAddr)
	apiServer = addr.String()

	if podBody != "" {
		mux.HandleFunc("/api/v1/namespaces/testns/pods/headscale-0",
			func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer test-token" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(podBody))
			})
	}
	return saRoot, procRoot, apiServer, mux
}

func TestKubernetesIsAvailable(t *testing.T) {
	saRoot, procRoot, apiServer, _ := k8sTestFixture(t,
		`{"spec":{"shareProcessNamespace":true}}`)

	k := newKubernetesIntegration(
		&KubernetesConfig{Enabled: true, PodName: "headscale-0"},
		testLogger(),
		WithServiceAccountRoot(saRoot),
		WithK8sProcRoot(procRoot),
		WithAPIServer(apiServer),
	)
	if !k.IsAvailable() {
		t.Fatal("expected integration to be available")
	}
	if k.pid != 4242 {
		t.Fatalf("unexpected pid: %d", k.pid)
	}
	if k.Name() != "Kubernetes (k8s)" {
		t.Fatalf("unexpected name: %q", k.Name())
	}
}

func TestKubernetesSharedNamespaceDisabled(t *testing.T) {
	saRoot, procRoot, apiServer, _ := k8sTestFixture(t,
		`{"spec":{"shareProcessNamespace":false}}`)

	k := newKubernetesIntegration(
		&KubernetesConfig{Enabled: true, PodName: "headscale-0"},
		testLogger(),
		WithServiceAccountRoot(saRoot),
		WithK8sProcRoot(procRoot),
		WithAPIServer(apiServer),
	)
	if k.IsAvailable() {
		t.Fatal("expected unavailable when shareProcessNamespace is false")
	}
}

func TestKubernetesSharedNamespaceUnset(t *testing.T) {
	saRoot, procRoot, apiServer, _ := k8sTestFixture(t, `{"spec":{}}`)

	k := newKubernetesIntegration(
		&KubernetesConfig{Enabled: true, PodName: "headscale-0"},
		testLogger(),
		WithServiceAccountRoot(saRoot),
		WithK8sProcRoot(procRoot),
		WithAPIServer(apiServer),
	)
	if k.IsAvailable() {
		t.Fatal("expected unavailable when shareProcessNamespace is unset")
	}
}

func TestKubernetesMissingPod(t *testing.T) {
	saRoot, procRoot, apiServer, _ := k8sTestFixture(t,
		`{"spec":{"shareProcessNamespace":true}}`)

	// Pod name not served by the fake apiserver -> 404.
	k := newKubernetesIntegration(
		&KubernetesConfig{Enabled: true, PodName: "does-not-exist"},
		testLogger(),
		WithServiceAccountRoot(saRoot),
		WithK8sProcRoot(procRoot),
		WithAPIServer(apiServer),
	)
	if k.IsAvailable() {
		t.Fatal("expected unavailable for missing pod")
	}
}

func TestKubernetesMissingPodName(t *testing.T) {
	saRoot, procRoot, apiServer, _ := k8sTestFixture(t,
		`{"spec":{"shareProcessNamespace":true}}`)

	k := newKubernetesIntegration(
		&KubernetesConfig{Enabled: true, PodName: ""},
		testLogger(),
		WithServiceAccountRoot(saRoot),
		WithK8sProcRoot(procRoot),
		WithAPIServer(apiServer),
	)
	if k.IsAvailable() {
		t.Fatal("expected unavailable with empty pod name")
	}
}

func TestKubernetesSkipManifestValidation(t *testing.T) {
	saRoot, procRoot, _, _ := k8sTestFixture(t, "")
	// No pod handler registered and no apiServer override needed: with
	// ValidateManifest=false the apiserver is never contacted.
	k := newKubernetesIntegration(
		&KubernetesConfig{Enabled: true, PodName: "", ValidateManifest: boolPtr(false)},
		testLogger(),
		WithServiceAccountRoot(saRoot),
		WithK8sProcRoot(procRoot),
	)
	if !k.IsAvailable() {
		t.Fatal("expected available when manifest validation is skipped")
	}
}

func TestKubernetesValidateManifestDefault(t *testing.T) {
	k := newKubernetesIntegration(
		&KubernetesConfig{Enabled: true, PodName: "x"},
		testLogger(),
	)
	if !k.validateManifest {
		t.Fatal("ValidateManifest should default to true when nil")
	}
}

func TestKubernetesMalformedServiceAccount(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("k8s integration is linux-only")
	}
	procRoot := fakeProc(t, map[string][2]string{
		"4242": {"headscale", "headscale\x00serve\x00"},
	})

	// Missing token file.
	saRoot := t.TempDir()
	_ = os.WriteFile(filepath.Join(saRoot, "ca.crt"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(saRoot, "namespace"), []byte("ns"), 0o600)

	k := newKubernetesIntegration(
		&KubernetesConfig{Enabled: true, PodName: "headscale-0"},
		testLogger(),
		WithServiceAccountRoot(saRoot),
		WithK8sProcRoot(procRoot),
		WithAPIServer("127.0.0.1:1"),
	)
	if k.IsAvailable() {
		t.Fatal("expected unavailable with malformed serviceaccount")
	}
}

func TestKubernetesMissingServiceAccount(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("k8s integration is linux-only")
	}
	procRoot := fakeProc(t, map[string][2]string{
		"4242": {"headscale", "headscale\x00serve\x00"},
	})
	k := newKubernetesIntegration(
		&KubernetesConfig{Enabled: true, PodName: "headscale-0"},
		testLogger(),
		WithServiceAccountRoot(filepath.Join(t.TempDir(), "nope")),
		WithK8sProcRoot(procRoot),
	)
	if k.IsAvailable() {
		t.Fatal("expected unavailable with missing serviceaccount")
	}
}

func TestKubernetesPodAuthHeader(t *testing.T) {
	saRoot, procRoot, apiServer, mux := k8sTestFixture(t, "")
	var gotAuth string
	mux.HandleFunc("/api/v1/namespaces/testns/pods/headscale-0",
		func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"spec":{"shareProcessNamespace":true}}`))
		})

	k := newKubernetesIntegration(
		&KubernetesConfig{Enabled: true, PodName: "headscale-0"},
		testLogger(),
		WithServiceAccountRoot(saRoot),
		WithK8sProcRoot(procRoot),
		WithAPIServer(apiServer),
	)
	if !k.IsAvailable() {
		t.Fatal("expected available")
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("unexpected Authorization header: %q", gotAuth)
	}
}
