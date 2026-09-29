// Package hsapi is the Go port of app/server/headscale/api (transport.ts,
// index.ts, capabilities.ts, server-version.ts and resources/*).
//
// It owns the HTTP transport to the Headscale server: base URL, the 30 s
// upstream timeout, the `Authorization: Bearer` injection (the browser never
// sees the key), the authenticated `/api/v1` path convention, error
// translation, and the version-derived capability flags that gate
// version-dependent wire behavior (pre-0.28 tag shapes, pre-auth key
// expiry, node owner reassignment).
package hsapi

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// version is the Headplane version reported in the User-Agent header,
// mirroring the TS transport's `Headplane/${__VERSION__}`. Overridable at
// build time: go build -ldflags "-X github.com/tale/headplane/internal/hsapi.version=1.2.3".
var version = "dev"

// upstreamTimeout mirrors the task contract: every Headscale request must
// fail fast rather than hang a handler goroutine forever.
const upstreamTimeout = 30 * time.Second

// maxBody caps how much of an upstream error body we buffer for the
// friendly error payload; success bodies are decoded by the streaming
// decoder without a cap beyond the connection itself.
const maxBody = 4 << 20

// Capabilities mirrors app/server/headscale/api/capabilities.ts. Each flag
// is named for what changed in Headscale, derived once from /version.
type Capabilities struct {
	// Pre-auth keys have stable IDs; GET /api/v1/preauthkey (no filter)
	// lists every key and expire takes {id}. Tag-only keys supported.
	// Introduced in Headscale 0.28.0.
	PreAuthKeysHaveStableIds bool
	// Node tags are a flat tags: string[] on the wire. Older servers
	// returned forcedTags/validTags/invalidTags. Introduced in 0.28.0.
	NodeTagsAreFlat bool
	// A node's owning user is immutable; POST /api/v1/node/{id}/user no
	// longer reassigns. Effective in 0.28.0+.
	NodeOwnerIsImmutable bool
}

// ServerVersion mirrors server-version.ts. Unknown versions are treated as
// having every known capability (capabilities-permissive), exactly like
// gte() returning true for unknown.
type ServerVersion struct {
	Raw     string
	Major   int
	Minor   int
	Patch   int
	Unknown bool
}

// ParseServerVersion mirrors parseServerVersion. Unparseable input yields
// an unknown version (capabilities-permissive).
func ParseServerVersion(raw string) ServerVersion {
	v := ServerVersion{Raw: raw}
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "v")
	// Strip pre-release/build metadata: "0.28.0-beta.1" -> "0.28.0".
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		v.Unknown = true
		return v
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			v.Unknown = true
			return v
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v
}

// GTE mirrors gte(): unknown versions compare true (permissive).
func (v ServerVersion) GTE(major, minor, patch int) bool {
	if v.Unknown {
		return true
	}
	if v.Major != major {
		return v.Major > major
	}
	if v.Minor != minor {
		return v.Minor > minor
	}
	return v.Patch >= patch
}

// CapabilitiesFor mirrors capabilitiesFor: every flag keys off 0.28.0.
func CapabilitiesFor(v ServerVersion) Capabilities {
	return Capabilities{
		PreAuthKeysHaveStableIds: v.GTE(0, 28, 0),
		NodeTagsAreFlat:          v.GTE(0, 28, 0),
		NodeOwnerIsImmutable:     v.GTE(0, 28, 0),
	}
}

// APIError mirrors HeadscaleAPIError: an upstream 4xx/5xx with the raw body
// preserved. The v1 layer preserves the upstream status code on the wire.
type APIError struct {
	RequestURL string // e.g. "GET v1/node"
	StatusCode int
	RawData    string
	Data       map[string]any // parsed JSON body, or nil
}

func (e *APIError) Error() string {
	return fmt.Sprintf("headscale: %s failed with status %d", e.RequestURL, e.StatusCode)
}

// ConnError mirrors HeadscaleConnectionError: the request never got a
// response. The v1 layer maps these to 502, like the TS data(..., 502).
type ConnError struct {
	RequestURL   string
	ErrorCode    string
	ErrorMessage string
}

func (e *ConnError) Error() string {
	return fmt.Sprintf("headscale: %s: %s", e.RequestURL, e.ErrorMessage)
}

// Client is a Headscale API client bound to one API key, mirroring
// HeadscaleClient in index.ts. Build one per request via Server.hsClientFor
// (per-request key resolution); the client itself holds no mutable state
// and is safe for concurrent use.
type Client struct {
	baseURL string
	apiKey  string
	caps    Capabilities
	http    *http.Client
	logger  *slog.Logger
}

