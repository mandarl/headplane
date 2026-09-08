package main

// Phase 4 integration tests: the v1 API against a stub Headscale server,
// covering the wire contract (shapes, status codes, error mapping) and the
// SSE stream.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tale/headplane/internal/hsapi"
	"github.com/tale/headplane/internal/hscfg"
	"github.com/tale/headplane/internal/live"
)

// stubHeadscaleV1 serves the Headscale endpoints Phase 4 needs. validKey is
// the only accepted Bearer <redacted>; everything else 401s on /api/v1/*.
type stubHeadscaleV1 struct {
	t        *testing.T
	validKey string
	nodes    []map[string]any
	users    []map[string]any
	policy   string
	preKeys  []map[string]any
	nodeErr  int // if nonzero, /api/v1/node answers with this status
}

func (s *stubHeadscaleV1) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.URL.Path == "/health":
		w.WriteHeader(http.StatusOK)
		return
	case r.URL.Path == "/version":
		write(map[string]any{"version": "0.28.1"})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		http.NotFound(w, r)
		return
	}
	if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != s.validKey {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/api/v1/apikey":
		write(map[string]any{"apiKeys": []any{map[string]any{"prefix": "valid"}}})
	case "/api/v1/node":
		if s.nodeErr != 0 {
			w.WriteHeader(s.nodeErr)
			return
		}
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		write(map[string]any{"nodes": s.nodes})
	case "/api/v1/user":
		if r.Method == http.MethodGet {
			write(map[string]any{"users": s.users})
		} else if r.Method == http.MethodPost {
			write(map[string]any{"user": map[string]any{"id": "9", "name": "new"}})
		}
	case "/api/v1/policy":
		if r.Method == http.MethodPut {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["policy"] == "bad policy {" {
				w.WriteHeader(http.StatusBadRequest)
				write(map[string]any{"message": "policy is not valid"})
				return
			}
			s.policy, _ = body["policy"].(string)
			write(map[string]any{"policy": s.policy, "updatedAt": "2026-09-08T00:00:00Z"})
			return
		}
		write(map[string]any{"policy": s.policy, "updatedAt": "2026-09-08T00:00:00Z"})
	case "/api/v1/preauthkey":
		if r.Method == http.MethodPost {
			write(map[string]any{"preAuthKey": map[string]any{"id": "7", "key": "nodekey-sekret"}})
			return
		}
		write(map[string]any{"preAuthKeys": s.preKeys})
	case "/api/v1/preauthkey/expire":
	default:
		switch {
		case r.URL.Path == "/api/v1/node/register" && r.Method == http.MethodPost:
			write(map[string]any{"node": map[string]any{"id": "42", "tags": []any{}}})
		case strings.HasPrefix(r.URL.Path, "/api/v1/node/") && r.Method == http.MethodGet:
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/node/")
			for _, n := range s.nodes {
				if n["id"] == id {
					write(map[string]any{"node": n})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			write(map[string]any{"message": "no such node"})
		case strings.HasSuffix(r.URL.Path, "/expire") && r.Method == http.MethodPost:
		case strings.Contains(r.URL.Path, "/tags") && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			for _, t := range body["tags"].([]any) {
				if t == "tag:undefined" {
					w.WriteHeader(http.StatusBadRequest)
					write(map[string]any{"message": "tag not defined"})
					return
				}
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/approve_routes"):
		case strings.Contains(r.URL.Path, "/rename/"):
		case r.Method == http.MethodDelete:
		default:
			http.NotFound(w, r)
		}
	}
}

func testV1Server(t *testing.T, stub *stubHeadscaleV1) (*Server, *httptest.Server, string) {
	t.Helper()
	hs := httptest.NewServer(stub)
	t.Cleanup(hs.Close)

	srv := testAuthServer(t, hs.URL)
	srv.hsCfg = &hscfg.Config{} // no config file: access "no"
	caps := hsapi.CapabilitiesFor(hsapi.ParseServerVersion("0.28.1"))
	srv.setHsCaps(caps)
	client := hsapi.NewClient(hs.URL, stub.validKey, "", caps, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.liveStore = live.NewStore(slog.New(slog.NewTextHandler(io.Discard, nil)), client)
	srv.healthCheck = hsapi.NewClient(hs.URL, "", "", caps, slog.New(slog.NewTextHandler(io.Discard, nil))).Health
	t.Cleanup(srv.liveStore.Dispose)

	setCookie, err := srv.authSvc.CreateAPIKeySession(stub.validKey, "valid", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cookie := setCookie
	if i := strings.IndexByte(cookie, ';'); i >= 0 {
		cookie = cookie[:i]
	}
	return srv, hs, cookie
}

func v1Get(t *testing.T, srv *Server, cookie, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/v1"+path, nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func v1Post(t *testing.T, srv *Server, cookie, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1"+path, rdr)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func v1Patch(t *testing.T, srv *Server, cookie, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPatch, "/admin/api/v1"+path, strings.NewReader(string(raw)))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not JSON: %q", rec.Body.String())
	}
	return out
}

func defaultStub() *stubHeadscaleV1 {
	return &stubHeadscaleV1{
		validKey: "valid-key",
		nodes: []map[string]any{{
			"id":   "1",
			"name": "node1",
			"user": map[string]any{"id": "1", "name": "alice"},
			"routes": []any{
				map[string]any{"prefix": "10.0.0.0/24", "enabled": true},
				map[string]any{"prefix": "10.0.0.0/24", "enabled": true},
			},
			"approvedRoutes": []any{"10.0.0.0/24"},
			"expiry":         "2099-01-01T00:00:00Z",
			"tags":           []any{"tag:server"},
		}},
		users: []map[string]any{{
			"id": "1", "name": "alice", "email": "alice@example.com",
			"provider": "local", "profilePicUrl": "",
		}},
		policy:  "accept-all",
		preKeys: []map[string]any{{"id": "7", "key": "nodekey-xxx", "reusable": true}},
	}
}

func TestV1Boot(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())
	rec := v1Get(t, srv, cookie, "/boot")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	user := body["user"].(map[string]any)
	if user["kind"] != "api_key" || user["name"] != "valid" {
		t.Fatalf("user = %v", user)
	}
	if body["isHealthy"] != true {
		t.Fatalf("isHealthy = %v", body["isHealthy"])
	}
	if body["configAvailable"] != false || body["baseUrl"] == "" {
		t.Fatalf("body = %v", body)
	}
	access := body["access"].(map[string]any)
	// API-key principals bypass permission checks: everything is true.
	for _, k := range []string{"dns", "machines", "policy", "settings", "ui", "users"} {
		if access[k] != true {
			t.Fatalf("access[%s] = %v, want true", k, access[k])
		}
	}
}

func TestV1BootUnauthorized(t *testing.T) {
	srv, _, _ := testV1Server(t, defaultStub())
	rec := v1Get(t, srv, "", "/boot")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestV1BootRevokedKeyDestroysSession(t *testing.T) {
	stub := defaultStub()
	stub.validKey = "other-key" // the session's key is no longer valid
	srv, _, cookie := testV1Server(t, stub)
	// The server's own live-store client uses stub.validKey too; fix the
	// session to carry a now-revoked key.
	setCookie, err := srv.authSvc.CreateAPIKeySession("revoked-key", "revoked", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_ = cookie
	cookie = setCookie
	if i := strings.IndexByte(cookie, ';'); i >= 0 {
		cookie = cookie[:i]
	}
	rec := v1Get(t, srv, cookie, "/boot")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d (%s), want 401", rec.Code, rec.Body.String())
	}
	if sc := rec.Header().Get("Set-Cookie"); !strings.Contains(sc, "_hp_auth=;") && !strings.Contains(sc, "Max-Age=0") {
		t.Fatalf("session was not destroyed: %q", sc)
	}
}

func TestV1Machines(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())
	rec := v1Get(t, srv, cookie, "/machines")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	pop := body["populatedNodes"].([]any)
	if len(pop) != 1 {
		t.Fatalf("populatedNodes = %v", body["populatedNodes"])
	}
	n := pop[0].(map[string]any)
	if len(n["availableRoutes"].([]any)) != 1 { // deduped
		t.Fatalf("availableRoutes = %v", n["availableRoutes"])
	}
	if n["customRouting"] != true || n["expired"] != false {
		t.Fatalf("node = %v", n)
	}
	if body["supportsNodeOwnerChange"] != false { // 0.28.1: immutable
		t.Fatalf("supportsNodeOwnerChange = %v", body["supportsNodeOwnerChange"])
	}
	if body["writable"] != true || body["server"] == "" {
		t.Fatalf("body = %v", body)
	}
}

func TestV1MachineDetail(t *testing.T) {
	stub := defaultStub()
	// Second node with different tags: existingTags must be the sorted
	// union across all nodes, tags only this node's.
	stub.nodes = append(stub.nodes, map[string]any{
		"id":   "2",
		"name": "node2",
		"user": map[string]any{"id": "1", "name": "alice"},
		"tags": []any{"tag:zeta", "tag:alpha"},
	})
	srv, _, cookie := testV1Server(t, stub)
	rec := v1Get(t, srv, cookie, "/machines/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["node"].(map[string]any)["name"] != "node1" {
		t.Fatalf("node = %v", body["node"])
	}
	// Exact key parity with the old loader / SPA MachineDetailData.
	wantKeys := []string{"agent", "existingTags", "magic", "node", "rdpGatewayEnabled",
		"serviceOverrides", "stats", "supportsNodeOwnerChange", "tags", "users"}
	if len(body) != len(wantKeys) {
		t.Fatalf("keys = %v, want %v", keysOf(body), wantKeys)
	}
	for _, k := range wantKeys {
		if _, ok := body[k]; !ok {
			t.Fatalf("missing key %q in %v", k, keysOf(body))
		}
	}
	if got := strSlice(body["tags"]); !equalStrs(got, []string{"tag:server"}) {
		t.Fatalf("tags = %v, want [tag:server]", got)
	}
	if got := strSlice(body["existingTags"]); !equalStrs(got, []string{"tag:alpha", "tag:server", "tag:zeta"}) {
		t.Fatalf("existingTags = %v", got)
	}
	if body["stats"] != nil || body["agent"] != nil {
		t.Fatalf("stats/agent should be null in phase 4, got %v / %v", body["stats"], body["agent"])
	}
	if body["rdpGatewayEnabled"] != false {
		t.Fatalf("rdpGatewayEnabled = %v, want false", body["rdpGatewayEnabled"])
	}

	rec = v1Get(t, srv, cookie, "/machines/999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func keysOf(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sortStrings(ks)
	return ks
}

func strSlice(v any) []string {
	var out []string
	for _, e := range v.([]any) {
		out = append(out, e.(string))
	}
	return out
}

func equalStrs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func TestV1MachineActionsValidation(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())

	rec := v1Post(t, srv, cookie, "/machines/actions", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing action_id: status = %d, want 400", rec.Code)
	}

	rec = v1Post(t, srv, cookie, "/machines/actions", map[string]any{
		"action_id": "update_tags", "node_id": "1",
	})
	body := decodeBody(t, rec)
	if rec.Code != http.StatusBadRequest || body["error"] == nil {
		t.Fatalf("missing tags: status = %d body = %v", rec.Code, body)
	}

	// Upstream 400 on setTags -> {success:false, error} with 400.
	rec = v1Post(t, srv, cookie, "/machines/actions", map[string]any{
		"action_id": "update_tags", "node_id": "1", "tags": "tag:undefined",
	})
	body = decodeBody(t, rec)
	if rec.Code != http.StatusBadRequest || body["success"] != false || body["error"] == nil {
		t.Fatalf("bad tag: status = %d body = %v", rec.Code, body)
	}

	// Happy path rename.
	rec = v1Post(t, srv, cookie, "/machines/actions", map[string]any{
		"action_id": "rename", "node_id": "1", "name": "node2",
	})
	body = decodeBody(t, rec)
	if rec.Code != http.StatusOK || body["message"] != "Machine renamed" {
		t.Fatalf("rename: status = %d body = %v", rec.Code, body)
	}

	// Register fast-tracks without node_id.
	rec = v1Post(t, srv, cookie, "/machines/actions", map[string]any{
		"action_id": "register", "register_key": "k", "user": "alice",
	})
	body = decodeBody(t, rec)
	if rec.Code != http.StatusOK || body["redirect"] != "/machines/42" {
		t.Fatalf("register: status = %d body = %v", rec.Code, body)
	}

	// Reassign is refused on 0.28+.
	rec = v1Post(t, srv, cookie, "/machines/actions", map[string]any{
		"action_id": "reassign", "node_id": "1", "user_id": "2",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reassign: status = %d, want 400", rec.Code)
	}
}

func TestV1Acls(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())
	rec := v1Get(t, srv, cookie, "/acls")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["policy"] != "accept-all" || body["writable"] != true {
		t.Fatalf("body = %v", body)
	}

	// PATCH without a policy string -> 400 {success:false}.
	rec = v1Patch(t, srv, cookie, "/acls", map[string]any{})
	body = decodeBody(t, rec)
	if rec.Code != http.StatusBadRequest || body["success"] != false {
		t.Fatalf("patch missing policy: status = %d body = %v", rec.Code, body)
	}

	// PATCH with an invalid policy -> upstream 400 -> {success:false, error}.
	rec = v1Patch(t, srv, cookie, "/acls", map[string]any{"policy": "bad policy {"})
	body = decodeBody(t, rec)
	if rec.Code != http.StatusBadRequest || body["success"] != false || body["error"] != "policy is not valid" {
		t.Fatalf("patch bad policy: status = %d body = %v", rec.Code, body)
	}

	// PATCH happy path.
	rec = v1Patch(t, srv, cookie, "/acls", map[string]any{"policy": "{\"accept\":[]}"})
	body = decodeBody(t, rec)
	if rec.Code != http.StatusOK || body["success"] != true || body["policy"] != "{\"accept\":[]}" {
		t.Fatalf("patch ok: status = %d body = %v", rec.Code, body)
	}
	if body["updatedAt"] != "2026-09-08T00:00:00Z" {
		t.Fatalf("updatedAt = %v", body["updatedAt"])
	}
}

func TestV1Users(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())
	rec := v1Get(t, srv, cookie, "/users")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	unlinked := body["unlinkedHeadscaleUsers"].([]any)
	if len(unlinked) != 1 {
		t.Fatalf("unlinked = %v", body["unlinkedHeadscaleUsers"])
	}
	if unlinked[0].(map[string]any)["name"] != "alice" {
		t.Fatalf("unlinked[0] = %v", unlinked[0])
	}
}

