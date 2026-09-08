// Package hscfg ports app/server/config.ts (getHeadscaleConfig with its
// arktype HeadscaleConfigSchema) into Go.
//
// The config YAML is read ONCE at startup — never watched, never written by
// the server — and surfaced read-only. Access is three-state: "rw" when the
// file parses and strictly validates, "ro" when it exists but doesn't
// validate (or headscale isn't otherwise configurable), "no" when there is
// no config file or it can't be read/parsed at all. DNS mutation, the agent
// sync and everything that writes the config back lands in Phase 5; this
// package only answers reads.
package hscfg

import (
	"encoding/json"
	"log/slog"
	"net/netip"
	"os"

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

// Config is the read-only view of the Headscale config file. Zero value
// means "no config" (access = "no").
type Config struct {
	access Access

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

// Writable reports whether the config strictly validates (Phase 5 writes
// would be safe).
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

// Load mirrors getHeadscaleConfig: read + parse the config file once at
// startup and classify access. A missing/empty/unparseable file yields
// access "no"; a parseable-but-invalid file yields "ro". dnsRecordsPath is
// the path of the separate DNS records file ("" when unconfigured).
func Load(path string, dnsRecordsPath string, logger *slog.Logger) *Config {
	c := &Config{access: AccessNo}
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
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		logger.Error("failed to parse Headscale config", "path", path, "error", err)
		return c
	}
	c.access = AccessRO
	if !strictlyValid(raw) {
		logger.Error("Headscale config does not validate; read-only access", "path", path)
		return c
	}
	c.access = AccessRW
	c.extract(raw)
	c.loadFileRecords(dnsRecordsPath, logger)
	return c
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

// loadFileRecords mirrors the getRecords() file branch: the separate DNS
// records file (a JSON array of {name, type, value}) takes precedence over
// the dns block when configured.
func (c *Config) loadFileRecords(path string, logger *slog.Logger) {
	if path == "" {
		return
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
