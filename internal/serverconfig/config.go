// Package serverconfig loads headplane's server configuration.
//
// It mirrors app/server/config (load.ts + config-schema.ts) with identical
// semantics so the Go server (cmd/hp_server) honors the same config surface
// as the Node server it replaces:
//
//  1. YAML config file at $HEADPLANE_CONFIG_PATH, default
//     /etc/headplane/config.yaml (a missing file is not an error).
//  2. HEADPLANE_* environment variables override the file. Nested keys use
//     double underscores: HEADPLANE_SERVER__PORT=8080 sets server.port.
//     Values are parsed like the TS loader: true/false/null/undefined and
//     numbers become typed values, everything else stays a string.
//  3. Secret keys may be loaded from files via `<key>_path` entries
//     (server.cookie_secret, headscale.api_key, oidc.client_secret,
//     oidc.headscale_api_key, rdp_gateway.webhook_token). The path supports
//     ${VAR} interpolation; setting both the key and its _path is an error.
//     File content is trimmed and Unicode-normalized (NFC), like the TS
//     loader's `.trim().normalize()`.
//
// Defaults and validation match config-schema.ts. Unknown keys in the file
// or environment are ignored (the TS schema deletes undeclared keys).
//
// The `integration` section is carried through as an untyped map for now;
// its docker/kubernetes/proc/agent sub-schemas will be validated when
// Phase 5 ports those integrations.
package serverconfig

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
	"gopkg.in/yaml.v3"
)

// Config file discovery.
const (
	// DefaultConfigPath is used when HEADPLANE_CONFIG_PATH is unset.
	DefaultConfigPath = "/etc/headplane/config.yaml"
	// EnvConfigPath overrides the config file path.
	EnvConfigPath = "HEADPLANE_CONFIG_PATH"
	// EnvPrefix marks environment variables that feed the config.
	EnvPrefix = "HEADPLANE_"
	// EnvListenFile is consumed by main, not the config tree.
	EnvListenFile = "HEADPLANE_LISTEN_FILE"
)

// pathSupportedKeys mirrors pathSupportedKeys in config-schema.ts.
var pathSupportedKeys = []string{
	"server.cookie_secret",
	"headscale.api_key",
	"oidc.client_secret",
	"oidc.headscale_api_key",
	"rdp_gateway.webhook_token",
}

// ServerConfig mirrors the `server` section of config-schema.ts.
type ServerConfig struct {
	Host         string `yaml:"host"`
	Port         int    `yaml:"port"`
	BaseURL      string `yaml:"base_url"`
	DataPath     string `yaml:"data_path"`
	InfoSecret   string `yaml:"info_secret"`
	CookieSecret string `yaml:"cookie_secret"`
	CookieSecure bool   `yaml:"cookie_secure"`
	CookieDomain string `yaml:"cookie_domain"`
	CookieMaxAge int    `yaml:"cookie_max_age"`
	TLSCertPath  string `yaml:"tls_cert_path"`
	TLSKeyPath   string `yaml:"tls_key_path"`
}

// HeadscaleConfig mirrors the `headscale` section.
type HeadscaleConfig struct {
	URL            string `yaml:"url"`
	PublicURL      string `yaml:"public_url"`
	APIKey         string `yaml:"api_key"`
	ConfigPath     string `yaml:"config_path"`
	ConfigStrict   bool   `yaml:"config_strict"`
	DNSRecordsPath string `yaml:"dns_records_path"`
	TLSCertPath    string `yaml:"tls_cert_path"`
}

