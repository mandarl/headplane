package serverconfig

import (
	"strings"
	"testing"
)

func TestWebAuthnDefaults(t *testing.T) {
	withEnv(t, map[string]string{})
	cfg, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// No webauthn section: passkeys stay disabled.
	if cfg.WebAuthn != nil {
		t.Fatalf("WebAuthn should be nil when the section is absent")
	}
	if cfg.WebAuthn.IsEnabled() {
		t.Errorf("IsEnabled on nil section should be false")
	}
}

func TestWebAuthnSectionDefaultsEnabled(t *testing.T) {
	withEnv(t, map[string]string{})
	// An explicit (even empty) map enables the section; a bare `webauthn:`
	// with a null value is the same as absent, mirroring the oidc section.
	cfg, err := Load(writeConfig(t, minimalConfig+"\nwebauthn: {}\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebAuthn == nil {
		t.Fatalf("WebAuthn should be non-nil when the section is present")
	}
	if !cfg.WebAuthn.IsEnabled() {
		t.Errorf("webauthn section without enabled should default to enabled")
	}
	if cfg.WebAuthn.RPID != "" {
		t.Errorf("rp_id = %q, want empty default", cfg.WebAuthn.RPID)
	}
}

func TestWebAuthnDisabled(t *testing.T) {
	withEnv(t, map[string]string{})
	cfg, err := Load(writeConfig(t, minimalConfig+"\nwebauthn:\n  enabled: false\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebAuthn.IsEnabled() {
		t.Errorf("enabled: false should disable passkeys")
	}
}

func TestWebAuthnRPIDOverride(t *testing.T) {
	withEnv(t, map[string]string{})
	cfg, err := Load(writeConfig(t, minimalConfig+"\nwebauthn:\n  rp_id: Login.EXAMPLE.com\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebAuthn.RPID != "login.example.com" {
		t.Errorf("rp_id = %q, want lowercased bare hostname", cfg.WebAuthn.RPID)
	}
}

func TestWebAuthnRPIDRejectsURL(t *testing.T) {
	withEnv(t, map[string]string{})
	for _, rp := range []string{"https://example.com", "example.com/path", "example.com:3000"} {
		cfg, err := Load(writeConfig(t, minimalConfig+"\nwebauthn:\n  rp_id: "+rp+"\n"))
		if err == nil {
			t.Errorf("rp_id %q should be rejected, got %+v", rp, cfg.WebAuthn)
			continue
		}
		if !strings.Contains(err.Error(), "webauthn.rp_id") {
			t.Errorf("rp_id %q: error %q should mention webauthn.rp_id", rp, err)
		}
	}
}
