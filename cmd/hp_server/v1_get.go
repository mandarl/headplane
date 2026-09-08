package main

// Phase 4 GET handlers for /admin/api/v1/*. Each mirrors the old React
// Router loader of the same route; auth failures are 401 JSON (the SPA's
// apiGet turns 401 into a /login redirect, mirroring the old loaders'
// redirect).

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/tale/headplane/internal/auth"
	"github.com/tale/headplane/internal/hsapi"
)

// handleBoot mirrors app/routes/_index/route.tsx: session/user profile,
// capability booleans, Headscale health, base URL, config availability.
func (s *Server) handleBoot(w http.ResponseWriter, r *http.Request) {
	api, p, err := s.hsClientFor(r)
	if err != nil {
		if err == errNeedAuth {
			writeError(w, http.StatusUnauthorized, "Authentication required.")
		} else {
			writeError(w, http.StatusInternalServerError, "No Headscale API key is configured for this session.")
		}
		return
	}

	// A healthy Headscale re-validates the key on every boot: a 401 means
	// the key was revoked server-side, so the session is destroyed.
	isHealthy := api.Health()
	if isHealthy {
		if _, err := api.ListAPIKeys(); err != nil {
			var apiErr *hsapi.APIError
			if isAPIErrorStatus(err, &apiErr, http.StatusUnauthorized) {
				w.Header().Set("Set-Cookie", s.authSvc.DestroySession(r))
				writeError(w, http.StatusUnauthorized, "Headscale API key is no longer valid.")
				return
			}
			writeAPIError(w, err)
			return
		}
	}

	user := map[string]any{
		"kind":    p.Kind,
		"subject": p.Subject,
		"name":    p.DisplayName,
	}
	if p.Kind == "oidc" {
		users, err := api.ListUsers("", "", "")
		if err != nil {
			writeAPIError(w, err)
			return
		}
		linked := false
		for _, u := range users {
			if hsapi.OIDCSubject(u) == p.Subject {
				linked = true
				break
			}
		}
		if !linked {
			if _, err := s.authSvc.UnlinkHeadscaleUser(p.UserID); err != nil {
				writeAPIError(w, err)
				return
			}
		}
		user["email"] = p.ProfileEmail
		user["username"] = p.ProfileUsername
		user["picture"] = p.ProfilePicture
		user["headscaleUserId"] = p.HeadscaleUserID
		user["role"] = string(p.Role)
	} else {
		user["name"] = p.DisplayName
	}

	can := func(caps auth.Capabilities) bool { return s.authSvc.Can(p, caps) }
	writeJSON(w, http.StatusOK, map[string]any{
		"user": user,
		"access": map[string]any{
			"dns":      can(auth.CapReadNetwork),
			"machines": can(auth.CapWriteMachines) || can(auth.CapReadMachines),
			"policy":   can(auth.CapReadPolicy),
			"settings": can(auth.CapGenerateAuthKeys) || can(auth.CapReadUsers),
			"ui":       can(auth.CapReadMachines),
			"users":    can(auth.CapReadUsers),
		},
		"baseUrl":         s.headscaleBaseURL(),
		"configAvailable": s.hsCfg.Readable(),
		"isHealthy":       isHealthy,
		"isDebug":         s.cfg.Debug,
	})
}

// headscaleBaseURL mirrors base_url: config.headscale.public_url ??
// config.headscale.url.
func (s *Server) headscaleBaseURL() string {
	if s.cfg.Headscale.PublicURL != "" {
		return s.cfg.Headscale.PublicURL
	}
	return s.cfg.Headscale.URL
}

// isAPIErrorStatus reports whether err is an *hsapi.APIError with the
// given status, storing it in out.
func isAPIErrorStatus(err error, out **hsapi.APIError, status int) bool {
	var apiErr *hsapi.APIError
	if errors.As(err, &apiErr) {
		*out = apiErr
		return apiErr.StatusCode == status
	}
	return false
}

