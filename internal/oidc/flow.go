package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/tale/headplane/internal/auth"
)

// randomString mirrors the TS generateRandom: base64url(crypto-random bytes).
func randomString(nbytes int) (string, error) {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func s256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// redirectURI mirrors `new URL(`${__PREFIX__}/oidc/callback`, config.baseUrl)`
// — an absolute path resolved against server.base_url. Like the TS code,
// which throws when base_url is unset, this returns an error in that case.
func (s *Service) redirectURI() (string, error) {
	if s.cfg.BaseURL == "" {
		return "", fmt.Errorf("server.base_url must be set to use OIDC login")
	}
	base, err := url.Parse(s.cfg.BaseURL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("server.base_url %q is not a valid URL", s.cfg.BaseURL)
	}
	ref, err := url.Parse(s.cfg.Basename + "/oidc/callback")
	if err != nil {
		return "", err
	}
	return base.ResolveReference(ref).String(), nil
}

// StartFlow mirrors the TS startFlow: it builds the authorization URL with
// the exact same parameters and returns the flow state for the state cookie.
//
// OIDC failures come back as *Error (→ redirect to /login?s=<code>); a
// missing/invalid server.base_url comes back as a plain error. The TS
// equivalent is an uncaught TypeError from `new URL(...)`, which React
// Router turns into a 500, so the handler maps plain errors to 500 too.
func (s *Service) StartFlow(ctx context.Context) (string, *FlowState, error) {
	ep, oerr := s.Discover(ctx)
	if oerr != nil {
		return "", nil, oerr
	}

	redirectURI, err := s.redirectURI()
	if err != nil {
		return "", nil, err
	}

	state, err := randomString(32)
	if err != nil {
		return "", nil, oidcErr(CodeDiscoveryFailed, fmt.Sprintf("Failed to generate OIDC state: %s", err), "")
	}
	nonce, err := randomString(32)
	if err != nil {
		return "", nil, oidcErr(CodeDiscoveryFailed, fmt.Sprintf("Failed to generate OIDC nonce: %s", err), "")
	}
	verifier, err := randomString(64)
	if err != nil {
		return "", nil, oidcErr(CodeDiscoveryFailed, fmt.Sprintf("Failed to generate PKCE verifier: %s", err), "")
	}

	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", s.cfg.ClientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", s.cfg.Scope)
	params.Set("state", state)
	params.Set("nonce", nonce)
	if s.cfg.UsePKCE {
		params.Set("code_challenge", s256Challenge(verifier))
		params.Set("code_challenge_method", "S256")
	}
	for k, v := range s.cfg.ExtraParams {
		params.Set(k, v)
	}

	// The TS builds `${authorizationEndpoint}?${params}` unconditionally.
	flow := &FlowState{State: state, Nonce: nonce, CodeVerifier: verifier, RedirectURI: redirectURI}
	return ep.AuthorizationEndpoint + "?" + params.Encode(), flow, nil
}

// HandleCallback mirrors the TS handleCallback.
func (s *Service) HandleCallback(ctx context.Context, query url.Values, flow *FlowState) (*Identity, *Error) {
	ep, oerr := s.Discover(ctx)
	if oerr != nil {
		return nil, oerr
	}

	if errParam := query.Get("error"); errParam != "" {
		desc := query.Get("error_description")
		msg := fmt.Sprintf("Provider returned error: %s — %s", errParam, desc)
		var hint string
		if desc != "" {
			hint = desc
		}
		return nil, oidcErr(CodeTokenExchange, msg, hint)
	}

	code := query.Get("code")
	if code == "" {
		return nil, oidcErr(CodeTokenExchange, "Callback is missing the authorization code", "")
	}

	if query.Get("state") != flow.State {
		return nil, oidcErr(CodeStateMismatch,
			fmt.Sprintf("State mismatch: expected %s, got %s", flow.State, query.Get("state")),
			"Please try signing in again. If this keeps happening, your reverse proxy may be interfering with cookies.")
	}

	tokens, oerr := s.exchangeCode(ctx, ep, code, flow)
	if oerr != nil {
		return nil, oerr
	}
	if tokens.IDToken == "" {
		return nil, oidcErr(CodeTokenExchange,
			"Token response is missing id_token",
			"Your identity provider did not return an ID token. Make sure the 'openid' scope is included in your OIDC client configuration.")
	}

	claims, oerr := s.verifyIDToken(ctx, tokens.IDToken, flow.Nonce)
	if oerr != nil {
		return nil, oerr
	}
	enriched := s.enrichWithUserinfo(ctx, ep, tokens.AccessToken, claims)

	if resolveSubject(enriched, s.subjectClaimOrder()) == "" {
		return nil, oidcErr(CodeMissingSub,
			"ID token and userinfo response are missing all configured subject claims",
			fmt.Sprintf("Your identity provider did not return a stable user identifier. Configure oidc.subject_claims or ensure one of these claims is present: %s.", strings.Join(s.subjectClaimOrder(), ", ")))
	}

	return buildIdentity(s.cfg, enriched, tokens.IDToken), nil
}

type tokenResponse struct {
	AccessToken  string
	IDToken      string
	TokenType    string
	ExpiresIn    int64
	RefreshToken string
}

