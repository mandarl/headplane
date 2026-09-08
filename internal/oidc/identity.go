package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// subjectClaimOrder mirrors the TS getSubjectClaimOrder: "sub" first, then
// the normalized configured claims (deduped, "sub" filtered out).
func (s *Service) subjectClaimOrder() []string {
	seen := map[string]struct{}{"sub": {}}
	order := []string{"sub"}
	for _, claim := range s.cfg.SubjectClaims {
		t := strings.TrimSpace(claim)
		if t == "" || t == "sub" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		order = append(order, t)
	}
	return order
}

// readClaimAsString mirrors the TS readClaimAsString: only non-empty trimmed
// strings count.
func readClaimAsString(claims map[string]any, name string) string {
	v, _ := claims[name].(string)
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return strings.TrimSpace(v)
}

func resolveSubject(claims map[string]any, order []string) string {
	for _, claim := range order {
		if v := readClaimAsString(claims, claim); v != "" {
			return v
		}
	}
	return ""
}

// enrichWithUserinfo mirrors the TS enrichWithUserInfo: when the ID token
// lacks a name/email/picture (but has a subject) or lacks a subject
// entirely, and a userinfo endpoint is known, merge the userinfo response
// in — ID token claims win. Failures are non-fatal, exactly like the TS.
func (s *Service) enrichWithUserinfo(ctx context.Context, ep *ResolvedEndpoints, accessToken string, claims map[string]any) map[string]any {
	order := s.subjectClaimOrder()
	needsEnrichment := readClaimAsString(claims, "name") == "" &&
		readClaimAsString(claims, "email") == "" &&
		readClaimAsString(claims, "picture") == "" &&
		resolveSubject(claims, order) != ""
	needsSubject := resolveSubject(claims, order) == ""
	if (!needsEnrichment && !needsSubject) || ep.UserinfoEndpoint == "" {
		return claims
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.UserinfoEndpoint, nil)
	if err != nil {
		s.logger.Debug("UserInfo request build failed (non-fatal)", "error", err)
		return claims
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		s.logger.Debug("UserInfo fetch failed (non-fatal)", "error", err)
		return claims
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.logger.Debug("UserInfo endpoint returned non-OK, skipping enrichment", "status", resp.StatusCode)
		return claims
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		s.logger.Debug("UserInfo read failed (non-fatal)", "error", err)
		return claims
	}
	var userinfo map[string]any
	if json.Unmarshal(body, &userinfo) != nil {
		s.logger.Debug("UserInfo returned non-JSON, skipping enrichment")
		return claims
	}

	merged := make(map[string]any, len(claims)+8)
	for k, v := range claims {
		merged[k] = v
	}
	for _, claim := range order {
		if claim == "sub" {
			continue
		}
		if v := readClaimAsString(claims, claim); v != "" {
			merged[claim] = v
		} else if v := readClaimAsString(userinfo, claim); v != "" {
			merged[claim] = v
		}
	}
	for _, field := range []string{"name", "given_name", "family_name", "preferred_username", "email", "picture"} {
		if _, ok := merged[field]; !ok {
			if v, ok := userinfo[field]; ok {
				merged[field] = v
			}
		}
	}
	if _, ok := merged["sub"]; !ok {
		if v := readClaimAsString(userinfo, "sub"); v != "" {
			merged["sub"] = v
		}
	}
	return merged
}

// buildIdentity mirrors the TS buildIdentity, including the exact gravatar
// URL formula: sha256hex(trim(lower(email))) with ?s=200&d=identicon&r=x.
func buildIdentity(cfg Config, claims map[string]any, idToken string) *Identity {
	subject := resolveSubject(claims, (&Service{cfg: cfg}).subjectClaimOrder())

	name, _ := claims["name"].(string)
	if name == "" {
		given, _ := claims["given_name"].(string)
		family, _ := claims["family_name"].(string)
		preferred, _ := claims["preferred_username"].(string)
		switch {
		case given != "" && family != "":
			name = given + " " + family
		case preferred != "":
			name = preferred
		default:
			name = "SSO User"
		}
	}

	username, _ := claims["preferred_username"].(string)
	if username == "" {
		if email, _ := claims["email"].(string); email != "" {
			username = strings.Split(email, "@")[0]
		} else {
			username = "user"
		}
	}

	email, _ := claims["email"].(string)
	picture, _ := claims["picture"].(string)
	if cfg.ProfilePictureSource == "gravatar" {
		picture = ""
		if email != "" {
			sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
			picture = "https://www.gravatar.com/avatar/" + hex.EncodeToString(sum[:]) + "?s=200&d=identicon&r=x"
		}
	}

	return &Identity{
		Issuer:   cfg.Issuer,
		Subject:  subject,
		Name:     name,
		Username: username,
		Email:    email,
		Picture:  picture,
		IDToken:  idToken,
	}
}

// BuildEndSessionURL mirrors the TS buildEndSessionUrl. It ensures discovery
// has run (like the TS logout action does) and returns "" when the provider
// does not advertise an end-session endpoint.
func (s *Service) BuildEndSessionURL(ctx context.Context, idToken string) string {
	ep, oerr := s.Discover(ctx)
	if oerr != nil || ep.EndSessionEndpoint == "" {
		return ""
	}
	params := url.Values{}
	if idToken != "" {
		params.Set("id_token_hint", idToken)
	}
	params.Set("client_id", s.cfg.ClientID)
	postLogout := s.cfg.PostLogoutRedirectURI
	if postLogout == "" {
		base, err := url.Parse(s.cfg.BaseURL)
		if err == nil && base.Scheme != "" && base.Host != "" {
			if ref, err := url.Parse(s.cfg.Basename + "/login?s=logout"); err == nil {
				postLogout = base.ResolveReference(ref).String()
			}
		}
		if postLogout == "" {
			postLogout = s.cfg.Basename + "/login?s=logout"
		}
	}
	params.Set("post_logout_redirect_uri", postLogout)

	// The TS builds `${endSessionEndpoint}?${params}` unconditionally.
	return ep.EndSessionEndpoint + "?" + params.Encode()
}

// WarnWeakRSAMode logs the TS maybeWarnWeakRsaMode warning; called once at
// server wiring when the config enables the compatibility mode.
func (s *Service) WarnWeakRSAMode() {
	if !s.cfg.AllowWeakRSAKeys {
		return
	}
	s.mu.Lock()
	warned := s.warnedWeakMode
	s.warnedWeakMode = true
	s.mu.Unlock()
	if !warned {
		s.logger.Warn("OIDC weak RSA compatibility mode is enabled. This lowers token verification security and should only be used as a temporary workaround.",
			"issuer", s.cfg.Issuer)
	}
}