// OIDCConfig mirrors the `oidc` section. Enabled is a *bool because the
// schema default is enabled=true when the section is present: nil means
// "not specified" and validates to true, while an explicit
// `enabled: false` disables OIDC.
type OIDCConfig struct {
	Enabled                 *bool             `yaml:"enabled"`
	Issuer                  string            `yaml:"issuer"`
	ClientID                string            `yaml:"client_id"`
	ClientSecret            string            `yaml:"client_secret"`
	HeadscaleAPIKey         string            `yaml:"headscale_api_key"`
	UsePKCE                 bool              `yaml:"use_pkce"`
	RedirectURI             string            `yaml:"redirect_uri"`
	DisableAPIKeyLogin      bool              `yaml:"disable_api_key_login"`
	Scope                   string            `yaml:"scope"`
	SubjectClaims           []string          `yaml:"subject_claims"`
	AllowWeakRSAKeys        bool              `yaml:"allow_weak_rsa_keys"`
	ProfilePictureSource    string            `yaml:"profile_picture_source"`
	ExtraParams             map[string]string `yaml:"extra_params"`
	AuthorizationEndpoint   string            `yaml:"authorization_endpoint"`
	TokenEndpoint           string            `yaml:"token_endpoint"`
	JWKsURI                 string            `yaml:"jwks_uri"`
	UserinfoEndpoint        string            `yaml:"userinfo_endpoint"`
	EndSessionEndpoint      string            `yaml:"end_session_endpoint"`
	PostLogoutRedirectURI   string            `yaml:"post_logout_redirect_uri"`
	UseEndSession           bool              `yaml:"use_end_session"`
	TokenEndpointAuthMethod string            `yaml:"token_endpoint_auth_method"`
}

// IsEnabled reports whether the OIDC section enables the provider. A nil
// receiver or nil Enabled (e.g. a struct built without validate()) means
// the schema default: enabled.
func (o *OIDCConfig) IsEnabled() bool {
	return o != nil && (o.Enabled == nil || *o.Enabled)
}

// RDPGatewayConfig mirrors the `rdp_gateway` section.
type RDPGatewayConfig struct {
	Enabled      bool   `yaml:"enabled"`
	WebhookURL   string `yaml:"webhook_url"`
	WebhookToken string `yaml:"webhook_token"`
}

// Config is the fully-resolved headplane configuration.
type Config struct {
	Debug       bool              `yaml:"debug"`
	Server      ServerConfig      `yaml:"server"`
	Headscale   HeadscaleConfig   `yaml:"headscale"`
	OIDC        *OIDCConfig       `yaml:"oidc"`
	Integration map[string]any    `yaml:"integration"`
	RDPGateway  *RDPGatewayConfig `yaml:"rdp_gateway"`
}

// defaults returns a Config with the schema defaults applied.
func defaults() *Config {
	return &Config{
		Debug: false,
		Server: ServerConfig{
			Host:         "0.0.0.0",
			Port:         3000,
			DataPath:     "/var/lib/headplane/",
			CookieSecure: true,
			CookieMaxAge: 86400,
		},
		Headscale: HeadscaleConfig{
			ConfigStrict: true,
		},
	}
}

