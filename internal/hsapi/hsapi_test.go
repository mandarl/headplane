package hsapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testClient(t *testing.T, h http.Handler, caps Capabilities) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "test-key", "", caps, testLogger()), srv
}

func TestParseServerVersion(t *testing.T) {
	v := ParseServerVersion("0.28.0")
	if v.Unknown || v.Major != 0 || v.Minor != 28 || v.Patch != 0 {
		t.Fatalf("bad parse: %+v", v)
	}
	caps := CapabilitiesFor(v)
	if !caps.PreAuthKeysHaveStableIds || !caps.NodeTagsAreFlat || !caps.NodeOwnerIsImmutable {
		t.Fatalf("0.28.0 should enable all caps: %+v", caps)
	}

	v = ParseServerVersion("0.27.3")
	caps = CapabilitiesFor(v)
	if caps.PreAuthKeysHaveStableIds || caps.NodeTagsAreFlat || caps.NodeOwnerIsImmutable {
		t.Fatalf("0.27.3 should disable all caps: %+v", caps)
	}

	// Unknown versions are capabilities-permissive, like TS gte().
	for _, raw := range []string{"unreachable", "garbage", "v1.2", "1.2.3.4"} {
		v := ParseServerVersion(raw)
		if !v.Unknown {
			t.Fatalf("%q should be unknown", raw)
		}
		if c := CapabilitiesFor(v); !c.PreAuthKeysHaveStableIds {
			t.Fatalf("%q should be permissive", raw)
		}
	}

	if v := ParseServerVersion("v0.28.1"); v.Unknown || v.Patch != 1 {
		t.Fatalf("v-prefix not handled: %+v", v)
	}
}

func TestRequestHeadersAndPath(t *testing.T) {
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/node" {
			t.Errorf("path = %q, want /api/v1/node", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q", got)
		}
		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "Headplane/") {
			t.Errorf("User-Agent = %q", ua)
		}
		if r.URL.Query().Get("user") != "1" {
			t.Errorf("query not passed through: %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"nodes":[]}`))
	}), Capabilities{})
	raw, err := c.request("GET", "v1/node", map[string]string{"user": "1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"nodes":[]}` {
		t.Fatalf("body = %s", raw)
	}
}

func TestRequestUpstreamError(t *testing.T) {
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"no such node"}`))
	}), Capabilities{})
	_, err := c.request("GET", "v1/node/99", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %T, want *APIError", err)
	}
	if apiErr.StatusCode != 404 || apiErr.RequestURL != "GET v1/node/99" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
	if apiErr.Data["message"] != "no such node" {
		t.Fatalf("parsed data = %v", apiErr.Data)
	}
}

func TestRequestConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now nothing listens
	c := NewClient(url, "k", "", Capabilities{}, testLogger())
	_, err := c.request("GET", "v1/node", nil, nil)
	var connErr *ConnError
	if !errors.As(err, &connErr) {
		t.Fatalf("err = %T (%v), want *ConnError", err, err)
	}
}

func TestRequestTimeout(t *testing.T) {
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // longer than the client timeout below
	}), Capabilities{})
	// Shrink the client timeout for the test via the exported knob.
	c.http.Timeout = 100 * time.Millisecond
	_, err := c.request("GET", "v1/node", nil, nil)
	var connErr *ConnError
	if !errors.As(err, &connErr) {
		t.Fatalf("err = %T, want *ConnError", err)
	}
	if connErr.ErrorCode != "TIMEOUT" {
		t.Fatalf("code = %q, want TIMEOUT", connErr.ErrorCode)
	}
}

func TestRegisterNodeSendsQueryAndBody(t *testing.T) {
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q", r.Method)
		}
		if r.URL.Query().Get("user") != "1" || r.URL.Query().Get("key") != "sekret" {
			t.Errorf("query = %q", r.URL.RawQuery)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["user"] != "1" || body["key"] != "sekret" {
			t.Errorf("body = %v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"node":{"id":"2","tags":[]}}`))
	}), Capabilities{NodeTagsAreFlat: true})
	node, err := c.RegisterNode("1", "sekret")
	if err != nil {
		t.Fatal(err)
	}
	if node["id"] != "2" {
		t.Fatalf("node = %v", node)
	}
}

func TestNormalizeNodeTags(t *testing.T) {
	flat := NewClient("http://x", "k", "", Capabilities{NodeTagsAreFlat: true}, testLogger())
	n := flat.normalizeNode(map[string]any{"id": "1"})
	if tags, ok := n["tags"].([]any); !ok || len(tags) != 0 {
		t.Fatalf("flat tags = %v", n["tags"])
	}

	old := NewClient("http://x", "k", "", Capabilities{}, testLogger())
	n = old.normalizeNode(map[string]any{
		"id":         "1",
		"forcedTags": []any{"tag:a", "tag:b"},
		"validTags":  []any{"tag:b", "tag:c"},
	})
	tags, ok := n["tags"].([]string)
	if !ok || len(tags) != 3 || tags[0] != "tag:a" || tags[1] != "tag:b" || tags[2] != "tag:c" {
		t.Fatalf("unioned tags = %#v", n["tags"])
	}
}

