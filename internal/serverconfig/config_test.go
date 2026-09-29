package serverconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// withEnv sets env vars for the duration of a test.
func withEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const minimalConfig = `
server:
  cookie_secret: "0123456789abcdef0123456789abcdef"
headscale:
  url: "http://127.0.0.1:8080/"
`

func TestLoadDefaults(t *testing.T) {
	withEnv(t, map[string]string{})
	cfg, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Host != "0.0.0.0" {
		t.Errorf("host = %q, want 0.0.0.0", cfg.Server.Host)
	}
	if cfg.Server.Port != 3000 {
		t.Errorf("port = %d, want 3000", cfg.Server.Port)
	}
	if cfg.Server.DataPath != "/var/lib/headplane/" {
		t.Errorf("data_path = %q", cfg.Server.DataPath)
	}
	if !cfg.Server.CookieSecure {
		t.Errorf("cookie_secure should default true")
	}
	if cfg.Server.CookieMaxAge != 86400 {
		t.Errorf("cookie_max_age = %d, want 86400", cfg.Server.CookieMaxAge)
	}
	// Trailing slash is stripped like the TS pipe.
	if cfg.Headscale.URL != "http://127.0.0.1:8080" {
		t.Errorf("headscale.url = %q, want trailing slash stripped", cfg.Headscale.URL)
	}
	if cfg.OIDC != nil {
		t.Errorf("oidc should be nil when absent")
	}
}

func TestEnvOverridesFile(t *testing.T) {
	path := writeConfig(t, `
server:
  cookie_secret: "0123456789abcdef0123456789abcdef"
  port: 3000
headscale:
  url: "http://127.0.0.1:8080"
`)
	withEnv(t, map[string]string{
		"HEADPLANE_SERVER__PORT": "8080",
		"HEADPLANE_DEBUG":        "true",
	})
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 8080 {
		t.Errorf("port = %d, want 8080 (env wins)", cfg.Server.Port)
	}
	if !cfg.Debug {
		t.Errorf("debug should be true from env")
	}
}

func TestEnvValueParsing(t *testing.T) {
	cases := map[string]any{
		"true":      true,
		"TRUE":      true,
		"false":     false,
		"null":      nil,
		"undefined": nil,
		"8080":      int64(8080),
		"-3":        int64(-3),
		"1.5":       1.5,
		"hello":     "hello",
		"  true  ":  true,
	}
	for in, want := range cases {
		if got := parseEnvValue(in); !equalAny(got, want) {
			t.Errorf("parseEnvValue(%q) = %#v, want %#v", in, got, want)
		}
	}
}

func equalAny(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a == b
}

func TestSecretPathLoading(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("  abcdef0123456789abcdef0123456789  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, "headscale:\n  url: \"http://127.0.0.1:8080\"\nserver:\n  cookie_secret_path: \""+secretFile+"\"\n")
	withEnv(t, map[string]string{})
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.CookieSecret != "abcdef0123456789abcdef0123456789" {
		t.Errorf("cookie_secret = %q, want trimmed file content", cfg.Server.CookieSecret)
	}
}

func TestSecretPathInterpolation(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("0123456789abcdef0123456789abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, "headscale:\n  url: \"http://127.0.0.1:8080\"\nserver:\n  cookie_secret_path: \"${SECRET_DIR}/secret.txt\"\n")
	withEnv(t, map[string]string{"SECRET_DIR": dir})
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.CookieSecret != "0123456789abcdef0123456789abcdef" {
		t.Errorf("cookie_secret = %q, want interpolated file content", cfg.Server.CookieSecret)
	}
}

func TestSecretPathMissingVar(t *testing.T) {
	path := writeConfig(t, "headscale:\n  url: \"http://127.0.0.1:8080\"\nserver:\n  cookie_secret_path: \"${DEFINITELY_NOT_SET_XYZ}/secret.txt\"\n")
	withEnv(t, map[string]string{})
	if _, err := Load(path); err == nil {
		t.Errorf("expected error for missing interpolation variable")
	}
}

