package main

// Server-driven color-scheme preference: POST {basename}/api/color-scheme.
// Ports app/routes/util/color-scheme.ts — validate the value, set the
// unsigned `color_scheme` cookie (react-router createCookie wire format:
// URL-encoded base64 of the JSON value), and redirect. The SPA client
// (app/layout/header.tsx) ignores the response body and does its own full
// reload, after which root.tsx's clientLoader reads the cookie.

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// colorSchemeMaxAge mirrors createCookie("color_scheme", {maxAge: 34560000}).
const colorSchemeMaxAge = 34560000

func (s *Server) handleColorScheme(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "Method not allowed.")
		return
	}
	// header.tsx posts a FormData body → multipart/form-data. ParseMultipartForm
	// also parses urlencoded/query (it calls ParseForm internally), so this
	// covers both; a non-multipart body just yields ErrNotMultipart here,
	// which is fine — FormValue still reads what ParseForm populated.
	_ = r.ParseMultipartForm(1 << 20)

	scheme := r.FormValue("colorScheme")
	switch scheme {
	case "dark", "light":
		raw, _ := json.Marshal(map[string]string{"colorScheme": scheme})
		http.SetCookie(w, &http.Cookie{
			Name:     "color_scheme",
			Value:    url.QueryEscape(base64.StdEncoding.EncodeToString(raw)),
			Path:     "/",
			MaxAge:   colorSchemeMaxAge,
			SameSite: http.SameSiteLaxMode,
		})
	case "system":
		// react-router setColorScheme("system") clears the cookie.
		http.SetCookie(w, &http.Cookie{
			Name:     "color_scheme",
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			SameSite: http.SameSiteLaxMode,
		})
	default:
		writeError(w, http.StatusBadRequest, "Bad Request")
		return
	}

	http.Redirect(w, r, s.basename+safeInternalRedirect(r.FormValue("returnTo")), http.StatusFound)
}

// safeInternalRedirect mirrors the safeRedirect helper in color-scheme.ts:
// only same-origin absolute paths are allowed, everything else falls back
// to "/".
func safeInternalRedirect(to string) string {
	if to == "" || !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") {
		return "/"
	}
	return to
}