// NewClient mirrors createTransport + the client() factory: it wires the
// base URL, the 30 s timeout, and the optional custom CA (headscale
// tls_cert_path). A cert load failure logs and falls back to the system
// pool, exactly like createUndiciAgent.
func NewClient(baseURL, apiKey, certPath string, caps Capabilities, logger *slog.Logger) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if certPath != "" {
		if pem, err := os.ReadFile(certPath); err != nil {
			logger.Error("failed to load Headscale TLS cert", "path", certPath, "error", err)
		} else {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				logger.Error("failed to parse Headscale TLS cert", "path", certPath)
			} else {
				t := transport.Clone()
				t.TLSClientConfig = &tls.Config{RootCAs: pool}
				transport = t
				logger.Info("using Headscale TLS certificate", "path", certPath)
			}
		}
	}
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		caps:    caps,
		http:    &http.Client{Timeout: upstreamTimeout, Transport: transport},
		logger:  logger.With("component", "hsapi"),
	}
}

// Caps returns the version-derived capability flags.
func (c *Client) Caps() Capabilities { return c.caps }

// request mirrors Transport.request: an authenticated JSON call against
// /api/{path}. Query params are appended for GET/DELETE; a body is JSON
// encoded for other methods. Transport failures become *ConnError;
// upstream >= 400 becomes *APIError with the status preserved.
func (c *Client) request(method, path string, query map[string]string, body any) (json.RawMessage, error) {
	requestURL := method + " " + path
	u := c.baseURL + "/api/" + strings.TrimPrefix(path, "/")
	if len(query) > 0 {
		q := url.Values{}
		for k, v := range query {
			if v != "" {
				q.Set(k, v)
			}
		}
		if enc := q.Encode(); enc != "" {
			u += "?" + enc
		}
	}

	var bodyReader io.Reader
	if body != nil && method != http.MethodGet && method != http.MethodDelete {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(buf)
	}

	req, err := http.NewRequest(method, u, bodyReader)
	if err != nil {
		return nil, err
	}
	// The key is injected server-side on every request; the browser never
	// sees it. Headers mirror the TS transport defaults.
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Headplane/"+version)
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	c.logger.Debug("request", "method", method, "url", u)
	res, err := c.http.Do(req)
	if err != nil {
		return nil, &ConnError{
			RequestURL:   requestURL,
			ErrorCode:    errorCodeOf(err),
			ErrorMessage: err.Error(),
		}
	}
	defer res.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return nil, &ConnError{RequestURL: requestURL, ErrorCode: "READ_BODY", ErrorMessage: err.Error()}
	}

	if res.StatusCode >= 400 {
		c.logger.Debug("request failed", "method", method, "path", path, "status", res.StatusCode)
		var parsed map[string]any
		if json.Unmarshal(raw, &parsed) != nil {
			parsed = nil
		}
		return nil, &APIError{
			RequestURL: requestURL,
			StatusCode: res.StatusCode,
			RawData:    string(raw),
			Data:       parsed,
		}
	}

	return json.RawMessage(bytes.Clone(raw)), nil
}

func errorCodeOf(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		if urlErr.Timeout() {
			return "TIMEOUT"
		}
	}
	return "CONNECTION_ERROR"
}

// getPublic mirrors Transport.getPublic: an unauthenticated GET against the
// server root (e.g. /version, /health).
func (c *Client) getPublic(path string) (json.RawMessage, int, error) {
	requestURL := "GET " + path
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Headplane/"+version)
	res, err := c.http.Do(req)
	if err != nil {
		return nil, 0, &ConnError{RequestURL: requestURL, ErrorCode: errorCodeOf(err), ErrorMessage: err.Error()}
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return nil, 0, &ConnError{RequestURL: requestURL, ErrorCode: "READ_BODY", ErrorMessage: err.Error()}
	}
	if res.StatusCode >= 400 {
		var parsed map[string]any
		if json.Unmarshal(raw, &parsed) != nil {
			parsed = nil
		}
		return nil, res.StatusCode, &APIError{RequestURL: requestURL, StatusCode: res.StatusCode, RawData: string(raw), Data: parsed}
	}
	return json.RawMessage(bytes.Clone(raw)), res.StatusCode, nil
}

// DetectVersion probes GET /version (unauthenticated, present since 0.27.0)
// and returns the parsed server version. It mirrors detectOnce in index.ts:
// a 404 means the server predates 0.27.0 (below the supported floor).
func DetectVersion(baseURL, certPath string, logger *slog.Logger) (ServerVersion, error) {
	c := NewClient(baseURL, "", certPath, Capabilities{}, logger)
	raw, _, err := c.getPublic("/version")
	if err != nil {
		return ServerVersion{}, err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ServerVersion{}, fmt.Errorf("hsapi: cannot parse /version: %w", err)
	}
	return ParseServerVersion(v.Version), nil
}

// Health mirrors Transport.health: true when GET /health returns 200.
// It never returns an error.
func (c *Client) Health() bool {
	_, status, err := c.getPublic("/health")
	if err != nil {
		return false
	}
	return status == http.StatusOK
}

// decode unmarshals a response envelope field (e.g. {"nodes": [...]})
// into out.
func decode(raw json.RawMessage, field string, out any) error {
	if field == "" {
		return json.Unmarshal(raw, out)
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	data, ok := env[field]
	if !ok {
		return fmt.Errorf("hsapi: response missing field %q", field)
	}
	return json.Unmarshal(data, out)
}
