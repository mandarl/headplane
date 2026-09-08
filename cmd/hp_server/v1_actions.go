package main

// Phase 4 POST/PATCH handlers for /admin/api/v1/*. Each mirrors the old
// React Router action of the same route. Validation failures keep the old
// status codes and messages; upstream Headscale failures go through
// writeAPIError; successful mutations refresh the live store like the TS
// hsLive.refresh calls did.

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tale/headplane/internal/auth"
	"github.com/tale/headplane/internal/hsapi"
	"github.com/tale/headplane/internal/live"
)

// handleMachineActions mirrors machineAction (app/routes/machines/machine-actions.ts).
func (s *Server) handleMachineActions(w http.ResponseWriter, r *http.Request) {
	api, p, err := s.hsClientFor(r)
	if err != nil {
		if err == errNeedAuth {
			writeError(w, http.StatusUnauthorized, "Authentication required.")
		} else {
			writeError(w, http.StatusInternalServerError, "No Headscale API key is configured for this session.")
		}
		return
	}
	body := readJSONBody(r)
	action := formStr(body, "action_id")
	if action == "" {
		writeError(w, http.StatusBadRequest, "Missing `action_id` in the form data.")
		return
	}

	// Fast track register: it needs no existing machine.
	if action == "register" {
		if !s.authSvc.Can(p, auth.CapWriteMachines) {
			writeError(w, http.StatusForbidden, "You do not have permission to manage machines")
			return
		}
		key := formStr(body, "register_key")
		if key == "" {
			writeError(w, http.StatusBadRequest, "Missing `register_key` in the form data.")
			return
		}
		user := formStr(body, "user")
		if user == "" {
			writeError(w, http.StatusBadRequest, "Missing `user` in the form data.")
			return
		}
		node, err := api.RegisterNode(user, key)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		if err := s.liveStore.Refresh(r.Context(), live.NodesResource, api); err != nil {
			s.logger.Warn("live refresh after register failed", "error", err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"redirect": "/machines/" + strOf(node["id"])})
		return
	}

	nodeID := formStr(body, "node_id")
	if nodeID == "" {
		writeError(w, http.StatusBadRequest, "Missing `node_id` in the form data.")
		return
	}
	node, err := api.GetNode(nodeID)
	if err != nil {
		writeAPIError(w, err)
		return
	}
	if node == nil {
		writeError(w, http.StatusNotFound, "Machine with ID "+nodeID+" not found")
		return
	}
	if !s.authSvc.CanManageNode(p, ptrOrNil(hsapi.NodeUserID(node))) {
		writeError(w, http.StatusForbidden, "You do not have permission to act on this machine")
		return
	}

	refresh := func() {
		if err := s.liveStore.Refresh(r.Context(), live.NodesResource, api); err != nil {
			s.logger.Warn("live refresh after machine action failed", "action", action, "error", err)
		}
	}

	switch action {
	case "rename":
		{
			name := formStr(body, "name")
			if name == "" {
				writeError(w, http.StatusBadRequest, "Missing `name` in the form data.")
				return
			}
			if err := api.RenameNode(nodeID, name); err != nil {
				writeAPIError(w, err)
				return
			}
			refresh()
			writeJSON(w, http.StatusOK, map[string]any{"message": "Machine renamed"})
		}
	case "delete":
		{
			if err := api.DeleteNode(nodeID); err != nil {
				writeAPIError(w, err)
				return
			}
			refresh()
			writeJSON(w, http.StatusOK, map[string]any{"redirect": "/machines"})
		}
	case "expire":
		{
			if err := api.ExpireNode(nodeID); err != nil {
				writeAPIError(w, err)
				return
			}
			refresh()
			writeJSON(w, http.StatusOK, map[string]any{"message": "Machine expired"})
		}
	case "update_tags":
		{
			rawTags, present := body["tags"]
			if !present || rawTags == nil {
				writeError(w, http.StatusBadRequest, "Missing `tags` in the form data.")
				return
			}
			tags := []string{}
			for _, t := range strings.Split(formStr(body, "tags"), ",") {
				if trimmed := strings.TrimSpace(t); trimmed != "" {
					tags = append(tags, trimmed)
				}
			}
			if err := api.SetNodeTags(nodeID, tags); err != nil {
				var apiErr *hsapi.APIError
				if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest {
					writeJSON(w, http.StatusBadRequest, map[string]any{
						"success": false,
						"error":   "One or more tags are not defined in your ACL policy. Please add them to your policy before assigning them to a machine.",
					})
					return
				}
				writeAPIError(w, err)
				return
			}
			refresh()
			writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "Tags updated"})
		}
	case "update_routes":
		{
			routes := formStr(body, "routes")
			if routes == "" {
				writeError(w, http.StatusBadRequest, "Missing `routes` in the form data.")
				return
			}
			allRoutes := []string{}
			for _, rt := range strings.Split(routes, ",") {
				allRoutes = append(allRoutes, strings.TrimSpace(rt))
			}
			if len(allRoutes) == 0 {
				writeError(w, http.StatusBadRequest, "No routes provided to update")
				return
			}
			enabled := formStr(body, "enabled")
			if _, present := body["enabled"]; !present {
				writeError(w, http.StatusBadRequest, "Missing `enabled` in the form data.")
				return
			}
			approved := strListOf(node["approvedRoutes"])
			if enabled == "true" {
				for _, rt := range allRoutes {
					if !containsStr(approved, rt) {
						approved = append(approved, rt)
					}
				}
			} else {
				kept := approved[:0]
				for _, a := range approved {
					if !containsStr(allRoutes, a) {
						kept = append(kept, a)
					}
				}
				approved = kept
			}
			if err := api.ApproveRoutes(nodeID, approved); err != nil {
				writeAPIError(w, err)
				return
			}
			refresh()
			writeJSON(w, http.StatusOK, map[string]any{"message": "Routes updated"})
		}
	case "reassign":
		{
			user := formStr(body, "user_id")
			if user == "" {
				writeError(w, http.StatusBadRequest, "Missing `user_id` in the form data.")
				return
			}
			if s.getHsCaps().NodeOwnerIsImmutable {
				writeError(w, http.StatusBadRequest, "Reassigning a node owner is no longer supported on this Headscale version.")
				return
			}
			if err := api.ReassignNodeUser(nodeID, user); err != nil {
				writeAPIError(w, err)
				return
			}
			refresh()
			writeJSON(w, http.StatusOK, map[string]any{"message": "Machine reassigned"})
		}
	case "update_service_description":
		{
			// Phase 5: service description overrides are local Headplane state
			// layered on Hostinfo; not implemented in Phase 4.
			writeJSON(w, http.StatusNotImplemented, map[string]any{
				"success": false,
				"error":   "Service description overrides are not implemented in this build yet.",
			})
		}
	default:
		writeError(w, http.StatusBadRequest, "Invalid action")
	}
}

