package tipsyabconfig

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

// This file covers the design "SDK Headless Service + 客户端 round_robin" feature
// for the Go SDK (design doc §Proposed Design 1.1 + §Testing Plan + §Acceptance
// Criteria #10). The unit under test is:
//
//   1. serviceConfigFor(dialTarget string) string — pure mapping from a dial
//      target to the JSON service config to inject (round_robin for dns:///,
//      empty for every other form).
//   2. (*Client).dial(grpcTarget) — must thread the serviceConfigFor result
//      into grpc.NewClient only for dns:/// targets, and stay byte-for-byte
//      identical to the legacy plaintext / TLS paths for everything else.
//
// We deliberately keep dial() assertions behavioural (no reflect / unsafe poke
// at grpc-go internals): grpc-go does not expose the resolved service config on
// *grpc.ClientConn, and design §"关键约束" forbids reflect black magic. The
// strong contract is therefore proved at the serviceConfigFor layer; the dial()
// layer is observed only through Target() + "does not error" smoke.

// TestServiceConfigFor_DnsPrefix_ReturnsRoundRobin proves the positive contract
// (design §Proposed Design 1.1 + §Testing Plan Go unit case 1): every dns:///
// target gets a non-empty service-config JSON whose loadBalancingConfig[0]
// names round_robin. Three host shapes — bare host, host:port, K8S FQDN — all
// take the same branch.
func TestServiceConfigFor_DnsPrefix_ReturnsRoundRobin(t *testing.T) {
	cases := []struct {
		name   string
		target string
	}{
		{name: "dns:/// bare host", target: "dns:///foo"},
		{name: "dns:/// host:port", target: "dns:///foo:50051"},
		{name: "dns:/// k8s headless fqdn", target: "dns:///foo.bar.svc.cluster.local:50051"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := serviceConfigFor(tc.target)
			if got == "" {
				t.Fatalf("serviceConfigFor(%q) = %q, want non-empty round_robin JSON", tc.target, got)
			}
			if !strings.Contains(got, "round_robin") {
				t.Errorf("serviceConfigFor(%q) = %q, want substring %q", tc.target, got, "round_robin")
			}

			// Parse the returned JSON and assert the structural shape grpc-go
			// expects: loadBalancingConfig[0] is an object with a round_robin
			// key. A plain Contains() is not enough — it would pass if a future
			// edit accidentally emitted {"loadBalancingPolicy":"round_robin"}
			// or some other malformed payload that still contains the literal.
			var parsed map[string]any
			if err := json.Unmarshal([]byte(got), &parsed); err != nil {
				t.Fatalf("serviceConfigFor(%q) returned non-JSON %q: %v", tc.target, got, err)
			}
			lbcRaw, ok := parsed["loadBalancingConfig"]
			if !ok {
				t.Fatalf("service config %q missing loadBalancingConfig key", got)
			}
			lbcList, ok := lbcRaw.([]any)
			if !ok || len(lbcList) == 0 {
				t.Fatalf("loadBalancingConfig is not a non-empty list in %q (got %T = %v)", got, lbcRaw, lbcRaw)
			}
			first, ok := lbcList[0].(map[string]any)
			if !ok {
				t.Fatalf("loadBalancingConfig[0] is not an object in %q (got %T)", got, lbcList[0])
			}
			if _, ok := first["round_robin"]; !ok {
				t.Errorf("loadBalancingConfig[0] missing round_robin key in %q", got)
			}
		})
	}
}

// TestServiceConfigFor_OtherPrefixes_ReturnsEmpty is the explicit negative
// regression test required by design §Acceptance Criteria #10 (design review
// M1): for every NON-dns:/// target the SDK MUST NOT inject a service config,
// preserving grpc-go default pick_first behaviour and keeping all existing
// deployment shapes byte-for-byte identical. This table is the canonical
// inventory of "every other form the SDK accepts" — adding a new accepted
// scheme to grpc_target.go without a row here is a coverage gap.
func TestServiceConfigFor_OtherPrefixes_ReturnsEmpty(t *testing.T) {
	cases := []struct {
		name   string
		target string
	}{
		{name: "empty string", target: ""},
		{name: "bare host:port", target: "foo:50051"},
		{name: "bare fqdn:port", target: "foo.bar:50051"},
		{name: "grpc:// plaintext", target: "grpc://foo:50051"},
		{name: "grpcs:// TLS", target: "grpcs://foo:443"},
		{name: "grpcs:// with authority + insecure (Dev串)", target: "grpcs://foo:443?authority=x.y&insecure=true"},
		{name: "passthrough:/// native resolver", target: "passthrough:///foo:50051"},
		{name: "unix: native resolver", target: "unix:/tmp/abconfig.sock"},
		{name: "xds:/// native resolver", target: "xds:///foo"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := serviceConfigFor(tc.target); got != "" {
				t.Errorf("serviceConfigFor(%q) = %q, want \"\" (AC #10: only dns:/// targets get a service config)", tc.target, got)
			}
		})
	}
}

// TestServiceConfigFor_DnsEmptyHost is the explicit boundary the design notes
// call out: strings.HasPrefix("dns:///", "dns:///") is true, so an empty-host
// dns:/// dial target still falls into the round_robin branch. This is
// "acceptable" — grpc-go will surface a DNS resolve failure at first-RPC time
// rather than at SDK init. The test pins the current behaviour so a future
// "tighten the prefix check" edit is intentional, not accidental.
func TestServiceConfigFor_DnsEmptyHost(t *testing.T) {
	got := serviceConfigFor("dns:///")
	if got == "" || !strings.Contains(got, "round_robin") {
		t.Errorf("serviceConfigFor(%q) = %q, want round_robin JSON (boundary: empty host still matches the dns:/// prefix)", "dns:///", got)
	}
}

