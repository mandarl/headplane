package serverconfig

import (
	"fmt"
	"time"
)

// AgentConfig mirrors the headplane.yaml `integration.agent` section (full
// schema in config-schema.ts). The section itself is optional; Enabled must
// be set explicitly when present.
type AgentConfig struct {
	Enabled        bool   `yaml:"enabled"`
	HostName       string `yaml:"host_name"`
	CacheTTLMs     int64  `yaml:"cache_ttl"`
	ExecutablePath string `yaml:"executable_path"`
	WorkDir        string `yaml:"work_dir"`
}

// applyDefaults fills the schema defaults, mirroring agentConfig.
func (a *AgentConfig) applyDefaults() {
	if a.HostName == "" {
		a.HostName = "headplane-agent"
	}
	if a.CacheTTLMs <= 0 {
		a.CacheTTLMs = 180000
	}
	if a.ExecutablePath == "" {
		a.ExecutablePath = "/usr/libexec/headplane/agent"
	}
	if a.WorkDir == "" {
		a.WorkDir = "/var/lib/headplane/agent"
	}
}

// CacheTTL returns the cache TTL as a duration (config unit is
// milliseconds, mirroring the TS schema).
func (a *AgentConfig) CacheTTL() time.Duration {
	return time.Duration(a.CacheTTLMs) * time.Millisecond
}

// DockerIntegrationConfig mirrors `integration.docker` (full schema).
type DockerIntegrationConfig struct {
	Enabled        bool   `yaml:"enabled"`
	ContainerName  string `yaml:"container_name"`
	ContainerLabel string `yaml:"container_label"`
	Socket         string `yaml:"socket"`
}

func (d *DockerIntegrationConfig) applyDefaults() {
	if d.ContainerLabel == "" {
		d.ContainerLabel = "me.tale.headplane.target=headscale"
	}
	if d.Socket == "" {
		d.Socket = "unix:///var/run/docker.sock"
	}
}

// KubernetesIntegrationConfig mirrors `integration.kubernetes` (full
// schema). PodName is required when Enabled; ValidateManifest defaults to
// true like the TS schema.
type KubernetesIntegrationConfig struct {
	Enabled          bool   `yaml:"enabled"`
	PodName          string `yaml:"pod_name"`
	ValidateManifest *bool  `yaml:"validate_manifest"`
}

func (k *KubernetesIntegrationConfig) validateManifest() bool {
	return k.ValidateManifest == nil || *k.ValidateManifest
}

// ProcIntegrationConfig mirrors `integration.proc` (full schema).
type ProcIntegrationConfig struct {
	Enabled bool `yaml:"enabled"`
}

// IntegrationConfig mirrors the headplane.yaml `integration` section.
type IntegrationConfig struct {
	Docker     *DockerIntegrationConfig     `yaml:"docker"`
	Kubernetes *KubernetesIntegrationConfig `yaml:"kubernetes"`
	Proc       *ProcIntegrationConfig       `yaml:"proc"`
	Agent      *AgentConfig                 `yaml:"agent"`
}

// applyDefaults applies schema defaults to the present sections.
func (c *IntegrationConfig) applyDefaults() {
	if c == nil {
		return
	}
	if c.Docker != nil {
		c.Docker.applyDefaults()
	}
	if c.Agent != nil {
		c.Agent.applyDefaults()
	}
}

// Validate mirrors the schema requirements: kubernetes.pod_name must be
// set when the kubernetes integration is enabled.
func (c *IntegrationConfig) Validate() error {
	if c == nil {
		return nil
	}
	if c.Kubernetes != nil && c.Kubernetes.Enabled && c.Kubernetes.PodName == "" {
		return fmt.Errorf("serverconfig: integration.kubernetes.pod_name is required when the kubernetes integration is enabled")
	}
	return nil
}
