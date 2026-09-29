package oidc

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// verifyIDToken mirrors the TS verifyIdToken: signature verification via the
// caching remote JWKS, 60 s clock tolerance on claims, nonce check, and the
// weak-RSA compatibility path.
//
// The TS relies on jose throwing when a token is signed with a weak RSA
// key; go-oidc's RemoteKeySet happily verifies such signatures, so the
// weakness is detected up front by inspecting the JWKS (weakRSAContext)
// before the normal verification runs. The outcomes are identical: weak
// keys are rejected unless allow_weak_rsa_keys is set, in which case they
// are verified manually with a one-time warning.
func (s *Service) verifyIDToken(ctx context.Context, rawIDToken, expectedNonce string) (map[string]any, *Error) {
	s.mu.Lock()
	ks := s.keySet
	s.mu.Unlock()
	if ks == nil {
		return nil, oidcErr(CodeInvalidIDToken,
			"JWKS resolver is not initialized — endpoints must be resolved first", "")
	}

	if weakCtx, werr := s.weakRSAContext(ctx, rawIDToken); werr != nil {
		return nil, oidcErr(CodeInvalidIDToken,
			fmt.Sprintf("ID token verification failed: %s", werr), "")
	} else if weakCtx != nil {
		return s.verifyWithWeakRSA(weakCtx, expectedNonce)
	}

	payloadBytes, err := ks.rs.VerifySignature(ctx, rawIDToken)
	if err != nil {
		return nil, oidcErr(CodeInvalidIDToken,
			"ID token signature verification failed",
			"The identity provider's signing keys may have changed. Try restarting Headplane to refresh the key cache.")
	}

	var claims map[string]any
	if json.Unmarshal(payloadBytes, &claims) != nil {
		return nil, oidcErr(CodeInvalidIDToken, "ID token verification failed: malformed payload", "")
	}
	if oerr := validateClaims(claims, s.cfg.Issuer, s.cfg.ClientID); oerr != nil {
		return nil, oerr
	}
	if nonce, _ := claims["nonce"].(string); nonce != expectedNonce {
		return nil, oidcErr(CodeNonceMismatch,
			fmt.Sprintf("Nonce mismatch: expected %s, got %s", expectedNonce, nonce),
			"Please try signing in again. This can happen with stale browser sessions.")
	}
	return claims, nil
}

// validateClaims mirrors the TS validateOidcClaims with jose's
// clockTolerance: 60 semantics, producing the same messages jose produces
// for claim failures.
func validateClaims(claims map[string]any, expectedIssuer, expectedAudience string) *Error {
	if iss, _ := claims["iss"].(string); iss != expectedIssuer {
		return oidcErr(CodeInvalidIDToken,
			`JWT claim validation failed: iss — unexpected "iss" claim value`, "")
	}
	audOk := false
	switch aud := claims["aud"].(type) {
	case string:
		audOk = aud == expectedAudience
	case []any:
		for _, a := range aud {
			if s, _ := a.(string); s == expectedAudience {
				audOk = true
				break
			}
		}
	}
	if !audOk {
		return oidcErr(CodeInvalidIDToken,
			`JWT claim validation failed: aud — unexpected "aud" claim value`, "")
	}

	now := time.Now().Unix()
	if exp, ok := toFloat(claims["exp"]); ok && now-clockToleranceSeconds >= int64(exp) {
		return oidcErr(CodeInvalidIDToken, "ID token is expired", "")
	}
	if nbf, ok := toFloat(claims["nbf"]); ok && now+clockToleranceSeconds < int64(nbf) {
		return oidcErr(CodeInvalidIDToken,
			"JWT claim validation failed: nbf — token is not active yet", "")
	}
	return nil
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int64:
		return float64(n), true
	}
	return 0, false
}

// weakRSAContext mirrors the TS getWeakRsaContext: for RS256/384/512 tokens
// it fetches the JWKS and returns the weak (< 2048-bit) RSA candidates.
type weakRSAContext struct {
	alg          string
	signingInput string
	signature    []byte
	claims       map[string]any
	keys         []weakJWK
}