// TestDial_DnsTarget_InjectsServiceConfig is the dial-layer positive smoke
// (design §Testing Plan Go unit case 3). grpc.NewClient is lazy — no network
// I/O until the first RPC — so we can build a ClientConn for a dns:///
// target without DNS resolution actually running. The strong service-config
// assertion already lives in TestServiceConfigFor_DnsPrefix_ReturnsRoundRobin;
// at the dial layer we only observe:
//
//   1. dial() does not error on a dns:/// target (so the new
//      WithDefaultServiceConfig append did not break the option list), and
//   2. the dialTarget reaches grpc.NewClient verbatim (Target() round-trip).
//
// grpc-go does not expose the resolved service config on *grpc.ClientConn,
// and design §"关键约束" forbids reflect / unsafe to dig it out, so this is
// the strongest behavioural assertion available at this layer.
func TestDial_DnsTarget_InjectsServiceConfig(t *testing.T) {
	c := newDialClient(Config{})

	tgt := grpcTarget{dialTarget: "dns:///foo:50051", useTLS: false}
	conn, err := c.dial(tgt)
	if err != nil {
		t.Fatalf("dial(dns:///) error: %v (the WithDefaultServiceConfig append must not break the option list)", err)
	}
	defer conn.Close()

	if got := conn.Target(); got != "dns:///foo:50051" {
		t.Errorf("conn.Target() = %q, want %q (dns:/// dialTarget must pass through verbatim)", got, "dns:///foo:50051")
	}
}

// TestDial_OtherTargets_NoServiceConfig is the dial-layer negative regression
// (design §Testing Plan Go unit case 4 + AC #10). For the five non-dns:///
// shapes from AC #10, dial() must continue to build a ClientConn with the same
// dialTarget as before — i.e. the new conditional append leaves these paths
// untouched. The strong "WithDefaultServiceConfig is NOT called" assertion
// lives at the pure-function layer (TestServiceConfigFor_OtherPrefixes_
// ReturnsEmpty); here we only assert the externally observable behaviour
// (dial succeeds, Target() round-trips), per design §"关键约束".
func TestDial_OtherTargets_NoServiceConfig(t *testing.T) {
	cases := []struct {
		name string
		tgt  grpcTarget
	}{
		{
			name: "bare host:port (legacy plaintext)",
			tgt:  grpcTarget{dialTarget: "foo:50051", useTLS: false},
		},
		{
			name: "grpcs:// TLS bare",
			tgt:  grpcTarget{dialTarget: "foo:443", useTLS: true},
		},
		{
			name: "grpcs:// TLS with authority + skip-verify (Dev串)",
			tgt: grpcTarget{
				dialTarget:         "foo:443",
				useTLS:             true,
				authority:          "x.y",
				insecureSkipVerify: true,
			},
		},
		{
			name: "passthrough:/// native resolver (bufconn backstop)",
			tgt:  grpcTarget{dialTarget: "passthrough:///bufnet-config", useTLS: false},
		},
		{
			name: "unix: native resolver",
			tgt:  grpcTarget{dialTarget: "unix:/tmp/abconfig.sock", useTLS: false},
		},
	}
	c := newDialClient(Config{})
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			conn, err := c.dial(tc.tgt)
			if err != nil {
				t.Fatalf("dial(%q) error: %v (non-dns:/// path must stay byte-for-byte compatible)", tc.tgt.dialTarget, err)
			}
			defer conn.Close()

			if got := conn.Target(); got != tc.tgt.dialTarget {
				t.Errorf("conn.Target() = %q, want %q (dialTarget must pass through verbatim)", got, tc.tgt.dialTarget)
			}
		})
	}
}

// TestDial_DnsTarget_KeepaliveStillApplied is the design R6 regression
// placeholder. R6 requires us to pin "dns:/// + round_robin does not silently
// drop the existing WithKeepaliveParams" so a future grpc-go upgrade does not
// regress per-subchannel keepalive propagation. dial() unconditionally calls
// grpc.WithKeepaliveParams, so if a future refactor accidentally moves that
// call inside an else-branch alongside WithDefaultServiceConfig, this test's
// dns:/// dial must still succeed and round-trip the target.
//
// NOTE: grpc-go v1.80 does not expose the per-channel keepalive parameters on
// *grpc.ClientConn, so this test is intentionally a SMOKE: we drive the
// dns:/// path through dial() with a bufconn dialer attached (proving the
// option list — including keepalive — composes cleanly) and assert dial does
// not error and Target() round-trips. When grpc-go exposes a keepalive
// introspection hook this test should be strengthened to read back
// Time / Timeout / PermitWithoutStream and compare to the configured values.
func TestDial_DnsTarget_KeepaliveStillApplied(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { _ = lis.Close() })

	// Attach a bufconn dialer through cfg.DialOptions to prove the option list
	// composes cleanly when WithDefaultServiceConfig, WithKeepaliveParams AND
	// a user-supplied WithContextDialer all coexist on a dns:/// target.
	c := newDialClient(Config{
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		},
	})

	tgt := grpcTarget{dialTarget: "dns:///foo:50051", useTLS: false}
	conn, err := c.dial(tgt)
	if err != nil {
		t.Fatalf("dial(dns:/// with keepalive + bufconn dialer) error: %v", err)
	}
	defer conn.Close()

	if got := conn.Target(); got != "dns:///foo:50051" {
		t.Errorf("conn.Target() = %q, want %q (dns:/// dialTarget must pass through verbatim even with keepalive + custom dialer)", got, "dns:///foo:50051")
	}
}
