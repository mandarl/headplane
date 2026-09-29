package main

// Phase 5 server tests: DNS actions, restriction actions, RDP gateway,
// /api/info, and service-description overrides.

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tale/headplane/internal/auth"
	"github.com/tale/headplane/internal/hscfg"
	"github.com/tale/headplane/internal/rdpgw"
)

const phase5TestConfig = `server_url: http://headscale:8080
listen_addr: 0.0.0.0:8080
noise:
  private_key_path: /tmp/noise.key
prefixes:
  v4: 100.64.0.0/10
  v6: fd7a:115c:a1e0::/48
derp:
  server:
    enabled: false
dns:
  magic_dns: true
  base_domain: example.com
  override_local_dns: true
  nameservers:
    global:
      - 1.1.1.1
  search_domains:
    - example.com
database:
  type: sqlite
`

func phase5Logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// writableTestServer returns a test server whose Headscale config is loaded
// from a writable temp file, plus the config file path for assertions.
func writableTestServer(t *testing.T, dnsRecordsPath string) (*Server, *httptest.Server, string, string) {
	t.Helper()
	srv, hs, cookie := testV1Server(t, defaultStub())
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(phase5TestConfig), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := hscfg.Load(path, dnsRecordsPath, phase5Logger())
	if !cfg.Writable() {
		t.Fatal("expected writable test config")
	}
	srv.hsCfg = cfg
	return srv, hs, cookie, path
}

