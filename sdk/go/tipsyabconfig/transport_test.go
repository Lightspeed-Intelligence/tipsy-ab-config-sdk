package tipsyabconfig

import (
	"context"
	"errors"
	"testing"
	"time"
)

// transportEnvVar is the environment variable the SDK reads at Init to pick the
// transport when Config.Transport is empty. Asserted here so a rename on the
// implementation side is caught by a failing test rather than silently ignored.
const transportEnvVar = "TIPSY_SDK_TRANSPORT"

// TestTransportConstants pins the exported transport constant values so the
// wire/contract names ("grpc"/"http") can't drift.
func TestTransportConstants(t *testing.T) {
	if TransportGRPC != "grpc" {
		t.Errorf("TransportGRPC = %q, want %q", TransportGRPC, "grpc")
	}
	if TransportHTTP != "http" {
		t.Errorf("TransportHTTP = %q, want %q", TransportHTTP, "http")
	}
}

// TestTransport_DefaultIsGRPC: empty Transport + unset env var → gRPC mode.
// Proven via the bufconn gRPC harness: a default-Transport Config drives the
// real ConfigService.Subscribe stream, which only the gRPC path ever opens.
func TestTransport_DefaultIsGRPC(t *testing.T) {
	t.Setenv(transportEnvVar, "") // ensure no ambient override leaks in
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cfg.Transport = "" // explicit: take the default
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init default (grpc): %v", err)
	}
	defer cli.Close()

	// Subscribe is gRPC-only; observing a Subscribe call proves grpc mode.
	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatal("default transport did not behave as gRPC (no Subscribe attached)")
	}
	if !waitFor(t, 2*time.Second, func() bool { return cli.Health().SubscribeConnected }) {
		t.Fatal("default transport: SubscribeConnected never true (expected for gRPC mode)")
	}
}

// TestTransport_ExplicitGRPC: Config.Transport set to the gRPC constant behaves
// identically to the default.
func TestTransport_ExplicitGRPC(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cfg.Transport = TransportGRPC
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init grpc: %v", err)
	}
	defer cli.Close()
	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatal("explicit grpc transport did not open Subscribe")
	}
}

// TestTransport_CaseAndWhitespaceTolerant exercises trim + lower-case
// normalization for both modes. " Grpc " and "HTTP" must be accepted.
func TestTransport_CaseAndWhitespaceTolerant(t *testing.T) {
	t.Run("grpc with surrounding whitespace + mixed case", func(t *testing.T) {
		h := newHarness(t)
		h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
		cfg := h.baseConfigNoAbtest([]string{"ns1"})
		cfg.Transport = "  Grpc  "
		cli, err := Init(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Init with '  Grpc  ': %v", err)
		}
		defer cli.Close()
		if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
			t.Fatal("'  Grpc  ' did not normalize to grpc mode (no Subscribe)")
		}
	})

	t.Run("HTTP upper-case", func(t *testing.T) {
		h := newHTTPHarness(t)
		h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
			full     int64
			versions map[int64]string
		}{"k": {full: 1, versions: map[int64]string{1: "v1"}}}))
		cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"})
		cfg.Transport = "HTTP"
		cli, err := Init(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Init with 'HTTP': %v", err)
		}
		defer cli.Close()
		// HTTP mode populated the cache via pull_all, and never opens Subscribe.
		if v, ok := cli.GetConfigStatic("ns1", "k", "def"); !ok || v != "v1" {
			t.Fatalf("'HTTP' did not normalize to http mode: cache (%q,%v)", v, ok)
		}
		if cli.Health().SubscribeConnected {
			t.Fatal("'HTTP' mode must not connect Subscribe")
		}
	})
}

// TestTransport_EnvVarSelectsHTTP: with Config.Transport empty, the env var
// drives selection.
func TestTransport_EnvVarSelectsHTTP(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "v1"}}}))

	t.Setenv(transportEnvVar, "http")
	cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"})
	cfg.Transport = "" // empty → env var wins
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init with env TIPSY_SDK_TRANSPORT=http: %v", err)
	}
	defer cli.Close()
	if v, ok := cli.GetConfigStatic("ns1", "k", "def"); !ok || v != "v1" {
		t.Fatalf("env-selected http mode did not populate cache: (%q,%v)", v, ok)
	}
}

// TestTransport_ConfigOverridesEnvVar: explicit Config.Transport beats the env
// var. Env says http, Config says grpc → gRPC mode (Subscribe opens).
func TestTransport_ConfigOverridesEnvVar(t *testing.T) {
	t.Setenv(transportEnvVar, "http")
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cfg.Transport = TransportGRPC // explicit grpc must override env http
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init grpc (env=http): %v", err)
	}
	defer cli.Close()
	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatal("Config.Transport=grpc did not override env TIPSY_SDK_TRANSPORT=http")
	}
}

// TestTransport_InvalidValueFromConfig: an unknown Transport value is a
// parameter error from Init and is NOT absorbed by StartupFailOpen.
func TestTransport_InvalidValueFromConfig(t *testing.T) {
	h := newHarness(t)
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cfg.Transport = "rest"
	cfg.StartupFailOpen = true // must not absorb a parameter error
	_, err := Init(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected Init to reject unknown Transport value 'rest'")
	}
	if errors.Is(err, ErrStartupPullFailed) {
		t.Fatalf("invalid transport must be a parameter error, not absorbed startup failure: %v", err)
	}
}

// TestTransport_InvalidValueFromEnv: an unknown env-var value is likewise a
// parameter error.
func TestTransport_InvalidValueFromEnv(t *testing.T) {
	t.Setenv(transportEnvVar, "websocket")
	h := newHarness(t)
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cfg.Transport = "" // empty → env var consulted
	_, err := Init(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected Init to reject unknown env TIPSY_SDK_TRANSPORT value")
	}
	if errors.Is(err, ErrStartupPullFailed) {
		t.Fatalf("invalid env transport must be a parameter error: %v", err)
	}
}