// handleUserActions mirrors userAction (app/routes/users/user-actions.ts).
func (s *Server) handleUserActions(w http.ResponseWriter, r *http.Request) {
	p := s.requirePrincipal(w, r)
	if p == nil {
		return
	}
	if !s.authSvc.Can(p, auth.CapWriteUsers) {
		writeError(w, http.StatusForbidden, "You do not have permission to update users")
		return
	}
	body := readJSONBody(r)
	action := formStr(body, "action_id")
	if action == "" {
		// Note: the old route used 404 here, preserved as-is.
		writeError(w, http.StatusNotFound, "Missing `action_id` in the form data.")
		return
	}
	api := s.apiClientFor(w, p)
	if api == nil {
		return
	}

	refresh := func() {
		if err := s.liveStore.Refresh(r.Context(), live.UsersResource, api); err != nil {
			s.logger.Warn("live refresh after user action failed", "action", action, "error", err)
		}
	}

	switch action {
	case "create_user":
		{
			name := formStr(body, "username")
			if name == "" {
				writeError(w, http.StatusBadRequest, "Missing `username` in the form data.")
				return
			}
			var email, displayName *string
			if v := formStr(body, "email"); v != "" {
				email = &v
			}
			if v := formStr(body, "display_name"); v != "" {
				displayName = &v
			}
			if _, err := api.CreateUser(name, email, displayName); err != nil {
				writeAPIError(w, err)
				return
			}
			refresh()
			writeJSON(w, http.StatusOK, map[string]any{"message": "User created successfully"})
		}
	case "delete_user":
		{
			id := formStr(body, "headscale_user_id")
			if id == "" {
				writeError(w, http.StatusBadRequest, "Missing `headscale_user_id` in the form data.")
				return
			}
			if err := api.DeleteUser(id); err != nil {
				writeAPIError(w, err)
				return
			}
			refresh()
			writeJSON(w, http.StatusOK, map[string]any{"message": "User deleted successfully"})
		}
	case "rename_user":
		{
			id := formStr(body, "headscale_user_id")
			newName := formStr(body, "new_name")
			if id == "" || newName == "" {
				writeJSON(w, http.StatusBadRequest, map[string]any{"success": false})
				return
			}
			users, err := api.ListUsers(id, "", "")
			if err != nil {
				writeAPIError(w, err)
				return
			}
			var found hsapi.User
			for _, u := range users {
				if strOf(u["id"]) == id {
					found = u
					break
				}
			}
			if found == nil {
				writeError(w, http.StatusBadRequest, "No user found with id: "+id)
				return
			}
			if strOf(found["provider"]) == "oidc" {
				writeError(w, http.StatusForbidden, "Users managed by OIDC cannot be renamed")
				return
			}
			if err := api.RenameUser(id, newName); err != nil {
				writeAPIError(w, err)
				return
			}
			refresh()
			writeJSON(w, http.StatusOK, map[string]any{"message": "User renamed successfully"})
		}
	case "reassign_user":
		{
			id := formStr(body, "headplane_user_id")
			newRole := formStr(body, "new_role")
			if id == "" || newRole == "" {
				writeError(w, http.StatusBadRequest, "Missing `headplane_user_id` or `new_role` in the form data.")
				return
			}
			ok, err := s.authSvc.ReassignUser(id, newRole)
			if err != nil {
				writeAPIError(w, err)
				return
			}
			if !ok {
				writeError(w, http.StatusInternalServerError, "Failed to reassign user role.")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"message": "User reassigned successfully"})
		}
	case "transfer_ownership":
		{
			if p.Kind != "oidc" || p.Role != "owner" {
				writeError(w, http.StatusForbidden, "Only the owner can transfer ownership.")
				return
			}
			id := formStr(body, "headplane_user_id")
			if id == "" {
				writeError(w, http.StatusBadRequest, "Missing `headplane_user_id` in the form data.")
				return
			}
			ok, err := s.authSvc.TransferOwnership(p.UserID, id)
			if err != nil {
				writeAPIError(w, err)
				return
			}
			if !ok {
				writeError(w, http.StatusInternalServerError, "Failed to transfer ownership.")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"message": "Ownership transferred successfully"})
		}
	case "link_user":
		{
			hpID := formStr(body, "headplane_user_id")
			hsID := formStr(body, "headscale_user_id")
			if hpID == "" || hsID == "" {
				writeError(w, http.StatusBadRequest, "Missing `headplane_user_id` or `headscale_user_id` in the form data.")
				return
			}
			linked, err := s.authSvc.LinkHeadscaleUser(hpID, hsID)
			if err != nil {
				writeAPIError(w, err)
				return
			}
			if !linked {
				writeError(w, http.StatusConflict, "That Headscale user is already linked to another account.")
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"message": "Headscale user linked successfully"})
		}
	default:
		writeError(w, http.StatusBadRequest, "Invalid `action_id` provided.")
	}
}