// Load reads configuration from the YAML file and HEADPLANE_* environment
// variables, applying the same precedence, secret-path, and validation
// semantics as the TS loader. configPathOverride forces the file path
// (used in tests); pass "" for normal discovery.
func Load(configPathOverride string) (*Config, error) {
	path := configPathOverride
	if path == "" {
		if p := os.Getenv(EnvConfigPath); p != "" {
			path = p
		} else {
			path = DefaultConfigPath
		}
	}

	fileCfg, err := loadFile(path)
	if err != nil {
		return nil, err
	}
	envCfg := loadEnv()

	merged := deepMerge(fileCfg, envCfg)

	if err := resolveSecretPaths(merged); err != nil {
		return nil, err
	}

	cfg := defaults()
	if err := applyMap(cfg, merged); err != nil {
		return nil, err
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// loadFile parses the YAML config file. A missing/unreadable file is not an
// error (mirrors the TS loader logging and returning undefined).
func loadFile(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}, nil
	}
	var out map[string]any
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(false)
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("invalid YAML in %s: %w", path, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

var envNumber = regexp.MustCompile(`^-?\d+(\.\d+)?$`)

// parseEnvValue mirrors parseEnvValue in load.ts.
func parseEnvValue(value string) any {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case "true":
		return true
	case "false":
		return false
	case "null", "undefined":
		return nil
	}
	if envNumber.MatchString(v) {
		if strings.Contains(v, ".") {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		} else if i, err := strconv.ParseInt(v, 10, 64); err == nil {
			return i
		}
	}
	return value
}

// loadEnv builds a nested map from HEADPLANE_* environment variables.
func loadEnv() map[string]any {
	out := map[string]any{}
	for _, kv := range os.Environ() {
		key, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(key, EnvPrefix) {
			continue
		}
		// These are consumed by the launcher, not the config tree; the TS
		// schema deletes them as undeclared keys.
		if key == EnvConfigPath || key == EnvListenFile {
			continue
		}
		name := strings.ToLower(strings.TrimPrefix(key, EnvPrefix))
		deepSet(out, strings.Split(name, "__"), parseEnvValue(value))
	}
	return out
}

// deepMerge overlays src onto dst; maps merge recursively, everything else
// (including arrays) is replaced. Mirrors deepMerge in load.ts.
func deepMerge(dst, src map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if v == nil {
			continue
		}
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := out[k].(map[string]any); ok {
				out[k] = deepMerge(dv, sv)
				continue
			}
			out[k] = deepMerge(map[string]any{}, sv)
			continue
		}
		out[k] = v
	}
	return out
}

func deepSet(obj map[string]any, path []string, value any) {
	cur := obj
	for _, key := range path[:len(path)-1] {
		next, ok := cur[key].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[key] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = value
}

func deepGet(obj map[string]any, path []string) any {
	var cur any = obj
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[seg]
	}
	return cur
}

var interpolationVar = regexp.MustCompile(`\$\{([^}]+)\}`)

// resolveSecretPaths implements loadConfigKeyPaths from load.ts: for each
// supported key, a `<key>_path` entry loads the file content into `<key>`
// (with ${VAR} interpolation, trim + NFC normalization). Setting both is an
// error, as is an unreadable file or a missing interpolation variable.
func resolveSecretPaths(cfg map[string]any) error {
	for _, key := range pathSupportedKeys {
		segments := strings.Split(key, ".")
		pathKey := segments[:len(segments)-1]
		pathKey = append(append([]string{}, pathKey...), segments[len(segments)-1]+"_path")

		pathValue, _ := deepGet(cfg, pathKey).(string)
		if pathValue == "" {
			continue
		}
		if existing := deepGet(cfg, segments); existing != nil {
			return fmt.Errorf("config: %s and %s_path are mutually exclusive", key, key)
		}

		realPath, err := interpolate(pathValue, key+"_path")
		if err != nil {
			return err
		}
		content, err := os.ReadFile(realPath)
		if err != nil {
			return fmt.Errorf("config: cannot read secret file for %s_path (%s): %w", key, realPath, err)
		}
		// Mirror `.trim().normalize()` (NFC).
		normalized := norm.NFC.String(strings.TrimSpace(string(content)))
		deepSet(cfg, segments, normalized)
		// Drop the _path key so it never leaks into the typed config.
		parent := deepGet(cfg, pathKey[:len(pathKey)-1])
		if m, ok := parent.(map[string]any); ok {
			delete(m, pathKey[len(pathKey)-1])
		}
	}
	return nil
}

func interpolate(s, pathKey string) (string, error) {
	var interpErr error
	out := interpolationVar.ReplaceAllStringFunc(s, func(match string) string {
		name := interpolationVar.FindStringSubmatch(match)[1]
		value, ok := os.LookupEnv(name)
		if !ok {
			interpErr = fmt.Errorf("config: %s references missing environment variable %s", pathKey, name)
			return match
		}
		return value
	})
	if interpErr != nil {
		return "", interpErr
	}
	return out, nil
}

