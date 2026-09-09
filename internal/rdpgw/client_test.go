package rdpgw

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type capturedRequest struct {
	body   string
	header http.Header
}

// webhookStub records the request and replies with status/body.
func webhookStub(t *testing.T, status int, respBody string, got *capturedRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.body = string(b)
		got.header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respBody))
	}))
}

func TestEnableWithTimeout(t *testing.T) {
	var got capturedRequest
	srv := webhookStub(t, 200, `{"host":"relay.example.com","port":33001,"expires_at":"2026-09-08T12:00:00Z"}`, &got)
	defer srv.Close()

	c := NewClient(srv.URL, "secret-token", testLogger())
	timeout := 180
	res, err := c.Enable(context.Background(), "100.64.0.1", "vdi-name", "1.2.3.4", &timeout)
	if err != nil {
		t.Fatalf("Enable returned error: %v", err)
	}
	if res.Host != "relay.example.com" || res.Port != 33001 || res.ExpiresAt != "2026-09-08T12:00:00Z" {
		t.Fatalf("unexpected result: %+v", res)
	}

	want := `{"action":"enable","target_ip":"100.64.0.1","hostname":"vdi-name","caller_ip":"1.2.3.4","timeout_mins":180}`
	if got.body != want {
		t.Fatalf("unexpected body:\n got: %s\nwant: %s", got.body, want)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("unexpected Content-Type: %q", ct)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer secret-token" {
		t.Fatalf("unexpected Authorization: %q", auth)
	}
}

func TestEnableWithoutTimeout(t *testing.T) {
	var got capturedRequest
	srv := webhookStub(t, 200, `{"host":"h","port":1,"expires_at":"x"}`, &got)
	defer srv.Close()

	c := NewClient(srv.URL, "", testLogger())
	if _, err := c.Enable(context.Background(), "100.64.0.1", "vdi-name", "1.2.3.4", nil); err != nil {
		t.Fatalf("Enable returned error: %v", err)
	}

	want := `{"action":"enable","target_ip":"100.64.0.1","hostname":"vdi-name","caller_ip":"1.2.3.4"}`
	if got.body != want {
		t.Fatalf("unexpected body:\n got: %s\nwant: %s", got.body, want)
	}
	if auth := got.header.Get("Authorization"); auth != "" {
		t.Fatalf("expected no Authorization header, got %q", auth)
	}
}

func TestDisable(t *testing.T) {
	var got capturedRequest
	srv := webhookStub(t, 200, `{"ok":true}`, &got)
	defer srv.Close()

	c := NewClient(srv.URL, "tok", testLogger())
	if err := c.Disable(context.Background(), "100.64.0.1", "vdi-name"); err != nil {
		t.Fatalf("Disable returned error: %v", err)
	}

	want := `{"action":"disable","target_ip":"100.64.0.1","hostname":"vdi-name"}`
	if got.body != want {
		t.Fatalf("unexpected body:\n got: %s\nwant: %s", got.body, want)
	}
	if auth := got.header.Get("Authorization"); auth != "Bearer tok" {
		t.Fatalf("unexpected Authorization: %q", auth)
	}
}

func TestStatusActive(t *testing.T) {
	var got capturedRequest
	srv := webhookStub(t, 200, `{"active":true,"host":"h","port":33001,"expires_at":"x"}`, &got)
	defer srv.Close()

	c := NewClient(srv.URL, "tok", testLogger())
	res, err := c.Status(context.Background(), "100.64.0.1", "vdi-name")
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if !res.Active || res.Host != "h" || res.Port != 33001 || res.ExpiresAt != "x" {
		t.Fatalf("unexpected result: %+v", res)
	}

	want := `{"action":"status","target_ip":"100.64.0.1","hostname":"vdi-name"}`
	if got.body != want {
		t.Fatalf("unexpected body:\n got: %s\nwant: %s", got.body, want)
	}
}

func TestStatusInactive(t *testing.T) {
	var got capturedRequest
	srv := webhookStub(t, 200, `{"active":false}`, &got)
	defer srv.Close()

	c := NewClient(srv.URL, "", testLogger())
	res, err := c.Status(context.Background(), "100.64.0.1", "vdi-name")
	if err != nil {
		t.Fatalf("Status returned error: %v", err)
	}
	if res.Active {
		t.Fatalf("expected inactive, got %+v", res)
	}
}

// TestNon2xxErrorHasNoBodyLeakage mirrors the TS behavior: the detail is
// logged server-side and the returned error is generic.
func TestNon2xxErrorHasNoBodyLeakage(t *testing.T) {
	const secretBody = "super-secret-webhook-internals"
	var got capturedRequest
	srv := webhookStub(t, 500, secretBody, &got)
	defer srv.Close()

	c := NewClient(srv.URL, "tok", testLogger())
	ctx := context.Background()

	if _, err := c.Enable(ctx, "100.64.0.1", "vdi-name", "1.2.3.4", nil); err == nil {
		t.Fatal("expected error from Enable on 500")
	} else if strings.Contains(err.Error(), secretBody) {
		t.Fatalf("error leaks response body: %v", err)
	}

	if err := c.Disable(ctx, "100.64.0.1", "vdi-name"); err == nil {
		t.Fatal("expected error from Disable on 500")
	} else if strings.Contains(err.Error(), secretBody) {
		t.Fatalf("error leaks response body: %v", err)
	}

	if _, err := c.Status(ctx, "100.64.0.1", "vdi-name"); err == nil {
		t.Fatal("expected error from Status on 500")
	} else if strings.Contains(err.Error(), secretBody) {
		t.Fatalf("error leaks response body: %v", err)
	}
}

func TestEnableNetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // connection refused

	c := NewClient(url, "", testLogger())
	if _, err := c.Enable(context.Background(), "100.64.0.1", "vdi-name", "1.2.3.4", nil); err == nil {
		t.Fatal("expected error on connection refused")
	}
}

func TestClientTimeoutIs60s(t *testing.T) {
	c := NewClient("http://example.invalid", "", testLogger())
	if c.http.Timeout.Seconds() != 60 {
		t.Fatalf("expected 60s timeout, got %v", c.http.Timeout)
	}
}
