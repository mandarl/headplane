package hsapi

import (
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"time"
)

// This file ports app/server/headscale/api/resources/*. The wire shapes are
// the Headscale REST API's; nodes and users flow through as
// map[string]any so the v1 layer can reshape them exactly like the TS
// loaders did, while the version-dependent normalizations (tag shapes,
// pre-auth key identity) live here next to the capability flags.

// Node is a Headscale node object with normalized flat tags.
type Node map[string]any

// User is a Headscale user object.
type User map[string]any

// sortStringSet sorts a []any-of-string field in place. Headscale returns
// approvedRoutes / availableRoutes / subnetRoutes as unordered sets whose
// element order varies between otherwise-identical responses; sorting them
// keeps the node payload byte-stable so the live store's change detection
// doesn't fire spuriously (and the v1 responses stay deterministic).
func sortStringSet(raw map[string]any, key string) {
	arr, ok := raw[key].([]any)
	if !ok || len(arr) < 2 {
		return
	}
	slices.SortFunc(arr, func(a, b any) int {
		as, _ := a.(string)
		bs, _ := b.(string)
		return strings.Compare(as, bs)
	})
}

// normalizeNode mirrors the normalize() closure in resources/nodes.ts:
// on 0.28+ the wire already carries flat tags; older servers returned
// forcedTags/validTags that the client unions (deduped, order-preserving).
// It also sorts Headscale's set-valued route fields for a stable payload.
func (c *Client) normalizeNode(raw map[string]any) Node {
	for _, k := range []string{"approvedRoutes", "availableRoutes", "subnetRoutes"} {
		sortStringSet(raw, k)
	}
	if c.caps.NodeTagsAreFlat {
		if _, ok := raw["tags"]; !ok {
			raw["tags"] = []any{}
		}
		return raw
	}
	seen := map[string]bool{}
	tags := []string{}
	for _, key := range []string{"forcedTags", "validTags"} {
		if arr, ok := raw[key].([]any); ok {
			for _, t := range arr {
				if s, ok := t.(string); ok && !seen[s] {
					seen[s] = true
					tags = append(tags, s)
				}
			}
		}
	}
	raw["tags"] = tags
	return raw
}

// ListNodes mirrors NodeApi.list: GET v1/node -> {nodes: [...]}.
func (c *Client) ListNodes() ([]Node, error) {
	raw, err := c.request("GET", "v1/node", nil, nil)
	if err != nil {
		return nil, err
	}
	var nodes []map[string]any
	if err := decode(raw, "nodes", &nodes); err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, c.normalizeNode(n))
	}
	return out, nil
}

