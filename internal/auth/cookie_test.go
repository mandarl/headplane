package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const testSecret = "0123456789abcdef0123456789abcdef" // exactly 32 chars

// decodeOuter reverses react-router's cookie value wrapping:
// btoa(JSON.stringify(inner)) -> inner.
func decodeOuter(t *testing.T, value string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("outer base64 decode: %v", err)
	}
	var inner string
	if err := json.Unmarshal(raw, &inner); err != nil {
		t.Fatalf("outer JSON unquote: %v", err)
	}
	return inner
}

func TestSignVerifyRoundTrip(t *testing.T) {
	payload := CookiePayload{SID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", APIKey: "test-prefix.secret-part"}
	value, err := SignCookie(testSecret, payload)
	if err != nil {
		t.Fatal(err)
	}
	// The wire value is btoa(JSON.stringify("signed.hmac")): unwrap and
	// check the inner signature separator.
	inner := decodeOuter(t, value)
	if !strings.Contains(inner, ".") {
		t.Fatalf("inner value missing signature separator: %q", inner)
	}

	got, err := VerifyCookie(testSecret, "_hp_auth="+value, "_hp_auth")
	if err != nil {
		t.Fatal(err)
	}
	if got.SID != payload.SID || got.APIKey != payload.APIKey {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestSignVerifyOIDCProfile(t *testing.T) {
	email := "user@example.com"
	payload := CookiePayload{
		SID:     "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		Profile: &CookieProfile{Name: "Test User", Email: &email},
	}
	value, err := SignCookie(testSecret, payload)
	if err != nil {
		t.Fatal(err)
	}
	// JSON field order must be sid, profile / name, email (no username).
	inner := decodeOuter(t, value)
	signed := inner[:strings.LastIndexByte(inner, '.')]
	rawJSON, err := base64.RawURLEncoding.DecodeString(signed)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON := `{"sid":"01ARZ3NDEKTSV4RRFFQ69G5FAV","profile":{"name":"Test User","email":"user@example.com"}}`
	if string(rawJSON) != wantJSON {
		t.Fatalf("payload JSON mismatch:\n got %s\nwant %s", rawJSON, wantJSON)
	}

	got, err := VerifyCookie(testSecret, "other=1; _hp_auth="+value+"; theme=dark", "_hp_auth")
	if err != nil {
		t.Fatal(err)
	}
	if got.Profile == nil || got.Profile.Name != "Test User" || got.Profile.Email == nil || *got.Profile.Email != email {
		t.Fatalf("profile mismatch: %+v", got.Profile)
	}
	if got.Profile.Username != nil {
		t.Fatalf("username should be omitted, got %q", *got.Profile.Username)
	}
}

func TestVerifyRejectsTamperedSignature(t *testing.T) {
	value, err := SignCookie(testSecret, CookiePayload{SID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"})
	if err != nil {
		t.Fatal(err)
	}
	inner := decodeOuter(t, value)
	dot := strings.LastIndexByte(inner, '.')
	tamperedInner := inner[:dot+1] + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	tampered, err := marshalJSON(tamperedInner)
	if err != nil {
		t.Fatal(err)
	}
	tamperedValue := base64.StdEncoding.EncodeToString(tampered)
	if _, err := VerifyCookie(testSecret, "_hp_auth="+tamperedValue, "_hp_auth"); err == nil {
		t.Fatal("tampered signature accepted")
	}

	// Tampered payload with the original signature must also fail.
	tamperedPayload := "X" + value[1:]
	if _, err := VerifyCookie(testSecret, "_hp_auth="+tamperedPayload, "_hp_auth"); err == nil {
		t.Fatal("tampered payload accepted")
	}
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	value, err := SignCookie(testSecret, CookiePayload{SID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyCookie("fedcba9876543210fedcba9876543210", "_hp_auth="+value, "_hp_auth"); err == nil {
		t.Fatal("wrong secret accepted")
	}
}

func TestVerifyRejectsMalformed(t *testing.T) {
	for _, header := range []string{
		"",
		"_hp_auth=",
		"_hp_auth=nosignature",
		"_hp_auth=%%%.%%%",
	} {
		if _, err := VerifyCookie(testSecret, header, "_hp_auth"); err == nil {
			t.Fatalf("malformed cookie accepted: %q", header)
		}
	}
	// Wrong cookie name must not match.
	value, _ := SignCookie(testSecret, CookiePayload{SID: "x"})
	if _, err := VerifyCookie(testSecret, "_hp_auth2="+value, "_hp_auth"); err == nil {
		t.Fatal("wrong cookie name accepted")
	}
}

func TestSerializeSetCookie(t *testing.T) {
	opts := CookieOptions{Name: "_hp_auth", Path: "/admin", MaxAge: 86400, Secure: true}

	// Mirrors cookie.serialize("signed.hmac", {maxAge, path, secure}):
	// the value is btoa(JSON.stringify(...)) with encodeURIComponent applied,
	// then Max-Age, Path, Secure, SameSite=Lax (react-router's default).
	got := SerializeSetCookie(opts, "signed.hmac", nil)
	want := "_hp_auth=signed.hmac; Max-Age=86400; Path=/admin; Secure; SameSite=Lax"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}

	// With a domain it sorts before Path, like the cookie package.
	opts.Domain = "example.com"
	got = SerializeSetCookie(opts, "v", nil)
	want = "_hp_auth=v; Max-Age=86400; Domain=example.com; Path=/admin; Secure; SameSite=Lax"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}

	// Clear-cookie: empty value + Expires epoch, Max-Age still present.
	got = ClearCookieValue(CookieOptions{Name: "_hp_auth", Path: "/admin", MaxAge: 86400, Secure: true})
	want = "_hp_auth=; Max-Age=86400; Path=/admin; Expires=Thu, 01 Jan 1970 00:00:00 GMT; Secure; SameSite=Lax"
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestEncodeURIComponent(t *testing.T) {
	// Matches JavaScript encodeURIComponent for the base64 alphabet: only
	// the padding '=' is escaped.
	if got := EncodeURIComponent("ImFiYyI="); got != "ImFiYyI%3D" {
		t.Fatalf("got %q", got)
	}
	if got := EncodeURIComponent("abcXYZ019-_.!~*'()"); got != "abcXYZ019-_.!~*'()" {
		t.Fatalf("got %q", got)
	}
	if got := EncodeURIComponent("a+b/c"); got != "a%2Bb%2Fc" {
		t.Fatalf("got %q", got)
	}
}

func TestHashAPIKey(t *testing.T) {
	// sha256("test") hex, independent of the implementation.
	if got := HashAPIKey("test"); got != "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08" {
		t.Fatalf("unexpected hash %q", got)
	}
}