func (s *Service) weakRSAContext(ctx context.Context, rawIDToken string) (*weakRSAContext, error) {
	s.mu.Lock()
	jwksURI := ""
	if s.endpoints != nil {
		jwksURI = s.endpoints.JWKsURI
	}
	s.mu.Unlock()
	if jwksURI == "" {
		return nil, nil
	}

	parts := strings.Split(rawIDToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("JWT must have exactly 3 parts")
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if raw, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil ||
		json.Unmarshal(raw, &header) != nil {
		return nil, fmt.Errorf("malformed JWT header")
	}
	if header.Alg != "RS256" && header.Alg != "RS384" && header.Alg != "RS512" {
		return nil, nil
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("malformed JWT signature")
	}
	var claims map[string]any
	if raw, err := base64.RawURLEncoding.DecodeString(parts[1]); err != nil ||
		json.Unmarshal(raw, &claims) != nil {
		return nil, fmt.Errorf("malformed JWT payload")
	}

	keys, err := s.fetchWeakJWKs(ctx, jwksURI)
	if err != nil {
		return nil, err
	}
	var candidates []weakJWK
	for _, k := range keys {
		if k.Kty != "RSA" {
			continue
		}
		if header.Kid != "" && k.Kid != header.Kid {
			continue
		}
		if rsaModulusBits(k.N) < 2048 {
			candidates = append(candidates, k)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	return &weakRSAContext{
		alg:          header.Alg,
		signingInput: parts[0] + "." + parts[1],
		signature:    sig,
		claims:       claims,
		keys:         candidates,
	}, nil
}

// fetchWeakJWKs mirrors the TS fetchSigningJwks with its own 60 s cache.
func (s *Service) fetchWeakJWKs(ctx context.Context, jwksURI string) ([]weakJWK, error) {
	s.mu.Lock()
	if entry, ok := s.weakCache[jwksURI]; ok && time.Now().Before(entry.expiresAt) {
		keys := entry.keys
		s.mu.Unlock()
		return keys, nil
	}
	s.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWKS endpoint returned %d: %s", resp.StatusCode, jwksURI)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Keys []weakJWK `json:"keys"`
	}
	if json.Unmarshal(body, &doc) != nil || len(doc.Keys) == 0 {
		return nil, fmt.Errorf("JWKS response did not contain any keys")
	}

	s.mu.Lock()
	s.weakCache[jwksURI] = weakCacheEntry{expiresAt: time.Now().Add(60 * time.Second), keys: doc.Keys}
	s.mu.Unlock()
	return doc.Keys, nil
}

// rsaModulusBits mirrors the TS getRsaModulusBitLength.
func rsaModulusBits(n string) int {
	raw, err := base64.RawURLEncoding.DecodeString(n)
	if err != nil || len(raw) == 0 {
		return 0
	}
	return new(big.Int).SetBytes(raw).BitLen()
}

// verifyWithWeakRSA mirrors the TS verifyIdTokenWithWeakRsa.
func (s *Service) verifyWithWeakRSA(wctx *weakRSAContext, expectedNonce string) (map[string]any, *Error) {
	if !s.cfg.AllowWeakRSAKeys {
		return nil, oidcErr(CodeInvalidIDToken,
			"ID token was signed with a weak RSA key that Headplane rejects by default",
			"If your provider cannot rotate to a 2048-bit-or-larger RSA signing key, set oidc.allow_weak_rsa_keys to true as a temporary compatibility fallback.")
	}

	for _, jwk := range wctx.keys {
		pub, err := weakRSAPublicKey(jwk)
		if err != nil {
			return nil, oidcErr(CodeInvalidIDToken,
				fmt.Sprintf("ID token verification failed: %s", err), "")
		}
		var h hash.Hash
		switch wctx.alg {
		case "RS256":
			h = sha256.New()
		case "RS384":
			h = sha512.New384()
		case "RS512":
			h = sha512.New()
		}
		h.Write([]byte(wctx.signingInput))
		if rsa.VerifyPKCS1v15(pub, hashForAlg(wctx.alg), h.Sum(nil), wctx.signature) != nil {
			continue
		}

		s.mu.Lock()
		warned := s.warnedWeakIssuer
		s.warnedWeakIssuer = true
		s.mu.Unlock()
		if !warned {
			s.logger.Warn("OIDC issuer is using a weak RSA signing key. Accepting it only because oidc.allow_weak_rsa_keys=true.",
				"issuer", s.cfg.Issuer)
		}

		if oerr := validateClaims(wctx.claims, s.cfg.Issuer, s.cfg.ClientID); oerr != nil {
			return nil, oerr
		}
		if nonce, _ := wctx.claims["nonce"].(string); nonce != expectedNonce {
			return nil, oidcErr(CodeNonceMismatch,
				fmt.Sprintf("Nonce mismatch: expected %s, got %s", expectedNonce, nonce),
				"Please try signing in again. This can happen with stale browser sessions.")
		}
		return wctx.claims, nil
	}

	return nil, oidcErr(CodeInvalidIDToken,
		"ID token signature verification failed",
		"The identity provider's signing keys may have changed. Try restarting Headplane to refresh the key cache.")
}

func hashForAlg(alg string) crypto.Hash {
	switch alg {
	case "RS256":
		return crypto.SHA256
	case "RS384":
		return crypto.SHA384
	case "RS512":
		return crypto.SHA512
	}
	return crypto.SHA256
}

func weakRSAPublicKey(jwk weakJWK) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(jwk.N)
	if err != nil {
		return nil, fmt.Errorf("invalid RSA modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(jwk.E)
	if err != nil {
		return nil, fmt.Errorf("invalid RSA exponent: %w", err)
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e == 0 {
		return nil, fmt.Errorf("invalid RSA exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}