// handleHome mirrors app/routes/home/route.tsx: it links the OIDC
// principal's Headscale user when the subject matches, then tells the SPA
// where to go.
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	api, p, err := s.hsClientFor(r)
	if err != nil {
		if err == errNeedAuth {
			writeError(w, http.StatusUnauthorized, "Authentication required.")
		} else {
			writeError(w, http.StatusInternalServerError, "No Headscale API key is configured for this session.")
		}
		return
	}

	if p.Kind == "oidc" {
		users, err := api.ListUsers("", "", "")
		if err != nil {
			writeAPIError(w, err)
			return
		}
		for _, u := range users {
			if hsapi.OIDCSubject(u) == p.Subject {
				if _, err := s.authSvc.LinkHeadscaleUser(p.UserID, strOf(u["id"])); err != nil {
					writeAPIError(w, err)
					return
				}
				break
			}
		}
	}

	resp := map[string]any{}
	if s.authSvc.Can(p, auth.CapReadMachines) {
		resp["redirect"] = "/machines"
	} else {
		resp["redirect"] = "/"
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleHomeLink mirrors the /home/link action: links an explicit
// Headscale user id to the OIDC principal.
func (s *Server) handleHomeLink(w http.ResponseWriter, r *http.Request) {
	p := s.requirePrincipal(w, r)
	if p == nil {
		return
	}
	if p.Kind != "oidc" {
		writeJSON(w, http.StatusOK, map[string]any{"redirect": "/"})
		return
	}
	body := readJSONBody(r)
	if id := formStr(body, "headscale_user_id"); id != "" {
		if _, err := s.authSvc.LinkHeadscaleUser(p.UserID, id); err != nil {
			writeAPIError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"redirect": "/"})
}

// handleMachines mirrors the machines overview loader: cached nodes/users,
// mapped routing/expiry data, capability flags.
func (s *Server) handleMachines(w http.ResponseWriter, r *http.Request) {
	api, p, err := s.hsClientFor(r)
	if err != nil {
		if err == errNeedAuth {
			writeError(w, http.StatusUnauthorized, "Authentication required.")
		} else {
			writeError(w, http.StatusInternalServerError, "No Headscale API key is configured for this session.")
		}
		return
	}

	nodes, users, err := s.liveSnapshots(r.Context(), api)
	if err != nil {
		writeAPIError(w, err)
		return
	}

	var magic *string
	if s.hsCfg.Readable() && s.hsCfg.MagicDNS {
		magic = ptrOrNil(s.hsCfg.BaseDomain)
	}

	resp := map[string]any{
		"agent":                   nil, // Phase 5: no agent integration yet
		"headscaleUserId":         p.HeadscaleUserID,
		"magic":                   magic,
		"nodes":                   nodes,
		"populatedNodes":          mapNodes(nodes),
		"preAuth":                 s.authSvc.Can(p, auth.CapGenerateAuthKeys),
		"publicServer":            ptrOrNil(s.cfg.Headscale.PublicURL),
		"rdpGatewayEnabled":       false, // Phase 5: no RDP gateway yet
		"server":                  s.cfg.Headscale.URL,
		"supportsNodeOwnerChange": !s.getHsCaps().NodeOwnerIsImmutable,
		"users":                   users,
		"writable":                s.authSvc.Can(p, auth.CapWriteMachines),
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleMachine mirrors the machine detail loader: the cached node by id,
// mapped users/tags, capability flags. Service/agent overrides are
// Phase 5 and read as empty.
func (s *Server) handleMachine(w http.ResponseWriter, r *http.Request, id string) {
	api, _, err := s.hsClientFor(r)
	if err != nil {
		if err == errNeedAuth {
			writeError(w, http.StatusUnauthorized, "Authentication required.")
		} else {
			writeError(w, http.StatusInternalServerError, "No Headscale API key is configured for this session.")
		}
		return
	}

	nodes, users, err := s.liveSnapshots(r.Context(), api)
	if err != nil {
		writeAPIError(w, err)
		return
	}

	var found hsapi.Node
	for _, n := range nodes {
		if strOf(n["id"]) == id {
			found = n
			break
		}
	}
	if found == nil {
		writeError(w, http.StatusNotFound, "Machine not found.")
		return
	}

	var magic *string
	if s.hsCfg.Readable() && s.hsCfg.MagicDNS {
		magic = ptrOrNil(s.hsCfg.BaseDomain)
	}

	// tags: this node's tags, sorted (mirrors [...node.tags].toSorted()).
	// existingTags: the sorted unique tag set across all nodes, for the
	// tag dialog autocomplete (mirrors sortNodeTags(nodes)).
	nodeTags := append([]string{}, strListOf(found["tags"])...)
	sort.Strings(nodeTags)
	seen := map[string]bool{}
	existingTags := []string{}
	for _, n := range nodes {
		for _, t := range strListOf(n["tags"]) {
			if !seen[t] {
				seen[t] = true
				existingTags = append(existingTags, t)
			}
		}
	}
	sort.Strings(existingTags)

	resp := map[string]any{
		"agent":                   nil, // Phase 5: no agent integration yet
		"existingTags":            existingTags,
		"magic":                   magic,
		"node":                    populateNode(found),
		"rdpGatewayEnabled":       false, // Phase 5: no RDP gateway yet
		"serviceOverrides":        map[string]any{},
		"stats":                   nil, // Phase 5: no agent lookups yet
		"supportsNodeOwnerChange": !s.getHsCaps().NodeOwnerIsImmutable,
		"tags":                    nodeTags,
		"users":                   users,
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleUsers mirrors the users overview loader: Headplane users merged
// with Headscale users/nodes, link options, and capability flags. Upstream
// failures degrade to apiError (200) instead of failing the page.
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	p := s.requirePrincipal(w, r)
	if p == nil {
		return
	}
	if !s.authSvc.Can(p, auth.CapReadUsers) {
		writeError(w, http.StatusForbidden, "You do not have permission to view users.")
		return
	}
	api := s.apiClientFor(w, p)
	if api == nil {
		return
	}

	var apiError *string
	var hsUsers []hsapi.User
	var nodes []hsapi.Node
	// Upstream failures degrade to apiError (200) instead of failing the
	// page, like the TS loader's catch.
	if n, u, err := s.liveSnapshots(r.Context(), api); err != nil {
		msg := friendlyUpstreamMessageOf(err)
		apiError = &msg
	} else {
		nodes, hsUsers = n, u
	}

	hpUsers, err := s.authSvc.ListUsers()
	if err != nil {
		writeAPIError(w, err)
		return
	}

	claimed, err := s.authSvc.ClaimedHeadscaleUserIds()
	if err != nil {
		writeAPIError(w, err)
		return
	}

	useGravatar := s.oidcGravatarPictures()

	type userView struct {
		ID              string           `json:"id"`
		Sub             string           `json:"sub"`
		Name            *string          `json:"name"`
		Email           *string          `json:"email"`
		Role            string           `json:"role"`
		HeadscaleUserID *string          `json:"headscaleUserId"`
		CreatedAt       *string          `json:"createdAt"`
		LastLoginAt     *string          `json:"lastLoginAt"`
		ProfilePicURL   *string          `json:"profilePicUrl"`
		Machines        []map[string]any `json:"machines"`
		Provider        *string          `json:"provider"`
	}
	headplaneUsers := []userView{}
	for _, hp := range hpUsers {
		role := hp.Role
		if _, ok := auth.Roles[auth.Role(role)]; !ok {
			role = "member"
		}
		var linked *hsapi.User
		var provider *string
		var machines []map[string]any
		if hp.HeadscaleUserID != nil {
			for i, u := range hsUsers {
				if strOf(u["id"]) == *hp.HeadscaleUserID {
					linked = &hsUsers[i]
					break
				}
			}
			if linked != nil {
				pv := strOf((*linked)["provider"])
				provider = &pv
				machines = userMachines(nodes, *hp.HeadscaleUserID)
			} else {
				machines = []map[string]any{}
			}
		} else {
			machines = []map[string]any{}
		}
		var pic *string
		if linked != nil {
			email := ptrOrNil(strOf((*linked)["email"]))
			picURL := ptrOrNil(strOf((*linked)["profilePicUrl"]))
			pic = resolveProfilePic(useGravatar, email, picURL)
		} else {
			pic = resolveProfilePic(useGravatar, hp.Email, nil)
		}
		headplaneUsers = append(headplaneUsers, userView{
			ID: hp.ID, Sub: hp.Sub, Name: hp.Name, Email: hp.Email,
			Role: role, HeadscaleUserID: hp.HeadscaleUserID,
			CreatedAt: timePtrOrNil(hp.CreatedAt), LastLoginAt: timePtrOrNil(hp.LastLoginAt),
			ProfilePicURL: pic, Machines: machines, Provider: provider,
		})
	}
	// sort by name ?? sub
	for i := 0; i < len(headplaneUsers); i++ {
		for j := i + 1; j < len(headplaneUsers); j++ {
			ni := strOrPtr(headplaneUsers[i].Name, headplaneUsers[i].Sub)
			nj := strOrPtr(headplaneUsers[j].Name, headplaneUsers[j].Sub)
			if strings.ToLower(nj) < strings.ToLower(ni) {
				headplaneUsers[i], headplaneUsers[j] = headplaneUsers[j], headplaneUsers[i]
			}
		}
	}

	unlinked := []map[string]any{}
	for _, u := range hsUsers {
		if claimed[strOf(u["id"])] {
			continue
		}
		email := ptrOrNil(strOf(u["email"]))
		picURL := ptrOrNil(strOf(u["profilePicUrl"]))
		flat := make(map[string]any, len(u)+2)
		for k, v := range u {
			flat[k] = v
		}
		flat["machines"] = userMachines(nodes, strOf(u["id"]))
		flat["profilePicUrl"] = resolveProfilePic(useGravatar, email, picURL)
		unlinked = append(unlinked, flat)
	}
	for i := 0; i < len(unlinked); i++ {
		for j := i + 1; j < len(unlinked); j++ {
			if strings.ToLower(strOf(unlinked[j]["name"])) <
				strings.ToLower(strOf(unlinked[i]["name"])) {
				unlinked[i], unlinked[j] = unlinked[j], unlinked[i]
			}
		}
	}

	type linkOption struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Claimed bool   `json:"claimed"`
	}
	linkOptions := []linkOption{}
	for _, u := range hsUsers {
		id := strOf(u["id"])
		linkOptions = append(linkOptions, linkOption{ID: id, Name: headscaleUserDisplayName(u), Claimed: claimed[id]})
	}

	var magic *string
	if s.hsCfg.Readable() && s.hsCfg.MagicDNS {
		magic = ptrOrNil(s.hsCfg.BaseDomain)
	}

	var currentUserID *string
	if p.Kind == "oidc" {
		currentUserID = &p.UserID
	}
	var oidcCfg any
	if s.oidcSvc != nil {
		oidcCfg = map[string]any{"issuer": s.oidcSvc.Issuer()}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"headplaneUsers":         headplaneUsers,
		"unlinkedHeadscaleUsers": unlinked,
		"headscaleUsersForLink":  linkOptions,
		"magic":                  magic,
		"isOwner":                p.Kind == "oidc" && p.Role == "owner",
		"oidc":                   oidcCfg,
		"currentUserId":          currentUserID,
		"apiError":               apiError,
	})
}

// friendlyUpstreamMessageOf renders any error like friendlyUpstreamMessage.
func friendlyUpstreamMessageOf(err error) string {
	var apiErr *hsapi.APIError
	if errors.As(err, &apiErr) {
		return friendlyUpstreamMessage(apiErr)
	}
	var connErr *hsapi.ConnError
	if errors.As(err, &connErr) {
		return "Headscale is not reachable. Check that Headscale is running and the configured URL is correct."
	}
	return "An unexpected error occurred."
}

func strOrPtr(p *string, fallback string) string {
	if p != nil {
		return *p
	}
	return fallback
}

// oidcGravatarPictures mirrors the users-route check for
// oidc.profile_picture_source === "gravatar".
func (s *Server) oidcGravatarPictures() bool {
	return s.oidcSvc != nil && s.oidcSvc.ProfilePictureSource() == "gravatar"
}

// handleAcls mirrors the ACLs loader: read permission gate, policy get,
// and the "acl policy not found"/500 → empty-but-writable fallback.
func (s *Server) handleAcls(w http.ResponseWriter, r *http.Request) {
	api, p, err := s.hsClientFor(r)
	if err != nil {
		if err == errNeedAuth {
			writeError(w, http.StatusUnauthorized, "Authentication required.")
		} else {
			writeError(w, http.StatusInternalServerError, "No Headscale API key is configured for this session.")
		}
		return
	}
	if !s.authSvc.Can(p, auth.CapReadPolicy) {
		writeError(w, http.StatusForbidden, "You do not have permission to read the ACL policy.")
		return
	}

	policy, updatedAt, err := api.GetPolicy()
	if err != nil {
		var apiErr *hsapi.APIError
		if e, ok := err.(*hsapi.APIError); ok {
			apiErr = e
		}
		if apiErr != nil && (strings.Contains(apiErr.RawData, "acl policy not found") || apiErr.StatusCode == 500) {
			writeJSON(w, http.StatusOK, map[string]any{
				"policy":   "",
				"writable": true,
				"access":   s.authSvc.Can(p, auth.CapWritePolicy),
			})
			return
		}
		writeAPIError(w, err)
		return
	}

	var updated *string
	if updatedAt != nil {
		u := updatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
		updated = &u
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"policy":    policy,
		"updatedAt": updated,
		"writable":  updatedAt != nil,
		"access":    s.authSvc.Can(p, auth.CapWritePolicy),
	})
}

// handleDns mirrors the DNS overview loader: config-backed DNS settings.
// DNS mutation is Phase 5; this is the read side plus the access flags.
func (s *Server) handleDns(w http.ResponseWriter, r *http.Request) {
	p := s.requirePrincipal(w, r)
	if p == nil {
		return
	}
	if !s.hsCfg.Readable() {
		writeError(w, http.StatusInternalServerError, "No configuration is available")
		return
	}
	if !s.authSvc.Can(p, auth.CapReadNetwork) {
		writeError(w, http.StatusForbidden, "You do not have permission to view DNS settings.")
		return
	}

	prefixes := []string{s.hsCfg.PrefixesV4, s.hsCfg.PrefixesV6}
	split := map[string][]string{}
	for domain, addrs := range s.hsCfg.SplitDNS {
		split[domain] = addrs
	}
	records := []map[string]any{}
	for _, rec := range s.hsCfg.DNSRecords() {
		records = append(records, map[string]any{"name": rec.Name, "type": rec.Type, "value": rec.Value})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"prefixes":      prefixes,
		"magicDns":      s.hsCfg.MagicDNS,
		"baseDomain":    s.hsCfg.BaseDomain,
		"nameservers":   s.hsCfg.Nameservers,
		"splitDns":      split,
		"searchDomains": s.hsCfg.SearchDomains,
		"overrideDns":   s.hsCfg.OverrideDNS,
		"extraRecords":  records,
		"access":        s.authSvc.Can(p, auth.CapWriteNetwork),
		"writable":      s.hsCfg.Writable(),
	})
}

