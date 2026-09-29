package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultDockerSocket         = "unix:///var/run/docker.sock"
	defaultDockerContainerLabel = "me.tale.headplane.target=headscale"
	requiredDockerAPIVersion    = "1.44"
)

// compareAPIVersions compares two dotted version strings ("1.44") using the
// same part-wise numeric logic as the TS compareApiVersions: longer versions
// are zero-padded, non-numeric parts are an error.
func compareAPIVersions(current, required string) (int, error) {
	parse := func(s string) ([]int, error) {
		parts := strings.Split(s, ".")
		nums := make([]int, len(parts))
		for i, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil, fmt.Errorf("invalid Docker API version format: %q", s)
			}
			nums[i] = n
		}
		return nums, nil
	}

	currentParts, err := parse(current)
	if err != nil {
		return 0, err
	}
	requiredParts, err := parse(required)
	if err != nil {
		return 0, err
	}

	length := max(len(currentParts), len(requiredParts))
	for i := range length {
		var currentPart, requiredPart int
		if i < len(currentParts) {
			currentPart = currentParts[i]
		}
		if i < len(requiredParts) {
			requiredPart = requiredParts[i]
		}
		if currentPart > requiredPart {
			return 1, nil
		}
		if currentPart < requiredPart {
			return -1, nil
		}
	}
	return 0, nil
}

type dockerVersionInfo struct {
	ApiVersion string `json:"ApiVersion"`
}

type dockerContainer struct {
	Id    string   `json:"Id"`
	Names []string `json:"Names"`
}

type dockerIntegration struct {
	containerName  string
	containerLabel string
	socket         string
	logger         *slog.Logger

	client      *http.Client
	baseURL     string
	containerID string
	maxAttempts int
}

// ProcRoot-style overrides are not needed for Docker: tests point the
// socket at an httptest unix listener via the Socket config field.
func newDockerIntegration(cfg *DockerConfig, logger *slog.Logger) *dockerIntegration {
	socket := cfg.Socket
	if socket == "" {
		socket = defaultDockerSocket
	}
	label := cfg.ContainerLabel
	if label == "" {
		label = defaultDockerContainerLabel
	}
	return &dockerIntegration{
		containerName:  cfg.ContainerName,
		containerLabel: label,
		socket:         socket,
		logger:         logger,
		maxAttempts:    10,
	}
}

func (d *dockerIntegration) Name() string { return "Docker" }

// containersURL builds the container list request path with the name or
// label filter, mirroring the TS filters query param.
func (d *dockerIntegration) containersURL() (string, error) {
	var filters map[string][]string
	if d.containerName != "" {
		filters = map[string][]string{"name": {d.containerName}}
	} else {
		filters = map[string][]string{"label": {d.containerLabel}}
	}
	raw, err := json.Marshal(filters)
	if err != nil {
		return "", err
	}
	q := url.Values{"filters": []string{string(raw)}}.Encode()
	return "/v" + requiredDockerAPIVersion + "/containers/json?" + q, nil
}

// newClient builds the Docker API client for a unix:// or tcp:// socket.
func (d *dockerIntegration) newClient() error {
	u, err := url.Parse(d.socket)
	if err != nil {
		return fmt.Errorf("invalid Docker socket path %q: %w", d.socket, err)
	}

	switch u.Scheme {
	case "tcp":
		d.logger.Info("Checking Docker API", "url", "http://"+u.Host)
		d.baseURL = "http://" + u.Host
		d.client = &http.Client{Timeout: 30 * time.Second}
	case "unix":
		d.logger.Info("Checking Docker socket", "path", u.Path)
		// Verify the socket is readable before dialing it.
		if err := socketReadable(u.Path); err != nil {
			return fmt.Errorf("failed to access Docker socket %q: %w", u.Path, err)
		}
		d.baseURL = "http://localhost"
		d.client = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", u.Path)
				},
			},
		}
	default:
		return fmt.Errorf("invalid Docker socket protocol: %q", u.Scheme)
	}
	return nil
}

