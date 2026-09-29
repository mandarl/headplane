package main

// Browser SSH / RDP terminal endpoints: GET /api/v1/ssh/{id} and
// /api/v1/rdp/{id}. Ports the server loaders in app/routes/ssh/page.tsx
// and app/routes/rdp/page.tsx — WASM-asset availability, agent-enabled
// gate, target-node lookup, Headscale-user resolution, and a 5-minute
// ephemeral pre-auth key once a username is chosen. The SPA pages fetch
// this and never mint keys themselves.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tale/headplane/internal/hsapi"
)

// Error bodies must match app/routes/ssh/errors.tsx (SSH carries an
// `anchor`) and the inline shapes in app/routes/rdp/page.tsx.
var (
	sshErrWasmMissing = map[string]any{"title": "Browser SSH is not available", "message": "This version of Headplane was not built with browser SSH support.", "anchor": "#ssh-not-available"}
	sshErrAgentReq    = map[string]any{"title": "Browser SSH requires the Headplane agent", "message": "Browser SSH is only available when the Headplane agent integration is enabled.", "anchor": "#agent-required"}
	sshErrUserNotLink = map[string]any{"title": "User account not linked", "message": "You'll need to link your user account to a Headscale user before you can use Browser SSH.", "anchor": "#user-not-linked"}
	rdpErrWasmMissing = map[string]any{"title": "RDP Not Available", "message": "hp_rdp.wasm is not available on this server."}
	rdpErrAgentReq    = map[string]any{"title": "Agent Required", "message": "The Headplane agent must be running to use Web RDP."}
	rdpErrUserNotLink = map[string]any{"title": "User Not Linked", "message": "Your user account is not linked to a Headscale user."}
)

func sshErrNodeNotFound(host string) map[string]any {
	return map[string]any{"title": "Node not found", "message": "No node found with hostname " + host + ".", "anchor": "#node-not-found"}
}

func rdpErrNodeNotFound(host string) map[string]any {
	return map[string]any{"title": "Node Not Found", "message": `Node "` + host + `" was not found.`}
}

// handleTerminal serves both /ssh/{id} and /rdp/{id}; kind is "ssh" or "rdp".
func (s *Server) handleTerminal(w http.ResponseWriter, r *http.Request, kind, id string) {
	ssh := kind == "ssh"

	wasm := "hp_rdp.wasm"
	errWasm, errAgent, errUnlinked := rdpErrWasmMissing, rdpErrAgentReq, rdpErrUserNotLink
	errNode := rdpErrNodeNotFound
	hostPrefix := "rdp-"
	if ssh {
		wasm = "hp_ssh.wasm"
		errWasm, errAgent, errUnlinked = sshErrWasmMissing, sshErrAgentReq, sshErrUserNotLink
		errNode = sshErrNodeNotFound
		hostPrefix = "ssh-"
	}

	if id == "" || strings.Contains(id, "/") {
		writeJSON(w, http.StatusNotFound, errNode(id))
		return
	}

	// Auth first. The TS loader runs the WASM/agent checks before
	// apiForRequest; every other /api/v1/* route authenticates first, and
	// there's no reason to expose the server's build/agent config to
	// unauthenticated callers — the flow for an authenticated caller is
	// unchanged.
	api, p, err := s.hsClientFor(r)
	if err != nil {
		if err == errNeedAuth {
			writeError(w, http.StatusUnauthorized, "Authentication required.")
		} else {
			writeError(w, http.StatusInternalServerError, "No Headscale API key is configured for this session.")
		}
		return
	}

	// WASM runtime + module must be present in the served client bundle
	// (the loader HEAD-checks them; here we stat the files directly).
	for _, f := range []string{wasm, "wasm_exec.js"} {
		if st, err := os.Stat(filepath.Join(s.clientDir, f)); err != nil || st.IsDir() {
			writeJSON(w, http.StatusMethodNotAllowed, errWasm)
			return
		}
	}

	// Browser SSH/RDP require the agent integration.
	if mgr, _ := s.agentManager(); mgr == nil {
		writeJSON(w, http.StatusBadRequest, errAgent)
		return
	}

	nodes, users, err := s.liveSnapshots(r.Context(), api)
	if err != nil {
		writeAPIError(w, err)
		return
	}

	var node hsapi.Node
	for _, n := range nodes {
		if strOf(n["givenName"]) == id {
			node = n
			break
		}
	}
	if node == nil {
		writeJSON(w, http.StatusNotFound, errNode(id))
		return
	}

	username := r.URL.Query().Get("user")

	resp := map[string]any{"hostname": id, "offline": false, "node": nil}
	if username == "" {
		resp["username"] = nil
	} else {
		resp["username"] = username
	}
	if ssh {
		resp["isWindows"] = s.nodeIsWindows(node)
	}

	if online, _ := node["online"].(bool); !online {
		resp["offline"] = true
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if username == "" {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Resolve the Headscale user: API-key principals act as the first
	// user; OIDC principals match by subject then email.
	var hsUser hsapi.User
	if p.Kind == "api_key" {
		if len(users) > 0 {
			hsUser = users[0]
		}
	} else {
		if p.Subject != "" {
			for _, u := range users {
				if hsapi.OIDCSubject(u) == p.Subject {
					hsUser = u
					break
				}
			}
		}
		if hsUser == nil && p.ProfileEmail != nil && *p.ProfileEmail != "" {
			for _, u := range users {
				if strOf(u["email"]) == *p.ProfileEmail {
					hsUser = u
					break
				}
			}
		}
	}
	if hsUser == nil {
		writeJSON(w, http.StatusNotFound, errUnlinked)
		return
	}
	uid := strOf(hsUser["id"])

	exp := time.Now().Add(5 * time.Minute)
	key, err := api.CreatePreAuthKey(&uid, true, false, &exp, nil)
	if err != nil {
		writeAPIError(w, err)
		return
	}

	var ip string
	if ips, ok := node["ipAddresses"].([]any); ok && len(ips) > 0 {
		ip, _ = ips[0].(string)
	}
	resp["node"] = map[string]any{
		"ipAddress":         ip,
		"controlURL":        s.headscaleBaseURL(),
		"preAuthKey":        strOf(key["key"]),
		"ephemeralHostname": hostPrefix + randHex(8) + "-" + username,
	}
	writeJSON(w, http.StatusOK, resp)
}

// nodeIsWindows reports whether the agent's hostinfo for the node says the
// OS is Windows (mirrors node.hostInfo?.OS?.toLowerCase() === "windows").
func (s *Server) nodeIsWindows(node hsapi.Node) bool {
	nk := strOf(node["nodeKey"])
	raw, ok := s.agentStats([]string{nk})[nk]
	if !ok {
		return false
	}
	var hi struct {
		OS string `json:"OS"`
	}
	if json.Unmarshal(raw, &hi) != nil {
		return false
	}
	return strings.EqualFold(hi.OS, "windows")
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}
