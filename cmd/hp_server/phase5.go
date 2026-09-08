package main

// Phase 5: the remaining endpoints — RDP gateway webhook, agent manager
// sync, service-description overrides, DNS and OIDC-restriction config
// patching, docker/kubernetes/proc restart integrations, and /api/info.
// This file ports app/routes/rdp-gateway/action.ts,
// app/routes/util/info.ts, app/routes/dns/dns-actions.ts,
// app/routes/settings/restrictions/actions.ts, the agent branch of
// app/server/context.ts + app/server/hp-agent.ts, and the
// update_service_description machine action.

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tale/headplane/internal/agent"
	"github.com/tale/headplane/internal/auth"
	"github.com/tale/headplane/internal/hsapi"
	"github.com/tale/headplane/internal/hscfg"
	"github.com/tale/headplane/internal/integration"
	"github.com/tale/headplane/internal/rdpgw"
	"github.com/tale/headplane/internal/serverconfig"
)

// headplaneVersion is the release version stamped at build time
// (-ldflags -X main.headplaneVersion=...); "dev" for local builds.
var headplaneVersion = "dev"

// phase5State holds the Phase 5 runtime state. The agent manager is built
// lazily on the first successful Headscale version detection (guarded by
// agentMu) because the agent needs tag-only pre-auth keys, a 0.28+
// capability that must not be assumed before detection.
type phase5State struct {
	agentMu             sync.RWMutex
	agentMgr            *agent.Manager
	agentDisabledReason string
	agentTried          bool
	// headscaleAPIKey is the configured default Headscale API key, used for
	// the pre-detection agent-disabled reason.
	headscaleAPIKey string

	hsVersionMu sync.RWMutex
	hsVersion   string

	rdpGw       *rdpgw.Client
	integration integration.Integration
	hsHealth    func() bool
}

// onVersionDetected records the detected Headscale version (for /api/info)
// and builds the agent manager on first detection, mirroring buildAgents in
// app/server/context.ts: enabled flag, default API key, 0.28+ tag-only key
// support, and executable/workdir validation each produce the TS disabled
// reason (or "Agent failed to initialize (see logs)" for spawn failures).
func (s *Server) onVersionDetected(v hsapi.ServerVersion, caps hsapi.Capabilities, apiKey string) {
	s.hsVersionMu.Lock()
	s.hsVersion = v.Raw
	s.hsVersionMu.Unlock()

	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if s.agentTried {
		return
	}
	s.agentTried = true

	var agentCfg *serverconfig.AgentConfig
	if s.cfg.Integration != nil {
		agentCfg = s.cfg.Integration.Agent
	}
	if agentCfg == nil || !agentCfg.Enabled {
		s.agentDisabledReason = "Agent is not enabled in the configuration"
		return
	}
	if apiKey == "" {
		s.agentDisabledReason = "Agent requires headscale.api_key to be configured"
		return
	}
	if !caps.PreAuthKeysHaveStableIds {
		s.agentDisabledReason = "Agent requires Headscale 0.28 or newer"
		return
	}
	mgr, err := agent.NewManager(agent.Config{
		HostName:       agentCfg.HostName,
		CacheTTL:       agentCfg.CacheTTL(),
		ExecutablePath: agentCfg.ExecutablePath,
		WorkDir:        agentCfg.WorkDir,
	}, s.authSvc, hsapi.NewClient(s.cfg.Headscale.URL, apiKey,
		s.cfg.Headscale.TLSCertPath, caps, s.logger), s.cfg.Headscale.URL, s.logger)
	if err != nil {
		s.logger.Error("agent failed to initialize", "error", err)
		s.agentDisabledReason = "Agent failed to initialize (see logs)"
		return
	}
	s.agentMgr = mgr
	s.logger.Info("headplane agent enabled")
}

// hsVersionRaw returns the last detected Headscale version string.
func (s *Server) hsVersionRaw() string {
	s.hsVersionMu.RLock()
	defer s.hsVersionMu.RUnlock()
	return s.hsVersion
}

// agentManager returns the running agent manager, or nil when the agent is
// disabled (reason explains why). Before version detection runs (e.g. in
// tests), the static configuration reasons are reported so the settings
// page never shows an empty reason.
func (s *Server) agentManager() (*agent.Manager, string) {
	s.agentMu.RLock()
	mgr, tried, reason := s.agentMgr, s.agentTried, s.agentDisabledReason
	s.agentMu.RUnlock()
	if mgr != nil || tried {
		return mgr, reason
	}
	var agentCfg *serverconfig.AgentConfig
	if s.cfg.Integration != nil {
		agentCfg = s.cfg.Integration.Agent
	}
	if agentCfg == nil || !agentCfg.Enabled {
		return nil, "Agent is not enabled in the configuration"
	}
	if s.headscaleAPIKey == "" {
		return nil, "Agent requires headscale.api_key to be configured"
	}
	return nil, "Waiting for Headscale version detection"
}

