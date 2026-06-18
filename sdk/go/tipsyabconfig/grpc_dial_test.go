package tipsyabconfig

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

// newDialClient builds a minimal *Client carrying just the Config that dial()
// reads (cfg.DialOptions, cfg.MaxRecv/SendMessageSize, token knobs). dial() does
// not touch the logger, so this is sufficient to exercise the dial seam in
// isolation without a full Init.
func newDialClient(cfg Config) *Client {
	if cfg.MaxRecvMessageSize <= 0 {
		cfg.MaxRecvMessageSize = 512 * 1024 * 1024
	}
	if cfg.MaxSendMessageSize <= 0 {
		cfg.MaxSendMessageSize = 512 * 1024 * 1024
	}
	return &Client{cfg: cfg}
}

// TestDial_PlaintextTargetPassedThrough proves the backward-compatibility seam:
// for a plaintext target (useTLS=false) the dialTarget reaches grpc.NewClient
// verbatim. grpc.NewClient is lazy (no connection yet), so we can inspect the
// resulting ClientConn's Target() without any network I/O. The injected
// cfg.DialOptions (a bufconn context dialer) are still appended last, confirming
// the test seam survives the grpcTarget refactor.
func TestDial_PlaintextTargetPassedThrough(t *testing.T) {
	lis := bufconn.Listen(1024 * 1024)
	t.Cleanup(func() { _ = lis.Close() })

	c := newDialClient(Config{
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				return lis.DialContext(ctx)
			}),
		},
	})

	tgt := grpcTarget{dialTarget: "passthrough:///bufnet-config", useTLS: false}
	conn, err := c.dial(tgt)
	if err != nil {
		t.Fatalf("dial(plaintext) error: %v", err)
	}
	defer conn.Close()

	if got := conn.Target(); got != "passthrough:///bufnet-config" {
		t.Errorf("conn.Target() = %q, want %q (dialTarget must pass through verbatim)", got, "passthrough:///bufnet-config")
	}
}

// TestDial_TLSTargetBuildsConn proves dial() builds a ClientConn for a grpcs://
// (useTLS=true) target without touching the network (grpc.NewClient is lazy)
// and that the bare host:port dialTarget — NOT the scheme/query — is what
// reaches grpc.NewClient. We deliberately do NOT dial out to a real TLS server;
// we only assert the parse→dial wiring carries the right target.
func TestDial_TLSTargetBuildsConn(t *testing.T) {
	c := newDialClient(Config{})

	tgt := grpcTarget{
		dialTarget:         "47.253.175.59:443",
		useTLS:             true,
		authority:          "dev-ab-config-grpc.infra.fantacy.live",
		insecureSkipVerify: true,
	}
	conn, err := c.dial(tgt)
	if err != nil {
		t.Fatalf("dial(tls) error: %v", err)
	}
	defer conn.Close()

	if got := conn.Target(); got != "47.253.175.59:443" {
		t.Errorf("conn.Target() = %q, want %q (TLS dialTarget must be the bare host:port, scheme/query stripped)", got, "47.253.175.59:443")
	}
}

// TestDial_BareTargetRoundTripsRPC is the end-to-end backward-compat guard: a
// bare/native plaintext target plus an injected bufconn dialer (cfg.DialOptions)
// must still complete a real RPC through dial()'s ClientConn. This proves the
// dial-time seam used by every bufconn regression test is unbroken by the
// grpcTarget refactor. It reuses the full harness (real *grpc.Server + auth
// interceptor + fakes) and drives one PullAll via Init.
func TestDial_BareTargetRoundTripsRPC(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	// Sanity: the harness drives a bare native target through the same dial()
	// path under test (plaintext + injected dialer).
	if got, err := parseGRPCTarget(cfg.ConfigServiceAddr); err != nil || got.useTLS {
		t.Fatalf("harness ConfigServiceAddr %q must parse plaintext: %+v err=%v",
			cfg.ConfigServiceAddr, got, err)
	}

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init over bufconn (plaintext dial seam): %v", err)
	}
	defer cli.Close()

	// Startup PullAll populated the cache → the RPC round-tripped through the
	// dial()-built ClientConn over the injected bufconn dialer.
	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.PullCalls() >= 1 }) {
		t.Fatal("PullAll never reached the server: dial() seam broken")
	}
}

// TestInit_GRPCParseErrorIsParameterError verifies that an unparseable gRPC
// address surfaces from Init as a parameter error and is NOT absorbed by
// StartupFailOpen (design F6: parse errors are surfaced BEFORE dialing). We set
// StartupFailOpen=true to prove the error is classified as a parameter error,
// not a startup-pull failure.
func TestInit_GRPCParseErrorIsParameterError(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want string
	}{
		{"grpcs missing port", "grpcs://ab-config-grpc.internal", "explicit port"},
		{"http base url in grpc mode", "http://lb.internal:8080", "HTTP base URL"},
		{"grpc with query", "grpc://ab-config-grpc:50051?insecure=true", "query parameters are only valid"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			cfg := h.baseConfigNoAbtest([]string{"ns1"})
			cfg.ConfigServiceAddr = tc.addr
			cfg.StartupFailOpen = true // must NOT absorb a parameter error
			_, err := Init(context.Background(), cfg)
			if err == nil {
				t.Fatalf("Init(%q) succeeded, want parameter error", tc.addr)
			}
			if errors.Is(err, ErrStartupPullFailed) {
				t.Fatalf("parse error wrongly absorbed as startup failure: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}
