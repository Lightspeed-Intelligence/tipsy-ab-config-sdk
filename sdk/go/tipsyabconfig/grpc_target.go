package tipsyabconfig

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// grpcTarget is the parsed form of a gRPC-mode address string (design
// "gRPC 地址 Scheme 解析 + TLS 接入(方案 Y)"). parseGRPCTarget produces it; dial
// consumes it.
type grpcTarget struct {
	// dialTarget is the address handed to grpc.NewClient: scheme + query are
	// stripped, leaving a bare "host:port" (for the managed grpc/grpcs
	// schemes) or the original string verbatim (for bare host:port and native
	// grpc-go resolver targets such as passthrough:///, dns:///, unix:, xds:///).
	dialTarget string
	// useTLS is true only for grpcs://; false for everything else (bare
	// host:port, grpc://, native resolver targets) → plaintext h2c, identical
	// to the pre-scheme behaviour.
	useTLS bool
	// authority overrides the HTTP/2 :authority and, under TLS, the SNI /
	// ServerName (certificate-name target). Empty means "do not override".
	authority string
	// insecureSkipVerify disables TLS certificate verification (grpcs:// with
	// ?insecure=true). Dev / Origin-Cert-direct-IP only; never in production.
	insecureSkipVerify bool
}

// managedGRPCSchemes is the closed set of scheme prefixes that drive
// scheme-based parsing. The judgement is a LITERAL prefix match on these four
// strings — NOT a generic "does the string contain '://'" test. This is load
// bearing: grpc-go native resolver targets such as "passthrough:///bufnet-config"
// also contain "://" but MUST pass through as plaintext (design rule 2 / Review
// F1). Only an address that literally begins with one of these prefixes is
// parsed by scheme.
var (
	schemeGRPC  = "grpc://"
	schemeGRPCS = "grpcs://"
	schemeHTTP  = "http://"
	schemeHTTPS = "https://"
)

// parseGRPCTarget resolves a gRPC-mode address string into a grpcTarget per the
// scheme-whitelist rules (design Proposed Design, rules 1-5). All errors are
// parameter errors (prefix "tipsyabconfig:") returned at Init parse time, before
// dialing; they are never absorbed by StartupFailOpen.
//
// Rules:
//  1. Only addresses literally prefixed with grpc:// / grpcs:// / http:// /
//     https:// are parsed by scheme.
//  2. Everything else (bare "host:port" like "ab-config-grpc:50051" or
//     "127.0.0.1:443"; native grpc-go resolver targets like
//     "passthrough:///bufnet-config", "dns:///host", "unix:path", "xds:///") is
//     passed through verbatim as plaintext (useTLS=false, no authority, no
//     skip-verify) — the unchanged legacy path.
//  3. grpc://host:port → plaintext; any ?authority= / ?insecure= query is a
//     parameter error (Q1: those params are meaningless on plaintext).
//  4. grpcs://host:port[?authority=&insecure=] → TLS; parse the query. A missing
//     port is a parameter error (Q2: no implicit :443).
//  5. http:// / https:// in gRPC mode is a parameter error (use Transport=http).
func parseGRPCTarget(addr string) (grpcTarget, error) {
	switch {
	case strings.HasPrefix(addr, schemeGRPCS):
		return parseGRPCSScheme(addr)
	case strings.HasPrefix(addr, schemeGRPC):
		return parseGRPCScheme(addr)
	case strings.HasPrefix(addr, schemeHTTP), strings.HasPrefix(addr, schemeHTTPS):
		// HTTP base URLs belong to the HTTP transport mode (design rule 5).
		return grpcTarget{}, fmt.Errorf(
			"tipsyabconfig: %q is an HTTP base URL; gRPC mode expects a gRPC target (use Transport=http for http(s):// base URLs)", addr)
	default:
		// Rule 2: bare host:port and native grpc-go resolver targets pass
		// through verbatim as plaintext. This is the unchanged legacy path that
		// keeps all bufconn / internal-DNS dialing working.
		return grpcTarget{dialTarget: addr, useTLS: false}, nil
	}
}

