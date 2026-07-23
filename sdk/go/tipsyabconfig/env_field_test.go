package tipsyabconfig

// ST5 test file (abconfig-env-field design D10 + Testing Plan §范围 B / Go SDK).
//
// Covers the client-level Config.Env knob and its propagation onto EVERY
// outbound request. Env is a single-value environment identifier stamped by the
// client onto all four request construction points:
//   - GetExperimentResultRequest (experiment_result.go, direct wrapper)
//   - GetExperimentResultRequest (abtest_context.go, config_version flat_kv
//     fast path behind GetConfig / WaitForAbtest)
//   - PullAllRequest             (pull.go, startup + periodic PullAll)
//   - SubscribeRequest           (subscribe.go, gRPC-only stream)
//
// The acceptance criteria specifically call out the "Env default '' " case,
// asserted on both channels: gRPC (LastRequest().GetEnv() == "") and HTTP
// (protojson omits the zero-value field, so the raw request body carries no
// "env" key). Set-value coverage asserts Env="prod" reaches the server verbatim
// on both channels and on all four construction points.

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// envProdPattern matches an `"env":"prod"` JSON member tolerant of the random
// whitespace protojson.Marshal deliberately injects around the colon to
// discourage byte-exact wire comparisons.
var envProdPattern = regexp.MustCompile(`"env"\s*:\s*"prod"`)

// ---- Env() accessor -------------------------------------------------------

func TestEnv_Accessor_ReturnsConfiguredValue(t *testing.T) {
	cli, _ := newClientWithCaptureAbtest(t, "ns1")
	if got := cli.Env(); got != "" {
		t.Fatalf("Env() with unset Config.Env = %q, want \"\"", got)
	}

	cli.cfg.Env = "prod"
	if got := cli.Env(); got != "prod" {
		t.Fatalf("Env() = %q, want %q", got, "prod")
	}
}

// ---- gRPC: default "" (acceptance-criteria named case) --------------------

func TestEnv_GRPC_DefaultEmpty_OnAllRequests(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{})

	cfg := h.baseConfig([]string{"ns1"}) // Env left unset ⇒ ""
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	// Client accessor reports the empty default.
	if got := cli.Env(); got != "" {
		t.Fatalf("Env() = %q, want \"\"", got)
	}

	// PullAll (pull.go) — startup pull already ran.
	if pr := h.cfgServer.LastPullReq(); pr == nil {
		t.Fatal("no PullAllRequest captured")
	} else if pr.GetEnv() != "" {
		t.Fatalf("PullAllRequest.Env = %q, want \"\"", pr.GetEnv())
	}

	// Subscribe (subscribe.go) — gRPC-only stream attaches after Init.
	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatalf("Subscribe never attached; calls=%d", h.cfgServer.SubscribeCalls())
	}
	if sr := h.cfgServer.LastSubscribeReq(); sr == nil {
		t.Fatal("no SubscribeRequest captured")
	} else if sr.GetEnv() != "" {
		t.Fatalf("SubscribeRequest.Env = %q, want \"\"", sr.GetEnv())
	}

	// GetExperimentResult (experiment_result.go).
	if _, err := cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
		Namespace: "ns1",
		UserInfo:  UserInfo{UID: "u1"},
	}); err != nil {
		t.Fatalf("GetExperimentResult: %v", err)
	}
	if lr := h.abServer.LastRequest(); lr == nil {
		t.Fatal("no GetExperimentResultRequest captured")
	} else if lr.GetEnv() != "" {
		t.Fatalf("GetExperimentResultRequest.Env = %q, want \"\"", lr.GetEnv())
	}
}

// ---- gRPC: Env="prod" reaches all four construction points ----------------