// applyMap overlays the merged untyped map onto the defaulted Config via a
// YAML round-trip. Unknown keys are ignored (KnownFields(false)).
func applyMap(cfg *Config, m map[string]any) error {
	raw, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("config: cannot encode merged config: %w", err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(false)
	if err := dec.Decode(cfg); err != nil {
		return fmt.Errorf("config: invalid value: %w", err)
	}
	return nil
}

// stripTrailingSlash mirrors the headscale url pipe in config-schema.ts.
func stripTrailingSlash(u string) string {
	return strings.TrimRight(u, "/")
}

// validate enforces the required fields and value constraints from
// config-schema.ts. Error messages intentionally name the config key.
func validate(cfg *Config) error {
	s := &cfg.Server
	if net.ParseIP(s.Host) == nil {
		return fmt.Errorf("config: server.host %q is not a valid IP address", s.Host)
	}
	if s.Port < 0 || s.Port > 65535 {
		return fmt.Errorf("config: server.port %d out of range", s.Port)
	}
	s.DataPath = strings.ToLower(s.DataPath)
	s.CookieDomain = strings.ToLower(s.CookieDomain)
	if len([]rune(s.CookieSecret)) != 32 {
		return fmt.Errorf("config: server.cookie_secret must be exactly 32 characters")
	}
	if (s.TLSCertPath == "") != (s.TLSKeyPath == "") {
		return fmt.Errorf("config: server.tls_cert_path and server.tls_key_path must both be provided")
	}
	// When either TLS path is set, cookie_secure is forced true (schema
	// comment); the default is already true, and an explicit false with TLS
	// is a misconfiguration.
	if s.TLSCertPath != "" && !s.CookieSecure {
		return fmt.Errorf("config: server.cookie_secure must be true when TLS is enabled")
	}

	h := &cfg.Headscale
	if h.URL == "" {
		return fmt.Errorf("config: headscale.url is required")
	}
	if _, err := url.ParseRequestURI(h.URL); err != nil {
		return fmt.Errorf("config: headscale.url %q is not a valid URL", h.URL)
	}
	h.URL = stripTrailingSlash(h.URL)
	h.PublicURL = stripTrailingSlash(h.PublicURL)
	h.ConfigPath = strings.ToLower(h.ConfigPath)
	h.DNSRecordsPath = strings.ToLower(h.DNSRecordsPath)
	h.TLSCertPath = strings.ToLower(h.TLSCertPath)

	if cfg.OIDC != nil {
		o := cfg.OIDC
		if o.Enabled == nil {
			// Schema default: the section being present means enabled.
			t := true
			o.Enabled = &t
		}
		if o.Issuer == "" {
			return fmt.Errorf("config: oidc.issuer is required")
		}
		if _, err := url.ParseRequestURI(o.Issuer); err != nil {
			return fmt.Errorf("config: oidc.issuer %q is not a valid URL", o.Issuer)
		}
		if o.ClientID == "" {
			return fmt.Errorf("config: oidc.client_id is required")
		}
		if o.ClientSecret == "" {
			return fmt.Errorf("config: oidc.client_secret is required")
		}
		if o.Scope == "" {
			o.Scope = "openid email profile"
		}
		if o.ProfilePictureSource == "" {
			o.ProfilePictureSource = "oidc"
		}
		// Normalize subject_claims like the TS normalizeStringArray pipe.
		o.SubjectClaims = normalizeStringArray(o.SubjectClaims)
	}

	if cfg.RDPGateway != nil {
		g := cfg.RDPGateway
		if g.WebhookURL == "" {
			return fmt.Errorf("config: rdp_gateway.webhook_url is required")
		}
		if _, err := url.ParseRequestURI(g.WebhookURL); err != nil {
			return fmt.Errorf("config: rdp_gateway.webhook_url %q is not a valid URL", g.WebhookURL)
		}
	}

	return nil
}

// normalizeStringArray mirrors normalizeStringArray in config-schema.ts:
// trim, drop empties, de-duplicate preserving order.
func normalizeStringArray(values []string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, v := range values {
		t := strings.TrimSpace(v)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}
