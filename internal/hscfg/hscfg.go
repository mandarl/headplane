// Package hscfg ports app/server/headscale/config-loader.ts (HeadscaleConfig)
// and config-dns.ts (HeadscaleDNSConfig) into Go.
//
// The config YAML is read ONCE at startup — never watched — and surfaced
// through an extracted read view. Access is three-state: "rw" when the file
// parses, strictly validates, and is writable; "ro" when it is readable but
// not writable or does not strictly validate; "no" when there is no config
// file or it can't be read/parsed at all. The parsed yaml.Node document is
// retained so Patch can mutate the file in place while preserving comments
// and formatting.
package hscfg

import (
	"encoding/json"
	"log/slog"
	"net/netip"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

// Access is the three-state config access used across the UI.
type Access string

const (
	AccessRW Access = "rw"
	AccessRO Access = "ro"
	AccessNo Access = "no"
)

// DNSRecord is one extra DNS record entry.
type DNSRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// OIDCSettings is the small slice of the OIDC block the restrictions page
// reads: the allow-lists it edits. (Writing them back is Phase 5.)
type OIDCSettings struct {
	AllowedDomains []string `json:"allowed_domains"`
	AllowedGroups  []string `json:"allowed_groups"`
	AllowedUsers   []string `json:"allowed_users"`
}

// Config is the view of the Headscale config file. Zero value means "no
// config" (access = "no"). The unexported fields back the Phase 5 patch
// engine: the file path, the retained yaml.Node document (comments and
// formatting preserved across patches), and the separate DNS records file
// state. mu serializes Patch/AddDNSRecord/RemoveDNSRecord.
type Config struct {
	mu sync.Mutex

	access    Access
	path      string
	doc       *yaml.Node // retained parsed document; nil when unreadable/unparseable
	dnsPath   string     // separate DNS records file path ("" when unconfigured)
	dnsAccess Access     // access of the DNS records file (AccessNo when unconfigured)
	logger    *slog.Logger

	PrefixesV4  string              `json:"-"`
	PrefixesV6  string              `json:"-"`
	MagicDNS    bool                `json:"-"`
	BaseDomain  string              `json:"-"`
	OverrideDNS bool                `json:"-"`
	Nameservers []string            `json:"-"`
	SplitDNS    map[string][]string `json:"-"`

	SearchDomains []string    `json:"-"`
	ExtraRecords  []DNSRecord `json:"-"` // from the dns block
	FileRecords   []DNSRecord `json:"-"` // from the separate dns records file, when present

	OIDC *OIDCSettings `json:"-"`
}

// Access returns "rw", "ro" or "no".
func (c *Config) Access() Access {
	if c == nil {
		return AccessNo
	}
	return c.access
}

// Readable reports whether the config file exists and parses.
func (c *Config) Readable() bool {
	a := c.Access()
	return a == AccessRW || a == AccessRO
}

// Writable reports whether the config file strictly validates and is
// writable — the gate for Patch and config-mode DNS mutation.
func (c *Config) Writable() bool { return c.Access() == AccessRW }

// DNSRecords mirrors the getRecords() helper in the DNS route: when a
// separate DNS records file is configured it wins; otherwise the records
// come from the dns.extra_records block.
func (c *Config) DNSRecords() []DNSRecord {
	if c == nil {
		return nil
	}
	if c.FileRecords != nil {
		return c.FileRecords
	}
	return c.ExtraRecords
}

// Load mirrors getHeadscaleConfig/loadHeadscaleConfig: read + parse the
// config file once at startup and classify access. A missing/empty/
// unparseable file yields access "no"; a parseable file that is not writable
// or does not strictly validate yields "ro"; a writable, strictly valid
// file yields "rw". The parsed yaml.Node document is retained for Patch.
// dnsRecordsPath is the path of the separate DNS records file ("" when
// unconfigured); the DNS file object exists whenever it is set, even when
// the file is unreadable, mirroring loadHeadscaleDNS.
func Load(path string, dnsRecordsPath string, logger *slog.Logger) *Config {
	c := &Config{access: AccessNo, dnsAccess: AccessNo, logger: logger}
	if path == "" {
		return c
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Error("failed to read Headscale config", "path", path, "error", err)
		}
		return c
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		logger.Error("failed to parse Headscale config", "path", path, "error", err)
		return c
	}
	c.path = path
	c.doc = &doc

	var raw map[string]any
	_ = doc.Decode(&raw) // best effort; nil/odd roots fail strict validation below

	c.access = AccessRO
	valid := strictlyValid(raw)
	if !valid {
		logger.Error("Headscale config does not validate; read-only access", "path", path)
	} else if !probeWritable(path) {
		logger.Warn("Headscale config is not writable; read-only access", "path", path)
	} else {
		c.access = AccessRW
	}
	c.extract(raw)
	c.loadFileRecords(dnsRecordsPath, logger)
	return c
}

