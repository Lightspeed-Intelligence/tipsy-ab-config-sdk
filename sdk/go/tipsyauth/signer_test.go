package tipsyauth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testSecret = "tipsyauth-unit-test-secret-do-not-use"

// parseUnverified decodes a token's claims WITHOUT checking the signature so
// the pure tipsyauth unit tests can assert iat/exp/alg without importing the
// internal verifier (tipsyauth must NOT depend on internal/auth). The signed
// cross-package round-trip lives in internal/auth/tipsyauth_contract_test.go.
func parseUnverified(t *testing.T, raw string) (jwt.MapClaims, *jwt.Token) {
	t.Helper()
	mc := jwt.MapClaims{}
	tok, _, err := jwt.NewParser().ParseUnverified(raw, mc)
	if err != nil {
		t.Fatalf("ParseUnverified: %v", err)
	}
	return mc, tok
}

func TestNewSigner_EmptySecretRejected(t *testing.T) {
	if _, err := NewSigner(""); err == nil {
		t.Fatal("NewSigner(\"\") = nil err; want non-nil")
	}
}

func TestNewSigner_NonEmptySecretOK(t *testing.T) {
	s, err := NewSigner(testSecret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if s == nil {
		t.Fatal("NewSigner returned nil signer with nil error")
	}
}

func TestIssue_RejectsNonPositiveTTL(t *testing.T) {
	s, err := NewSigner(testSecret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	// TTL == 0
	if _, err := s.Issue(IssueOptions{Subject: "x"}); err == nil {
		t.Fatal("TTL=0 accepted; want error")
	}
	// TTL < 0
	if _, err := s.Issue(IssueOptions{Subject: "x", TTL: -time.Second}); err == nil {
		t.Fatal("TTL<0 accepted; want error")
	}
}

// TestIssue_ZeroIssuedAtUsesNow asserts that when IssuedAt is the zero value,
// the signer stamps iat ~= now and exp ~= now+TTL.
func TestIssue_ZeroIssuedAtUsesNow(t *testing.T) {
	s, err := NewSigner(testSecret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	const ttl = time.Hour
	before := time.Now()
	tok, err := s.Issue(IssueOptions{Subject: "x", TTL: ttl})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	after := time.Now()

	mc, parsed := parseUnverified(t, tok)
	if parsed.Method.Alg() != jwt.SigningMethodHS256.Alg() {
		t.Fatalf("alg = %q want HS256", parsed.Method.Alg())
	}

	iat := numericClaim(t, mc, "iat")
	exp := numericClaim(t, mc, "exp")

	// iat should sit inside [before, after] (allow 1s slack for clock-second
	// truncation in jwt.NewNumericDate).
	if iat < before.Add(-time.Second).Unix() || iat > after.Add(time.Second).Unix() {
		t.Fatalf("iat=%d not within [%d,%d]", iat, before.Unix(), after.Unix())
	}
	// exp should be iat + TTL.
	if want := iat + int64(ttl/time.Second); exp != want {
		t.Fatalf("exp=%d want iat+TTL=%d", exp, want)
	}
}

// TestIssue_ExplicitIssuedAtHonored asserts a supplied IssuedAt is used verbatim
// for iat and exp = IssuedAt + TTL.
func TestIssue_ExplicitIssuedAtHonored(t *testing.T) {
	s, err := NewSigner(testSecret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	issued := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	const ttl = 2 * time.Hour
	tok, err := s.Issue(IssueOptions{Subject: "x", TTL: ttl, IssuedAt: issued})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	mc, _ := parseUnverified(t, tok)
	if got := numericClaim(t, mc, "iat"); got != issued.Unix() {
		t.Fatalf("iat=%d want %d", got, issued.Unix())
	}
	if got := numericClaim(t, mc, "exp"); got != issued.Add(ttl).Unix() {
		t.Fatalf("exp=%d want %d", got, issued.Add(ttl).Unix())
	}
}

// TestIssue_ClaimsShape pins the on-wire JSON shape: roles / namespaces / sub
// carry the expected values. This guards the byte-compatibility contract with
// the server's internal auth.Claims independent of the signed round-trip.
func TestIssue_ClaimsShape(t *testing.T) {
	s, err := NewSigner(testSecret)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	tok, err := s.Issue(IssueOptions{
		Subject:    "tipsy-backend",
		Roles:      []string{"business_sdk"},
		Namespaces: []string{"tipsy-backend", "ns2"},
		TTL:        time.Hour,
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	mc, _ := parseUnverified(t, tok)

	if sub, _ := mc["sub"].(string); sub != "tipsy-backend" {
		t.Fatalf("sub=%v want tipsy-backend", mc["sub"])
	}
	roles := stringSliceClaim(t, mc, "roles")
	if len(roles) != 1 || roles[0] != "business_sdk" {
		t.Fatalf("roles=%v want [business_sdk]", roles)
	}
	ns := stringSliceClaim(t, mc, "namespaces")
	if len(ns) != 2 || ns[0] != "tipsy-backend" || ns[1] != "ns2" {
		t.Fatalf("namespaces=%v want [tipsy-backend ns2]", ns)
	}
}

func numericClaim(t *testing.T, mc jwt.MapClaims, key string) int64 {
	t.Helper()
	v, ok := mc[key]
	if !ok {
		t.Fatalf("claim %q missing", key)
	}
	f, ok := v.(float64) // JSON numbers decode to float64
	if !ok {
		t.Fatalf("claim %q is %T want number", key, v)
	}
	return int64(f)
}

func stringSliceClaim(t *testing.T, mc jwt.MapClaims, key string) []string {
	t.Helper()
	raw, ok := mc[key]
	if !ok {
		t.Fatalf("claim %q missing", key)
	}
	arr, ok := raw.([]any)
	if !ok {
		t.Fatalf("claim %q is %T want array", key, raw)
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		s, ok := x.(string)
		if !ok {
			t.Fatalf("claim %q element is %T want string", key, x)
		}
		out = append(out, s)
	}
	return out
}