func TestSecretPathConflict(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeConfig(t, "headscale:\n  url: \"http://127.0.0.1:8080\"\nserver:\n  cookie_secret: \"0123456789abcdef0123456789abcdef\"\n  cookie_secret_path: \""+secretFile+"\"\n")
	withEnv(t, map[string]string{})
	if _, err := Load(path); err == nil {
		t.Errorf("expected error when both key and _path are set")
	}
}

func TestValidation(t *testing.T) {
	withEnv(t, map[string]string{})
	// Missing headscale.url.
	if _, err := Load(writeConfig(t, "server:\n  cookie_secret: \"0123456789abcdef0123456789abcdef\"\n")); err == nil {
		t.Errorf("expected error for missing headscale.url")
	}
	// Short cookie secret.
	if _, err := Load(writeConfig(t, "server:\n  cookie_secret: \"short\"\nheadscale:\n  url: \"http://127.0.0.1:8080\"\n")); err == nil {
		t.Errorf("expected error for short cookie_secret")
	}
	// Bad host.
	if _, err := Load(writeConfig(t, "server:\n  host: \"not-an-ip\"\n  cookie_secret: \"0123456789abcdef0123456789abcdef\"\nheadscale:\n  url: \"http://127.0.0.1:8080\"\n")); err == nil {
		t.Errorf("expected error for non-IP host")
	}
	// Half-configured TLS.
	if _, err := Load(writeConfig(t, "server:\n  cookie_secret: \"0123456789abcdef0123456789abcdef\"\n  tls_cert_path: \"/tmp/cert.pem\"\nheadscale:\n  url: \"http://127.0.0.1:8080\"\n")); err == nil {
		t.Errorf("expected error for cert without key")
	}
}

func TestMissingFileOK(t *testing.T) {
	withEnv(t, map[string]string{
		"HEADPLANE_SERVER__COOKIE_SECRET": "0123456789abcdef0123456789abcdef",
		"HEADPLANE_HEADSCALE__URL":        "http://127.0.0.1:8080",
	})
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("Load with missing file: %v", err)
	}
	if cfg.Server.CookieSecret != "0123456789abcdef0123456789abcdef" {
		t.Errorf("env-only config failed: %q", cfg.Server.CookieSecret)
	}
}

func TestDeepMergeNested(t *testing.T) {
	path := writeConfig(t, "server:\n  cookie_secret: \"0123456789abcdef0123456789abcdef\"\n  port: 3000\nheadscale:\n  url: \"http://127.0.0.1:8080\"\n  config_strict: true\n")
	withEnv(t, map[string]string{
		"HEADPLANE_HEADSCALE__CONFIG_STRICT": "false",
	})
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Headscale.ConfigStrict {
		t.Errorf("nested env override did not win")
	}
	if cfg.Server.Port != 3000 {
		t.Errorf("file value lost in merge: port = %d", cfg.Server.Port)
	}
}

func TestOidcEnabledDefault(t *testing.T) {
	withEnv(t, map[string]string{})
	base := `
server:
  cookie_secret: "0123456789abcdef0123456789abcdef"
headscale:
  url: "http://127.0.0.1:8080/"
oidc:
  issuer: "https://idp.example.com"
  client_id: "c"
  client_secret: "s3cr3t-s3cr3t-s3cr3t-s3cr3t12"
`
	// Omitted `enabled` defaults to true when the section is present
	// (matches the TS zod schema default).
	cfg, err := Load(writeConfig(t, base))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.OIDC == nil || !cfg.OIDC.IsEnabled() {
		t.Errorf("omitted oidc.enabled should default to enabled")
	}

	// An explicit `enabled: false` must survive validation as disabled.
	cfgOff, err := Load(writeConfig(t, base+"  enabled: false\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfgOff.OIDC == nil || cfgOff.OIDC.IsEnabled() {
		t.Errorf("explicit oidc.enabled: false should stay disabled")
	}

	cfgOn, err := Load(writeConfig(t, base+"  enabled: true\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfgOn.OIDC == nil || !cfgOn.OIDC.IsEnabled() {
		t.Errorf("explicit oidc.enabled: true should stay enabled")
	}
}
