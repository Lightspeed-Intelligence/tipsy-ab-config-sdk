// Package tipsyauth issues HS256 JWTs accepted by the ab-config gRPC
// services. It is the PUBLIC counterpart of the server's internal auth
// boundary: it owns only token signing (Signer / IssueOptions / Issue) and
// the on-wire claims shape, so external modules (e.g. tipsy-backend) can
// mint their own service tokens without depending on internal packages.
//
// Verification (signature + exp/iat + namespace authorization) stays inside
// the server and is intentionally NOT exported here.
//
// Compatibility contract: the signing algorithm (HS256) and the claims JSON
// shape ({roles, namespaces, sub, iat, exp}) are byte-identical to the
// server's internal signer, so tokens minted here are accepted by the
// server's verifier when both sides share the same secret
// (TIPSY_SERVICE_SECRET).
package tipsyauth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// claims is the on-wire claim set. It MUST stay byte-identical to the
// server's internal auth.Claims: iat/exp/sub come from jwt.RegisteredClaims
// and roles/namespaces are custom claims with the exact JSON tags below.
type claims struct {
	Roles      []string `json:"roles"`
	Namespaces []string `json:"namespaces"`
	jwt.RegisteredClaims
}

// IssueOptions describes a token to mint. TTL is required (> 0).
type IssueOptions struct {
	Subject    string
	Roles      []string
	Namespaces []string
	TTL        time.Duration
	IssuedAt   time.Time // if zero, time.Now() is used
}

// Signer issues new tokens with a shared HMAC secret.
type Signer struct {
	secret []byte
}

// NewSigner constructs a Signer; an empty secret is rejected.
//
// Callers MUST check the returned error before using the Signer. Do not chain
// NewSigner(secret).Issue(...): on a bad secret NewSigner returns a nil
// *Signer, and calling Issue on it would panic (nil dereference). Use the
// two-step form instead:
//
//	s, err := tipsyauth.NewSigner(secret)
//	if err != nil { /* handle */ }
//	token, err := s.Issue(opts)
func NewSigner(secret string) (*Signer, error) {
	if secret == "" {
		return nil, errors.New("tipsyauth: HMAC secret must be non-empty")
	}
	return &Signer{secret: []byte(secret)}, nil
}

// Issue mints an HS256 JWT. TTL must be > 0; if IssuedAt is zero, time.Now()
// is used.
func (s *Signer) Issue(opts IssueOptions) (string, error) {
	if opts.TTL <= 0 {
		return "", errors.New("tipsyauth: TTL must be > 0")
	}
	now := opts.IssuedAt
	if now.IsZero() {
		now = time.Now()
	}
	cl := claims{
		Roles:      append([]string{}, opts.Roles...),
		Namespaces: append([]string{}, opts.Namespaces...),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   opts.Subject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(opts.TTL)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, cl)
	signed, err := tok.SignedString(s.secret)
	if err != nil {
		return "", fmt.Errorf("tipsyauth: sign: %w", err)
	}
	return signed, nil
}