// memberCookie creates an OIDC session for a member-role user (no caps).
func memberCookie(t *testing.T, srv *Server) string {
	t.Helper()
	if _, err := srv.authSvc.DB().Exec(
		`INSERT INTO users (id, sub, name, role, created_at) VALUES (?,?,?,?,?)`,
		"01MEMBER", "member-sub", "Member", "member", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	setCookie, err := srv.authSvc.CreateOidcSession("01MEMBER",
		auth.CookieProfile{Name: "Member"}, nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if i := strings.IndexByte(setCookie, ';'); i >= 0 {
		setCookie = setCookie[:i]
	}
	return setCookie
}

func readTestConfig(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestPhase5DNSActionsWritable(t *testing.T) {
	srv, _, cookie, cfgPath := writableTestServer(t, "")

	post := func(body map[string]any) *httptest.ResponseRecorder {
		return v1Post(t, srv, cookie, "/dns/actions", body)
	}

	// rename_tailnet
	rec := post(map[string]any{"action_id": "rename_tailnet", "new_name": "tail.example.net"})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if decodeBody(t, rec)["message"] != "Tailnet renamed successfully" {
		t.Fatalf("rename: body = %s", rec.Body.String())
	}
	if !strings.Contains(readTestConfig(t, cfgPath), "tail.example.net") {
		t.Fatal("rename: config file not updated")
	}

	// toggle_magic off then on
	rec = post(map[string]any{"action_id": "toggle_magic", "new_state": "disabled"})
	if rec.Code != http.StatusOK || decodeBody(t, rec)["message"] != "Magic DNS state updated successfully" {
		t.Fatalf("toggle off: status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(readTestConfig(t, cfgPath), "magic_dns: false") {
		t.Fatal("toggle off: config file not updated")
	}
	rec = post(map[string]any{"action_id": "toggle_magic", "new_state": "enabled"})
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle on: status = %d", rec.Code)
	}

	// add/remove global nameserver
	rec = post(map[string]any{"action_id": "add_ns", "ns": "9.9.9.9", "split_name": "global"})
	if rec.Code != http.StatusOK || decodeBody(t, rec)["message"] != "Nameserver added successfully" {
		t.Fatalf("add_ns: status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(readTestConfig(t, cfgPath), "9.9.9.9") {
		t.Fatal("add_ns: config file not updated")
	}
	rec = post(map[string]any{"action_id": "remove_ns", "ns": "9.9.9.9", "split_name": "global"})
	if rec.Code != http.StatusOK || decodeBody(t, rec)["message"] != "Nameserver removed successfully" {
		t.Fatalf("remove_ns: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// split nameservers use the quoted dotted-key path form.
	rec = post(map[string]any{"action_id": "add_ns", "ns": "10.0.0.53", "split_name": "corp.example.com"})
	if rec.Code != http.StatusOK {
		t.Fatalf("add split ns: status = %d (%s)", rec.Code, rec.Body.String())
	}
	contents := readTestConfig(t, cfgPath)
	if !strings.Contains(contents, "corp.example.com") || !strings.Contains(contents, "10.0.0.53") {
		t.Fatalf("add split ns: config file missing split entry:\n%s", contents)
	}
	rec = post(map[string]any{"action_id": "remove_ns", "ns": "10.0.0.53", "split_name": "corp.example.com"})
	if rec.Code != http.StatusOK {
		t.Fatalf("remove split ns: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(readTestConfig(t, cfgPath), "corp.example.com") {
		t.Fatal("remove split ns: empty split key not deleted")
	}

	// domains
	rec = post(map[string]any{"action_id": "add_domain", "domain": "extra.example.com"})
	if rec.Code != http.StatusOK || decodeBody(t, rec)["message"] != "Domain added successfully" {
		t.Fatalf("add_domain: status = %d body = %s", rec.Code, rec.Body.String())
	}
	rec = post(map[string]any{"action_id": "remove_domain", "domain": "extra.example.com"})
	if rec.Code != http.StatusOK || decodeBody(t, rec)["message"] != "Domain removed successfully" {
		t.Fatalf("remove_domain: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// override_dns
	rec = post(map[string]any{"action_id": "override_dns", "override_dns": "false"})
	if rec.Code != http.StatusOK || decodeBody(t, rec)["message"] != "DNS override updated successfully" {
		t.Fatalf("override_dns: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// add_record / remove_record in config mode (no separate DNS file):
	// patches dns.extra_records and reports the restart message.
	rec = post(map[string]any{"action_id": "add_record", "record_name": "app", "record_type": "A", "record_value": "100.64.0.5"})
	if rec.Code != http.StatusOK || decodeBody(t, rec)["message"] != "DNS record added successfully" {
		t.Fatalf("add_record: status = %d body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(readTestConfig(t, cfgPath), "extra_records") {
		t.Fatal("add_record: config file missing extra_records")
	}
	rec = post(map[string]any{"action_id": "remove_record", "record_name": "app", "record_type": "A"})
	if rec.Code != http.StatusOK || decodeBody(t, rec)["message"] != "DNS record removed successfully" {
		t.Fatalf("remove_record: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// validation: unknown action and missing action_id are 400.
	rec = post(map[string]any{"action_id": "bogus"})
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["success"] != false {
		t.Fatalf("unknown action: status = %d body = %s", rec.Code, rec.Body.String())
	}
	rec = post(map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing action: status = %d", rec.Code)
	}
	rec = post(map[string]any{"action_id": "rename_tailnet"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rename without name: status = %d", rec.Code)
	}
}

func TestPhase5DNSRecordFileMode(t *testing.T) {
	// With a separate DNS records JSON file, add/remove return an empty
	// (null) JSON body and never touch the main config.
	recordsPath := filepath.Join(t.TempDir(), "records.json")
	if err := os.WriteFile(recordsPath, []byte("[]"), 0644); err != nil {
		t.Fatal(err)
	}
	srv, _, cookie, cfgPath := writableTestServer(t, recordsPath)

	rec := v1Post(t, srv, cookie, "/dns/actions",
		map[string]any{"action_id": "add_record", "record_name": "app", "record_type": "A", "record_value": "100.64.0.5"})
	if rec.Code != http.StatusOK {
		t.Fatalf("add_record: status = %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("add_record: expected null body, got %q", rec.Body.String())
	}
	data, _ := os.ReadFile(recordsPath)
	if !strings.Contains(string(data), "100.64.0.5") {
		t.Fatalf("add_record: records file not updated: %s", data)
	}
	if strings.Contains(readTestConfig(t, cfgPath), "extra_records") {
		t.Fatal("add_record: main config must not change in file mode")
	}

	rec = v1Post(t, srv, cookie, "/dns/actions",
		map[string]any{"action_id": "remove_record", "record_name": "app", "record_type": "A"})
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "null" {
		t.Fatalf("remove_record: status = %d body = %q", rec.Code, rec.Body.String())
	}
}

func TestPhase5DNSActionsAuth(t *testing.T) {
	srv, _, _, _ := writableTestServer(t, "")

	// Unauthenticated.
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/dns/actions", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status = %d", rec.Code)
	}

	// Member without write_network.
	mc := memberCookie(t, srv)
	req = httptest.NewRequest(http.MethodPost, "/admin/api/v1/dns/actions",
		strings.NewReader(`{"action_id":"toggle_magic","new_state":"enabled"}`))
	req.Header.Set("Cookie", mc)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member: status = %d", rec.Code)
	}
}

func TestPhase5Restrictions(t *testing.T) {
	srv, _, cookie, cfgPath := writableTestServer(t, "")

	// Member without configure_iam gets the exact JSON string.
	mc := memberCookie(t, srv)
	req := httptest.NewRequest(http.MethodPost, "/admin/api/v1/settings/restrictions/actions",
		strings.NewReader(`{"action_id":"add_domain","domain":"x.example.com"}`))
	req.Header.Set("Cookie", mc)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member: status = %d", rec.Code)
	}
	var msg string
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil || msg != "You do not have permission to modify IAM settings." {
		t.Fatalf("member: body = %q", rec.Body.String())
	}

	post := func(body map[string]any) *httptest.ResponseRecorder {
		return v1Post(t, srv, cookie, "/settings/restrictions/actions", body)
	}
	strBody := func(rec *httptest.ResponseRecorder) string {
		t.Helper()
		var s string
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatalf("expected JSON string body, got %q", rec.Body.String())
		}
		return s
	}

	// No action / unknown action.
	rec = post(map[string]any{})
	if rec.Code != http.StatusBadRequest || strBody(rec) != "No action provided." {
		t.Fatalf("no action: status = %d body = %s", rec.Code, rec.Body.String())
	}
	rec = post(map[string]any{"action_id": "bogus"})
	if rec.Code != http.StatusBadRequest || strBody(rec) != "Invalid action provided." {
		t.Fatalf("unknown action: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Domains: add trims, dedupes, removes; removal of an absent domain 400s.
	rec = post(map[string]any{"action_id": "add_domain", "domain": "  corp.example.com  "})
	if rec.Code != http.StatusOK || strBody(rec) != "Domain added successfully." {
		t.Fatalf("add_domain: status = %d body = %s", rec.Code, rec.Body.String())
	}
	rec = post(map[string]any{"action_id": "add_domain", "domain": "corp.example.com"})
	if rec.Code != http.StatusOK {
		t.Fatalf("add_domain dup: status = %d", rec.Code)
	}
	contents := readTestConfig(t, cfgPath)
	if strings.Count(contents, "corp.example.com") != 1 {
		t.Fatalf("add_domain: not deduped:\n%s", contents)
	}
	rec = post(map[string]any{"action_id": "add_domain"})
	if rec.Code != http.StatusBadRequest || strBody(rec) != "No domain provided." {
		t.Fatalf("add_domain missing: status = %d body = %s", rec.Code, rec.Body.String())
	}
	rec = post(map[string]any{"action_id": "remove_domain", "domain": "nope.example.com"})
	if rec.Code != http.StatusBadRequest || strBody(rec) != `Domain "nope.example.com" not found in allowed domains.` {
		t.Fatalf("remove_domain absent: status = %d body = %s", rec.Code, rec.Body.String())
	}
	rec = post(map[string]any{"action_id": "remove_domain", "domain": "corp.example.com"})
	if rec.Code != http.StatusOK || strBody(rec) != "Domain removed successfully." {
		t.Fatalf("remove_domain: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Groups and users.
	for _, tc := range []struct {
		add, field, value, added, removed, missing string
	}{
		{"add_group", "group", "eng", "Group added successfully.", "Group removed successfully.", `Group "missing" not found in allowed groups.`},
		{"add_user", "user", "alice@example.com", "User added successfully.", "User removed successfully.", `User "missing" not found in allowed users.`},
	} {
		remove := "remove_" + strings.TrimPrefix(tc.add, "add_")
		rec = post(map[string]any{"action_id": tc.add, tc.field: tc.value})
		if rec.Code != http.StatusOK || strBody(rec) != tc.added {
			t.Fatalf("%s: status = %d body = %s", tc.add, rec.Code, rec.Body.String())
		}
		rec = post(map[string]any{"action_id": remove, tc.field: "missing"})
		if rec.Code != http.StatusBadRequest || strBody(rec) != tc.missing {
			t.Fatalf("%s absent: status = %d body = %s", remove, rec.Code, rec.Body.String())
		}
		rec = post(map[string]any{"action_id": remove, tc.field: tc.value})
		if rec.Code != http.StatusOK || strBody(rec) != tc.removed {
			t.Fatalf("%s: status = %d body = %s", remove, rec.Code, rec.Body.String())
		}
	}
}

func TestPhase5RDPGateway(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())

	// GET is 405 with Allow: POST.
	req := httptest.NewRequest(http.MethodGet, "/admin/api/rdp-gateway", nil)
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET: status = %d allow = %q", rec.Code, rec.Header().Get("Allow"))
	}

	postForm := func(cookie string, fields map[string]string, headers map[string]string) *httptest.ResponseRecorder {
		var b strings.Builder
		w := multipart.NewWriter(&b)
		for k, v := range fields {
			_ = w.WriteField(k, v)
		}
		_ = w.Close()
		r := httptest.NewRequest(http.MethodPost, "/admin/api/rdp-gateway", strings.NewReader(b.String()))
		r.Header.Set("Content-Type", w.FormDataContentType())
		if cookie != "" {
			r.Header.Set("Cookie", cookie)
		}
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, r)
		return rec
	}
	post := func(cookie string, body map[string]any) *httptest.ResponseRecorder {
		fields := map[string]string{}
		for k, v := range body {
			fields[k] = fmt.Sprint(v)
		}
		return postForm(cookie, fields, nil)
	}

	// Unauthenticated.
	if rec := post("", map[string]any{"action_id": "status"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status = %d", rec.Code)
	}

	// Member without write_machines.
	mc := memberCookie(t, srv)
	rec = post(mc, map[string]any{"action_id": "status", "target_ip": "100.64.0.2", "hostname": "vdi"})
	if rec.Code != http.StatusForbidden || decodeBody(t, rec)["error"] != "Insufficient permissions" {
		t.Fatalf("member: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Not configured.
	rec = post(cookie, map[string]any{"action_id": "status", "target_ip": "100.64.0.2", "hostname": "vdi"})
	if rec.Code != http.StatusNotFound || decodeBody(t, rec)["error"] != "RDP gateway is not configured on this server" {
		t.Fatalf("unconfigured: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Point the gateway at a fake webhook.
	var gotBody map[string]any
	var gotAuth string
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		switch gotBody["action"] {
		case "enable":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"host": "relay.example.com", "port": 33001, "expires_at": "2026-09-08T15:00:00Z",
			})
		case "status":
			_ = json.NewEncoder(w).Encode(map[string]any{"active": false})
		case "disable":
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(webhook.Close)
	srv.rdpGw = rdpgw.NewClient(webhook.URL, "tok123", phase5Logger())

	// Missing fields.
	rec = post(cookie, map[string]any{"action_id": "enable", "hostname": "vdi"})
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "Missing required fields" {
		t.Fatalf("missing fields: status = %d body = %s", rec.Code, rec.Body.String())
	}
	// Unknown action.
	rec = post(cookie, map[string]any{"action_id": "frobnicate", "target_ip": "100.64.0.2", "hostname": "vdi"})
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "Unknown action" {
		t.Fatalf("unknown action: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Enable: caller IP from X-Forwarded-For, token forwarded, timeout omitted.
	rec = postForm(cookie,
		map[string]string{"action_id": "enable", "target_ip": "100.64.0.2", "hostname": "vdi"},
		map[string]string{"X-Forwarded-For": "203.0.113.7, 70.0.0.1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: status = %d (%s)", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["host"] != "relay.example.com" || body["port"] != float64(33001) {
		t.Fatalf("enable: body = %v", body)
	}
	if gotAuth != "Bearer tok123" {
		t.Fatalf("enable: webhook auth = %q", gotAuth)
	}
	if gotBody["caller_ip"] != "203.0.113.7" {
		t.Fatalf("enable: caller_ip = %v", gotBody["caller_ip"])
	}
	if _, ok := gotBody["timeout_mins"]; ok {
		t.Fatalf("enable: timeout_mins should be omitted, got %v", gotBody["timeout_mins"])
	}

	// Timeout clamping: low -> 30, high -> 480, garbage -> 180.
	for _, tc := range []struct {
		in   string
		want float64
	}{
		{"10", 30},
		{"9999", 480},
		{"abc", 180},
		{"120", 120},
	} {
		rec = post(cookie, map[string]any{"action_id": "enable", "target_ip": "100.64.0.2", "hostname": "vdi", "timeout_mins": tc.in})
		if rec.Code != http.StatusOK {
			t.Fatalf("enable timeout %q: status = %d", tc.in, rec.Code)
		}
		if gotBody["timeout_mins"] != tc.want {
			t.Fatalf("enable timeout %q: webhook got %v, want %v", tc.in, gotBody["timeout_mins"], tc.want)
		}
	}

	// Status and disable.
	rec = post(cookie, map[string]any{"action_id": "status", "target_ip": "100.64.0.2", "hostname": "vdi"})
	if rec.Code != http.StatusOK || decodeBody(t, rec)["active"] != false {
		t.Fatalf("status: status = %d body = %s", rec.Code, rec.Body.String())
	}
	rec = post(cookie, map[string]any{"action_id": "disable", "target_ip": "100.64.0.2", "hostname": "vdi"})
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: status = %d (%s)", rec.Code, rec.Body.String())
	}

	// Webhook failure -> generic 502.
	srv.rdpGw = rdpgw.NewClient(webhook.URL+"/nope", "", phase5Logger())
	rec = post(cookie, map[string]any{"action_id": "status", "target_ip": "100.64.0.2", "hostname": "vdi"})
	if rec.Code != http.StatusBadGateway || decodeBody(t, rec)["error"] != "Gateway request failed. Check server logs." {
		t.Fatalf("webhook failure: status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestPhase5APIInfo(t *testing.T) {
	srv, _, _ := testV1Server(t, defaultStub())

	get := func(auth string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/admin/api/info", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}

	// Unset secret: 403 Forbidden regardless of auth.
	if rec := get("Bearer x"); rec.Code != http.StatusForbidden || decodeBody(t, rec)["status"] != "Forbidden" {
		t.Fatalf("unset secret: status = %d body = %s", rec.Code, rec.Body.String())
	}

	srv.cfg.Server.InfoSecret = "s3cret"
	// Missing auth: 401.
	if rec := get(""); rec.Code != http.StatusUnauthorized || decodeBody(t, rec)["status"] != "Unauthorized" {
		t.Fatalf("missing auth: status = %d body = %s", rec.Code, rec.Body.String())
	}
	// Non-bearer: 401.
	if rec := get("Token s3cret"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("non-bearer: status = %d", rec.Code)
	}
	// Wrong token: 403.
	if rec := get("Bearer wrong"); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong token: status = %d", rec.Code)
	}

	// Healthy: seed the detected raw version like detectHeadscaleVersion would.
	srv.hsHealth = func() bool { return true }
	srv.hsVersionMu.Lock()
	srv.hsVersion = "0.28.1"
	srv.hsVersionMu.Unlock()
	rec := get("Bearer s3cret")
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy: status = %d", rec.Code)
	}
	body := decodeBody(t, rec)
	if body["status"] != "healthy" || body["headscale_canonical_version"] != "0.28.1" {
		t.Fatalf("healthy: body = %v", body)
	}
	if body["headplane_version"] == "" {
		t.Fatalf("healthy: missing headplane_version: %v", body)
	}
	iv, ok := body["internal_versions"].(map[string]any)
	if !ok || iv["go"] == nil || iv["os"] == nil || iv["arch"] == nil {
		t.Fatalf("healthy: internal_versions = %v", body["internal_versions"])
	}

	// Unhealthy.
	srv.hsHealth = func() bool { return false }
	rec = get("Bearer s3cret")
	if body := decodeBody(t, rec); body["status"] != "unhealthy" || body["headscale_canonical_version"] != "unknown" {
		t.Fatalf("unhealthy: body = %v", body)
	}

	// Wrong method: 405 with Allow.
	r := httptest.NewRequest(http.MethodPost, "/admin/api/info", nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("POST: status = %d allow = %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestPhase5ServiceDescription(t *testing.T) {
	stub := defaultStub()
	stub.nodes[0]["nodeKey"] = "nodekey:test123"
	srv, _, cookie := testV1Server(t, stub)
	nodeID := stub.nodes[0]["id"].(string)

	post := func(body map[string]any) *httptest.ResponseRecorder {
		body["action_id"] = "update_service_description"
		body["node_id"] = nodeID
		return v1Post(t, srv, cookie, "/machines/actions", body)
	}

	// Missing proto/port.
	rec := post(map[string]any{"description": "x"})
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "Missing `proto` or `port` in the form data." {
		t.Fatalf("missing proto/port: status = %d body = %s", rec.Code, rec.Body.String())
	}
	// Invalid port.
	rec = post(map[string]any{"proto": "tcp", "port": "abc", "description": "x"})
	if rec.Code != http.StatusBadRequest || decodeBody(t, rec)["error"] != "Invalid `port` in the form data." {
		t.Fatalf("invalid port: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Update.
	rec = post(map[string]any{"proto": "tcp", "port": 80, "description": "Web server"})
	if rec.Code != http.StatusOK || decodeBody(t, rec)["message"] != "Description updated" {
		t.Fatalf("update: status = %d body = %s", rec.Code, rec.Body.String())
	}

	// Detail includes the override keyed by proto:port, with the API-key
	// principal's display name and an ISO updatedAt.
	drec := v1Get(t, srv, cookie, "/machines/"+nodeID)
	if drec.Code != http.StatusOK {
		t.Fatalf("detail: status = %d", drec.Code)
	}
	body := decodeBody(t, drec)
	overrides, ok := body["serviceOverrides"].(map[string]any)
	if !ok {
		t.Fatalf("detail: missing serviceOverrides: %v", keysOf(body))
	}
	ov, ok := overrides["tcp:80"].(map[string]any)
	if !ok || ov["description"] != "Web server" || ov["updatedBy"] != "valid" {
		t.Fatalf("detail: overrides = %v", overrides)
	}
	if _, ok := ov["updatedAt"].(string); !ok {
		t.Fatalf("detail: updatedAt missing: %v", ov)
	}

	// Empty description resets.
	rec = post(map[string]any{"proto": "tcp", "port": 80, "description": "   "})
	if rec.Code != http.StatusOK || decodeBody(t, rec)["message"] != "Description reset" {
		t.Fatalf("reset: status = %d body = %s", rec.Code, rec.Body.String())
	}
	drec = v1Get(t, srv, cookie, "/machines/"+nodeID)
	overrides, _ = decodeBody(t, drec)["serviceOverrides"].(map[string]any)
	if len(overrides) != 0 {
		t.Fatalf("reset: overrides not cleared: %v", overrides)
	}
}

func TestPhase5MachineDetailPhase5Fields(t *testing.T) {
	srv, _, cookie := testV1Server(t, defaultStub())
	stub := defaultStub()
	nodeID := stub.nodes[0]["id"].(string)

	// Overview: populated nodes carry hostInfo (null), top-level agent and
	// rdpGatewayEnabled reflect the disabled agent / unconfigured gateway.
	rec := v1Get(t, srv, cookie, "/machines")
	if rec.Code != http.StatusOK {
		t.Fatalf("overview: status = %d", rec.Code)
	}
	body := decodeBody(t, rec)
	nodes, ok := body["populatedNodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("overview: populatedNodes = %v", body["populatedNodes"])
	}
	node := nodes[0].(map[string]any)
	if hi, ok := node["hostInfo"]; !ok || hi != nil {
		t.Fatalf("overview: hostInfo = %v (present=%v)", node["hostInfo"], ok)
	}
	if body["agent"] != nil {
		t.Fatalf("overview: agent = %v, want null", body["agent"])
	}
	if body["rdpGatewayEnabled"] != false {
		t.Fatalf("overview: rdpGatewayEnabled = %v", body["rdpGatewayEnabled"])
	}

	// Detail: top-level agent/rdpGatewayEnabled/stats/serviceOverrides, and
	// node.hostInfo.
	drec := v1Get(t, srv, cookie, "/machines/"+nodeID)
	if drec.Code != http.StatusOK {
		t.Fatalf("detail: status = %d", drec.Code)
	}
	detail := decodeBody(t, drec)
	for _, key := range []string{"agent", "rdpGatewayEnabled", "stats", "serviceOverrides"} {
		if _, ok := detail[key]; !ok {
			t.Fatalf("detail: missing %q: %v", key, keysOf(detail))
		}
	}
	if detail["stats"] != nil {
		t.Fatalf("detail: stats should be null, got %v", detail["stats"])
	}
	dnode := detail["node"].(map[string]any)
	if hi, ok := dnode["hostInfo"]; !ok || hi != nil {
		t.Fatalf("detail: node.hostInfo = %v (present=%v)", dnode["hostInfo"], ok)
	}
}