// exchangeCode mirrors the TS exchangeCode: try the resolved (or default
// post) auth method, and on invalid_client fall back to the other method,
// caching whichever succeeds.
func (s *Service) exchangeCode(ctx context.Context, ep *ResolvedEndpoints, code string, flow *FlowState) (*tokenResponse, *Error) {
	body := url.Values{}
	body.Set("grant_type", "authorization_code")
	body.Set("code", code)
	body.Set("redirect_uri", flow.RedirectURI)
	if s.cfg.UsePKCE {
		body.Set("code_verifier", flow.CodeVerifier)
	}

	s.mu.Lock()
	method := s.resolvedMethod
	auto := method == AuthMethodAuto
	if method == AuthMethodAuto {
		method = AuthMethodPost
	}
	s.mu.Unlock()

	result, oerr := s.fetchToken(ctx, ep.TokenEndpoint, body, method)
	if oerr != nil && auto && isInvalidClient(oerr) {
		fallback := AuthMethodBasic
		if method == AuthMethodBasic {
			fallback = AuthMethodPost
		}
		s.logger.Debug("Token exchange failed with "+method+", retrying with "+fallback,
			"method", method, "fallback", fallback)
		retry, rerr := s.fetchToken(ctx, ep.TokenEndpoint, body, fallback)
		if rerr == nil {
			s.mu.Lock()
			s.resolvedMethod = fallback
			s.mu.Unlock()
			s.logger.Debug("Auth method "+fallback+" succeeded, caching for future requests", "method", fallback)
		}
		return retry, rerr
	}
	if oerr == nil && auto {
		s.mu.Lock()
		s.resolvedMethod = method
		s.mu.Unlock()
	}
	return result, oerr
}

func isInvalidClient(err *Error) bool {
	return err.Code == CodeInvalidClient ||
		(err.Code == CodeTokenExchange && strings.Contains(err.Message, "invalid_client"))
}

// fetchToken mirrors the TS fetchToken.
func (s *Service) fetchToken(ctx context.Context, tokenEndpoint string, body url.Values, method string) (*tokenResponse, *Error) {
	// The body is mutated for client_secret_post; copy so the fallback retry
	// starts from the pristine body, exactly like the TS (which reuses the
	// same URLSearchParams — but body.set is idempotent there; copying is
	// equivalent and safer).
	form := url.Values{}
	for k, vs := range body {
		form[k] = append([]string(nil), vs...)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, oidcErr(CodeTokenExchange, fmt.Sprintf("Failed to reach token endpoint: %s", err), "")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	if method == AuthMethodPost {
		form.Set("client_id", s.cfg.ClientID)
		form.Set("client_secret", s.cfg.ClientSecret)
		// Re-encode with the credentials included (mirrors body.set before
		// the TS fetch).
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, oidcErr(CodeTokenExchange, fmt.Sprintf("Failed to reach token endpoint: %s", err), "")
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
	} else {
		// Mirrors btoa(`${encodeURIComponent(clientId)}:${encodeURIComponent(clientSecret)}`).
		creds := auth.EncodeURIComponent(s.cfg.ClientID) + ":" + auth.EncodeURIComponent(s.cfg.ClientSecret)
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(creds)))
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, oidcErr(CodeTokenExchange, fmt.Sprintf("Failed to reach token endpoint: %s", err), "")
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, oidcErr(CodeTokenExchange, fmt.Sprintf("Failed to read token endpoint response: %s", err), "")
	}
	var parsed map[string]any
	if json.Unmarshal(respBody, &parsed) != nil {
		return nil, oidcErr(CodeTokenExchange,
			fmt.Sprintf("Token endpoint returned non-JSON response (status %d)", resp.StatusCode), "")
	}

	if errStr, _ := parsed["error"].(string); resp.StatusCode < 200 || resp.StatusCode >= 300 || errStr != "" {
		desc, _ := parsed["error_description"].(string)
		if errStr == "invalid_client" {
			return nil, oidcErr(CodeInvalidClient,
				fmt.Sprintf("invalid_client: %s", desc),
				"Your identity provider rejected the client credentials. Try setting oidc.token_endpoint_auth_method to 'client_secret_post' or 'client_secret_basic' in your config.")
		}
		if isPKCEError(errStr, desc) {
			var hint string
			if s.cfg.UsePKCE {
				hint = "Your identity provider may not support PKCE. Try setting oidc.use_pkce to false in your config."
			} else {
				hint = "Your identity provider may require PKCE. Try setting oidc.use_pkce to true in your config."
			}
			return nil, oidcErr(CodePKCEError,
				fmt.Sprintf("PKCE error: %s — %s. Current use_pkce=%v", errStr, desc, s.cfg.UsePKCE), hint)
		}
		return nil, oidcErr(CodeTokenExchange, fmt.Sprintf("Token exchange error: %s — %s", errStr, desc), "")
	}

	accessToken, _ := parsed["access_token"].(string)
	if accessToken == "" {
		return nil, oidcErr(CodeTokenExchange, "Token response is missing access_token", "")
	}
	tokens := &tokenResponse{AccessToken: accessToken}
	tokens.IDToken, _ = parsed["id_token"].(string)
	tokens.TokenType, _ = parsed["token_type"].(string)
	if f, ok := parsed["expires_in"].(float64); ok {
		tokens.ExpiresIn = int64(f)
	}
	tokens.RefreshToken, _ = parsed["refresh_token"].(string)
	return tokens, nil
}

// isPKCEError mirrors the TS "praying on hopes and dreams" PKCE detection.
func isPKCEError(errStr, desc string) bool {
	lower := strings.ToLower(errStr)
	dlower := strings.ToLower(desc)
	for _, needle := range []string{"pkce", "code_verifier", "code verifier"} {
		if strings.Contains(lower, needle) || strings.Contains(dlower, needle) {
			return true
		}
	}
	return false
}