// agentInfo builds the `agent` object for the machine overview/detail
// loaders: null when disabled, else {syncedAt, nodeCount, nodeKey?}.
func (s *Server) agentInfo() map[string]any {
	mgr, _ := s.agentManager()
	if mgr == nil {
		return nil
	}
	syncedAt, nodeCount, _ := mgr.LastSync()
	var synced *string
	if syncedAt != nil {
		v := syncedAt.UTC().Format(time.RFC3339)
		synced = &v
	}
	info := map[string]any{
		"syncedAt":  synced,
		"nodeCount": nodeCount,
	}
	if key := mgr.AgentNodeKey(); key != "" {
		info["nodeKey"] = key
	}
	return info
}

// agentStats looks up hostinfo payloads for the given node keys; nil when
// the agent is disabled.
func (s *Server) agentStats(nodeKeys []string) map[string]json.RawMessage {
	mgr, _ := s.agentManager()
	if mgr == nil || len(nodeKeys) == 0 {
		return nil
	}
	stats, err := mgr.Lookup(nodeKeys)
	if err != nil {
		s.logger.Warn("agent hostinfo lookup failed", "error", err)
		return nil
	}
	return stats
}

// handleAgent mirrors the agent page loader: {enabled:false,reason} when
// disabled, else {enabled:true,syncedAt,nodeCount,error}.
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	if s.requirePrincipal(w, r) == nil {
		return
	}
	mgr, reason := s.agentManager()
	if mgr == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false,
			"reason":  reason,
		})
		return
	}
	syncedAt, nodeCount, errMsg := mgr.LastSync()
	var synced *string
	if syncedAt != nil {
		v := syncedAt.UTC().Format(time.RFC3339)
		synced = &v
	}
	var syncErr *string
	if errMsg != "" {
		syncErr = &errMsg
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":   true,
		"syncedAt":  synced,
		"nodeCount": nodeCount,
		"error":     syncErr,
	})
}

// handleAgentSync mirrors the agent sync action: always HTTP 200 carrying
// {success, error}; the SPA passes it through untouched.
func (s *Server) handleAgentSync(w http.ResponseWriter, r *http.Request) {
	if s.requirePrincipal(w, r) == nil {
		return
	}
	mgr, reason := s.agentManager()
	if mgr == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"error":   "Agent is not enabled: " + reason,
		})
		return
	}
	mgr.TriggerSync()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "error": nil})
}

// onConfigChange runs the selected restart integration after a Headscale
// config change, mirroring context.integration?.onConfigChange(headscale).
// A nil integration is a no-op; a failed restart is a 500 like the TS
// action's thrown error.
func (s *Server) onConfigChange(w http.ResponseWriter) bool {
	if s.integration == nil {
		return true
	}
	health := s.hsHealth
	if health == nil {
		health = func() bool { return true }
	}
	if err := s.integration.OnConfigChange(health); err != nil {
		s.logger.Error("integration onConfigChange failed", "error", err)
		writeError(w, http.StatusInternalServerError,
			"Configuration updated but Headscale failed to restart: "+err.Error())
		return false
	}
	return true
}

// strListAny converts a []string to []any for YAML patch values.
func strListAny(in []string) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}

