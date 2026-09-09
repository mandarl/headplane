package main

// Phase 4: the versioned JSON API the SPA talks to (the /admin/api/v1/*
// surface), plus the /admin/events/live SSE stream. This is the Go port of
// the old React Router loaders/actions in app/routes/**, the
// app/server/headscale/api resource modules behind them, and
// app/routes/util/live.ts.
//
// Wire contract notes (mirroring server/interim-stub.mjs, the Phase 0
// authority on shapes): every response is application/json; absent optional
// values serialize as null, never undefined; redirects are JSON bodies
// {redirect: "..."} the SPA follows client-side.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tale/headplane/internal/auth"
	"github.com/tale/headplane/internal/hsapi"
	"github.com/tale/headplane/internal/live"
)

// apiPrefix is the SPA's API mount point; apiGet/apiAction call
// `${basename}/api/v1${path}`.
const apiPrefix = "/api/v1"

// maxJSONBody bounds v1 request bodies (actions carry small JSON payloads).
const maxJSONBody = 1 << 20

// writeJSON serializes v as application/json with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "Failed to encode response."})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError is the v1 equivalent of React Router's data(message, {status}):
// a JSON error body with the status the old route used.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": message})
}

// writeAPIError mirrors writeAPIError.ts + error-client.ts: upstream
// Headscale failures keep their status code on the wire with a friendly
// message; connection failures become 502; anything else is a 500. The
// raw upstream body stays server-side (the TS only surfaced rawData for
// "acl policy not found"-style inspection, never to the browser).
func writeAPIError(w http.ResponseWriter, err error) {
	var apiErr *hsapi.APIError
	if errors.As(err, &apiErr) {
		status := apiErr.StatusCode
		if status < 400 {
			status = http.StatusBadGateway
		}
		if status == http.StatusNotFound {
			writeError(w, status, "The requested resource was not found.")
			return
		}
		writeError(w, status, friendlyUpstreamMessage(apiErr))
		return
	}
	var connErr *hsapi.ConnError
	if errors.As(err, &connErr) {
		writeError(w, http.StatusBadGateway,
			"Headscale is not reachable. Check that Headscale is running and the configured URL is correct.")
		return
	}
	writeError(w, http.StatusInternalServerError, "An unexpected error occurred.")
}

// friendlyUpstreamMessage mirrors error-client.ts's friendlyMessage: it
// unwraps the {message} envelope when the upstream body parses, and falls
// back to the raw body.
func friendlyUpstreamMessage(apiErr *hsapi.APIError) string {
	if apiErr.Data != nil {
		if msg, ok := apiErr.Data["message"].(string); ok && msg != "" {
			return msg
		}
	}
	if apiErr.RawData != "" {
		return apiErr.RawData
	}
	return fmt.Sprintf("Headscale returned status %d.", apiErr.StatusCode)
}

// readJSONBody decodes a v1 action body into a string map. Missing or
// malformed bodies yield an empty map (form fields are all optional at the
// transport layer; each action validates what it needs).
func readJSONBody(r *http.Request) map[string]any {
	out := map[string]any{}
	if r.Body == nil {
		return out
	}
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, maxJSONBody))
	if err := dec.Decode(&out); err != nil {
		return map[string]any{}
	}
	return out
}

// formStr pulls an optional string field out of an action body. A JSON
// array (from repeated form fields) joins with "," mirroring
// formData.get(...).toString() on a multi-value entry.
func formStr(body map[string]any, key string) string {
	v, ok := body[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case []any:
		parts := make([]string, 0, len(t))
		for _, p := range t {
			if s, ok := p.(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ",")
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

// hsClientFor mirrors apiForRequest in app/server/headscale/api/index.ts:
// it authenticates the request, resolves the per-request Headscale API key
// (the caller's own key for API-key principals, the configured key for
// OIDC principals), and returns a client bound to it. Auth failures are
// 401; a missing key is a 500, like the TS "No Headscale API key is
// configured".
func (s *Server) hsClientFor(r *http.Request) (*hsapi.Client, *auth.Principal, error) {
	p, err := s.authSvc.Require(r)
	if err != nil {
		return nil, nil, errNeedAuth
	}
	key, err := s.authSvc.GetHeadscaleAPIKey(p)
	if err != nil || key == "" {
		return nil, nil, errNoAPIKey
	}
	return hsapi.NewClient(s.cfg.Headscale.URL, key, s.cfg.Headscale.TLSCertPath, s.getHsCaps(), s.logger), p, nil
}

var (
	errNeedAuth = errors.New("auth required")
	errNoAPIKey = errors.New("no Headscale API key is configured for this session")
)

// requirePrincipal is the v1 analogue of the TS loaders' apiForRequest
// auth step: 401 when the session is missing/invalid.
func (s *Server) requirePrincipal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p, err := s.authSvc.Require(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "Authentication required.")
		return nil
	}
	return p
}

// apiClientFor is hsClientFor adapted for handlers that already hold the
// principal: it writes the 500 for a missing key and returns nil.
func (s *Server) apiClientFor(w http.ResponseWriter, p *auth.Principal) *hsapi.Client {
	key, err := s.authSvc.GetHeadscaleAPIKey(p)
	if err != nil || key == "" {
		writeError(w, http.StatusInternalServerError, "No Headscale API key is configured for this session.")
		return nil
	}
	return hsapi.NewClient(s.cfg.Headscale.URL, key, s.cfg.Headscale.TLSCertPath, s.getHsCaps(), s.logger)
}

// liveSnapshots fetches the nodes+users snapshots, using the shared store
// when it has a client and falling back to a direct per-request fetch when
// it doesn't (OIDC-only deployments with no default API key). Upstream
// failures propagate like the TS loaders' thrown errors.
func (s *Server) liveSnapshots(ctx context.Context, api *hsapi.Client) (nodes []hsapi.Node, users []hsapi.User, err error) {
	nodeSnap, err := s.liveStore.Get(ctx, live.NodesResource, api)
	if err != nil {
		if errors.Is(err, live.ErrNoClient) {
			var raw any
			if raw, err = live.NodesResource.Fetch(ctx, api); err != nil {
				return nil, nil, err
			}
			nodes, _ = raw.([]hsapi.Node)
		} else {
			return nil, nil, err
		}
	} else {
		nodes, _ = nodeSnap.Data.([]hsapi.Node)
	}
	userSnap, err := s.liveStore.Get(ctx, live.UsersResource, api)
	if err != nil {
		if errors.Is(err, live.ErrNoClient) {
			var raw any
			if raw, err = live.UsersResource.Fetch(ctx, api); err != nil {
				return nil, nil, err
			}
			users, _ = raw.([]hsapi.User)
		} else {
			return nil, nil, err
		}
	} else {
		users, _ = userSnap.Data.([]hsapi.User)
	}
	return nodes, users, nil
}