func TestNormalizeNodeSortsRouteSets(t *testing.T) {
	c := NewClient("http://x", "k", "", Capabilities{NodeTagsAreFlat: true}, testLogger())
	// Headscale returns these sets in arbitrary, request-varying order.
	n := c.normalizeNode(map[string]any{
		"id":              "1",
		"approvedRoutes":  []any{"192.168.200.0/21", "10.0.0.0/24", "::/0"},
		"availableRoutes": []any{"::/0", "192.168.200.0/21", "0.0.0.0/0", "10.0.0.0/24"},
		"subnetRoutes":    []any{"10.0.10.0/23", "10.0.0.0/24"},
	})
	want := map[string][]string{
		"approvedRoutes":  {"10.0.0.0/24", "192.168.200.0/21", "::/0"},
		"availableRoutes": {"0.0.0.0/0", "10.0.0.0/24", "192.168.200.0/21", "::/0"},
		"subnetRoutes":    {"10.0.0.0/24", "10.0.10.0/23"},
	}
	for k, exp := range want {
		got := n[k].([]any)
		if len(got) != len(exp) {
			t.Fatalf("%s len = %d, want %d", k, len(got), len(exp))
		}
		for i := range exp {
			if got[i] != exp[i] {
				t.Fatalf("%s = %v, want %v", k, got, exp)
			}
		}
	}

	// Two responses that differ only in element order must normalize to
	// byte-identical JSON (this is what stops the live store from firing a
	// spurious "changed" every poll).
	a := c.normalizeNode(map[string]any{"id": "1", "approvedRoutes": []any{"b", "a", "c"}})
	b := c.normalizeNode(map[string]any{"id": "1", "approvedRoutes": []any{"c", "b", "a"}})
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("reordered routes did not normalize equal:\n %s\n %s", ja, jb)
	}
}

func TestRenameNodeEscapesName(t *testing.T) {
	c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.EscapedPath(), "/api/v1/node/1/rename/my%20node") {
			t.Errorf("path = %q", r.URL.EscapedPath())
		}
		w.WriteHeader(http.StatusOK)
	}), Capabilities{})
	if err := c.RenameNode("1", "my node"); err != nil {
		t.Fatal(err)
	}
}

func TestExpirePreAuthKeyStableVsLegacy(t *testing.T) {
	var gotBody map[string]any
	newSrv := func(caps Capabilities) *Client {
		c, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotBody = map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusOK)
		}), caps)
		return c
	}
	if err := newSrv(Capabilities{PreAuthKeysHaveStableIds: true}).ExpirePreAuthKey("42", "1", "keystr"); err != nil {
		t.Fatal(err)
	}
	if gotBody["id"] != "42" || len(gotBody) != 1 {
		t.Fatalf("stable body = %v", gotBody)
	}
	if err := newSrv(Capabilities{}).ExpirePreAuthKey("42", "1", "keystr"); err != nil {
		t.Fatal(err)
	}
	if gotBody["user"] != "1" || gotBody["key"] != "keystr" {
		t.Fatalf("legacy body = %v", gotBody)
	}
}

func TestOIDCSubject(t *testing.T) {
	u := User{"provider": "oidc", "providerId": "https://issuer.example.com/abc%2Fdef"}
	if sub := OIDCSubject(u); sub != "abc/def" {
		t.Fatalf("subject = %q", sub)
	}
	if sub := OIDCSubject(User{"provider": "local", "providerId": "x/y"}); sub != "" {
		t.Fatalf("non-oidc subject = %q", sub)
	}
	if sub := OIDCSubject(User{"provider": "oidc"}); sub != "" {
		t.Fatalf("missing providerId = %q", sub)
	}
}

func TestHealth(t *testing.T) {
	ok, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if h := r.Header.Get("Authorization"); h != "" {
			t.Errorf("health must be unauthenticated, got %q", h)
		}
		w.WriteHeader(http.StatusOK)
	}), Capabilities{})
	if !ok.Health() {
		t.Fatal("expected healthy")
	}

	bad, _ := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}), Capabilities{})
	if bad.Health() {
		t.Fatal("expected unhealthy")
	}
}

func TestDetectVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"version":"0.28.1"}`)
	}))
	defer srv.Close()
	v, err := DetectVersion(srv.URL, "", testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if v.Minor != 28 || v.Patch != 1 {
		t.Fatalf("v = %+v", v)
	}
}
