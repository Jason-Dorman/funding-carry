package coinbase

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// testKey is a deterministic Ed25519 key. It is generated from a fixed seed
// rather than committed as a literal so that nothing in this repository ever
// looks like a real credential, to a scanner or to a reader.
func testKey(t *testing.T) (keyID, secret string, pub ed25519.PublicKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return "11111111-2222-3333-4444-555555555555",
		base64.StdEncoding.EncodeToString(priv),
		priv.Public().(ed25519.PublicKey)
}

func fixedSigner(t *testing.T) (*Signer, ed25519.PublicKey) {
	t.Helper()
	keyID, secret, pub := testKey(t)
	s, err := NewSigner(keyID, secret)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	s.now = func() time.Time { return time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC) }
	s.nonce = func() (string, error) { return "deadbeef", nil }
	return s, pub
}

func decodeSegment(t *testing.T, seg string) map[string]any {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		t.Fatalf("segment is not unpadded base64url: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("segment is not JSON: %v", err)
	}
	return out
}

func TestTokenIsVerifiableAndBoundToOneRequest(t *testing.T) {
	s, pub := fixedSigner(t)

	tok, err := s.Token("GET", "api.coinbase.com", "/api/v3/brokerage/cfm/balance_summary")
	if err != nil {
		t.Fatalf("token: %v", err)
	}

	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}

	// The signature verifies against the public half of the key. Without this
	// the rest of the assertions would pass over a token the venue rejects.
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("signature is not unpadded base64url: %v", err)
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatal("signature does not verify: the venue would reject this token")
	}

	header := decodeSegment(t, parts[0])
	if header["alg"] != "EdDSA" {
		t.Errorf("alg = %v, want EdDSA — ES256 is the legacy ECDSA flow", header["alg"])
	}
	if header["kid"] != s.keyID {
		t.Errorf("kid = %v, want the key id verbatim", header["kid"])
	}
	if header["nonce"] != "deadbeef" {
		t.Errorf("nonce = %v, want the injected value", header["nonce"])
	}

	claims := decodeSegment(t, parts[1])
	// The binding that makes a captured token nearly worthless: one method, one
	// path, no scheme, no query.
	if want := "GET api.coinbase.com/api/v3/brokerage/cfm/balance_summary"; claims["uri"] != want {
		t.Errorf("uri = %v, want %q", claims["uri"], want)
	}
	if claims["iss"] != "cdp" {
		t.Errorf("iss = %v, want cdp", claims["iss"])
	}
	if claims["sub"] != s.keyID {
		t.Errorf("sub = %v, want the key id", claims["sub"])
	}

	nbf, exp := claims["nbf"].(float64), claims["exp"].(float64)
	if int64(exp-nbf) != int64(tokenTTL.Seconds()) {
		t.Errorf("token lifetime = %ds, want %ds", int64(exp-nbf), int64(tokenTTL.Seconds()))
	}
	// The venue caps the lifetime at two minutes; asking for more is rejected.
	if tokenTTL > 2*time.Minute {
		t.Errorf("tokenTTL = %s, above the venue's 2m ceiling", tokenTTL)
	}
}

func TestADifferentRequestGetsADifferentToken(t *testing.T) {
	s, _ := fixedSigner(t)

	a, err := s.Token("GET", "api.coinbase.com", "/api/v3/brokerage/cfm/positions")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Token("GET", "api.coinbase.com", "/api/v3/brokerage/cfm/balance_summary")
	if err != nil {
		t.Fatal(err)
	}
	// Same clock, same nonce, different path. If these matched, the uri claim
	// would not be binding anything.
	if a == b {
		t.Fatal("two different paths produced the same token")
	}
}

func TestNonceIsUnpredictable(t *testing.T) {
	keyID, secret, _ := testKey(t)
	s, err := NewSigner(keyID, secret)
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]struct{}{}
	for range 100 {
		n, err := s.nonce()
		if err != nil {
			t.Fatalf("nonce: %v", err)
		}
		if _, dup := seen[n]; dup {
			t.Fatal("a nonce repeated in 100 draws; replay protection is not protecting anything")
		}
		seen[n] = struct{}{}
	}
}

func TestKeyMaterialIsValidated(t *testing.T) {
	_, secret, _ := testKey(t)

	for _, tc := range []struct {
		name, keyID, key, wantIn string
	}{
		{"no key id", "", secret, "no key id"},
		{"no private key", "abc", "", "no private key"},
		{
			// The mistake the algorithm change makes likely: pasting the ECDSA
			// PEM from the legacy flow. It must say so, not fail later inside a
			// signature the venue rejects.
			name: "an ECDSA PEM", keyID: "abc",
			key:    "-----BEGIN EC PRIVATE KEY-----\\nMHcCAQEE\\n-----END EC PRIVATE KEY-----",
			wantIn: "PEM",
		},
		{"not base64", "abc", "not!valid!base64", "not base64"},
		{"wrong length", "abc", base64.StdEncoding.EncodeToString([]byte("short")), "decodes to 5 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewSigner(tc.keyID, tc.key)
			var keyErr *KeyError
			if !errors.As(err, &keyErr) {
				t.Fatalf("err = %v, want a KeyError", err)
			}
			if !strings.Contains(keyErr.Reason, tc.wantIn) {
				t.Errorf("reason = %q, want it to mention %q", keyErr.Reason, tc.wantIn)
			}
		})
	}
}

func TestASeedOnlyKeyIsAccepted(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 7)
	}
	// Portals have handed out the 32-byte seed as well as the 64-byte key; both
	// describe the same key and both must work.
	s, err := NewSigner("abc", base64.StdEncoding.EncodeToString(seed))
	if err != nil {
		t.Fatalf("a 32-byte seed was rejected: %v", err)
	}
	tok, err := s.Token("GET", "api.coinbase.com", "/x")
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	parts := strings.Split(tok, ".")
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		t.Fatal("a token signed from a seed-only key does not verify")
	}
}