// GetNode mirrors NodeApi.get: GET v1/node/{id} -> {node: {...}}. It
// returns (nil, nil) when the response carries no node, mirroring the TS
// `node ? normalize(node) : undefined`.
func (c *Client) GetNode(id string) (Node, error) {
	raw, err := c.request("GET", "v1/node/"+id, nil, nil)
	if err != nil {
		return nil, err
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	rawNode, ok := env["node"]
	if !ok || string(rawNode) == "null" {
		return nil, nil
	}
	var node map[string]any
	if err := json.Unmarshal(rawNode, &node); err != nil {
		return nil, err
	}
	return c.normalizeNode(node), nil
}

// DeleteNode mirrors NodeApi.delete: DELETE v1/node/{id}.
func (c *Client) DeleteNode(id string) error {
	_, err := c.request("DELETE", "v1/node/"+id, nil, nil)
	return err
}

// RegisterNode mirrors NodeApi.register: Headscale expects the registration
// params as both query string and body — preserved as-is.
func (c *Client) RegisterNode(user, key string) (Node, error) {
	qp := url.Values{}
	qp.Set("user", user)
	qp.Set("key", key)
	raw, err := c.request("POST", "v1/node/register?"+qp.Encode(), nil, map[string]any{
		"user": user,
		"key":  key,
	})
	if err != nil {
		return nil, err
	}
	var node map[string]any
	if err := decode(raw, "node", &node); err != nil {
		return nil, err
	}
	return c.normalizeNode(node), nil
}

// ApproveRoutes mirrors NodeApi.approveRoutes: POST v1/node/{id}/approve_routes.
func (c *Client) ApproveRoutes(id string, routes []string) error {
	_, err := c.request("POST", "v1/node/"+id+"/approve_routes", nil, map[string]any{"routes": routes})
	return err
}

// ExpireNode mirrors NodeApi.expire: POST v1/node/{id}/expire.
func (c *Client) ExpireNode(id string) error {
	_, err := c.request("POST", "v1/node/"+id+"/expire", nil, nil)
	return err
}

// RenameNode mirrors NodeApi.rename: POST v1/node/{id}/rename/{name} with
// the name path-escaped, exactly like encodeURIComponent.
func (c *Client) RenameNode(id, name string) error {
	_, err := c.request("POST", "v1/node/"+id+"/rename/"+url.PathEscape(name), nil, nil)
	return err
}

// SetNodeTags mirrors NodeApi.setTags: POST v1/node/{id}/tags {tags}.
func (c *Client) SetNodeTags(id string, tags []string) error {
	_, err := c.request("POST", "v1/node/"+id+"/tags", nil, map[string]any{"tags": tags})
	return err
}

// ReassignNodeUser mirrors the optional NodeApi.reassignUser (only valid
// when !NodeOwnerIsImmutable, i.e. Headscale < 0.28): POST
// v1/node/{id}/user {user}.
func (c *Client) ReassignNodeUser(id, user string) error {
	_, err := c.request("POST", "v1/node/"+id+"/user", nil, map[string]any{"user": user})
	return err
}

// ListUsers mirrors UserApi.list: GET v1/user[?id=&name=&email=] ->
// {users: [...]}. Only one filter may be set, like the TS guard.
func (c *Client) ListUsers(id, name, email string) ([]User, error) {
	set := 0
	for _, f := range []string{id, name, email} {
		if f != "" {
			set++
		}
	}
	if set > 1 {
		return nil, &ConnError{RequestURL: "GET v1/user", ErrorCode: "INVALID_FILTER", ErrorMessage: "Only one of id, name, or email filters can be provided"}
	}
	raw, err := c.request("GET", "v1/user", map[string]string{"id": id, "name": name, "email": email}, nil)
	if err != nil {
		return nil, err
	}
	var users []map[string]any
	if err := decode(raw, "users", &users); err != nil {
		return nil, err
	}
	out := make([]User, 0, len(users))
	for _, u := range users {
		out = append(out, u)
	}
	return out, nil
}

// CreateUser mirrors UserApi.create: POST v1/user {name, email,
// displayName} -> {user: {...}}. Nil fields are omitted, like undefined.
func (c *Client) CreateUser(name string, email, displayName *string) (User, error) {
	body := map[string]any{"name": name}
	if email != nil {
		body["email"] = *email
	}
	if displayName != nil {
		body["displayName"] = *displayName
	}
	raw, err := c.request("POST", "v1/user", nil, body)
	if err != nil {
		return nil, err
	}
	var user map[string]any
	if err := decode(raw, "user", &user); err != nil {
		return nil, err
	}
	return user, nil
}

// DeleteUser mirrors UserApi.delete: DELETE v1/user/{id}.
func (c *Client) DeleteUser(id string) error {
	_, err := c.request("DELETE", "v1/user/"+id, nil, nil)
	return err
}

// RenameUser mirrors UserApi.rename: POST v1/user/{id}/rename/{name}.
func (c *Client) RenameUser(id, name string) error {
	_, err := c.request("POST", "v1/user/"+id+"/rename/"+url.PathEscape(name), nil, nil)
	return err
}

// GetPolicy mirrors PolicyApi.get: GET v1/policy ->
// {policy, updatedAt: string|null}. A null updatedAt means file-mode
// without a stored timestamp (not writable via API).
func (c *Client) GetPolicy() (string, *time.Time, error) {
	raw, err := c.request("GET", "v1/policy", nil, nil)
	if err != nil {
		return "", nil, err
	}
	var p struct {
		Policy    string  `json:"policy"`
		UpdatedAt *string `json:"updatedAt"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", nil, err
	}
	if p.UpdatedAt == nil {
		return p.Policy, nil, nil
	}
	t, err := time.Parse(time.RFC3339, *p.UpdatedAt)
	if err != nil {
		return "", nil, err
	}
	return p.Policy, &t, nil
}

// SetPolicy mirrors PolicyApi.set: PUT v1/policy {policy} ->
// {policy, updatedAt}.
func (c *Client) SetPolicy(policy string) (string, time.Time, error) {
	raw, err := c.request("PUT", "v1/policy", nil, map[string]any{"policy": policy})
	if err != nil {
		return "", time.Time{}, err
	}
	var p struct {
		Policy    string `json:"policy"`
		UpdatedAt string `json:"updatedAt"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, p.UpdatedAt)
	if err != nil {
		return "", time.Time{}, err
	}
	return p.Policy, t, nil
}

// ListPreAuthKeysAll mirrors the 0.28+ PreAuthKeyApi.listAll:
// GET v1/preauthkey -> {preAuthKeys: [...]}.
func (c *Client) ListPreAuthKeysAll() ([]map[string]any, error) {
	raw, err := c.request("GET", "v1/preauthkey", nil, nil)
	if err != nil {
		return nil, err
	}
	var keys []map[string]any
	if err := decode(raw, "preAuthKeys", &keys); err != nil {
		return nil, err
	}
	return keys, nil
}

// ListPreAuthKeysForUser mirrors PreAuthKeyApi.listForUser:
// GET v1/preauthkey?user={id} -> {preAuthKeys: [...]}.
func (c *Client) ListPreAuthKeysForUser(userID string) ([]map[string]any, error) {
	raw, err := c.request("GET", "v1/preauthkey", map[string]string{"user": userID}, nil)
	if err != nil {
		return nil, err
	}
	var keys []map[string]any
	if err := decode(raw, "preAuthKeys", &keys); err != nil {
		return nil, err
	}
	return keys, nil
}

// CreatePreAuthKey mirrors PreAuthKeyApi.create: POST v1/preauthkey ->
// {preAuthKey: {...}}. expiration is ISO-8601 or null; user/aclTags are
// omitted when empty, like the TS conditional body fields.
func (c *Client) CreatePreAuthKey(user *string, ephemeral, reusable bool, expiration *time.Time, aclTags []string) (map[string]any, error) {
	body := map[string]any{
		"ephemeral": ephemeral,
		"reusable":  reusable,
	}
	if expiration != nil {
		body["expiration"] = expiration.UTC().Format(time.RFC3339)
	} else {
		body["expiration"] = nil
	}
	if user != nil {
		body["user"] = *user
	}
	if len(aclTags) > 0 {
		body["aclTags"] = aclTags
	}
	raw, err := c.request("POST", "v1/preauthkey", nil, body)
	if err != nil {
		return nil, err
	}
	var key map[string]any
	if err := decode(raw, "preAuthKey", &key); err != nil {
		return nil, err
	}
	return key, nil
}

// ExpirePreAuthKey mirrors PreAuthKeyApi.expire: on 0.28+ it posts {id};
// older servers need the owning user's numeric id plus the key string
// (Headscale rejects names with "proto: invalid value for uint64 field
// user"), which the caller supplies via userID/key.
func (c *Client) ExpirePreAuthKey(id, userID, key string) error {
	var body map[string]any
	if c.caps.PreAuthKeysHaveStableIds {
		body = map[string]any{"id": id}
	} else {
		body = map[string]any{"user": userID, "key": key}
	}
	_, err := c.request("POST", "v1/preauthkey/expire", nil, body)
	return err
}

// ListAPIKeys mirrors ApiKeyApi.list: GET v1/apikey -> {apiKeys: [...]}.
func (c *Client) ListAPIKeys() ([]map[string]any, error) {
	raw, err := c.request("GET", "v1/apikey", nil, nil)
	if err != nil {
		return nil, err
	}
	var keys []map[string]any
	if err := decode(raw, "apiKeys", &keys); err != nil {
		return nil, err
	}
	return keys, nil
}

// nodeString pulls an optional string field out of a node/user map.
func strField(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

// nodeUserID returns the owning Headscale user id of a node, or "".
func NodeUserID(n Node) string {
	if u, ok := n["user"].(map[string]any); ok {
		return strField(u, "id")
	}
	return ""
}

// OIDCSubject extracts the OIDC subject from a Headscale user's providerId,
// mirroring getOidcSubject in app/server/web/headscale-identity.ts:
// providerId is a URL whose last path segment is the subject. This is the
// ONLY place this parsing occurs.
func OIDCSubject(u User) string {
	if strField(u, "provider") != "oidc" {
		return ""
	}
	pid := strField(u, "providerId")
	if pid == "" {
		return ""
	}
	seg := pid[strings.LastIndex(pid, "/")+1:]
	if un, err := url.PathUnescape(seg); err == nil {
		return un
	}
	return seg
}
