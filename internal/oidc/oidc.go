// Package oidc is the Go port of app/server/oidc/provider.ts.
//
// It replicates the hand-rolled TypeScript OIDC provider quirk-for-quirk:
//   - use_pkce defaults to false (schema default, applied by the config loader)
//   - token endpoint auth tries client_secret_post first, then falls back to
//     client_secret_basic, caching whichever method works
//   - 60 s clock tolerance on ID token verification
//   - configurable subject-claim fallback order ("sub" first)
//   - gravatar picture formula identical to the TS implementation
//   - unsigned __oidc_state cookie semantics (handled by the HTTP layer)
//
// Like the TS provider, JWKS verification uses a caching remote key set
// (go-oidc's RemoteKeySet, the Go equivalent of jose's createRemoteJWKSet),
// while the weak-RSA compatibility path fetches the JWKS document directly
// with its own 60 s cache. golang.org/x/oauth2 is deliberately not used for
// the token exchange: its Exchange cannot express the post→basic fallback
// order or the method caching, so the exchange is hand-rolled exactly like
// the TS fetchToken.
package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	oidcv3 "github.com/coreos/go-oidc/v3/oidc"
)

// Error codes mirror the TS OidcErrorCode union (only the codes the provider
// actually produces).
const (
	CodeDiscoveryFailed  = "discovery_failed"
	CodeMissingEndpoints = "missing_endpoints"
	CodeStateMismatch    = "state_mismatch"
	CodeNonceMismatch    = "nonce_mismatch"
	CodeTokenExchange    = "token_exchange_failed"
	CodeInvalidClient    = "invalid_client"
	CodePKCEError        = "pkce_error"
	CodeInvalidIDToken   = "invalid_id_token"
	CodeMissingSub       = "missing_sub"
)

// Error mirrors the TS OidcError.
type Error struct {
	Code    string
	Message string
	Hint    string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func oidcErr(code, message, hint string) *Error {
	return &Error{Code: code, Message: message, Hint: hint}
}

// Auth methods for the token endpoint.
const (
	AuthMethodAuto  = ""
	AuthMethodBasic = "client_secret_basic"
	AuthMethodPost  = "client_secret_post"
)

// clockToleranceSeconds mirrors jose's clockTolerance: 60 in the TS provider.
const clockToleranceSeconds = 60

// httpTimeout mirrors the TS AbortSignal.timeout(10_000) on every OIDC fetch.
const httpTimeout = 10 * time.Second

// Config mirrors the TS OidcConfig. UsePKCE carries the schema default
// (false); Scope and ProfilePictureSource are defaulted by the config
// loader before the service is built.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	BaseURL      string
	Basename     string

	AuthorizationEndpoint string
	TokenEndpoint         string
	UserinfoEndpoint      string
	EndSessionEndpoint    string
	JWKsURI               string

	// TokenEndpointAuthMethod is "", "client_secret_basic" or
	// "client_secret_post". "" (and the TS-mapped "client_secret_jwt")
	// means auto-detect with the post→basic fallback.
	TokenEndpointAuthMethod string

	UsePKCE               bool
	Scope                 string
	SubjectClaims         []string
	AllowWeakRSAKeys      bool
	ExtraParams           map[string]string
	ProfilePictureSource  string // "oidc" | "gravatar"
	PostLogoutRedirectURI string
}

// ResolvedEndpoints mirrors the TS ResolvedEndpoints.
type ResolvedEndpoints struct {
	AuthorizationEndpoint string
	TokenEndpoint         string
	JWKsURI               string
	UserinfoEndpoint      string
	EndSessionEndpoint    string
}

// FlowState mirrors the TS OidcFlowState.
type FlowState struct {
	State        string
	Nonce        string
	CodeVerifier string
	RedirectURI  string
}

// Identity mirrors the TS OidcIdentity.
type Identity struct {
	Issuer   string
	Subject  string
	Name     string
	Username string
	Email    string // "" when absent
	Picture  string // "" when absent
	IDToken  string // "" when absent
}

// Service mirrors the TS OidcService. All mutable state is mutex-guarded;
// discovery is lazy, exactly like the TS provider.
type Service struct {
	cfg    Config
	http   *http.Client
	logger *slog.Logger

	mu               sync.Mutex
	endpoints        *ResolvedEndpoints
	lastErr          *Error
	keySet           *oidcKeySet
	resolvedMethod   string // "", "client_secret_basic", "client_secret_post"
	weakCache        map[string]weakCacheEntry
	warnedWeakMode   bool
	warnedWeakIssuer bool
}

type weakCacheEntry struct {
	expiresAt time.Time
	keys      []weakJWK
}

type weakJWK struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// oidcKeySet wraps go-oidc's RemoteKeySet (the Go equivalent of jose's
// createRemoteJWKSet: caching remote JWKS with rotation on unknown kids).
type oidcKeySet struct {
	rs *oidcv3.RemoteKeySet
}

// NewService builds the service. A nil logger discards logs.
func NewService(cfg Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Scope == "" {
		cfg.Scope = "openid email profile"
	}
	if cfg.ProfilePictureSource == "" {
		cfg.ProfilePictureSource = "oidc"
	}
	return &Service{
		cfg:       cfg,
		http:      &http.Client{Timeout: httpTimeout},
		logger:    logger.With("component", "oidc"),
		weakCache: map[string]weakCacheEntry{},
	}
}

// Status mirrors the TS status().
func (s *Service) Status() (state string, endpoints *ResolvedEndpoints, err *Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastErr != nil {
		return "error", nil, s.lastErr
	}
	if s.endpoints != nil {
		return "ready", s.endpoints, nil
	}
	return "pending", nil, nil
}