// handleSettings mirrors the settings overview loader.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	p := s.requirePrincipal(w, r)
	if p == nil {
		return
	}
	isOidcEnabled := false
	if s.oidcSvc != nil {
		if state, _, _ := s.oidcSvc.Status(); state == "ready" {
			isOidcEnabled = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"config":        s.hsCfg.Writable(),
		"isOidcEnabled": isOidcEnabled,
	})
}

// handleAuthKeys mirrors the TS auth-keys loader exactly: access flags, the
// key groups shaped as [{user, preAuthKeys}] (tag-only keys first, then one
// entry per user in user order), the per-user "missing" collection (legacy
// per-user listing only), and the Headscale URL for the setup command.
func (s *Server) handleAuthKeys(w http.ResponseWriter, r *http.Request) {
	api, p, err := s.hsClientFor(r)
	if err != nil {
		if err == errNeedAuth {
			writeError(w, http.StatusUnauthorized, "Authentication required.")
		} else {
			writeError(w, http.StatusInternalServerError, "No Headscale API key is configured for this session.")
		}
		return
	}

	canGenerateAny := s.authSvc.Can(p, auth.CapGenerateAuthKeys)
	canGenerateOwn := s.authSvc.Can(p, auth.CapGenerateOwnAuthKs)
	if !canGenerateAny && !canGenerateOwn {
		writeError(w, http.StatusForbidden, "You do not have permission to generate pre-auth keys.")
		return
	}

	var currentSubject *string
	if p.Kind == "oidc" {
		currentSubject = &p.Subject
	}

	type keyGroup struct {
		User        hsapi.User       `json:"user"`
		PreAuthKeys []map[string]any `json:"preAuthKeys"`
	}
	type missingEntry struct {
		User  hsapi.User `json:"user"`
		Error string     `json:"error"`
	}
	groups := []keyGroup{}
	missing := []missingEntry{}

	if s.getHsCaps().PreAuthKeysHaveStableIds {
		// 0.28+: one global listing, grouped by user.
		all, err := api.ListPreAuthKeysAll()
		if err != nil {
			writeAPIError(w, err)
			return
		}
		byUser := map[string][]map[string]any{}
		var tagOnly []map[string]any
		for _, k := range all {
			var uid string
			if ku, ok := k["user"].(map[string]any); ok {
				uid = strOf(ku["id"])
			}
			if uid == "" {
				tagOnly = append(tagOnly, k)
			} else {
				byUser[uid] = append(byUser[uid], k)
			}
		}
		if len(tagOnly) > 0 {
			groups = append(groups, keyGroup{User: nil, PreAuthKeys: tagOnly})
		}
		for _, u := range mustUsers(api) {
			if ks := byUser[strOf(u["id"])]; len(ks) > 0 {
				groups = append(groups, keyGroup{User: u, PreAuthKeys: ks})
			}
		}
	} else {
		// Older Headscale: list per user; failures are collected per user.
		for _, u := range mustUsers(api) {
			ukeys, err := api.ListPreAuthKeysForUser(strOf(u["id"]))
			if err != nil {
				missing = append(missing, missingEntry{User: u, Error: friendlyUpstreamMessageOf(err)})
				continue
			}
			if len(ukeys) > 0 {
				groups = append(groups, keyGroup{User: u, PreAuthKeys: ukeys})
			}
		}
	}

	url := s.cfg.Headscale.PublicURL
	if url == "" {
		url = s.cfg.Headscale.URL
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access":          canGenerateAny || canGenerateOwn,
		"currentSubject":  currentSubject,
		"keys":            groups,
		"missing":         missing,
		"selfServiceOnly": canGenerateOwn && !canGenerateAny,
		"url":             url,
		"users":           mustUsers(api),
	})
}

