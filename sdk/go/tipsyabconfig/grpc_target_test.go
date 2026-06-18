package tipsyabconfig

import (
	"strings"
	"testing"
)

// TestParseGRPCTarget_Success is the table-driven coverage of every VALID
// address form in the 方案 Y grammar (design "gRPC 地址 Scheme 解析 + TLS 接入").
//
// The contract under test is the parser's classification, NOT dialing:
//
//	dialTarget         — scheme+query stripped (managed schemes) or verbatim
//	useTLS             — true only for grpcs://
//	authority          — query override (empty = no override)
//	insecureSkipVerify — grpcs://...?insecure=true
//
// The most load-bearing rows are the regression backstops (F1/F9): native
// grpc-go resolver targets (passthrough:///, dns:///, unix:, xds:///) and bare
// host:port MUST be judged "pass-through + plaintext" so all bufconn /
// internal-DNS dialing keeps working unchanged.
func TestParseGRPCTarget_Success(t *testing.T) {
	type want struct {
		dialTarget         string
		useTLS             bool
		authority          string
		insecureSkipVerify bool
	}
	cases := []struct {
		name string
		addr string
		want want
	}{
		// ---- bare host:port → pass-through + plaintext (backward compatible) ----
		{
			name: "bare hostname:port",
			addr: "ab-config-grpc:50051",
			want: want{dialTarget: "ab-config-grpc:50051", useTLS: false},
		},
		{
			// F9 core: 127.0.0.1:443 must NOT be misread (the dotted-quad must
			// not be treated as a scheme); plaintext pass-through.
			name: "bare ipv4:port (F9 regression — not a scheme)",
			addr: "127.0.0.1:443",
			want: want{dialTarget: "127.0.0.1:443", useTLS: false},
		},
		// ---- native grpc-go resolver targets → pass-through + plaintext (F1) ----
		{
			// F1 core: the Go harness uses exactly this address; it contains
			// "://" yet MUST pass through plaintext so every bufconn regression
			// (pull/subscribe/cache/exposure/...) is untouched.
			name: "passthrough:/// (F1 regression — bufconn backstop)",
			addr: "passthrough:///bufnet-config",
			want: want{dialTarget: "passthrough:///bufnet-config", useTLS: false},
		},
		{
			name: "dns:/// native target",
			addr: "dns:///ab-config-grpc.svc.cluster.local:50051",
			want: want{dialTarget: "dns:///ab-config-grpc.svc.cluster.local:50051", useTLS: false},
		},
		{
			name: "unix: native target",
			addr: "unix:/var/run/abconfig.sock",
			want: want{dialTarget: "unix:/var/run/abconfig.sock", useTLS: false},
		},
		{
			name: "xds:/// native target",
			addr: "xds:///ab-config-grpc",
			want: want{dialTarget: "xds:///ab-config-grpc", useTLS: false},
		},
		// ---- grpc:// (explicit plaintext) ----
		{
			name: "grpc:// strips scheme, plaintext",
			addr: "grpc://ab-config-grpc:50051",
			want: want{dialTarget: "ab-config-grpc:50051", useTLS: false},
		},
		// ---- grpcs:// (TLS) ----
		{
			name: "grpcs:// no query → TLS, no authority",
			addr: "grpcs://prod-ab-config-grpc.infra.example.com:443",
			want: want{dialTarget: "prod-ab-config-grpc.infra.example.com:443", useTLS: true},
		},
		{
			// The exact Dev接入串 from the design.
			name: "grpcs:// with authority + insecure=true (Dev串)",
			addr: "grpcs://47.253.175.59:443?authority=dev-ab-config-grpc.infra.fantacy.live&insecure=true",
			want: want{
				dialTarget:         "47.253.175.59:443",
				useTLS:             true,
				authority:          "dev-ab-config-grpc.infra.fantacy.live",
				insecureSkipVerify: true,
			},
		},
		{
			name: "grpcs:// authority only (no insecure)",
			addr: "grpcs://10.0.0.5:443?authority=ab-config-grpc.internal",
			want: want{
				dialTarget: "10.0.0.5:443",
				useTLS:     true,
				authority:  "ab-config-grpc.internal",
			},
		},
		{
			name: "grpcs:// insecure=false is explicit-on-verify",
			addr: "grpcs://host.example.com:443?insecure=false",
			want: want{dialTarget: "host.example.com:443", useTLS: true, insecureSkipVerify: false},
		},
		{
			name: "grpcs:// insecure=1 truthy",
			addr: "grpcs://host.example.com:443?insecure=1",
			want: want{dialTarget: "host.example.com:443", useTLS: true, insecureSkipVerify: true},
		},
		{
			name: "grpcs:// insecure=0 falsy",
			addr: "grpcs://host.example.com:443?insecure=0",
			want: want{dialTarget: "host.example.com:443", useTLS: true, insecureSkipVerify: false},
		},
		// ---- IPv6 ----
		{
			name: "grpcs:// IPv6 literal with port → TLS",
			addr: "grpcs://[::1]:443",
			want: want{dialTarget: "[::1]:443", useTLS: true},
		},
		{
			name: "grpcs:// IPv6 literal with authority",
			addr: "grpcs://[2001:db8::1]:443?authority=ab-config-grpc.internal",
			want: want{
				dialTarget: "[2001:db8::1]:443",
				useTLS:     true,
				authority:  "ab-config-grpc.internal",
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGRPCTarget(tc.addr)
			if err != nil {
				t.Fatalf("parseGRPCTarget(%q) unexpected error: %v", tc.addr, err)
			}
			if got.dialTarget != tc.want.dialTarget {
				t.Errorf("dialTarget = %q, want %q", got.dialTarget, tc.want.dialTarget)
			}
			if got.useTLS != tc.want.useTLS {
				t.Errorf("useTLS = %v, want %v", got.useTLS, tc.want.useTLS)
			}
			if got.authority != tc.want.authority {
				t.Errorf("authority = %q, want %q", got.authority, tc.want.authority)
			}
			if got.insecureSkipVerify != tc.want.insecureSkipVerify {
				t.Errorf("insecureSkipVerify = %v, want %v", got.insecureSkipVerify, tc.want.insecureSkipVerify)
			}
		})
	}
}

