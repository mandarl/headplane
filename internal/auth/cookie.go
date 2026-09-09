package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CookieProfile mirrors the profile object embedded in the _hp_auth cookie
// payload by the TS server. Field order and omission semantics must match
// JSON.stringify: {name, email?, username?} — undefined fields are omitted,
// which we model with nil pointers.
type CookieProfile struct {
	Name     string  `json:"name"`
	Email    *string `json:"email,omitempty"`
	Username *string `json:"username,omitempty"`
}

// CookiePayload mirrors the TS CookiePayload interface. Field order matters:
// api_key sessions serialize as {"sid","api_key"}, OIDC sessions as
// {"sid","profile"}.
type CookiePayload struct {
	SID     string         `json:"sid"`
	APIKey  string         `json:"api_key,omitempty"`
	Profile *CookieProfile `json:"profile,omitempty"`
}

// marshalJSON encodes v exactly like JSON.stringify: UTF-8 output with no
// HTML escaping (Go's encoder escapes <, >, & by default; TS does not).
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// b64 is raw (unpadded) base64url, the same alphabet Node's
// Buffer.toString("base64url") produces. It is used for the inner signed
// payload and HMAC.
var b64 = base64.RawURLEncoding

// b64outer is standard base64 WITH padding, matching the browser btoa() that
// react-router's cookie encodeData applies to the JSON-stringified value.
var b64outer = base64.StdEncoding

// SignCookie builds the cookie VALUE (not the Set-Cookie header), byte-
// identical to the TS encodeCookie. The inner value is
// base64url(JSON).base64url(HMAC-SHA256(secret, base64url(JSON))); react-
// router's createCookie then wraps it as btoa(JSON.stringify(inner)) —
// standard base64 with padding. The HMAC key is the UTF-8 bytes of the
// secret and the message is the ASCII signed part.
func SignCookie(secret string, payload CookiePayload) (string, error) {
	raw, err := marshalJSON(payload)
	if err != nil {
		return "", err
	}
	signed := b64.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signed))
	sig := b64.EncodeToString(mac.Sum(nil))
	inner := signed + "." + sig
	quoted, err := marshalJSON(inner)
	if err != nil {
		return "", err
	}
	return b64outer.EncodeToString(quoted), nil
}

// VerifyCookie parses the Cookie header, finds name, and verifies the HMAC.
// It mirrors the TS decodeCookie: percent-decode, base64-decode, JSON.parse
// back to the inner signed value, split on the LAST ".", recompute the HMAC
// over the received signed part, compare, then base64url-decode and parse
// the payload JSON.
func VerifyCookie(secret, cookieHeader, name string) (CookiePayload, error) {
	var payload CookiePayload
	raw := parseCookieHeader(cookieHeader, name)
	if raw == "" {
		return payload, fmt.Errorf("auth: no session cookie found")
	}
	outer, err := b64outer.DecodeString(raw)
	if err != nil {
		return payload, fmt.Errorf("auth: malformed session cookie")
	}
	var inner string
	if err := json.Unmarshal(outer, &inner); err != nil {
		return payload, fmt.Errorf("auth: malformed session cookie")
	}
	dot := strings.LastIndexByte(inner, '.')
	if dot == -1 {
		return payload, fmt.Errorf("auth: malformed session cookie")
	}
	signed, sig := inner[:dot], inner[dot+1:]

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signed))
	expected := b64.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(expected)) != 1 {
		return payload, fmt.Errorf("auth: invalid session cookie signature")
	}

	rawJSON, err := b64.DecodeString(signed)
	if err != nil {
		return payload, fmt.Errorf("auth: malformed session cookie payload")
	}
	if err := json.Unmarshal(rawJSON, &payload); err != nil {
		return payload, fmt.Errorf("auth: malformed session cookie payload")
	}
	return payload, nil
}

// CookieValue extracts the raw (still-encoded) value of name from a Cookie
// header. It is the exported form of parseCookieHeader, for callers (like the
// OIDC state cookie) that do their own value decoding.
func CookieValue(header, name string) string { return parseCookieHeader(header, name) }