// parseGRPCScheme handles grpc:// (plaintext, explicit form). Query parameters
// are rejected because they only make sense under TLS (Q1).
func parseGRPCScheme(addr string) (grpcTarget, error) {
	rest := strings.TrimPrefix(addr, schemeGRPC)
	hostport, query, hasQuery := splitQuery(rest)
	if hasQuery && query != "" {
		return grpcTarget{}, fmt.Errorf(
			"tipsyabconfig: query parameters are only valid under grpcs:// (got %q on a plaintext grpc:// target); did you mean grpcs://?", addr)
	}
	if hostport == "" {
		return grpcTarget{}, fmt.Errorf("tipsyabconfig: grpc:// target is missing host:port in %q", addr)
	}
	return grpcTarget{dialTarget: hostport, useTLS: false}, nil
}

// parseGRPCSScheme handles grpcs:// (TLS). It requires an explicit port (Q2: no
// implicit :443) and parses the authority / insecure query parameters.
func parseGRPCSScheme(addr string) (grpcTarget, error) {
	rest := strings.TrimPrefix(addr, schemeGRPCS)
	hostport, rawQuery, _ := splitQuery(rest)
	if hostport == "" {
		return grpcTarget{}, fmt.Errorf("tipsyabconfig: grpcs:// target is missing host:port in %q", addr)
	}
	// Require an explicit, numeric port (Q2 / S2). A missing port is one error
	// ("explicit port"); a present-but-non-numeric port is a distinct parameter
	// error ("invalid port"), surfaced at parse time rather than deferred to the
	// dialer — symmetric with Python's urlparse `.port` "invalid port" failure.
	if err := validateHostPort(hostport, addr); err != nil {
		return grpcTarget{}, err
	}

	out := grpcTarget{dialTarget: hostport, useTLS: true}

	if rawQuery == "" {
		return out, nil
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return grpcTarget{}, fmt.Errorf("tipsyabconfig: invalid query in grpcs:// target %q: %v", addr, err)
	}
	for key := range values {
		switch key {
		case "authority", "insecure":
			// recognised
		default:
			return grpcTarget{}, fmt.Errorf(
				"tipsyabconfig: unknown query parameter %q in grpcs:// target %q (supported: authority, insecure)", key, addr)
		}
	}
	out.authority = values.Get("authority")
	if raw := values.Get("insecure"); raw != "" {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "true", "1":
			out.insecureSkipVerify = true
		case "false", "0":
			out.insecureSkipVerify = false
		default:
			return grpcTarget{}, fmt.Errorf(
				"tipsyabconfig: invalid insecure value %q in grpcs:// target %q (expected true/false)", raw, addr)
		}
	}
	return out, nil
}

// splitQuery splits "host:port?query" into ("host:port", "query", hadQuery).
func splitQuery(s string) (head, query string, hadQuery bool) {
	if i := strings.IndexByte(s, '?'); i >= 0 {
		return s[:i], s[i+1:], true
	}
	return s, "", false
}

// validateHostPort checks that hostport carries an explicit, numeric port. It
// returns a parameter error (prefix "tipsyabconfig:") otherwise:
//   - missing port (no ":port" suffix, or a trailing-colon-only form, or a bare
//     IPv6 literal "[::1]") → "must specify an explicit port".
//   - present-but-non-numeric / out-of-range port ("host:abc", "[::1]:abc",
//     "host:99999") → "invalid port".
//
// net.SplitHostPort handles bracketed IPv6 literals ("[::1]:443" → host "::1",
// port "443"); strconv.ParseUint then pins the port to a 16-bit unsigned
// integer. addr is the original target string, used for the error message.
func validateHostPort(hostport, addr string) error {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil || port == "" {
		// SplitHostPort errors on a missing port ("host", "[::1]") and on a
		// trailing-colon-only form yields an empty port ("host:"). Both mean
		// "no explicit port".
		return fmt.Errorf(
			"tipsyabconfig: grpcs:// target must specify an explicit port (e.g. :443) in %q", addr)
	}
	if host == "" {
		// e.g. ":443" — no host. Treat as a malformed target.
		return fmt.Errorf(
			"tipsyabconfig: grpcs:// target is missing a host before the port in %q", addr)
	}
	if _, perr := strconv.ParseUint(port, 10, 16); perr != nil {
		return fmt.Errorf(
			"tipsyabconfig: invalid port %q in grpcs:// target %q (expected a number 0-65535)", port, addr)
	}
	return nil
}