// handleDnsActions mirrors dnsAction in app/routes/dns/dns-actions.ts:
// write_network + writable-config gating, then the nine config mutations.
func (s *Server) handleDnsActions(w http.ResponseWriter, r *http.Request) {
	p := s.requirePrincipal(w, r)
	if p == nil {
		return
	}
	if !s.authSvc.Can(p, auth.CapWriteNetwork) {
		writeJSON(w, http.StatusForbidden, map[string]any{"success": false})
		return
	}
	if !s.hsCfg.Writable() {
		writeJSON(w, http.StatusForbidden, map[string]any{"success": false})
		return
	}
	body := readJSONBody(r)
	action := formStr(body, "action_id")
	if action == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false})
		return
	}

	cfg := s.hsCfg
	patch := func(patches []hscfg.Patch, message string) {
		if err := cfg.Patch(patches); err != nil {
			s.logger.Error("dns config patch failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false})
			return
		}
		if !s.onConfigChange(w) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"message": message})
	}

	switch action {
	case "rename_tailnet":
		{
			newName := formStr(body, "new_name")
			if newName == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"success": false})
				return
			}
			patch([]hscfg.Patch{{Path: "dns.base_domain", Value: newName}},
				"Tailnet renamed successfully")
		}
	case "toggle_magic":
		{
			newState := formStr(body, "new_state")
			if newState == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"success": false})
				return
			}
			patch([]hscfg.Patch{{Path: "dns.magic_dns", Value: newState == "enabled"}},
				"Magic DNS state updated successfully")
		}
	case "remove_ns":
		{
			ns := formStr(body, "ns")
			splitName := formStr(body, "split_name")
			if ns == "" || splitName == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"success": false})
				return
			}
			if splitName == "global" {
				servers := []string{}
				for _, v := range cfg.Nameservers {
					if v != ns {
						servers = append(servers, v)
					}
				}
				patch([]hscfg.Patch{{Path: "dns.nameservers.global", Value: strListAny(servers)}},
					"Nameserver removed successfully")
			} else {
				servers := []string{}
				for _, v := range cfg.SplitDNS[splitName] {
					if v != ns {
						servers = append(servers, v)
					}
				}
				var value any = strListAny(servers)
				if len(servers) == 0 {
					value = nil // delete the split key, like the TS null
				}
				patch([]hscfg.Patch{{Path: `dns.nameservers.split."` + splitName + `"`, Value: value}},
					"Nameserver removed successfully")
			}
		}
	case "add_ns":
		{
			ns := formStr(body, "ns")
			splitName := formStr(body, "split_name")
			if ns == "" || splitName == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"success": false})
				return
			}
			if splitName == "global" {
				servers := append(append([]string{}, cfg.Nameservers...), ns)
				patch([]hscfg.Patch{{Path: "dns.nameservers.global", Value: strListAny(servers)}},
					"Nameserver added successfully")
			} else {
				servers := append(append([]string{}, cfg.SplitDNS[splitName]...), ns)
				patch([]hscfg.Patch{{Path: `dns.nameservers.split."` + splitName + `"`, Value: strListAny(servers)}},
					"Nameserver added successfully")
			}
		}
	case "remove_domain":
		{
			domain := formStr(body, "domain")
			if domain == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"success": false})
				return
			}
			domains := []string{}
			for _, d := range cfg.SearchDomains {
				if d != domain {
					domains = append(domains, d)
				}
			}
			patch([]hscfg.Patch{{Path: "dns.search_domains", Value: strListAny(domains)}},
				"Domain removed successfully")
		}
	case "add_domain":
		{
			domain := formStr(body, "domain")
			if domain == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"success": false})
				return
			}
			domains := append(append([]string{}, cfg.SearchDomains...), domain)
			patch([]hscfg.Patch{{Path: "dns.search_domains", Value: strListAny(domains)}},
				"Domain added successfully")
		}
	case "remove_record":
		{
			name := formStr(body, "record_name")
			typ := formStr(body, "record_type")
			if name == "" || typ == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"success": false})
				return
			}
			restart, err := cfg.RemoveDNSRecord(name, typ)
			if err != nil {
				s.logger.Error("dns remove record failed", "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false})
				return
			}
			if !restart {
				// Mirrors the TS `return;` (undefined body): no restart-worthy
				// change, so no integration run and no message.
				writeJSON(w, http.StatusOK, nil)
				return
			}
			if !s.onConfigChange(w) {
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"message": "DNS record removed successfully"})
		}
	case "add_record":
		{
			name := formStr(body, "record_name")
			typ := formStr(body, "record_type")
			value := formStr(body, "record_value")
			if name == "" || typ == "" || value == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"success": false})
				return
			}
			restart, err := cfg.AddDNSRecord(hscfg.DNSRecord{Type: typ, Name: name, Value: value})
			if err != nil {
				s.logger.Error("dns add record failed", "error", err)
				writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false})
				return
			}
			if !restart {
				writeJSON(w, http.StatusOK, nil)
				return
			}
			if !s.onConfigChange(w) {
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"message": "DNS record added successfully"})
		}
	case "override_dns":
		{
			override := formStr(body, "override_dns")
			if override == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"success": false})
				return
			}
			patch([]hscfg.Patch{{Path: "dns.override_local_dns", Value: override == "true"}},
				"DNS override updated successfully")
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false})
	}
}

// oidcAllowLists returns the current OIDC allow-lists (empty when the OIDC
// block is absent).
func (s *Server) oidcAllowLists() (domains, groups, users []string) {
	if o := s.hsCfg.OIDC; o != nil {
		return o.AllowedDomains, o.AllowedGroups, o.AllowedUsers
	}
	return nil, nil, nil
}

