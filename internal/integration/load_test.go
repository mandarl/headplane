package integration

import "testing"

func TestLoadNoneEnabled(t *testing.T) {
	if got := Load(Config{}, testLogger()); got != nil {
		t.Fatalf("expected nil, got %v", got.Name())
	}
	if got := Load(Config{
		Docker:     &DockerConfig{Enabled: false},
		Kubernetes: &KubernetesConfig{Enabled: false},
		Proc:       &ProcConfig{Enabled: false},
	}, testLogger()); got != nil {
		t.Fatalf("expected nil, got %v", got.Name())
	}
}

func TestLoadMultipleEnabled(t *testing.T) {
	// Every pair must be rejected, not just all three (see Load docs).
	pairs := []Config{
		{Docker: &DockerConfig{Enabled: true}, Kubernetes: &KubernetesConfig{Enabled: true}},
		{Docker: &DockerConfig{Enabled: true}, Proc: &ProcConfig{Enabled: true}},
		{Kubernetes: &KubernetesConfig{Enabled: true}, Proc: &ProcConfig{Enabled: true}},
		{
			Docker:     &DockerConfig{Enabled: true},
			Kubernetes: &KubernetesConfig{Enabled: true},
			Proc:       &ProcConfig{Enabled: true},
		},
	}
	for i, cfg := range pairs {
		if got := Load(cfg, testLogger()); got != nil {
			t.Fatalf("pair %d: expected nil for multiple integrations, got %v", i, got.Name())
		}
	}
}

func TestLoadUnavailableReturnsNil(t *testing.T) {
	// Docker pointed at a nonexistent socket: chosen but not available.
	got := Load(Config{
		Docker: &DockerConfig{Enabled: true, Socket: "unix:///nonexistent/docker.sock"},
	}, testLogger())
	if got != nil {
		t.Fatalf("expected nil, got %v", got.Name())
	}
}
