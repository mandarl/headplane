package hscfg

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSplitPatchPath(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"dns.nameservers.split.\"foo.bar\"", []string{"dns", "nameservers", "split", "foo.bar"}},
		{"a.b.c", []string{"a", "b", "c"}},
		{"dns.search_domains", []string{"dns", "search_domains"}},
		{"server_url", []string{"server_url"}},
		// Quoted first segment: the TS code only stripped quotes from the
		// last segment (latent bug); ours strips all of them.
		{"\"a.b\".c", []string{"a.b", "c"}},
		{"dns.nameservers.split.\"corp.example\"", []string{"dns", "nameservers", "split", "corp.example"}},
	}
	for _, tc := range cases {
		got := SplitPatchPath(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("SplitPatchPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPatchSetNested(t *testing.T) {
	p := writeConfig(t, validConfig)
	c := Load(p, "", testLogger())
	if err := c.Patch([]Patch{{Path: "dns.base_domain", Value: "new.ts.net"}}); err != nil {
		t.Fatal(err)
	}
	if c.BaseDomain != "new.ts.net" {
		t.Fatalf("BaseDomain = %q, want new.ts.net", c.BaseDomain)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "new.ts.net") {
		t.Fatalf("patched value missing from file:\n%s", data)
	}
}

func TestPatchDeleteNil(t *testing.T) {
	p := writeConfig(t, validConfig)
	c := Load(p, "", testLogger())
	// Delete one split-DNS domain via a quoted path segment.
	if err := c.Patch([]Patch{{Path: `dns.nameservers.split."corp.example"`, Value: nil}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.SplitDNS["corp.example"]; ok {
		t.Fatalf("split domain still present: %v", c.SplitDNS)
	}
	data, _ := os.ReadFile(p)
	if strings.Contains(string(data), "corp.example") {
		t.Fatalf("deleted key still in file:\n%s", data)
	}
}

func TestPatchQuotedKeySet(t *testing.T) {
	p := writeConfig(t, validConfig)
	c := Load(p, "", testLogger())
	if err := c.Patch([]Patch{{Path: `dns.nameservers.split."foo.bar"`, Value: []string{"9.9.9.9"}}}); err != nil {
		t.Fatal(err)
	}
	got := c.SplitDNS["foo.bar"]
	if len(got) != 1 || got[0] != "9.9.9.9" {
		t.Fatalf("SplitDNS[foo.bar] = %v", got)
	}
}

func TestPatchPreservesComments(t *testing.T) {
	body := `# headscale config
server_url: http://headscale:8080 # the server URL
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
  # the base domain for magic dns
  base_domain: example.ts.net
  override_local_dns: true
  nameservers:
    global: ["1.1.1.1"]
  search_domains: ["example.ts.net"]
`
	p := writeConfig(t, body)
	c := Load(p, "", testLogger())
	if err := c.Patch([]Patch{{Path: "dns.override_local_dns", Value: false}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	for _, comment := range []string{"# headscale config", "# the server URL", "# the base domain for magic dns"} {
		if !strings.Contains(string(data), comment) {
			t.Fatalf("comment %q lost after patch:\n%s", comment, data)
		}
	}
}

func TestPatchPreservesFileMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(validConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Load(p, "", testLogger())
	if err := c.Patch([]Patch{{Path: "dns.base_domain", Value: "mode.ts.net"}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestPatchInvalidReverts(t *testing.T) {
	p := writeConfig(t, validConfig)
	before, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	c := Load(p, "", testLogger())
	// server_url is required: deleting it must fail validation.
	if err := c.Patch([]Patch{{Path: "server_url", Value: nil}}); err == nil {
		t.Fatal("expected error when patch invalidates the config")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("file changed despite failed revalidation")
	}
	if c.BaseDomain != "example.ts.net" {
		t.Fatalf("in-memory view changed: BaseDomain = %q", c.BaseDomain)
	}
	// The retained document must still carry server_url too.
	var raw map[string]any
	if err := c.doc.Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["server_url"]; !ok {
		t.Fatal("retained document lost server_url despite failed revalidation")
	}
}

func TestPatchRequiresWritable(t *testing.T) {
	p := writeConfig(t, validConfig)
	c := Load(p, "", testLogger())
	// Simulate read-only access (same-package override; running as root
	// makes real chmod-based simulation unreliable).
	c.access = AccessRO
	if err := c.Patch([]Patch{{Path: "dns.base_domain", Value: "x"}}); err == nil {
		t.Fatal("expected error patching a read-only config")
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "example.ts.net") {
		t.Fatal("read-only config file was modified")
	}

	var nilCfg *Config
	if err := nilCfg.Patch(nil); err == nil {
		t.Fatal("expected error patching a nil config")
	}
}

func TestAddDNSRecordConfigMode(t *testing.T) {
	p := writeConfig(t, validConfig)
	c := Load(p, "", testLogger())

	restart, err := c.AddDNSRecord(DNSRecord{Type: "A", Name: "www", Value: "5.6.7.8"})
	if err != nil {
		t.Fatal(err)
	}
	if !restart {
		t.Fatal("config mode add should request a restart")
	}
	recs := c.DNSRecords()
	if len(recs) != 2 || recs[1].Name != "www" {
		t.Fatalf("records = %+v", recs)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "www") {
		t.Fatalf("new record missing from config file:\n%s", data)
	}

	// Duplicate name+type is a no-op.
	restart, err = c.AddDNSRecord(DNSRecord{Type: "A", Name: "www", Value: "9.9.9.9"})
	if err != nil || restart {
		t.Fatalf("duplicate add = (%v, %v), want (false, nil)", restart, err)
	}
	if len(c.DNSRecords()) != 2 {
		t.Fatalf("records = %+v", c.DNSRecords())
	}
}

func TestRemoveDNSRecordConfigMode(t *testing.T) {
	p := writeConfig(t, validConfig)
	c := Load(p, "", testLogger())

	restart, err := c.RemoveDNSRecord("test", "A")
	if err != nil {
		t.Fatal(err)
	}
	if !restart {
		t.Fatal("config mode remove should request a restart")
	}
	if len(c.DNSRecords()) != 0 {
		t.Fatalf("records = %+v", c.DNSRecords())
	}
	data, _ := os.ReadFile(p)
	if strings.Contains(string(data), "1.2.3.4") {
		t.Fatalf("removed record still in file:\n%s", data)
	}

	// Removing again is a no-op.
	restart, err = c.RemoveDNSRecord("test", "A")
	if err != nil || restart {
		t.Fatalf("no-op remove = (%v, %v), want (false, nil)", restart, err)
	}
}

func writeDNSFile(t *testing.T, recs string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "records.json")
	if err := os.WriteFile(p, []byte(recs), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAddDNSRecordFileMode(t *testing.T) {
	p := writeConfig(t, validConfig)
	dnsPath := writeDNSFile(t, `[{"name":"file","type":"AAAA","value":"::1"}]`)
	c := Load(p, dnsPath, testLogger())

	restart, err := c.AddDNSRecord(DNSRecord{Type: "A", Name: "www", Value: "5.6.7.8"})
	if err != nil {
		t.Fatal(err)
	}
	if restart {
		t.Fatal("file mode add must not request a restart")
	}
	if len(c.DNSRecords()) != 2 {
		t.Fatalf("records = %+v", c.DNSRecords())
	}
	data, _ := os.ReadFile(dnsPath)
	if strings.HasSuffix(string(data), "\n") {
		t.Fatal("DNS file must not have a trailing newline (JSON.stringify has none)")
	}
	if !strings.Contains(string(data), `"www"`) || !strings.Contains(string(data), `    "name"`) {
		t.Fatalf("unexpected DNS file format:\n%s", data)
	}
	// The main config file must be untouched in file mode.
	cfgData, _ := os.ReadFile(p)
	if strings.Contains(string(cfgData), "www") {
		t.Fatal("main config file was modified in file mode")
	}

	// Duplicate name+type is a no-op.
	restart, err = c.AddDNSRecord(DNSRecord{Type: "A", Name: "www", Value: "9.9.9.9"})
	if err != nil || restart {
		t.Fatalf("duplicate add = (%v, %v), want (false, nil)", restart, err)
	}
	if len(c.DNSRecords()) != 2 {
		t.Fatal("duplicate record was appended")
	}
}

func TestAddDNSRecordFileModeNotWritable(t *testing.T) {
	p := writeConfig(t, validConfig)
	dnsPath := writeDNSFile(t, `[]`)
	c := Load(p, dnsPath, testLogger())
	c.dnsAccess = AccessRO // simulate a non-writable DNS file

	restart, err := c.AddDNSRecord(DNSRecord{Type: "A", Name: "www", Value: "5.6.7.8"})
	if err != nil || restart {
		t.Fatalf("not-writable add = (%v, %v), want (false, nil)", restart, err)
	}
	data, _ := os.ReadFile(dnsPath)
	if strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("DNS file changed despite not being writable: %s", data)
	}
}

func TestRemoveDNSRecordFileMode(t *testing.T) {
	p := writeConfig(t, validConfig)
	dnsPath := writeDNSFile(t, `[{"name":"a","type":"A","value":"1.1.1.1"},{"name":"b","type":"AAAA","value":"::1"}]`)
	c := Load(p, dnsPath, testLogger())

	restart, err := c.RemoveDNSRecord("a", "A")
	if err != nil {
		t.Fatal(err)
	}
	if restart {
		t.Fatal("file mode remove must not request a restart")
	}
	recs := c.DNSRecords()
	if len(recs) != 1 || recs[0].Name != "b" {
		t.Fatalf("records = %+v", recs)
	}
	data, _ := os.ReadFile(dnsPath)
	if strings.Contains(string(data), `"a"`) {
		t.Fatalf("removed record still in DNS file:\n%s", data)
	}
}