// handleRestrictionActions mirrors restrictionAction in
// app/routes/settings/restrictions/actions.ts. Every outcome is a plain
// JSON string, like the TS data("...") payloads.
func (s *Server) handleRestrictionActions(w http.ResponseWriter, r *http.Request) {
	p := s.requirePrincipal(w, r)
	if p == nil {
		return
	}
	if !s.authSvc.Can(p, auth.CapConfigureIAM) {
		writeJSON(w, http.StatusForbidden, "You do not have permission to modify IAM settings.")
		return
	}
	if !s.hsCfg.Writable() {
		writeJSON(w, http.StatusForbidden, "The Headscale configuration file is not editable.")
		return
	}
	body := readJSONBody(r)
	action := formStr(body, "action_id")
	if action == "" {
		writeJSON(w, http.StatusBadRequest, "No action provided.")
		return
	}

	dedupeAdd := func(list []string, v string) []string {
		for _, e := range list {
			if e == v {
				return list
			}
		}
		return append(list, v)
	}
	remove := func(list []string, v string) []string {
		out := []string{}
		for _, e := range list {
			if e != v {
				out = append(out, e)
			}
		}
		return out
	}
	patchList := func(path string, list []string, message string) {
		if err := s.hsCfg.Patch([]hscfg.Patch{{Path: path, Value: strListAny(list)}}); err != nil {
			s.logger.Error("restrictions patch failed", "error", err)
			writeJSON(w, http.StatusInternalServerError, "Failed to update restrictions.")
			return
		}
		if !s.onConfigChange(w) {
			return
		}
		writeJSON(w, http.StatusOK, message)
	}

	domains, groups, users := s.oidcAllowLists()
	switch action {
	case "add_domain", "add_group", "add_user",
		"remove_domain", "remove_group", "remove_user":
		// validated below
	default:
		writeJSON(w, http.StatusBadRequest, "Invalid action provided.")
		return
	}

	switch action {
	case "add_domain":
		{
			domain := strings.TrimSpace(formStr(body, "domain"))
			if domain == "" {
				writeJSON(w, http.StatusBadRequest, "No domain provided.")
				return
			}
			patchList("oidc.allowed_domains", dedupeAdd(domains, domain), "Domain added successfully.")
		}
	case "remove_domain":
		{
			domain := strings.TrimSpace(formStr(body, "domain"))
			if domain == "" {
				writeJSON(w, http.StatusBadRequest, "No domain provided.")
				return
			}
			found := false
			for _, d := range domains {
				if d == domain {
					found = true
					break
				}
			}
			if !found {
				writeJSON(w, http.StatusBadRequest, `Domain "`+domain+`" not found in allowed domains.`)
				return
			}
			patchList("oidc.allowed_domains", remove(domains, domain), "Domain removed successfully.")
		}
	case "add_group":
		{
			group := strings.TrimSpace(formStr(body, "group"))
			if group == "" {
				writeJSON(w, http.StatusBadRequest, "No group provided.")
				return
			}
			patchList("oidc.allowed_groups", dedupeAdd(groups, group), "Group added successfully.")
		}
	case "remove_group":
		{
			group := strings.TrimSpace(formStr(body, "group"))
			if group == "" {
				writeJSON(w, http.StatusBadRequest, "No group provided.")
				return
			}
			found := false
			for _, g := range groups {
				if g == group {
					found = true
					break
				}
			}
			if !found {
				writeJSON(w, http.StatusBadRequest, `Group "`+group+`" not found in allowed groups.`)
				return
			}
			patchList("oidc.allowed_groups", remove(groups, group), "Group removed successfully.")
		}
	case "add_user":
		{
			user := strings.TrimSpace(formStr(body, "user"))
			if user == "" {
				writeJSON(w, http.StatusBadRequest, "No user provided.")
				return
			}
			patchList("oidc.allowed_users", dedupeAdd(users, user), "User added successfully.")
		}
	case "remove_user":
		{
			user := strings.TrimSpace(formStr(body, "user"))
			if user == "" {
				writeJSON(w, http.StatusBadRequest, "No user provided.")
				return
			}
			found := false
			for _, u := range users {
				if u == user {
					found = true
					break
				}
			}
			if !found {
				writeJSON(w, http.StatusBadRequest, `User "`+user+`" not found in allowed users.`)
				return
			}
			patchList("oidc.allowed_users", remove(users, user), "User removed successfully.")
		}
	}
}