// Invalidate clears cached discovery state, mirroring the TS invalidate().
func (s *Service) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endpoints = nil
	s.lastErr = nil
	s.keySet = nil
	s.resolvedMethod = s.cfg.TokenEndpointAuthMethod
	if s.resolvedMethod == "client_secret_jwt" {
		s.resolvedMethod = AuthMethodAuto
	}
}

// Reload replaces the config and invalidates, mirroring the TS reload().
func (s *Service) Reload(cfg Config) {
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	if !cfg.AllowWeakRSAKeys {
		// maybeWarnWeakRsaMode is TS-side; the Go server logs at wiring time.
	}
	s.Invalidate()
}

// Discover resolves the OIDC endpoints, mirroring the TS discover()
// including the manual-endpoint short-circuit and the discovery URL quirk.
func (s *Service) Discover(ctx context.Context) (*ResolvedEndpoints, *Error) {
	s.mu.Lock()
	if s.endpoints != nil {
		ep := s.endpoints
		s.mu.Unlock()
		return ep, nil
	}
	s.mu.Unlock()

	if s.cfg.AuthorizationEndpoint != "" && s.cfg.TokenEndpoint != "" && s.cfg.JWKsURI != "" {
		ep := &ResolvedEndpoints{
			AuthorizationEndpoint: s.cfg.AuthorizationEndpoint,
			TokenEndpoint:         s.cfg.TokenEndpoint,
			JWKsURI:               s.cfg.JWKsURI,
			UserinfoEndpoint:      s.cfg.UserinfoEndpoint,
			EndSessionEndpoint:    s.cfg.EndSessionEndpoint,
		}
		s.mu.Lock()
		s.endpoints = ep
		s.lastErr = nil
		s.keySet = &oidcKeySet{rs: oidcv3.NewRemoteKeySet(oidcv3.ClientContext(ctx, s.http), ep.JWKsURI)}
		s.mu.Unlock()
		s.logger.Debug("OIDC endpoints configured manually, skipping discovery")
		return ep, nil
	}

	discoveryURL, err := discoveryURL(s.cfg.Issuer)
	if err != nil {
		return nil, s.fail(oidcErr(CodeDiscoveryFailed, fmt.Sprintf("Invalid issuer URL: %s", s.cfg.Issuer), ""))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return nil, s.fail(oidcErr(CodeDiscoveryFailed, fmt.Sprintf("Invalid issuer URL: %s", s.cfg.Issuer), ""))
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, s.fail(oidcErr(CodeDiscoveryFailed,
			fmt.Sprintf("Failed to reach OIDC discovery endpoint: %s", err),
			"Unable to reach your identity provider. SSO will automatically retry on the next login attempt."))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, s.fail(oidcErr(CodeDiscoveryFailed,
			fmt.Sprintf("Discovery endpoint returned %d: %s", resp.StatusCode, discoveryURL),
			"Check that your issuer URL is correct and that the identity provider is online."))
	}
	var metadata struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
		JWKsURI               string `json:"jwks_uri"`
		UserinfoEndpoint      string `json:"userinfo_endpoint"`
		EndSessionEndpoint    string `json:"end_session_endpoint"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil || json.Unmarshal(body, &metadata) != nil {
		return nil, s.fail(oidcErr(CodeDiscoveryFailed,
			fmt.Sprintf("Failed to parse OIDC discovery document: %s", discoveryURL),
			"Check that your issuer URL is correct and that the identity provider is online."))
	}

	if metadata.Issuer != "" && metadata.Issuer != s.cfg.Issuer {
		s.logger.Debug("Discovery issuer does not match configured issuer",
			"discovery", metadata.Issuer, "configured", s.cfg.Issuer)
	}

	ep := &ResolvedEndpoints{
		AuthorizationEndpoint: firstNonEmpty(s.cfg.AuthorizationEndpoint, metadata.AuthorizationEndpoint),
		TokenEndpoint:         firstNonEmpty(s.cfg.TokenEndpoint, metadata.TokenEndpoint),
		JWKsURI:               firstNonEmpty(s.cfg.JWKsURI, metadata.JWKsURI),
		UserinfoEndpoint:      firstNonEmpty(s.cfg.UserinfoEndpoint, metadata.UserinfoEndpoint),
		EndSessionEndpoint:    firstNonEmpty(s.cfg.EndSessionEndpoint, metadata.EndSessionEndpoint),
	}
	var missing []string
	if ep.AuthorizationEndpoint == "" {
		missing = append(missing, "authorization_endpoint")
	}
	if ep.TokenEndpoint == "" {
		missing = append(missing, "token_endpoint")
	}
	if ep.JWKsURI == "" {
		missing = append(missing, "jwks_uri")
	}
	if len(missing) > 0 {
		return nil, s.fail(oidcErr(CodeMissingEndpoints,
			fmt.Sprintf("Discovery is missing required endpoints: %s", strings.Join(missing, ", ")),
			"Your identity provider did not return all required endpoints. You can set them manually in your Headplane config."))
	}

	s.mu.Lock()
	s.endpoints = ep
	s.lastErr = nil
	s.keySet = &oidcKeySet{rs: oidcv3.NewRemoteKeySet(oidcv3.ClientContext(ctx, s.http), ep.JWKsURI)}
	s.mu.Unlock()
	s.logger.Debug("OIDC discovery completed successfully")
	return ep, nil
}

func (s *Service) fail(err *Error) *Error {
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
	return err
}

// discoveryURL mirrors the TS discovery URL construction quirk.
func discoveryURL(issuer string) (string, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid issuer")
	}
	p := strings.TrimSuffix(u.Path, "/")
	if p == "" {
		u.Path = "/.well-known/openid-configuration"
	} else {
		u.Path = p + "/.well-known/openid-configuration"
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