// probeWritable mirrors the W_OK half of validateConfigPath: open for write
// and close immediately, without touching the file.
func probeWritable(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// strictlyValid mirrors the required keys of the TS HeadscaleConfigSchema
// (the parts Headplane reads). Unknown keys are ignored, matching the
// arktype `.onUndeclaredKey("ignore")` behavior.
func strictlyValid(raw map[string]any) bool {
	str := func(m map[string]any, key string) (string, bool) {
		v, ok := m[key].(string)
		return v, ok && v != ""
	}
	get := func(key string) (map[string]any, bool) {
		m, ok := raw[key].(map[string]any)
		return m, ok
	}

	if _, ok := str(raw, "server_url"); !ok {
		return false
	}
	if _, ok := str(raw, "listen_addr"); !ok {
		return false
	}
	noise, ok := get("noise")
	if !ok {
		return false
	}
	if _, ok := str(noise, "private_key_path"); !ok {
		return false
	}
	if _, ok := get("prefixes"); !ok {
		return false
	}
	if _, ok := get("derp"); !ok {
		return false
	}
	if _, ok := get("database"); !ok {
		return false
	}
	dns, ok := get("dns")
	if !ok {
		return false
	}
	for _, k := range []string{"magic_dns", "base_domain", "override_local_dns", "nameservers", "search_domains"} {
		if _, ok := dns[k]; !ok {
			return false
		}
	}
	return true
}

func (c *Config) extract(raw map[string]any) {
	// Reset the accumulated slices: extract runs on Load and again after
	// every successful Patch, and must be idempotent.
	c.ExtraRecords = nil
	c.Nameservers = nil
	c.SearchDomains = nil
	dns, _ := raw["dns"].(map[string]any)
	if dns == nil {
		return
	}
	prefixes, _ := raw["prefixes"].(map[string]any)
	c.PrefixesV4, _ = prefixes["v4"].(string)
	c.PrefixesV6, _ = prefixes["v6"].(string)

	// Defaults mirror the arktype schema defaults.
	c.MagicDNS = boolOr(dns["magic_dns"], true)
	c.BaseDomain, _ = dns["base_domain"].(string)
	if c.BaseDomain == "" {
		c.BaseDomain = "headscale.net"
	}
	c.OverrideDNS = boolOr(dns["override_local_dns"], false)

	ns, _ := dns["nameservers"].(map[string]any)
	if ns != nil {
		c.Nameservers = strList(ns["global"])
		if split, ok := ns["split"].(map[string]any); ok {
			c.SplitDNS = map[string][]string{}
			for domain, addrs := range split {
				c.SplitDNS[domain] = strList(addrs)
			}
		}
	}
	c.SearchDomains = strList(dns["search_domains"])
	if recs, ok := dns["extra_records"].([]any); ok {
		for _, r := range recs {
			if m, ok := r.(map[string]any); ok {
				c.ExtraRecords = append(c.ExtraRecords, DNSRecord{
					Type:  strOr(m["type"], ""),
					Name:  strOr(m["name"], ""),
					Value: strOr(m["value"], ""),
				})
			}
		}
	}

	if oidcRaw, ok := raw["oidc"].(map[string]any); ok {
		c.OIDC = &OIDCSettings{
			AllowedDomains: dedupe(strList(oidcRaw["allowed_domains"])),
			AllowedGroups:  dedupe(strList(oidcRaw["allowed_groups"])),
			AllowedUsers:   dedupe(strList(oidcRaw["allowed_users"])),
		}
	}
}

// loadFileRecords mirrors the getRecords() file branch and loadHeadscaleDNS:
// the separate DNS records file (a JSON array of {name, type, value}) takes
// precedence over the dns block when configured. The DNS file state exists
// whenever a path is configured, even when unreadable — that is what puts
// AddDNSRecord/RemoveDNSRecord into file mode.
func (c *Config) loadFileRecords(path string, logger *slog.Logger) {
	if path == "" {
		return
	}
	c.dnsPath = path
	readable, writable := probeRW(path)
	switch {
	case readable && writable:
		c.dnsAccess = AccessRW
	case readable:
		c.dnsAccess = AccessRO
	default:
		c.dnsAccess = AccessNo
	}
	data, err := os.ReadFile(path)
	if err != nil {
		logger.Error("failed to read DNS records file", "path", path, "error", err)
		return
	}
	var recs []DNSRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		logger.Error("failed to parse DNS records file", "path", path, "error", err)
		return
	}
	c.FileRecords = recs
}

// probeRW mirrors validateConfigPath in config-dns.ts: read probe, then
// write probe.
func probeRW(path string) (readable, writable bool) {
	if f, err := os.Open(path); err == nil {
		readable = true
		_ = f.Close()
	}
	writable = probeWritable(path)
	return readable, writable
}

func boolOr(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

func strOr(v any, def string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

func strList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ValidPrefix reports whether s parses as an IP prefix (used to guard the
// DNS page's prefix display, mirroring the TS zod .refine on prefixes).
func ValidPrefix(s string) bool {
	_, err := netip.ParsePrefix(s)
	return err == nil
}