// mustUsers fetches the Headscale user list for the auth-keys page; a
// failure degrades to an empty list (the page renders keys regardless).
func mustUsers(api *hsapi.Client) []hsapi.User {
	users, err := api.ListUsers("", "", "")
	if err != nil {
		return []hsapi.User{}
	}
	return users
}

// handleRestrictions mirrors the restrictions overview loader: the OIDC
// allow-lists from the Headscale config plus access flags.
func (s *Server) handleRestrictions(w http.ResponseWriter, r *http.Request) {
	p := s.requirePrincipal(w, r)
	if p == nil {
		return
	}
	if !s.authSvc.Can(p, auth.CapReadUsers) {
		writeError(w, http.StatusForbidden, "You do not have permission to view IAM settings.")
		return
	}
	if s.hsCfg.OIDC == nil {
		writeError(w, http.StatusNotImplemented, "OIDC is not configured on this Headscale instance.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"settings": map[string]any{
			"domains": dedupeStrings(s.hsCfg.OIDC.AllowedDomains),
			"groups":  dedupeStrings(s.hsCfg.OIDC.AllowedGroups),
			"users":   dedupeStrings(s.hsCfg.OIDC.AllowedUsers),
		},
		"access":   s.authSvc.Can(p, auth.CapConfigureIAM),
		"writable": s.hsCfg.Writable(),
	})
}

// dedupeStrings mirrors the TS [...new Set(list)] on the OIDC allow-lists.
func dedupeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}
