package main

// Data-mapping helpers for the v1 API: the Go port of
// app/utils/node-info.ts (mapNodes/populateNode), the profile-picture and
// display-name logic in the users route, and the OIDC unlink detection from
// the boot route.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/tale/headplane/internal/auth"
	"github.com/tale/headplane/internal/hsapi"
)

// goZeroTimes mirrors GO_ZERO_TIMES: Headscale serializes the zero time in
// a few shapes depending on version, all meaning "no expiry".
var goZeroTimes = map[string]bool{
	"0001-01-01 00:00:00":  true,
	"0001-01-01T00:00:00Z": true,
}

// populateNode mirrors populateNode in node-info.ts: it derives the
// client-visible routing/expiry fields from the raw node object. The raw
// node is kept verbatim under the same keys the TS spread kept. stats is
// the agent's hostinfo keyed by node key; the node's payload lands on
// `hostInfo` (nil when the agent is disabled or has no data for the node),
// mirroring mapNodes' stats argument.
func populateNode(node hsapi.Node, stats map[string]json.RawMessage) map[string]any {
	out := make(map[string]any, len(node)+6)
	for k, v := range node {
		out[k] = v
	}

	// availableRoutes: unique route prefixes across all hostinfo routes.
	seen := map[string]bool{}
	available := []string{}
	if routes, ok := node["routes"].([]any); ok {
		for _, r := range routes {
			if rm, ok := r.(map[string]any); ok {
				if prefix, ok := rm["prefix"].(string); ok && prefix != "" && !seen[prefix] {
					seen[prefix] = true
					available = append(available, prefix)
				}
			}
		}
	}
	out["availableRoutes"] = available

	approved := strListOf(node["approvedRoutes"])
	out["approvedRoutes"] = approved
	out["customRouting"] = len(approved) > 0

	expiry := strOf(node["expiry"])
	expired := false
	if expiry != "" && !goZeroTimes[expiry] {
		if t, err := time.Parse(time.RFC3339, expiry); err == nil {
			expired = t.Before(time.Now())
		}
	}
	out["expired"] = expired

	var hostInfo any
	if raw, ok := stats[strOf(node["nodeKey"])]; ok {
		hostInfo = raw
	}
	out["hostInfo"] = hostInfo
	return out
}

// mapNodes mirrors mapNodes: populate every node, attaching agent stats.
func mapNodes(nodes []hsapi.Node, stats map[string]json.RawMessage) []map[string]any {
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, populateNode(n, stats))
	}
	return out
}

// gravatarURL mirrors gravatarUrl: SHA-256 of the trimmed, lowercased
// email, identicon fallback.
func gravatarURL(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return "https://www.gravatar.com/avatar/" + hex.EncodeToString(sum[:]) + "?s=200&d=identicon&r=x"
}

// resolveProfilePic mirrors resolveProfilePic in the users route.
func resolveProfilePic(useGravatar bool, email, profilePicURL *string) *string {
	if useGravatar {
		if email != nil && *email != "" {
			u := gravatarURL(*email)
			return &u
		}
		return nil
	}
	return profilePicURL
}

// headscaleUserDisplayName mirrors getUserDisplayName: tagged-devices
// renders as "Tag-owned", otherwise name || displayName || email || id.
func headscaleUserDisplayName(u hsapi.User) string {
	if strOf(u["name"]) == "tagged-devices" {
		return "Tag-owned"
	}
	for _, k := range []string{"name", "displayName", "email", "id"} {
		if s := strOf(u[k]); s != "" {
			return s
		}
	}
	return ""
}

// userMachines mirrors the users-route machines mapping.
func userMachines(nodes []hsapi.Node, hsUserID string) []map[string]any {
	out := []map[string]any{}
	for _, n := range nodes {
		if hsapi.NodeUserID(n) == hsUserID {
			out = append(out, map[string]any{
				"id":   strOf(n["id"]),
				"name": strOf(n["name"]),
			})
		}
	}
	return out
}

// strOf coerces a JSON scalar to string.
func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// strListOf coerces a JSON array to []string (nil-safe, never nil so it
// serializes as []).
func strListOf(v any) []string {
	out := []string{}
	if arr, ok := v.([]any); ok {
		for _, e := range arr {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// ptrOrNil returns a *string for JSON (nil serializes as null).
func ptrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// timePtrOrNil formats a unix-seconds timestamp as RFC3339, or nil.
func timePtrOrNil(ts int64) *string {
	if ts == 0 {
		return nil
	}
	s := time.Unix(ts, 0).UTC().Format(time.RFC3339)
	return &s
}

// oidcUnlinked mirrors the boot route's unlink detection: an OIDC
// principal whose linked Headscale user no longer exists is treated as
// unlinked (and the stale link is removed).
func (s *Server) oidcUnlinked(p *auth.Principal, users []hsapi.User) (bool, error) {
	if p.Kind != "oidc" || p.HeadscaleUserID == nil {
		return false, nil
	}
	for _, u := range users {
		if strOf(u["id"]) == *p.HeadscaleUserID {
			return false, nil
		}
	}
	if _, err := s.authSvc.UnlinkHeadscaleUser(p.UserID); err != nil {
		return false, err
	}
	return true, nil
}
