package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tale/headplane/internal/auth"
	"github.com/tale/headplane/internal/oidc"
	"github.com/tale/headplane/internal/serverconfig"
)

// oidcState mirrors the TS OidcStateCookie. JSON field order (state, nonce,
// verifier, redirect_uri) matches the object the TS start route serializes,
// so a state cookie written by either server reads back identically.
type oidcState struct {
	State       string `json:"state"`
	Nonce       string `json:"nonce"`
	Verifier    string `json:"verifier"`
	RedirectURI string `json:"redirect_uri"`
}

func (st oidcState) valid() bool {
	return st.State != "" && st.Nonce != "" && st.Verifier != "" && st.RedirectURI != ""
}

// encodeOidcState mirrors createOidcStateCookie().serialize: standard-base64
// of the JSON object, then encodeURIComponent (React Router does the same
// when serializing cookie values).
func encodeOidcState(st oidcState) string {
	raw, _ := json.Marshal(st)
	return auth.EncodeURIComponent(base64.StdEncoding.EncodeToString(raw))
}

// parseOidcStateCookie mirrors createOidcStateCookie().parse plus the
// route's field check: it reports whether a parseable cookie was present.
// decodeURIComponent semantics are used for unescaping (a raw "+" survives,
// unlike url.QueryUnescape).
func parseOidcStateCookie(r *http.Request) (*oidcState, bool) {
	raw := auth.CookieValue(r.Header.Get("Cookie"), "__oidc_state")
	if raw == "" {
		return nil, false
	}
	unescaped, err := url.PathUnescape(raw)
	if err != nil {
		return nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(unescaped)
	if err != nil {
		return nil, false
	}
	var st oidcState
	if err := json.Unmarshal(decoded, &st); err != nil {
		return nil, false
	}
	return &st, true
}

// setOidcStateCookie writes the __oidc_state cookie with the exact
// attributes the TS createCookie call sets: HttpOnly, 1800 s max age,
// callback path, Lax, and the configured secure/domain flags.
func (s *Server) setOidcStateCookie(w http.ResponseWriter, st oidcState) {
	var sb strings.Builder
	sb.WriteString("__oidc_state=")
	sb.WriteString(encodeOidcState(st))
	sb.WriteString("; Path=")
	sb.WriteString(s.basename + "/oidc/callback")
	sb.WriteString("; Max-Age=1800")
	sb.WriteString("; HttpOnly")
	sb.WriteString("; SameSite=Lax")
	if s.cfg.Server.CookieSecure {
		sb.WriteString("; Secure")
	}
	if s.cfg.Server.CookieDomain != "" {
		sb.WriteString("; Domain=")
		sb.WriteString(s.cfg.Server.CookieDomain)
	}
	w.Header().Set("Set-Cookie", sb.String())
}

// newOidcService mirrors buildOidc in app/server/context.ts: it returns the
// service, or nil plus the TS disabled reason ("OIDC is unavailable:
// <reason>", HTTP 501 in the routes).
func newOidcService(cfg *serverconfig.Config, basename string, logger *slog.Logger) (*oidc.Service, string) {
	if cfg.OIDC == nil {
		return nil, "OIDC is not configured"
	}
	if !cfg.OIDC.IsEnabled() {
		return nil, "OIDC is disabled in the configuration"
	}
	apiKey := cfg.Headscale.APIKey
	if apiKey == "" {
		apiKey = cfg.OIDC.HeadscaleAPIKey // deprecated fallback, mirrors context.ts
	}
	if apiKey == "" {
		return nil, "OIDC requires headscale.api_key to be configured"
	}
	o := cfg.OIDC
	svc := oidc.NewService(oidc.Config{
		Issuer:       o.Issuer,
		ClientID:     o.ClientID,
		ClientSecret: o.ClientSecret,
		// baseUrl is server.base_url in the TS context, not an OIDC field.
		BaseURL:                 cfg.Server.BaseURL,
		Basename:                basename,
		AuthorizationEndpoint:   o.AuthorizationEndpoint,
		TokenEndpoint:           o.TokenEndpoint,
		UserinfoEndpoint:        o.UserinfoEndpoint,
		EndSessionEndpoint:      o.EndSessionEndpoint,
		JWKsURI:                 o.JWKsURI,
		TokenEndpointAuthMethod: o.TokenEndpointAuthMethod,
		UsePKCE:                 o.UsePKCE,
		Scope:                   o.Scope,
		SubjectClaims:           o.SubjectClaims,
		AllowWeakRSAKeys:        o.AllowWeakRSAKeys,
		ExtraParams:             o.ExtraParams,
		ProfilePictureSource:    o.ProfilePictureSource,
		PostLogoutRedirectURI:   o.PostLogoutRedirectURI,
	}, logger)
	// Mirrors maybeWarnWeakRsaMode at service creation.
	svc.WarnWeakRSAMode()
	return svc, ""
}

// handleOidcStart serves GET {basename}/oidc/start, mirroring
// app/routes/auth/oidc-start.ts: the authenticated check comes before the
// OIDC-enabled gate, exactly like the TS loader.
func (s *Server) handleOidcStart(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authSvc.Require(r); err == nil {
		// Already authenticated → app root. The TS redirects to "/",
		// resolved under the basename per the Phase 2 convention.
		w.Header().Set("Location", s.basename+"/")
		w.WriteHeader(http.StatusFound)
		return
	}
	if s.oidcSvc == nil {
		http.Error(w, "OIDC is unavailable: "+s.oidcDisabledReason, http.StatusNotImplemented)
		return
	}
	authURL, flow, err := s.oidcSvc.StartFlow(r.Context())
	if err != nil {
		var oerr *oidc.Error
		if errors.As(err, &oerr) {
			w.Header().Set("Location", s.basename+"/login?s="+oerr.Code)
			w.WriteHeader(http.StatusFound)
			return
		}
		// The TS equivalent is an uncaught throw → 500.
		s.logger.Error("OIDC start failed", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	s.setOidcStateCookie(w, oidcState{
		State:       flow.State,
		Nonce:       flow.Nonce,
		Verifier:    flow.CodeVerifier,
		RedirectURI: flow.RedirectURI,
	})
	w.Header().Set("Location", authURL)
	w.WriteHeader(http.StatusFound)
}

// handleOidcCallback serves GET {basename}/oidc/callback, mirroring
// app/routes/auth/oidc-callback.ts.
func (s *Server) handleOidcCallback(w http.ResponseWriter, r *http.Request) {
	if s.oidcSvc == nil {
		http.Error(w, "OIDC is unavailable: "+s.oidcDisabledReason, http.StatusNotImplemented)
		return
	}
	loginWith := func(code string) {
		w.Header().Set("Location", s.basename+"/login?s="+code)
		w.WriteHeader(http.StatusFound)
	}

	query := r.URL.Query()
	if len(query) == 0 {
		loginWith("error_no_query")
		return
	}
	st, ok := parseOidcStateCookie(r)
	if !ok {
		s.logger.Warn("Called OIDC callback without session cookie", "component", "auth")
		loginWith("error_no_session")
		return
	}
	if !st.valid() {
		s.logger.Warn("OIDC session cookie is missing required fields", "component", "auth")
		loginWith("error_invalid_session")
		return
	}

	identity, oerr := s.oidcSvc.HandleCallback(r.Context(), query, &oidc.FlowState{
		State:        st.State,
		Nonce:        st.Nonce,
		CodeVerifier: st.Verifier,
		RedirectURI:  st.RedirectURI,
	})
	if oerr != nil {
		s.logger.Error("OIDC callback failed", "component", "auth", "code", oerr.Code, "error", oerr.Message)
		if oerr.Hint != "" {
			s.logger.Error("OIDC callback hint", "component", "auth", "hint", oerr.Hint)
		}
		loginWith("error_auth_failed")
		return
	}

	userID, err := s.authSvc.FindOrCreateUser(identity.Subject, orNil(identity.Name), orNil(identity.Email), orNil(identity.Picture))
	if err != nil {
		s.logger.Error("OIDC failed to create user", "component", "auth", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Best-effort Headscale user link, mirroring the TS try/catch.
	s.linkHeadscaleUser(r.Context(), userID, identity.Subject, identity.Email)

	// Only persist the id_token when RP-initiated logout is enabled —
	// otherwise we'd be storing a credential we never use.
	var idToken *string
	if s.cfg.OIDC != nil && s.cfg.OIDC.UseEndSession && identity.IDToken != "" {
		idToken = &identity.IDToken
	}
	profile := auth.CookieProfile{Name: identity.Name, Username: &identity.Username}
	if identity.Email != "" {
		profile.Email = &identity.Email
	}
	setCookie, err := s.authSvc.CreateOidcSession(userID, profile, idToken, time.Duration(s.cfg.Server.CookieMaxAge)*time.Second)
	if err != nil {
		s.logger.Error("OIDC failed to create session", "component", "auth", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Set-Cookie", setCookie)
	// The TS redirects to "/", resolved under the basename per the Phase 2
	// convention.
	w.Header().Set("Location", s.basename+"/")
	w.WriteHeader(http.StatusFound)
}

func orNil(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// headscaleUser mirrors the fields of ~/types/User used by
// findHeadscaleUserBySubject.
type headscaleUser struct {
	ID         string `json:"id"`
	Email      string `json:"email"`
	Provider   string `json:"provider"`
	ProviderID string `json:"providerId"`
}

// oidcSubjectFromProviderID mirrors getOidcSubject in
// app/server/web/headscale-identity.ts: Headscale stores providerId as a URL
// whose last path segment is the subject.
func oidcSubjectFromProviderID(provider, providerID string) string {
	if provider != "oidc" || providerID == "" {
		return ""
	}
	segment := providerID
	if i := strings.LastIndex(segment, "/"); i >= 0 {
		segment = segment[i+1:]
	}
	if unescaped, err := url.PathUnescape(segment); err == nil {
		return unescaped
	}
	return segment
}

// findHeadscaleUserBySubject mirrors findHeadscaleUserBySubject: subject
// match first (providerId last segment), then email fallback.
func findHeadscaleUserBySubject(users []headscaleUser, subject, email string) *headscaleUser {
	for i := range users {
		if s := oidcSubjectFromProviderID(users[i].Provider, users[i].ProviderID); s != "" && s == subject {
			return &users[i]
		}
	}
	if email == "" {
		return nil
	}
	for i := range users {
		if users[i].Email == email {
			return &users[i]
		}
	}
	return nil
}

// linkHeadscaleUser is the Phase 3 stand-in for the callback's Headscale
// linking step: it lists users via the configured admin API key (the same
// GET v1/user the TS hsApi.users.list() issues) and records the link.
// Failures are non-fatal, mirroring the TS try/catch. Phase 4 replaces this
// with the full Headscale API layer.
func (s *Server) linkHeadscaleUser(ctx context.Context, userID, subject, email string) {
	apiKey := s.cfg.Headscale.APIKey
	if apiKey == "" && s.cfg.OIDC != nil {
		apiKey = s.cfg.OIDC.HeadscaleAPIKey
	}
	base := strings.TrimSuffix(s.cfg.Headscale.URL, "/")
	if apiKey == "" || base == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/user", nil)
	if err != nil {
		s.logger.Warn("Failed to link Headscale user", "component", "auth", "error", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		s.logger.Warn("Failed to link Headscale user", "component", "auth", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.logger.Warn("Failed to link Headscale user", "component", "auth", "status", resp.StatusCode)
		return
	}
	var body struct {
		Users []headscaleUser `json:"users"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		s.logger.Warn("Failed to link Headscale user", "component", "auth", "error", err)
		return
	}
	hsUser := findHeadscaleUserBySubject(body.Users, subject, email)
	if hsUser == nil {
		return
	}
	ok, err := s.authSvc.LinkHeadscaleUser(userID, hsUser.ID)
	if err != nil {
		s.logger.Warn("Failed to link Headscale user", "component", "auth", "error", err)
	} else if !ok {
		s.logger.Warn("Headscale user is already linked to a different Headplane user",
			"component", "auth", "headscale_user_id", hsUser.ID)
	}
}