func (d *dockerIntegration) IsAvailable() bool {
	d.logger.Info("Requiring Docker API version or newer",
		"required", requiredDockerAPIVersion)

	// The name overrides the container_label selector (legacy support).
	if d.containerName == "" && d.containerLabel == "" {
		d.logger.Error("Missing a Docker `container_name` or `container_label`")
		return false
	}

	if err := d.newClient(); err != nil {
		d.logger.Error("Failed to create Docker client", "error", err)
		return false
	}

	// Validate the Docker API version.
	versionRes, err := d.client.Get(d.baseURL + "/version")
	if err != nil {
		d.logger.Error("Failed to validate Docker API version", "error", err)
		return false
	}
	defer versionRes.Body.Close()

	if versionRes.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(versionRes.Body, 4096))
		d.logger.Error("Could not request Docker API version", "detail", string(detail))
		return false
	}

	var versionInfo dockerVersionInfo
	if err := json.NewDecoder(versionRes.Body).Decode(&versionInfo); err != nil {
		d.logger.Error("Failed to parse Docker API version response", "error", err)
		return false
	}
	if versionInfo.ApiVersion == "" {
		d.logger.Error("Docker API version response is missing `ApiVersion`")
		return false
	}

	d.logger.Info("Detected Docker API version", "version", versionInfo.ApiVersion)
	supported, err := compareAPIVersions(versionInfo.ApiVersion, requiredDockerAPIVersion)
	if err != nil || supported < 0 {
		d.logger.Error("Docker API version is too old",
			"detected", versionInfo.ApiVersion, "required", requiredDockerAPIVersion)
		return false
	}

	// Discover the single Headscale container.
	path, err := d.containersURL()
	if err != nil {
		d.logger.Error("Failed to build container filter URL", "error", err)
		return false
	}
	d.logger.Debug("Requesting Docker containers", "path", path)
	res, err := d.client.Get(d.baseURL + path)
	if err != nil {
		d.logger.Error("Could not request available Docker containers", "error", err)
		return false
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		d.logger.Error("Could not request available Docker containers", "detail", string(detail))
		return false
	}

	var containers []dockerContainer
	if err := json.NewDecoder(res.Body).Decode(&containers); err != nil {
		d.logger.Error("Failed to parse Docker containers response", "error", err)
		return false
	}

	selector := d.containerName
	if selector == "" {
		selector = d.containerLabel
	}
	switch {
	case len(containers) > 1:
		d.logger.Error("Found multiple containers matching selector", "selector", selector)
		return false
	case len(containers) == 0:
		d.logger.Error("No container found matching selector", "selector", selector)
		return false
	}

	d.containerID = containers[0].Id
	name := ""
	if len(containers[0].Names) > 0 {
		name = containers[0].Names[0]
	}
	d.logger.Info("Using container", "name", name, "id", d.containerID)
	return d.client != nil && d.containerID != ""
}

func (d *dockerIntegration) OnConfigChange(health func() bool) error {
	if d.client == nil {
		return nil
	}

	d.logger.Info("Restarting Headscale via Docker")

	// Restart retry loop, mirroring the TS `attempts <= maxAttempts` loop
	// (up to maxAttempts+1 attempts, 1s sleeps between failures).
	attempts := 0
	for attempts <= d.maxAttempts {
		d.logger.Debug("Restarting container", "id", d.containerID, "attempt", attempts)
		res, err := d.client.Post(
			d.baseURL+"/v"+requiredDockerAPIVersion+"/containers/"+d.containerID+"/restart",
			"", nil)
		if err != nil {
			return fmt.Errorf("failed to restart container %s: %w", d.containerID, err)
		}
		body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		res.Body.Close()

		if res.StatusCode != http.StatusNoContent {
			if attempts < d.maxAttempts {
				attempts++
				sleep(time.Second)
				continue
			}
			return fmt.Errorf("API request failed: %d %s", res.StatusCode, string(body))
		}
		break
	}

	// Wait for Headscale to become healthy, mirroring the TS loop: the
	// first check is immediate, then up to maxAttempts more 1s apart. The
	// TS logs "Missed restart deadline" and returns without throwing here,
	// so we return nil (not an error) on exhaustion.
	attempts = 0
	for attempts <= d.maxAttempts {
		d.logger.Debug("Checking Headscale status", "attempt", attempts)
		if health() {
			d.logger.Info("Headscale is up and running")
			return nil
		}
		if attempts < d.maxAttempts {
			attempts++
			sleep(time.Second)
			continue
		}
		d.logger.Error("Missed restart deadline", "container", d.containerID)
		return nil
	}
	return nil
}
