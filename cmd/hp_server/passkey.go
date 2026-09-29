// Passkey (WebAuthn) endpoints.
//
// Registration is authenticated: the passkey binds to the caller's
// headplane user. OIDC principals use their users row directly; API-key
// principals (which have no users row) get a derived owner row via
// FindOrCreatePasskeyUser so capability parity with the API key is kept.
//
// Login is unauthenticated and uses discoverable credentials: the browser
// offers the platform authenticator, a synced provider (e.g. LastPass),
// or the hybrid QR/phone flow, with no allowCredentials filter.
//
// Ceremony bodies are raw WebAuthn JSON (navigator.credentials.create /
// .get output); the ceremony token and the registration label travel as
// query parameters so the body passes through untouched to the library.
package main

import (
	"net/http"
	"time"

	"github.com/tale/headplane/internal/auth"
)

// passkeyEnabled reports whether the passkey endpoints serve traffic.
func (s *Server) passkeyEnabled() bool {
	return s.passkeySvc != nil && s.cfg.WebAuthn.IsEnabled()
}

// passkeyUserID resolves the users row a passkey registration binds to.
func (s *Server) passkeyUserID(p *auth.Principal) (string, error) {
	switch p.Kind {
	case "oidc", "passkey":
		return p.UserID, nil
	case "api_key":
		return s.authSvc.FindOrCreatePasskeyUser(p.APIKey, p.DisplayName)
	default:
		return "", errNeedAuth
	}
}

// handlePasskeyRegisterOptions serves
// POST {basename}/api/v1/passkeys/register/options (authenticated).
func (s *Server) handlePasskeyRegisterOptions(w http.ResponseWriter, r *http.Request) {
	if !s.passkeyEnabled() {
		writeError(w, http.StatusNotFound, "Not found.")
		return
	}
	p := s.requirePrincipal(w, r)
	if p == nil {
		return
	}
	userID, err := s.passkeyUserID(p)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "Authentication required.")
		return
	}
	token, creation, err := s.passkeySvc.BeginRegistration(userID, r)
	if err != nil {
		s.logger.Error("passkey begin registration failed", "component", "passkey", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not start passkey registration.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "options": creation})
}

// handlePasskeyRegisterVerify serves
// POST {basename}/api/v1/passkeys/register/verify?token=…&label=…
// (authenticated). The body is the raw navigator.credentials.create()
// JSON.
func (s *Server) handlePasskeyRegisterVerify(w http.ResponseWriter, r *http.Request) {
	if !s.passkeyEnabled() {
		writeError(w, http.StatusNotFound, "Not found.")
		return
	}
	p := s.requirePrincipal(w, r)
	if p == nil {
		return
	}
	if _, err := s.passkeyUserID(p); err != nil {
		writeError(w, http.StatusUnauthorized, "Authentication required.")
		return
	}
	query := r.URL.Query()
	info, err := s.passkeySvc.FinishRegistration(query.Get("token"), query.Get("label"), r)
	if err != nil {
		s.logger.Warn("passkey registration failed", "component", "passkey", "error", err)
		writeError(w, http.StatusBadRequest, "Passkey registration failed.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "credential": info})
}

// handlePasskeysList serves GET {basename}/api/v1/passkeys (authenticated).
func (s *Server) handlePasskeysList(w http.ResponseWriter, r *http.Request) {
	if !s.passkeyEnabled() {
		writeError(w, http.StatusNotFound, "Not found.")
		return
	}
	p := s.requirePrincipal(w, r)
	if p == nil {
		return
	}
	userID, err := s.passkeyUserID(p)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "Authentication required.")
		return
	}
	creds, err := s.passkeySvc.List(userID)
	if err != nil {
		s.logger.Error("passkey list failed", "component", "passkey", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not list passkeys.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"credentials": creds})
}

// handlePasskeyUpdate serves PATCH {basename}/api/v1/passkeys
// (authenticated) with a JSON {id, label} body.
func (s *Server) handlePasskeyUpdate(w http.ResponseWriter, r *http.Request) {
	if !s.passkeyEnabled() {
		writeError(w, http.StatusNotFound, "Not found.")
		return
	}
	p := s.requirePrincipal(w, r)
	if p == nil {
		return
	}
	userID, err := s.passkeyUserID(p)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "Authentication required.")
		return
	}
	body := readJSONBody(r)
	id, label := formStr(body, "id"), formStr(body, "label")
	if id == "" {
		writeError(w, http.StatusBadRequest, "Missing credential id.")
		return
	}
	if err := s.passkeySvc.UpdateLabel(userID, id, label); err != nil {
		writeError(w, http.StatusNotFound, "Passkey not found.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// handlePasskeyDelete serves DELETE {basename}/api/v1/passkeys
// (authenticated) with a JSON {id} body.
func (s *Server) handlePasskeyDelete(w http.ResponseWriter, r *http.Request) {
	if !s.passkeyEnabled() {
		writeError(w, http.StatusNotFound, "Not found.")
		return
	}
	p := s.requirePrincipal(w, r)
	if p == nil {
		return
	}
	userID, err := s.passkeyUserID(p)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "Authentication required.")
		return
	}
	body := readJSONBody(r)
	if id := formStr(body, "id"); id == "" {
		writeError(w, http.StatusBadRequest, "Missing credential id.")
		return
	} else if err := s.passkeySvc.Delete(userID, id); err != nil {
		writeError(w, http.StatusNotFound, "Passkey not found.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// handlePasskeyLoginOptions serves
// POST {basename}/api/v1/passkeys/login/options (unauthenticated).
func (s *Server) handlePasskeyLoginOptions(w http.ResponseWriter, r *http.Request) {
	if !s.passkeyEnabled() {
		writeError(w, http.StatusNotFound, "Not found.")
		return
	}
	token, assertion, err := s.passkeySvc.BeginLogin(r)
	if err != nil {
		s.logger.Error("passkey begin login failed", "component", "passkey", "error", err)
		writeError(w, http.StatusInternalServerError, "Could not start passkey login.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "options": assertion})
}

// handlePasskeyLoginVerify serves
// POST {basename}/api/v1/passkeys/login/verify?token=… (unauthenticated).
// The body is the raw navigator.credentials.get() JSON. On success it
// issues the same _hp_auth session cookie as the other login methods.
func (s *Server) handlePasskeyLoginVerify(w http.ResponseWriter, r *http.Request) {
	if !s.passkeyEnabled() {
		writeError(w, http.StatusNotFound, "Not found.")
		return
	}
	userID, _, err := s.passkeySvc.FinishLogin(r.URL.Query().Get("token"), r)
	if err != nil {
		s.logger.Warn("passkey login failed", "component", "passkey", "error", err)
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": "Passkey authentication failed."})
		return
	}
	name, email, err := s.authSvc.UserProfile(userID)
	if err != nil {
		s.logger.Error("passkey login: cannot load profile", "component", "passkey", "error", err)
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": "Passkey authentication failed."})
		return
	}
	profile := auth.CookieProfile{Name: name}
	if email != nil {
		profile.Email = email
	}
	setCookie, err := s.authSvc.CreatePasskeySession(userID, profile,
		time.Duration(s.cfg.Server.CookieMaxAge)*time.Second)
	if err != nil {
		s.logger.Error("passkey login: cannot create session", "component", "passkey", "error", err)
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "message": "Passkey authentication failed."})
		return
	}
	_ = s.authSvc.RecordLogin(userID)
	w.Header().Set("Set-Cookie", setCookie)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}