// TestParseGRPCTarget_Errors covers every ERROR path. All are parameter errors
// (prefix "tipsyabconfig:") surfaced at parse time. We assert an error is
// returned and that its message contains a discriminating keyword so a future
// reword that drops the contract intent is caught.
func TestParseGRPCTarget_Errors(t *testing.T) {
	cases := []struct {
		name        string
		addr        string
		wantSubstrs []string // all must appear (case-sensitive) somewhere in err
	}{
		{
			// Q1: query on a plaintext grpc:// target is meaningless → error.
			name:        "grpc:// with authority query rejected",
			addr:        "grpc://ab-config-grpc:50051?authority=x",
			wantSubstrs: []string{"query parameters are only valid"},
		},
		{
			name:        "grpc:// with insecure query rejected",
			addr:        "grpc://ab-config-grpc:50051?insecure=true",
			wantSubstrs: []string{"query parameters are only valid"},
		},
		{
			// Q2: grpcs:// without an explicit port is an error (no implicit :443).
			name:        "grpcs:// missing port",
			addr:        "grpcs://ab-config-grpc.internal",
			wantSubstrs: []string{"explicit port"},
		},
		{
			name:        "grpcs:// IPv6 missing port",
			addr:        "grpcs://[::1]",
			wantSubstrs: []string{"explicit port"},
		},
		{
			// S2: a present-but-non-numeric port is "invalid port", distinct from
			// the missing-port "explicit port" error above and surfaced at parse
			// time (not deferred to the dialer).
			name:        "grpcs:// non-numeric port",
			addr:        "grpcs://host:abc",
			wantSubstrs: []string{"invalid port"},
		},
		{
			name:        "grpcs:// IPv6 non-numeric port",
			addr:        "grpcs://[::1]:abc",
			wantSubstrs: []string{"invalid port"},
		},
		{
			name:        "grpcs:// out-of-range port",
			addr:        "grpcs://host:99999",
			wantSubstrs: []string{"invalid port"},
		},
		{
			name:        "grpcs:// unknown query key rejected",
			addr:        "grpcs://host.example.com:443?foo=bar",
			wantSubstrs: []string{"unknown query parameter"},
		},
		{
			name:        "grpcs:// invalid insecure value rejected",
			addr:        "grpcs://host.example.com:443?insecure=maybe",
			wantSubstrs: []string{"invalid insecure value"},
		},
		{
			// design rule 5: http:// in gRPC mode is a parameter error.
			name:        "http:// in gRPC mode rejected",
			addr:        "http://lb.internal:8080",
			wantSubstrs: []string{"HTTP base URL"},
		},
		{
			name:        "https:// in gRPC mode rejected",
			addr:        "https://lb.internal:8443",
			wantSubstrs: []string{"HTTP base URL"},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseGRPCTarget(tc.addr)
			if err == nil {
				t.Fatalf("parseGRPCTarget(%q) = %+v, want error", tc.addr, got)
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "tipsyabconfig:") {
				t.Errorf("error should be a tipsyabconfig: parameter error, got %q", msg)
			}
			for _, sub := range tc.wantSubstrs {
				if !strings.Contains(msg, sub) {
					t.Errorf("error %q does not contain expected keyword %q", msg, sub)
				}
			}
		})
	}
}

// TestParseGRPCTarget_PassthroughIsPlaintext is an explicit, standalone
// assertion of the single most important regression backstop (design Risks /
// F1): the bufconn harness address must parse to "pass-through + plaintext"
// with no authority and no skip-verify. Kept separate from the table so a
// breakage names this exact contract.
func TestParseGRPCTarget_PassthroughIsPlaintext(t *testing.T) {
	for _, addr := range []string{
		"passthrough:///bufnet-config",
		"passthrough:///bufnet-abtest",
	} {
		got, err := parseGRPCTarget(addr)
		if err != nil {
			t.Fatalf("parseGRPCTarget(%q): unexpected error %v", addr, err)
		}
		if got.dialTarget != addr {
			t.Errorf("dialTarget = %q, want verbatim %q", got.dialTarget, addr)
		}
		if got.useTLS {
			t.Errorf("useTLS = true for %q; native resolver targets must stay plaintext", addr)
		}
		if got.authority != "" {
			t.Errorf("authority = %q for %q; must be empty (no override)", got.authority, addr)
		}
		if got.insecureSkipVerify {
			t.Errorf("insecureSkipVerify = true for %q; must be false", addr)
		}
	}
}

// TestParseGRPCTarget_BareLoopbackIsPlaintext pins the other F9 backstop: a
// bare 127.0.0.1:443 (the form Python's conftest uses; symmetric here for the
// Go contract) is plaintext pass-through, never misclassified as TLS.
func TestParseGRPCTarget_BareLoopbackIsPlaintext(t *testing.T) {
	got, err := parseGRPCTarget("127.0.0.1:443")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.useTLS || got.dialTarget != "127.0.0.1:443" {
		t.Fatalf("bare 127.0.0.1:443 misclassified: %+v", got)
	}
}