// handleAclsPatch mirrors the ACL editor action: write permission gate,
// policy body validation, and the AclActionResult shapes.
func (s *Server) handleAclsPatch(w http.ResponseWriter, r *http.Request) {
	api, p, err := s.hsClientFor(r)
	if err != nil {
		if err == errNeedAuth {
			writeError(w, http.StatusUnauthorized, "Authentication required.")
		} else {
			writeError(w, http.StatusInternalServerError, "No Headscale API key is configured for this session.")
		}
		return
	}
	if !s.authSvc.Can(p, auth.CapWritePolicy) {
		writeError(w, http.StatusForbidden, "You do not have permission to write to the ACL policy")
		return
	}
	body := readJSONBody(r)
	policy, ok := body["policy"].(string)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "Missing `policy` in the form data.", "policy": nil, "updatedAt": nil,
		})
		return
	}
	saved, updatedAt, err := api.SetPolicy(policy)
	if err != nil {
		var apiErr *hsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": friendlyUpstreamMessage(apiErr), "policy": nil, "updatedAt": nil,
			})
			return
		}
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "error": nil, "policy": saved,
		"updatedAt": updatedAt.UTC().Format(time.RFC3339),
	})
}

// handleAuthKeyActions mirrors the auth-keys actions route.
func (s *Server) handleAuthKeyActions(w http.ResponseWriter, r *http.Request) {
	api, p, err := s.hsClientFor(r)
	if err != nil {
		if err == errNeedAuth {
			writeError(w, http.StatusUnauthorized, "Authentication required.")
		} else {
			writeError(w, http.StatusInternalServerError, "No Headscale API key is configured for this session.")
		}
		return
	}
	body := readJSONBody(r)
	action := formStr(body, "action_id")
	if action == "" {
		writeError(w, http.StatusBadRequest, "Missing `action_id` in the form data.")
		return
	}

	canGenerateAny := s.authSvc.Can(p, auth.CapGenerateAuthKeys)
	canGenerateOwn := s.authSvc.Can(p, auth.CapGenerateOwnAuthKs)
	var currentSubject *string
	if p.Kind == "oidc" {
		currentSubject = &p.Subject
	}

	// checkSelfServiceOwnership mirrors the TS helper: self-service-only
	// principals may only touch keys for their own Headscale user.
	checkSelfServiceOwnership := func(headscaleUserID string) bool {
		if !canGenerateOwn || canGenerateAny {
			return true
		}
		if p.HeadscaleUserID != nil && *p.HeadscaleUserID == headscaleUserID {
			return true
		}
		if currentSubject == nil {
			return false
		}
		users, err := api.ListUsers(headscaleUserID, "", "")
		if err != nil {
			return false
		}
		for _, u := range users {
			if strOf(u["id"]) == headscaleUserID && hsapi.OIDCSubject(u) == *currentSubject {
				return true
			}
		}
		return false
	}

	switch action {
	case "add_preauthkey":
		{
			user := formStr(body, "user")
			aclTagsRaw := formStr(body, "acl_tags")
			var aclTags []string
			for _, t := range strings.Split(aclTagsRaw, ",") {
				if trimmed := strings.TrimSpace(t); trimmed != "" {
					aclTags = append(aclTags, trimmed)
				}
			}
			if user == "" && len(aclTags) == 0 {
				writeError(w, http.StatusBadRequest, "Must specify either a user or ACL tags.")
				return
			}
			if user != "" && !checkSelfServiceOwnership(user) {
				writeError(w, http.StatusForbidden, "You do not have permission to generate auth keys for this user.")
				return
			}

			expiry := formStr(body, "expiry")
			if expiry == "" {
				writeError(w, http.StatusBadRequest, "Missing `expiry` in the form data.")
				return
			}
			reusable := formStr(body, "reusable")
			if reusable == "" {
				writeError(w, http.StatusBadRequest, "Missing `reusable` in the form data.")
				return
			}
			ephemeral := formStr(body, "ephemeral")
			if ephemeral == "" {
				writeError(w, http.StatusBadRequest, "Missing `ephemeral` in the form data.")
				return
			}

			// The expiry field is "<days> ..." (e.g. "30 days"); only the day
			// count matters, mirroring Number(expiry.toString().split(" ")[0]).
			days, err := strconv.Atoi(strings.Split(expiry, " ")[0])
			if err != nil {
				writeError(w, http.StatusBadRequest, "Invalid `expiry` in the form data.")
				return
			}
			var expiration *time.Time
			if days > 0 {
				t := time.Now().Add(time.Duration(days) * 24 * time.Hour)
				expiration = &t
			}

			var userPtr *string
			if user != "" {
				userPtr = &user
			}
			created, err := api.CreatePreAuthKey(userPtr, ephemeral == "on", reusable == "on", expiration, aclTags)
			if err != nil {
				writeAPIError(w, err)
				return
			}
			// The dialog interpolates `key` into a `tailscale up --authkey`
			// command, so it is the key string, like the old action's key.key.
			writeJSON(w, http.StatusOK, map[string]any{"success": true, "key": strOf(created["key"])})
		}
	case "expire_preauthkey":
		{
			keyID := formStr(body, "key_id")
			key := formStr(body, "key")
			userID := formStr(body, "user_id")
			if keyID == "" {
				writeError(w, http.StatusBadRequest, "Missing `key_id` in the form data.")
				return
			}
			if key == "" {
				writeError(w, http.StatusBadRequest, "Missing `key` in the form data.")
				return
			}
			if userID == "" {
				writeError(w, http.StatusBadRequest, "Missing `user_id` in the form data.")
				return
			}
			if !checkSelfServiceOwnership(userID) {
				writeError(w, http.StatusForbidden, "You do not have permission to expire auth keys for this user.")
				return
			}
			if err := api.ExpirePreAuthKey(keyID, userID, key); err != nil {
				writeAPIError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, "Pre-auth key expired")
		}
	default:
		writeError(w, http.StatusBadRequest, "Invalid `action_id` provided.")
	}
}

func containsStr(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// handleAgent mirrors the TS agent loader's disabled branch: the agent is
// not part of the Go server yet (Phase 5), so the page renders its
// "not enabled" notice with the reason.
func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	if s.requirePrincipal(w, r) == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": false,
		"reason":  "The Headplane agent is not enabled in this build",
	})
}

// handleAgentSyncStub answers the agent sync action while the agent is
// unimplemented. The SPA passes {success, error} through untouched.
func (s *Server) handleAgentSyncStub(w http.ResponseWriter, r *http.Request) {
	if s.requirePrincipal(w, r) == nil {
		return
	}
	writeJSON(w, http.StatusNotImplemented, map[string]any{
		"success": false,
		"error":   "The Headplane agent is not enabled in this build",
	})
}

// handleConfigActionStub answers Phase 5 config-mutation actions (DNS,
// auth restrictions) with an explicit 501 JSON string, matching the plain
// string payloads those actions' clientActions expect.
func (s *Server) handleConfigActionStub(w http.ResponseWriter, r *http.Request, what string) {
	if s.requirePrincipal(w, r) == nil {
		return
	}
	writeJSON(w, http.StatusNotImplemented,
		what+" changes are not yet supported by the Go server.")
}