// parseCookieHeader extracts the value of name from a Cookie header,
// mirroring the `cookie` npm package's parse: split on ';', match on the
// first '=', first occurrence wins, surrounding whitespace trimmed. The
// value is percent-decoded on a best-effort basis (decodeURIComponent in
// TS); our values are base64url so decoding is the identity.
func parseCookieHeader(header, name string) string {
	for _, part := range strings.Split(header, ";") {
		part = strings.TrimSpace(part)
		eq := strings.IndexByte(part, '=')
		if eq == -1 {
			continue
		}
		if strings.TrimSpace(part[:eq]) != name {
			continue
		}
		val := strings.TrimSpace(part[eq+1:])
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		if decoded, err := urlUnescape(val); err == nil {
			return decoded
		}
		return val
	}
	return ""
}

// urlUnescape percent-decodes s like JS decodeURIComponent: unlike
// url.QueryUnescape it never converts '+' to space. It returns an error if
// a '%' sequence is malformed, in which case the caller keeps the raw value
// (the `cookie` package falls back similarly).
func urlUnescape(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			sb.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("auth: bad percent-encoding")
		}
		hi := unhex(s[i+1])
		lo := unhex(s[i+2])
		if hi == 255 || lo == 255 {
			return "", fmt.Errorf("auth: bad percent-encoding")
		}
		sb.WriteByte(hi<<4 | lo)
		i += 2
	}
	return sb.String(), nil
}

func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10
	}
	return 255
}

// CookieOptions mirrors the react-router createCookie options used for
// _hp_auth in app/server/context.ts: name is fixed, path is the basename,
// maxAge is seconds (config server.cookie_max_age).
type CookieOptions struct {
	Name   string
	Path   string
	MaxAge int
	Secure bool
	Domain string
}

// SerializeSetCookie renders a Set-Cookie header byte-identical to the
// `cookie` npm package's serialize as used by react-router's createCookie.
// Attribute order is Max-Age, Domain, Path, Expires, HttpOnly, Secure,
// SameSite; react-router defaults sameSite to "lax" and the TS auth service
// never overrides it. The value is percent-encoded with encodeURIComponent
// semantics (only the base64 padding '=' ever needs it in practice).
func SerializeSetCookie(opts CookieOptions, value string, expires *time.Time) string {
	var sb strings.Builder
	sb.WriteString(opts.Name)
	sb.WriteByte('=')
	sb.WriteString(EncodeURIComponent(value))
	sb.WriteString("; Max-Age=")
	fmt.Fprintf(&sb, "%d", opts.MaxAge)
	if opts.Domain != "" {
		sb.WriteString("; Domain=")
		sb.WriteString(opts.Domain)
	}
	sb.WriteString("; Path=")
	sb.WriteString(opts.Path)
	if expires != nil {
		sb.WriteString("; Expires=")
		sb.WriteString(expires.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT"))
	}
	if opts.Secure {
		sb.WriteString("; Secure")
	}
	sb.WriteString("; SameSite=Lax")
	return sb.String()
}

// encodeURIComponent percent-encodes s exactly like JavaScript's
// encodeURIComponent: everything except A-Za-z0-9 and -_.!~*'() is escaped
// as %XX (UTF-8 bytes). Go's url.QueryEscape differs (it escapes !'() and
// leaves nothing else), so this is hand-rolled.
// EncodeURIComponent mirrors JavaScript's encodeURIComponent, for values
// that must round-trip with the TypeScript implementation (OIDC Basic
// credentials, the __oidc_state cookie).
func EncodeURIComponent(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '!', c == '~', c == '*', c == '\'', c == '(', c == ')':
			sb.WriteByte(c)
		default:
			fmt.Fprintf(&sb, "%%%02X", c)
		}
	}
	return sb.String()
}

// ClearCookieValue renders the session-clearing Set-Cookie header, mirroring
// TS destroySession: cookie.serialize("", {expires: new Date(0)}) with the
// base cookie options (Max-Age is still present from the cookie defaults).
func ClearCookieValue(opts CookieOptions) string {
	epoch := time.Unix(0, 0).UTC()
	return SerializeSetCookie(opts, "", &epoch)
}

// HashAPIKey mirrors the TS hashApiKey: hex(sha256(key)).
func HashAPIKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", sum)
}