func TestV1UsersDegraded(t *testing.T) {
	stub := defaultStub()
	stub.nodeErr = http.StatusInternalServerError
	srv, _, cookie := testV1Server(t, stub)
	rec := v1Get(t, srv, cookie, "/users")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 degraded", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["apiError"] == nil {
		t.Fatalf("apiError missing: %v", body)
	}
}

func TestV1AuthKeys(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())
	rec := v1Get(t, srv, cookie, "/settings/auth-keys")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	// keys: [{user, preAuthKeys}] with the tag-only group first.
	groups := body["keys"].([]any)
	if len(groups) != 1 {
		t.Fatalf("keys = %v", body["keys"])
	}
	g := groups[0].(map[string]any)
	if g["user"] != nil {
		t.Fatalf("group user = %v, want null (tag-only)", g["user"])
	}
	if len(g["preAuthKeys"].([]any)) != 1 {
		t.Fatalf("preAuthKeys = %v", g["preAuthKeys"])
	}
	if len(body["missing"].([]any)) != 0 {
		t.Fatalf("missing = %v", body["missing"])
	}
	if body["access"] != true || body["selfServiceOnly"] != false {
		t.Fatalf("access = %v selfServiceOnly = %v", body["access"], body["selfServiceOnly"])
	}
	if body["currentSubject"] != nil {
		t.Fatalf("currentSubject = %v, want null for api-key sessions", body["currentSubject"])
	}
	if _, ok := body["url"]; !ok {
		t.Fatal("url missing")
	}
	if len(body["users"].([]any)) != 1 {
		t.Fatalf("users = %v", body["users"])
	}

	// add_preauthkey validation order: user-or-tags first.
	rec = v1Post(t, srv, cookie, "/settings/auth-keys/actions", map[string]any{"action_id": "add_preauthkey"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	// Happy path returns the key string.
	rec = v1Post(t, srv, cookie, "/settings/auth-keys/actions", map[string]any{
		"action_id": "add_preauthkey", "user": "alice",
		"expiry": "30 days", "reusable": "on", "ephemeral": "off",
	})
	body = decodeBody(t, rec)
	if rec.Code != http.StatusOK || body["success"] != true || body["key"] != "nodekey-sekret" {
		t.Fatalf("add: status = %d body = %v", rec.Code, body)
	}
	// expire_preauthkey.
	rec = v1Post(t, srv, cookie, "/settings/auth-keys/actions", map[string]any{
		"action_id": "expire_preauthkey", "key_id": "7", "key": "nodekey-xxx", "user_id": "1",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expire: status = %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestV1UpstreamErrorPreserved(t *testing.T) {
	stub := defaultStub()
	stub.nodeErr = http.StatusBadGateway
	srv, _, cookie := testV1Server(t, stub)
	rec := v1Get(t, srv, cookie, "/machines")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want upstream 502 preserved", rec.Code)
	}
}

func TestV1Healthz(t *testing.T) {
	srv, _, _ := testV1Server(t, defaultStub())
	req := httptest.NewRequest(http.MethodGet, "/admin/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// sseReader consumes a live SSE stream from a real HTTP server.
type sseReader struct {
	t      *testing.T
	reader *bufio.Reader
	cancel context.CancelFunc
}

func dialLive(t *testing.T, srv *Server, cookie, lastEventID string) *sseReader {
	t.Helper()
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/admin/events/live", nil)
	req.Header.Set("Cookie", cookie)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	res, err := httpSrv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	t.Cleanup(func() { res.Body.Close() })
	return &sseReader{t: t, reader: bufio.NewReader(res.Body), cancel: cancel}
}

// nextEvent reads one SSE event: returns (id, name, data).
func (s *sseReader) nextEvent() (string, string, string) {
	s.t.Helper()
	var id, name string
	var dataLines []string
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil {
			s.t.Fatalf("reading event: %v", err)
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			return id, name, strings.Join(dataLines, "\n")
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			dataLines = append(dataLines, strings.TrimPrefix(line, "data: "))
		case strings.HasPrefix(line, ":"):
			// heartbeat/comment
		}
	}
}

func TestV1LiveSSE(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())
	stream := dialLive(t, srv, cookie, "")
	defer stream.cancel()

	// hello with the current version map
	id, name, data := stream.nextEvent()
	if name != "hello" || id != "" {
		t.Fatalf("first event = id:%q name:%q data:%q", id, name, data)
	}
	var versions map[string]string
	if err := json.Unmarshal([]byte(data), &versions); err != nil {
		t.Fatal(err)
	}
	if versions["nodes"] == "" || versions["users"] == "" {
		t.Fatalf("versions = %v", versions)
	}

	// Trigger a change: the stub nodes are static, so refresh against a
	// mutated store resource.
	nodes := defaultStub().nodes
	nodes[0]["name"] = "node1-renamed"
	client := hsapi.NewClient("http://x", "k", "", hsapi.Capabilities{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	res := live.Resource{Key: "nodes", PollInterval: time.Hour,
		Fetch: func(ctx context.Context, c *hsapi.Client) (any, error) {
			out := make([]hsapi.Node, 0, len(nodes))
			for _, n := range nodes {
				out = append(out, n)
			}
			return out, nil
		}}
	if err := srv.liveStore.Refresh(context.Background(), res, client); err != nil {
		t.Fatal(err)
	}

	id, name, data = stream.nextEvent()
	if name != "changed" || id == "" {
		t.Fatalf("changed event = id:%q name:%q data:%q", id, name, data)
	}
	var change map[string]string
	if err := json.Unmarshal([]byte(data), &change); err != nil {
		t.Fatal(err)
	}
	if change["resource"] != "nodes" || change["version"] == versions["nodes"] {
		t.Fatalf("change = %v (old %v)", change, versions)
	}
	stream.cancel()

	// Reconnect with Last-Event-ID before the change: the missed events
	// replay (initial user snapshot event, then the node change).
	stream2 := dialLive(t, srv, cookie, versions["nodes"])
	defer stream2.cancel()
	// hello first
	if _, name, _ := stream2.nextEvent(); name != "hello" {
		t.Fatalf("expected hello, got %q", name)
	}
	// Drain replayed events until the recorded node change reappears.
	found := false
	for i := 0; i < 4 && !found; i++ {
		rid, name, data := stream2.nextEvent()
		if name != "changed" {
			t.Fatalf("replay event %d = name:%q", i, name)
		}
		var ch map[string]string
		_ = json.Unmarshal([]byte(data), &ch)
		if ch["resource"] == "nodes" && rid == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("recorded change id %q did not replay", id)
	}
}

func writeHscfg(t *testing.T) string {
	t.Helper()
	p := t.TempDir() + "/config.yaml"
	body := `server_url: http://headscale:8080
listen_addr: 0.0.0.0:8080
noise:
  private_key_path: /tmp/noise.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
derp:
  server:
    enabled: false
database:
  type: sqlite3
dns:
  magic_dns: true
  base_domain: example.ts.net
  override_local_dns: true
  nameservers:
    global: ["1.1.1.1"]
    split:
      "corp.example": ["10.0.0.53"]
  search_domains: ["example.ts.net"]
  extra_records:
    - {name: "test", type: "A", value: "1.2.3.4"}
oidc:
  allowed_domains: ["example.com", "example.com"]
  allowed_groups: ["ops"]
  allowed_users: []
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestV1Restrictions(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())
	// No Headscale config in the test server: OIDC block absent -> 501.
	rec := v1Get(t, srv, cookie, "/settings/restrictions")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}

	srv.hsCfg = hscfg.Load(writeHscfg(t), "", discardLogger())
	rec = v1Get(t, srv, cookie, "/settings/restrictions")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	settings := body["settings"].(map[string]any)
	if domains := settings["domains"].([]any); len(domains) != 1 || domains[0] != "example.com" {
		t.Fatalf("domains = %v (want deduped)", domains)
	}
	if body["access"] != true || body["writable"] != true {
		t.Fatalf("access = %v writable = %v", body["access"], body["writable"])
	}
}

func TestV1Dns(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())
	// Unreadable config -> 500, mirroring the TS loader's throw.
	rec := v1Get(t, srv, cookie, "/dns")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}

	srv.hsCfg = hscfg.Load(writeHscfg(t), "", discardLogger())
	rec = v1Get(t, srv, cookie, "/dns")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["magicDns"] != true || body["baseDomain"] != "example.ts.net" {
		t.Fatalf("body = %v", body)
	}
	if len(body["prefixes"].([]any)) != 2 {
		t.Fatalf("prefixes = %v", body["prefixes"])
	}
	if len(body["extraRecords"].([]any)) != 1 {
		t.Fatalf("extraRecords = %v", body["extraRecords"])
	}
	if body["access"] != true || body["writable"] != true {
		t.Fatalf("access = %v writable = %v", body["access"], body["writable"])
	}
}

func TestV1AgentDisabledAndPhase5Handlers(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())

	// Agent is not configured in the test server: the page reports the
	// static disabled reason.
	rec := v1Get(t, srv, cookie, "/settings/agent")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["enabled"] != false || body["reason"] != "Agent is not enabled in the configuration" {
		t.Fatalf("body = %v", body)
	}

	// Sync with no agent: always 200 with {success:false, error}.
	rec = v1Post(t, srv, cookie, "/settings/agent/sync", map[string]any{})
	body = decodeBody(t, rec)
	if rec.Code != http.StatusOK || body["success"] != false {
		t.Fatalf("agent/sync: status = %d body = %v", rec.Code, body)
	}
	if _, ok := body["error"].(string); !ok {
		t.Fatalf("agent/sync: missing error string: %v", body)
	}

	// DNS actions with a non-writable config: 403 {success:false}.
	rec = v1Post(t, srv, cookie, "/dns/actions", map[string]any{"action_id": "toggle_magic", "new_state": "enabled"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("dns/actions: status = %d body = %v", rec.Code, rec.Body.String())
	}

	// Restrictions with a non-writable config: 403 JSON string.
	rec = v1Post(t, srv, cookie, "/settings/restrictions/actions", map[string]any{"action_id": "add_domain", "domain": "example.com"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("restrictions/actions: status = %d body = %v", rec.Code, rec.Body.String())
	}
	var msg string
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil || msg != "The Headscale configuration file is not editable." {
		t.Fatalf("restrictions/actions: body = %v", rec.Body.String())
	}
}
