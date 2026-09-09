// Package rdpgw is a thin HTTP client for the user-configured RDP gateway
// webhook. The webhook is responsible for the actual relay setup (e.g.
// starting a socat forwarder and toggling a cloud firewall rule); headplane
// only knows about the generic enable/disable/status contract below.
//
// Webhook contract
// ─────────────────
// POST {webhook_url}
// Authorization: Bearer {webhook_token}   (only if configured)
// Content-Type: application/json
//
// Enable:
//
//	{ "action": "enable", "target_ip": "100.x.y.z",
//	  "hostname": "vdi-name", "caller_ip": "1.2.3.4",
//	  "timeout_mins": 180 }                            (optional, webhook default if absent)
//	→ 200 { "host": "...", "port": 33001, "expires_at": "<ISO8601>" }
//
// Disable:
//
//	{ "action": "disable", "target_ip": "100.x.y.z", "hostname": "vdi-name" }
//	→ 200 { "ok": true }
//
// Status:
//
//	{ "action": "status", "target_ip": "100.x.y.z", "hostname": "vdi-name" }
//	→ 200 { "active": false }
//	→ 200 { "active": true, "host": "...", "port": 33001, "expires_at": "<ISO8601>" }
//
// This is a port of app/server/web/rdp-gateway.ts.
package rdpgw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// EnableResult is the webhook's enable response.
type EnableResult struct {
	// Host is the publicly reachable hostname or IP of the relay host.
	Host string `json:"host"`
	// Port is the TCP port that has been opened on the relay host.
	Port int `json:"port"`
	// ExpiresAt is the ISO8601 timestamp when the gateway will auto-close
	// this session.
	ExpiresAt string `json:"expires_at"`
}

// StatusResult is the webhook's status response.
type StatusResult struct {
	Active    bool   `json:"active"`
	Host      string `json:"host,omitempty"`
	Port      int    `json:"port,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// Client talks to the RDP gateway webhook.
type Client struct {
	webhookURL   string
	webhookToken string
	http         *http.Client
	logger       *slog.Logger
}

// NewClient builds a Client for the given webhook URL and optional bearer
// token (empty token = no Authorization header). The HTTP client has a 60s
// timeout.
func NewClient(webhookURL, webhookToken string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		webhookURL:   webhookURL,
		webhookToken: webhookToken,
		http:         &http.Client{Timeout: 60 * time.Second},
		logger:       logger,
	}
}

type enablePayload struct {
	Action      string `json:"action"`
	TargetIP    string `json:"target_ip"`
	Hostname    string `json:"hostname"`
	CallerIP    string `json:"caller_ip"`
	TimeoutMins *int   `json:"timeout_mins,omitempty"`
}

type actionPayload struct {
	Action   string `json:"action"`
	TargetIP string `json:"target_ip"`
	Hostname string `json:"hostname"`
}

// do POSTs payload as JSON and decodes the JSON response into out (when
// non-nil). A non-2xx response is an error; like the TS client, the detail
// is logged server-side and the returned error is generic — the response
// body text is never included in the error value.
func (c *Client) do(ctx context.Context, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to encode webhook request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("failed to build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.webhookToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.webhookToken)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("rdp gateway webhook request failed: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// Drain for connection reuse, but keep the body out of the error.
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64*1024))
		c.logger.Error("RDP gateway webhook error", "status", res.Status)
		return fmt.Errorf("rdp gateway webhook error: %s", res.Status)
	}

	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			return fmt.Errorf("failed to decode webhook response: %w", err)
		}
	} else {
		_, _ = io.Copy(io.Discard, res.Body)
	}
	return nil
}

// Enable asks the webhook to open an RDP relay session. timeoutMins is
// omitted from the request when nil (the webhook then uses its default).
func (c *Client) Enable(ctx context.Context, targetIP, hostname, callerIP string, timeoutMins *int) (EnableResult, error) {
	var result EnableResult
	err := c.do(ctx, enablePayload{
		Action:      "enable",
		TargetIP:    targetIP,
		Hostname:    hostname,
		CallerIP:    callerIP,
		TimeoutMins: timeoutMins,
	}, &result)
	return result, err
}

// Disable asks the webhook to close the RDP relay session.
func (c *Client) Disable(ctx context.Context, targetIP, hostname string) error {
	return c.do(ctx, actionPayload{
		Action:   "disable",
		TargetIP: targetIP,
		Hostname: hostname,
	}, nil)
}

// Status asks the webhook for the current RDP relay session state.
func (c *Client) Status(ctx context.Context, targetIP, hostname string) (StatusResult, error) {
	var result StatusResult
	err := c.do(ctx, actionPayload{
		Action:   "status",
		TargetIP: targetIP,
		Hostname: hostname,
	}, &result)
	return result, err
}
