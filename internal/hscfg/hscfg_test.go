package hscfg

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

const validConfig = `
server_url: http://headscale:8080
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

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	c := Load(writeConfig(t, validConfig), "", testLogger())
	if c.Access() != AccessRW {
		t.Fatalf("access = %q, want rw", c.Access())
	}
	if !c.Readable() || !c.Writable() {
		t.Fatal("should be readable and writable")
	}
	if c.PrefixesV4 != "100.64.0.0/10" || c.PrefixesV6 != "fd7a:115c:a1e0::/48" {
		t.Fatalf("prefixes = %q %q", c.PrefixesV4, c.PrefixesV6)
	}
	if !c.MagicDNS || c.BaseDomain != "example.ts.net" || !c.OverrideDNS {
		t.Fatalf("dns = %v %q %v", c.MagicDNS, c.BaseDomain, c.OverrideDNS)
	}
	if len(c.Nameservers) != 1 || c.Nameservers[0] != "1.1.1.1" {
		t.Fatalf("nameservers = %v", c.Nameservers)
	}
	if len(c.SplitDNS["corp.example"]) != 1 {
		t.Fatalf("split = %v", c.SplitDNS)
	}
	if len(c.SearchDomains) != 1 {
		t.Fatalf("search = %v", c.SearchDomains)
	}
	recs := c.DNSRecords()
	if len(recs) != 1 || recs[0].Name != "test" || recs[0].Type != "A" || recs[0].Value != "1.2.3.4" {
		t.Fatalf("records = %+v", recs)
	}
	if c.OIDC == nil || len(c.OIDC.AllowedDomains) != 1 || c.OIDC.AllowedDomains[0] != "example.com" {
		t.Fatalf("oidc = %+v", c.OIDC)
	}
	if len(c.OIDC.AllowedGroups) != 1 || c.OIDC.AllowedGroups[0] != "ops" {
		t.Fatalf("oidc groups = %+v", c.OIDC)
	}
}

func TestLoadMissingFile(t *testing.T) {
	c := Load(filepath.Join(t.TempDir(), "nope.yaml"), "", testLogger())
	if c.Access() != AccessNo || c.Readable() || c.Writable() {
		t.Fatalf("access = %q", c.Access())
	}
	if c.DNSRecords() != nil {
		t.Fatal("no records expected")
	}
}

func TestLoadUnparseable(t *testing.T) {
	c := Load(writeConfig(t, ":\t: bad:\n  - [\n"), "", testLogger())
	if c.Access() != AccessNo {
		t.Fatalf("access = %q, want no", c.Access())
	}
}

func TestLoadInvalidIsReadOnly(t *testing.T) {
	// Missing the required dns block -> parses but does not validate.
	c := Load(writeConfig(t, "server_url: http://x\nlisten_addr: 0.0.0.0:1\nnoise:\n  private_key_path: /tmp/k\nprefixes:\n  v4: 100.64.0.0/10\nderp:\n  a: b\ndatabase:\n  type: sqlite\n"), "", testLogger())
	if c.Access() != AccessRO {
		t.Fatalf("access = %q, want ro", c.Access())
	}
	if !c.Readable() || c.Writable() {
		t.Fatal("ro should be readable but not writable")
	}
}

func TestDNSRecordsFileWins(t *testing.T) {
	dir := t.TempDir()
	recordsPath := filepath.Join(dir, "records.json")
	if err := os.WriteFile(recordsPath, []byte(`[{"name":"file","type":"AAAA","value":"::1"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := Load(writeConfig(t, validConfig), recordsPath, testLogger())
	recs := c.DNSRecords()
	if len(recs) != 1 || recs[0].Name != "file" || recs[0].Type != "AAAA" {
		t.Fatalf("records = %+v", recs)
	}
}

func TestValidPrefix(t *testing.T) {
	if !ValidPrefix("100.64.0.0/10") || !ValidPrefix("fd7a:115c:a1e0::/48") {
		t.Fatal("valid prefixes rejected")
	}
	if ValidPrefix("not-a-prefix") {
		t.Fatal("invalid prefix accepted")
	}
}