// rdpCallerIP mirrors getCallerIp in the rdp-gateway action: first
// X-Forwarded-For entry, then X-Real-IP, else 0.0.0.0.
func rdpCallerIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, _ := strings.Cut(xff, ","); strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	return "0.0.0.0"
}

// mergeSuccess merges a webhook result struct into {success:true,...}.
func mergeSuccess(v any) map[string]any {
	raw, _ := json.Marshal(v)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	out["success"] = true
	return out
}

// handleRdpGateway mirrors the rdp-gateway action route
// (app/routes/rdp-gateway/action.ts): POST-only at
// ${basename}/api/rdp-gateway, gated to write_machines principals.
func (s *Server) handleRdpGateway(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, nil)
		return
	}
	p := s.requirePrincipal(w, r)
	if p == nil {
		return
	}
	if !s.authSvc.Can(p, auth.CapWriteMachines) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"success": false, "error": "Insufficient permissions",
		})
		return
	}
	if s.rdpGw == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"success": false, "error": "RDP gateway is not configured on this server",
		})
		return
	}

	// The SPA posts FormData (multipart), not JSON.
	actionID := strings.TrimSpace(r.FormValue("action_id"))
	targetIP := strings.TrimSpace(r.FormValue("target_ip"))
	hostname := strings.TrimSpace(r.FormValue("hostname"))
	if actionID == "" || targetIP == "" || hostname == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing required fields",
		})
		return
	}

	ctx := r.Context()
	switch actionID {
	case "enable":
		{
			callerIP := rdpCallerIP(r)
			var timeoutMins *int
			if raw := strings.TrimSpace(r.FormValue("timeout_mins")); raw != "" {
				mins, err := strconv.Atoi(raw)
				if err != nil {
					mins = 180
				}
				if mins < 30 {
					mins = 30
				}
				if mins > 480 {
					mins = 480
				}
				timeoutMins = &mins
			}
			result, err := s.rdpGw.Enable(ctx, targetIP, hostname, callerIP, timeoutMins)
			if err != nil {
				s.logger.Error("rdp gateway enable failed", "hostname", hostname, "error", err)
				writeJSON(w, http.StatusBadGateway, map[string]any{
					"success": false, "error": "Gateway request failed. Check server logs.",
				})
				return
			}
			writeJSON(w, http.StatusOK, mergeSuccess(result))
		}
	case "disable":
		{
			if err := s.rdpGw.Disable(ctx, targetIP, hostname); err != nil {
				s.logger.Error("rdp gateway disable failed", "hostname", hostname, "error", err)
				writeJSON(w, http.StatusBadGateway, map[string]any{
					"success": false, "error": "Gateway request failed. Check server logs.",
				})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"success": true})
		}
	case "status":
		{
			result, err := s.rdpGw.Status(ctx, targetIP, hostname)
			if err != nil {
				s.logger.Error("rdp gateway status failed", "hostname", hostname, "error", err)
				writeJSON(w, http.StatusBadGateway, map[string]any{
					"success": false, "error": "Gateway request failed. Check server logs.",
				})
				return
			}
			writeJSON(w, http.StatusOK, mergeSuccess(result))
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Unknown action",
		})
	}
}

// handleInfo mirrors the util/info route: a bearer-token-guarded health
// snapshot. internal_versions carries the Go-equivalent of the TS
// process.versions map (the Go toolchain has no node/v8/uv/zlib/openssl
// split, so it reports the Go release plus the OS/arch target).
func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, nil)
		return
	}
	secret := s.cfg.Server.InfoSecret
	if secret == "" {
		writeJSON(w, http.StatusForbidden, map[string]any{"status": "Forbidden"})
		return
	}
	bearer := r.Header.Get("Authorization")
	if !strings.HasPrefix(bearer, "Bearer ") {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"status": "Unauthorized"})
		return
	}
	if strings.TrimSpace(strings.TrimPrefix(bearer, "Bearer ")) != secret {
		writeJSON(w, http.StatusForbidden, map[string]any{"status": "Forbidden"})
		return
	}

	healthy := false
	if s.hsHealth != nil {
		healthy = s.hsHealth()
	}
	hsVersion := "unknown"
	if healthy {
		if raw := s.hsVersionRaw(); raw != "" {
			hsVersion = raw
		}
	}
	status := "unhealthy"
	if healthy {
		status = "healthy"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":                      status,
		"headplane_version":           headplaneVersion,
		"headscale_canonical_version": hsVersion,
		"internal_versions": map[string]any{
			"go":   runtime.Version(),
			"os":   runtime.GOOS,
			"arch": runtime.GOARCH,
		},
	})
}
