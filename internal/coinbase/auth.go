package coinbase

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Authentication is a JWT per request, signed EdDSA over the CDP Ed25519 key
// (API spec section 1).
//
// This is hand-rolled rather than taken from a JWT library, and it is about
// fifty lines: two base64url-encoded JSON objects, a signature over their
// concatenation, and nothing else. A dependency here would be larger than the
// thing it replaced, would need auditing for the same reason the key does, and
// would hide the one detail that actually matters — that the token is bound to
// a single method and path and expires in two minutes, so a captured one is
// worth almost nothing.

const (
	// tokenTTL is how long a minted token is valid. The venue caps this at two
	// minutes; there is no reason to ask for the maximum, and a short life is
	// what makes the per-request binding below worth having.
	tokenTTL = 90 * time.Second

	// nonceBytes is the size of the per-token nonce that goes in the header.
	nonceBytes = 16
)

// KeyError is a credential that cannot be used. It is a distinct type because
// the difference between "no key configured" and "a key that will not parse"
// is the difference between a feature being off and an operator having made a
// mistake they need to be told about.
type KeyError struct {
	Reason string
}

func (e *KeyError) Error() string { return "cdp key: " + e.Reason }

// Signer mints request tokens from a CDP Ed25519 key.
type Signer struct {
	keyID string
	key   ed25519.PrivateKey
	// now and nonce are injected so a token can be asserted byte for byte in a
	// test. Production leaves both nil.
	now   func() time.Time
	nonce func() (string, error)
}

// NewSigner parses the CDP credential.
//
// keyID is the API key id exactly as the portal issues it. Current keys are a
// bare UUID; older ones are the longer organizations/.../apiKeys/... name. Both
// are used verbatim — this code never parses or reformats it, because the venue
// matches it against what it issued.
//
// privateKey is the base64 secret from the portal. Ed25519 material is 64 bytes
// (seed and public half) or 32 (seed alone); both are accepted, since which one
// a portal hands out has changed before.
func NewSigner(keyID, privateKey string) (*Signer, error) {
	if keyID == "" {
		return nil, &KeyError{Reason: "no key id (CB_API_KEY_NAME)"}
	}
	if privateKey == "" {
		return nil, &KeyError{Reason: "no private key (CB_API_PRIVATE_KEY)"}
	}
	if strings.Contains(privateKey, "BEGIN") {
		// The likeliest configuration mistake, and worth naming precisely: a PEM
		// is an ECDSA key from the legacy flow, and no amount of base64 handling
		// will turn it into an Ed25519 one.
		return nil, &KeyError{Reason: "looks like a PEM; this build signs EdDSA and needs the base64 Ed25519 secret, not an ECDSA PEM"}
	}

	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(privateKey))
	if err != nil {
		return nil, &KeyError{Reason: fmt.Sprintf("not base64: %v", err)}
	}

	var key ed25519.PrivateKey
	switch len(raw) {
	case ed25519.PrivateKeySize: // 64: seed followed by the public half
		key = ed25519.PrivateKey(raw)
		if !isConsistentEd25519(key) {
			return nil, &KeyError{Reason: "64 bytes, but the public half does not match the seed: this is not an Ed25519 key"}
		}
	case ed25519.SeedSize: // 32
		key = ed25519.NewKeyFromSeed(raw)
	default:
		return nil, &KeyError{Reason: fmt.Sprintf(
			"decodes to %d bytes; an Ed25519 key is %d or %d",
			len(raw), ed25519.SeedSize, ed25519.PrivateKeySize)}
	}

	return &Signer{keyID: keyID, key: key, now: time.Now, nonce: randomNonce}, nil
}

// Token mints a bearer token for exactly one request.
//
// The uri claim binds the token to a method and a path — "GET
// api.coinbase.com/api/v3/brokerage/portfolios" — with no scheme and no query
// string. That binding plus the ninety-second expiry is the whole reason a
// leaked token is not a leaked key: it authorises one call to one endpoint, for
// a minute and a half.
func (s *Signer) Token(method, host, path string) (string, error) {
	now := s.now()
	nonce, err := s.nonce()
	if err != nil {
		return "", fmt.Errorf("mint nonce: %w", err)
	}

	header := map[string]any{
		"alg":   "EdDSA",
		"typ":   "JWT",
		"kid":   s.keyID,
		"nonce": nonce,
	}
	claims := map[string]any{
		"sub": s.keyID,
		"iss": "cdp",
		"nbf": now.Unix(),
		"exp": now.Add(tokenTTL).Unix(),
		"uri": method + " " + host + "/" + strings.TrimPrefix(path, "/"),
	}

	encodedHeader, err := encodeSegment(header)
	if err != nil {
		return "", fmt.Errorf("encode header: %w", err)
	}
	encodedClaims, err := encodeSegment(claims)
	if err != nil {
		return "", fmt.Errorf("encode claims: %w", err)
	}

	signingInput := encodedHeader + "." + encodedClaims
	signature := ed25519.Sign(s.key, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// encodeSegment renders one JWT segment: compact JSON, base64url, unpadded.
func encodeSegment(v any) (string, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

// randomNonce is crypto/rand, unlike the reconnect jitter in internal/ingest.
// The distinction is not stylistic: this value exists to stop a token being
// replayed, so a predictable one would defeat it.
func randomNonce() (string, error) {
	b := make([]byte, nonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// isConsistentEd25519 checks that a 64-byte key really is one.
//
// The trailing 32 bytes must be the public key derived from the leading 32. Go
// only checks the length, so without this any 64-byte blob produces
// syntactically valid tokens that the venue rejects — turning an operator's
// paste error into an authentication failure every five seconds, hours after
// startup, instead of a refusal to start. cmd/ingest documents exactly that
// distinction: an absent credential disables the account half, a malformed one
// fails the boot.
func isConsistentEd25519(key ed25519.PrivateKey) bool {
	return bytes.Equal(ed25519.NewKeyFromSeed(key.Seed()), key)
}
