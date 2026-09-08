package main

// serveAPIv1 dispatches the /admin/api/v1/* surface (apiPrefix, defined in
// v1.go). `rest` is the path after the prefix, starting with "/" (or empty
// for the bare prefix). Unknown routes and wrong methods answer 404/405
// JSON, never the SPA shell.

import (
	"net/http"
	"strings"
)

func (s *Server) serveAPIv1(w http.ResponseWriter, r *http.Request, rest string) {
	if rest == "" {
		rest = "/"
	}
	method := r.Method

	switch {
	case rest == "/boot" && method == http.MethodGet:
		s.handleBoot(w, r)
	case rest == "/home" && method == http.MethodGet:
		s.handleHome(w, r)
	case rest == "/home/link" && method == http.MethodPost:
		s.handleHomeLink(w, r)
	case rest == "/machines" && method == http.MethodGet:
		s.handleMachines(w, r)
	case rest == "/machines/actions" && method == http.MethodPost:
		s.handleMachineActions(w, r)
	case strings.HasPrefix(rest, "/machines/") && method == http.MethodGet:
		id := strings.TrimPrefix(rest, "/machines/")
		if id == "" || id == "actions" || strings.Contains(id, "/") {
			if id == "actions" {
				writeError(w, http.StatusMethodNotAllowed, "Method not allowed.")
			} else {
				writeError(w, http.StatusNotFound, "Not found.")
			}
			return
		}
		s.handleMachine(w, r, id)
	case rest == "/users" && method == http.MethodGet:
		s.handleUsers(w, r)
	case rest == "/users/actions" && method == http.MethodPost:
		s.handleUserActions(w, r)
	case rest == "/acls" && method == http.MethodGet:
		s.handleAcls(w, r)
	case rest == "/acls" && method == http.MethodPatch:
		s.handleAclsPatch(w, r)
	case rest == "/dns" && method == http.MethodGet:
		s.handleDns(w, r)
	case rest == "/settings" && method == http.MethodGet:
		s.handleSettings(w, r)
	case rest == "/settings/auth-keys" && method == http.MethodGet:
		s.handleAuthKeys(w, r)
	case rest == "/settings/auth-keys/actions" && method == http.MethodPost:
		s.handleAuthKeyActions(w, r)
	case rest == "/settings/restrictions" && method == http.MethodGet:
		s.handleRestrictions(w, r)
	case rest == "/settings/agent" && method == http.MethodGet:
		s.handleAgent(w, r)
	case rest == "/settings/agent/sync" && method == http.MethodPost:
		s.handleAgentSyncStub(w, r)
	case rest == "/dns/actions" && method == http.MethodPost:
		s.handleConfigActionStub(w, r, "DNS")
	case rest == "/settings/restrictions/actions" && method == http.MethodPost:
		s.handleConfigActionStub(w, r, "Authentication restriction")
	default:
		if knownAPIPath(rest) {
			writeError(w, http.StatusMethodNotAllowed, "Method not allowed.")
			return
		}
		writeError(w, http.StatusNotFound, "Not found.")
	}
}

// knownAPIPath reports whether rest names a real v1 route (used to pick
// 405 over 404 for a right-path/wrong-method request).
func knownAPIPath(rest string) bool {
	switch rest {
	case "/boot", "/home", "/home/link", "/machines", "/machines/actions",
		"/users", "/users/actions", "/acls", "/dns", "/dns/actions", "/settings",
		"/settings/auth-keys", "/settings/auth-keys/actions",
		"/settings/restrictions", "/settings/restrictions/actions",
		"/settings/agent", "/settings/agent/sync":
		return true
	}
	return strings.HasPrefix(rest, "/machines/")
}
