//go:build live

// Live verification of the CDP credential against the real Advanced Trade API.
//
// Behind a build tag for the same reason internal/ingest's live suite is:
// building with the tag is a request to run it, so it fails rather than skips
// when the credential is absent — a `live` run printing ok for a test that
// authenticated against nothing would be a green tick meaning the opposite.
//
//	go test -tags live -run TestLive -v ./internal/coinbase/
//
// Every call is a GET. Nothing here can move money, and the key it is meant to
// be run with is view-only. Balances are never printed: the test asserts on the
// shape of a response and on the fields the risk engine will read, not on their
// values, so a transcript of a passing run discloses nothing about the account.
package coinbase

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const liveHost = "api.coinbase.com"

// credentials reads .env.private from the repository root.
func credentials(t *testing.T) (keyID, secret string) {
	t.Helper()

	if id, key := os.Getenv("CB_API_KEY_NAME"), os.Getenv("CB_API_PRIVATE_KEY"); id != "" && key != "" {
		return id, key
	}

	path := filepath.Join("..", "..", ".env.private")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no credential: set CB_API_KEY_NAME and CB_API_PRIVATE_KEY, or create %s (%v)", path, err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		env[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	keyID, secret = env["CB_API_KEY_NAME"], env["CB_API_PRIVATE_KEY"]
	if keyID == "" || secret == "" {
		t.Fatalf("%s is missing CB_API_KEY_NAME or CB_API_PRIVATE_KEY", path)
	}
	return keyID, secret
}

// liveGet performs one authenticated GET and returns the status and body.
func liveGet(t *testing.T, s *Signer, path string) (int, []byte) {
	t.Helper()

	tok, err := s.Token(http.MethodGet, liveHost, path)
	if err != nil {
		t.Fatalf("mint token for %s: %v", path, err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+liveHost+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, body
}

// keysOf reports a payload's top-level field names. Used instead of printing a
// body so that an account's numbers never reach the test log.
func keysOf(t *testing.T, body []byte) []string {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestLiveCredentialAuthenticates(t *testing.T) {
	keyID, secret := credentials(t)
	s, err := NewSigner(keyID, secret)
	if err != nil {
		t.Fatalf("the credential will not parse: %v", err)
	}

	status, body := liveGet(t, s, "/api/v3/brokerage/portfolios")
	if status == http.StatusUnauthorized {
		t.Fatalf("401: the key, the signature or the uri claim is wrong. Body: %s", truncate(body, 300))
	}
	if status != http.StatusOK {
		t.Fatalf("portfolios returned %d: %s", status, truncate(body, 300))
	}

	// The portfolio uuid is a configuration input, not a secret, and finding it
	// is half the point of this test: CB_PORTFOLIO_ID has to come from
	// somewhere, and hunting for it in a web UI is how it gets typed wrong.
	var p struct {
		Portfolios []struct {
			Name    string `json:"name"`
			UUID    string `json:"uuid"`
			Type    string `json:"type"`
			Deleted bool   `json:"deleted"`
		} `json:"portfolios"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode portfolios: %v", err)
	}
	if len(p.Portfolios) == 0 {
		t.Fatal("no portfolios: the key authenticates but sees no account")
	}
	t.Log("portfolios visible to this key:")
	for _, x := range p.Portfolios {
		t.Logf("  type=%-12s deleted=%-5v name=%-24q uuid=%s", x.Type, x.Deleted, x.Name, x.UUID)
	}
}

func TestLiveFuturesEndpoints(t *testing.T) {
	keyID, secret := credentials(t)
	s, err := NewSigner(keyID, secret)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}

	// Part 5 reads these three. Whether they answer at all depends on the
	// account having been approved for futures, which is a fact about the
	// account and not about this code — so the failure has to say which.
	for _, tc := range []struct {
		name, path string
	}{
		{"balance summary", "/api/v3/brokerage/cfm/balance_summary"},
		{"positions", "/api/v3/brokerage/cfm/positions"},
		{"intraday margin setting", "/api/v3/brokerage/cfm/intraday/margin_setting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := liveGet(t, s, tc.path)
			switch status {
			case http.StatusOK:
				t.Logf("200, fields: %v", keysOf(t, body))
			case http.StatusUnauthorized:
				t.Errorf("401: the credential does not carry the permission this endpoint needs")
			case http.StatusForbidden, http.StatusNotFound:
				t.Skipf("%d — no futures account on this key yet, which is an onboarding step "+
					"rather than a code problem (venue doc section 7). Body: %s",
					status, truncate(body, 200))
			default:
				t.Errorf("unexpected %d: %s", status, truncate(body, 300))
			}
		})
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
