package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// headscaleAPIKeyInfo is the subset of Headscale's API key record the login
// flow needs. Headscale's GET /api/v1/apikey returns {"apiKeys": [...]}
// (see app/server/headscale/api/resources/api-keys.ts).
type headscaleAPIKeyInfo struct {
	Prefix     string  `json:"prefix"`
	Expiration *string `json:"expiration"`
}

// validateAPIKey checks the candidate key against Headscale itself, mirroring
// the TS loginAction: GET /api/v1/apikey with the candidate as the bearer
// token, then match by prefix (with the 0.28.0 asterisk quirk) and expiry.
// It returns the matched key record.
func (s *Server) validateAPIKey(apiKey string) (*headscaleAPIKeyInfo, error) {
	url := strings.TrimSuffix(s.cfg.Headscale.URL, "/") + "/api/v1/apikey"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	// Same shape as the TS transport default (undici request timeout).
	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))

	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden,
		res.StatusCode == http.StatusInternalServerError && strings.TrimSpace(string(body)) == "Unauthorized":
		return nil, &apiKeyError{invalid: true}
	case res.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("headscale apikey list: unexpected status %d", res.StatusCode)
	}

	var list struct {
		APIKeys []headscaleAPIKeyInfo `json:"apiKeys"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	// 0.28.0 pointlessly added asterisks to key prefixes; strip them before
	// matching, exactly like the TS lookup.
	for i := range list.APIKeys {
		if strings.HasPrefix(apiKey, strings.ReplaceAll(list.APIKeys[i].Prefix, "*", "")) {
			return &list.APIKeys[i], nil
		}
	}
	return nil, &apiKeyError{notFound: true}
}

type apiKeyError struct {
	invalid  bool
	notFound bool
}

func (e *apiKeyError) Error() string {
	if e.invalid {
		return "api key invalid"
	}
	return "api key not found"
}

// loginFailure mirrors the TS loginFailure: status 200 JSON
// {success:false,message}. React Router turns 4xx/5xx action responses into
// error pages for document requests, so failures must stay 200.
func (s *Server) loginFailure(w http.ResponseWriter, message string) {
	body, _ := json.Marshal(map[string]any{"success": false, "message": message})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// handleLogin serves POST {basename}/login. On success it issues the _hp_auth
// session cookie and 302s to {basename}/machines (the TS loginAction
// redirects to "/machines", resolved under the basename); on failure it
// returns the 200 JSON failure contract the SPA clientAction reads.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.loginFailure(w, "Error while validating API key (see logs for details)")
		return
	}
	apiKey := r.FormValue("api_key")
	if r.Form["api_key"] == nil {
		s.logger.Warn("request made without API key", "component", "auth")
		s.loginFailure(w, "Missing API key. Please enter your API key.")
		return
	}
	if apiKey == "" {
		s.logger.Warn("request made with empty API key", "component", "auth")
		s.loginFailure(w, "API key cannot be empty. Please enter a valid API key.")
		return
	}

	lookup, err := s.validateAPIKey(apiKey)
	if err != nil {
		var kerr *apiKeyError
		switch {
		case errors.As(err, &kerr) && kerr.invalid:
			s.loginFailure(w, "API key is invalid (it may be incorrect or expired)")
		case errors.As(err, &kerr) && kerr.notFound:
			s.loginFailure(w, "API key was not found in the Headscale database")
		default:
			s.logger.Error("error while validating API key", "component", "auth", "error", err)
			s.loginFailure(w, "Error while validating API key (see logs for details)")
		}
		return
	}

	if lookup.Expiration == nil || *lookup.Expiration == "" {
		s.logger.Error("got an API key without an expiration", "component", "auth")
		s.loginFailure(w, "API key is malformed (missing expiration). Please generate a new API key.")
		return
	}
	expiry, err := time.Parse(time.RFC3339, *lookup.Expiration)
	if err != nil {
		s.logger.Error("got an API key with an unparsable expiration", "component", "auth", "expiration", *lookup.Expiration)
		s.loginFailure(w, "API key is malformed (missing expiration). Please generate a new API key.")
		return
	}
	if expiry.Before(time.Now()) {
		s.loginFailure(w, "API key has expired")
		return
	}

	setCookie, err := s.authSvc.CreateAPIKeySession(apiKey, lookup.Prefix+"...", time.Until(expiry))
	if err != nil {
		s.logger.Error("failed to create session", "component", "auth", "error", err)
		s.loginFailure(w, "Error while validating API key (see logs for details)")
		return
	}
	w.Header().Set("Set-Cookie", setCookie)
	w.Header().Set("Location", s.basename+"/machines")
	w.WriteHeader(http.StatusFound)
}

// handleLogout serves {basename}/logout: GET redirects to the machines page
// (mirroring the TS loader); POST deletes the session, clears the cookie,
// and redirects to the login page (or the OIDC end-session URL in Phase 3).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Location", s.basename+"/machines")
		w.WriteHeader(http.StatusFound)
		return
	}

	redirectTo := s.basename + "/login"
	if s.cfg.OIDC != nil && s.cfg.OIDC.DisableAPIKeyLogin {
		// Matches the TS logout action: with API-key login disabled, flag
		// the logout so the login page does not auto-log-in again.
		redirectTo = s.basename + "/login?s=logout"
	}
	// Phase 3 adds the RP-initiated OIDC end-session redirect here.
	if _, err := s.authSvc.Require(r); err != nil {
		// Mirrors the TS action: an unauthenticated logout just bounces to
		// the login page without touching a cookie.
		w.Header().Set("Location", s.basename+"/login")
		w.WriteHeader(http.StatusFound)
		return
	}
	w.Header().Set("Set-Cookie", s.authSvc.DestroySession(r))
	w.Header().Set("Location", redirectTo)
	w.WriteHeader(http.StatusFound)
}