func TestEnv_GRPC_SetValue_OnAllRequests(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{})

	cfg := h.baseConfig([]string{"ns1"})
	cfg.Env = "prod"
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	if got := cli.Env(); got != "prod" {
		t.Fatalf("Env() = %q, want %q", got, "prod")
	}

	// PullAll (pull.go).
	if pr := h.cfgServer.LastPullReq(); pr == nil || pr.GetEnv() != "prod" {
		t.Fatalf("PullAllRequest.Env = %v, want \"prod\"", pr)
	}

	// Subscribe (subscribe.go).
	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() >= 1 }) {
		t.Fatalf("Subscribe never attached; calls=%d", h.cfgServer.SubscribeCalls())
	}
	if sr := h.cfgServer.LastSubscribeReq(); sr == nil || sr.GetEnv() != "prod" {
		t.Fatalf("SubscribeRequest.Env = %v, want \"prod\"", sr)
	}

	// GetExperimentResult (experiment_result.go, direct wrapper).
	if _, err := cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
		Namespace: "ns1",
		UserInfo:  UserInfo{UID: "u1"},
	}); err != nil {
		t.Fatalf("GetExperimentResult: %v", err)
	}
	if lr := h.abServer.LastRequest(); lr == nil || lr.GetEnv() != "prod" {
		t.Fatalf("GetExperimentResultRequest.Env = %v, want \"prod\"", lr)
	}

	// config_version flat_kv fast path (abtest_context.go): WaitForAbtest on a
	// subscribed ns fires a CONFIG_VERSION GetExperimentResult, which must also
	// carry env.
	abctx := cli.NewAbtestContext(context.Background(), "u2", nil)
	if _, err := abctx.WaitForAbtest(context.Background(), "ns1"); err != nil {
		t.Fatalf("WaitForAbtest: %v", err)
	}
	lr := h.abServer.LastRequest()
	if lr == nil || lr.GetEnv() != "prod" {
		t.Fatalf("config_version GetExperimentResultRequest.Env = %v, want \"prod\"", lr)
	}
	if lr.GetExperimentType() != abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION {
		t.Fatalf("expected config_version fetch, got type %v", lr.GetExperimentType())
	}
}

// ---- HTTP: default "" omits the env key from the protojson packet ---------

func TestEnv_HTTP_DefaultEmpty_OmittedFromWire(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{})

	cfg := h.baseHTTPConfig([]string{"ns1"}) // Env unset ⇒ ""
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http): %v", err)
	}
	defer cli.Close()

	if _, err := cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
		Namespace: "ns1",
		UserInfo:  UserInfo{UID: "u1"},
	}); err != nil {
		t.Fatalf("GetExperimentResult (http): %v", err)
	}

	// Decoded server-side request: env is the zero value.
	if lr := h.abServer.LastRequest(); lr == nil || lr.GetEnv() != "" {
		t.Fatalf("decoded GetExperimentResultRequest.Env = %v, want \"\"", lr)
	}
	// Whole-packet protojson: an unset Env must be OMITTED (proto3 scalar
	// zero-value omission), keeping the wire byte-for-byte compatible with an
	// older server that has no env field.
	if body := h.lastAbtestBody(); body == nil {
		t.Fatal("no experiment_result body captured")
	} else if strings.Contains(string(body), `"env"`) {
		t.Fatalf("experiment_result body must omit env when unset; body=%s", body)
	}
	if body := h.lastPullBody(); body == nil {
		t.Fatal("no pull_all body captured")
	} else if strings.Contains(string(body), `"env"`) {
		t.Fatalf("pull_all body must omit env when unset; body=%s", body)
	}
}

// ---- HTTP: Env="prod" rides the whole protojson packet --------------------

func TestEnv_HTTP_SetValue_OnWire(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{})

	cfg := h.baseHTTPConfig([]string{"ns1"})
	cfg.Env = "prod"
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http): %v", err)
	}
	defer cli.Close()

	if _, err := cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
		Namespace: "ns1",
		UserInfo:  UserInfo{UID: "u1"},
	}); err != nil {
		t.Fatalf("GetExperimentResult (http): %v", err)
	}

	// Server-side decode sees env=prod.
	if lr := h.abServer.LastRequest(); lr == nil || lr.GetEnv() != "prod" {
		t.Fatalf("decoded GetExperimentResultRequest.Env = %v, want \"prod\"", lr)
	}
	// Whole-packet protojson carries the env key verbatim (transport is a
	// full-message protojson serialize, so no transport-layer change was
	// needed to add the field).
	if body := h.lastAbtestBody(); body == nil || !envProdPattern.Match(body) {
		t.Fatalf("experiment_result body must contain env=prod; body=%s", body)
	}
	// PullAll over HTTP also carries env.
	if lr := h.cfgServer.LastPullReq(); lr == nil || lr.GetEnv() != "prod" {
		t.Fatalf("decoded PullAllRequest.Env = %v, want \"prod\"", lr)
	}
	if body := h.lastPullBody(); body == nil || !envProdPattern.Match(body) {
		t.Fatalf("pull_all body must contain env=prod; body=%s", body)
	}
}
