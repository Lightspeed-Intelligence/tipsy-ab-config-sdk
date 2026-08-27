package tipsyabconfig

// Issue #16: secretKey-only authentication. Covers the Config.SecretKey knob,
// the Init validation relaxation, the per-request credential precedence
// (SecretKey > TokenProvider > Token), and the exact wire literal
// `SecretKey <v>` on BOTH transports (gRPC metadata and HTTP header — the
// SDK sends the precise literal; the platform matches the scheme EqualFold).

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// ---- credential precedence at the single取值点 (tokenSource) ---------------

func TestTokenSource_CredentialPrecedence(t *testing.T) {
	var providerCalls atomic.Int64
	provider := func(ctx context.Context) (string, error) {
		providerCalls.Add(1)
		return "dyn-token", nil
	}

	cases := []struct {
		name         string
		cfg          Config
		want         string
		wantProvider int64 // expected provider call count after the request
	}{
		{
			name: "secretKey wins over provider and token",
			cfg:  Config{SecretKey: "s3cr3t", TokenProvider: provider, Token: "static"},
			want: "SecretKey s3cr3t",
			// SecretKey short-circuits: the provider must NOT be consulted.
			wantProvider: 0,
		},
		{
			name:         "provider wins over token",
			cfg:          Config{TokenProvider: provider, Token: "static"},
			want:         "Bearer dyn-token",
			wantProvider: 1,
		},
		{
			name: "static token last",
			cfg:  Config{Token: "static"},
			want: "Bearer static",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			providerCalls.Store(0)
			ts := bearerCredentialsFromConfig(tc.cfg)
			md, err := ts.GetRequestMetadata(context.Background())
			if err != nil {
				t.Fatalf("GetRequestMetadata: %v", err)
			}
			if got := md["authorization"]; got != tc.want {
				t.Fatalf("authorization = %q, want %q (exact literal)", got, tc.want)
			}
			if got := providerCalls.Load(); got != tc.wantProvider {
				t.Fatalf("provider calls = %d, want %d", got, tc.wantProvider)
			}
		})
	}
}

// ---- Init validation --------------------------------------------------------

func TestInit_MissingAllCredentials_ErrorMessage(t *testing.T) {
	h := newHarness(t)
	cfg := h.baseConfig([]string{"ns1"})
	cfg.SecretKey = ""
	cfg.Token = ""
	cfg.TokenProvider = nil
	_, err := Init(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected Init to error when SecretKey, Token and TokenProvider are all unset")
	}
	// Exact message: the wording is aligned across the three SDK languages
	// (issue #16). Exact equality also pins the validation to the parameter
	// check (a relaxed-away check would surface as ErrStartupPullFailed
	// instead, with a different message).
	const want = "tipsyabconfig: SecretKey, Token or TokenProvider must be set"
	if err.Error() != want {
		t.Fatalf("Init error = %q, want %q", err.Error(), want)
	}
}

// ---- gRPC path: metadata literal + full RPC surface -------------------------

func TestInit_SecretKeyOnly_GRPC(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{})

	cfg := h.baseConfig([]string{"ns1"})
	cfg.Token = ""
	cfg.TokenProvider = nil
	cfg.SecretKey = testSecret

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (secretKey only): %v", err)
	}
	defer cli.Close()

	// Exact wire literal on the gRPC metadata path.
	want := "SecretKey " + testSecret
	if got := h.cfgServer.LastPullAuth(); got != want {
		t.Fatalf("PullAll authorization metadata = %q, want %q (exact literal)", got, want)
	}

	// Subscribe stream authenticates with the same credentials.
	if !waitFor(t, 2*time.Second, func() bool { return h.cfgServer.SubscribeCalls() > 0 }) {
		t.Fatal("Subscribe never reached the server under secretKey-only auth")
	}

	// Abtest RPC works under secretKey-only auth too.
	if _, err := cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
		Namespace: "ns1",
		UserInfo:  UserInfo{UID: "u1"},
	}); err != nil {
		t.Fatalf("GetExperimentResult (secretKey only): %v", err)
	}
}

func TestInit_SecretKeyPrecedence_GRPC(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	var providerCalls atomic.Int64
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cfg.SecretKey = testSecret
	cfg.Token = h.token // valid Bearer credential, must be outranked
	cfg.TokenProvider = func(ctx context.Context) (string, error) {
		providerCalls.Add(1)
		return h.token, nil
	}

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (secretKey + token + provider): %v", err)
	}
	defer cli.Close()

	want := "SecretKey " + testSecret
	if got := h.cfgServer.LastPullAuth(); got != want {
		t.Fatalf("PullAll authorization metadata = %q, want %q (SecretKey must outrank token forms)", got, want)
	}
	if got := providerCalls.Load(); got != 0 {
		t.Fatalf("TokenProvider consulted %d time(s); SecretKey must short-circuit it", got)
	}
}

// ---- HTTP path: header literal ----------------------------------------------

func TestHTTP_AuthorizationHeader_SecretKeyOnly(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{})

	const secret = "http-mode-secret"
	cfg := h.baseHTTPConfig([]string{"ns1"})
	cfg.Token = ""
	cfg.TokenProvider = nil
	cfg.SecretKey = secret

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http, secretKey only): %v", err)
	}
	defer cli.Close()

	want := "SecretKey " + secret
	if got := h.lastPullAuth(); got != want {
		t.Fatalf("pull_all Authorization = %q, want %q (exact literal)", got, want)
	}

	// Force an abtest call so the header on that route is captured too.
	_, _ = cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
		Namespace: "ns1",
		UserInfo:  UserInfo{UID: "u1"},
	})
	if got := h.lastAbtestAuth(); got != want {
		t.Fatalf("experiment_result Authorization = %q, want %q (exact literal)", got, want)
	}
}

func TestHTTP_SecretKeyPrecedence_OverToken(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	const secret = "http-precedence-secret"
	cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"})
	cfg.SecretKey = secret // Token stays set from baseHTTPConfig

	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http, secretKey + token): %v", err)
	}
	defer cli.Close()

	want := "SecretKey " + secret
	if got := h.lastPullAuth(); got != want {
		t.Fatalf("pull_all Authorization = %q, want %q (SecretKey must outrank Token)", got, want)
	}
}

// ---- review item: the secret value must never reach any log line ------------

func TestSecretKey_NeverLogged_AtDebugLevel(t *testing.T) {
	run := func(t *testing.T, secret string, cfg Config) {
		// syncBuffer (pull_traceid_test.go): the SDK logs from background
		// pull/subscribe goroutines concurrently with the test body.
		buf := newSyncBuffer()
		cfg.Logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		cfg.Token = ""
		cfg.TokenProvider = nil
		cfg.SecretKey = secret

		cli, err := Init(context.Background(), cfg)
		if err != nil {
			t.Fatalf("Init: %v", err)
		}
		_, _ = cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
			Namespace: "ns1",
			UserInfo:  UserInfo{UID: "u1"},
		})
		cli.Close()

		logged := buf.String()
		if logged == "" {
			// Guard against a no-op assertion: Debug-level SDK logging must
			// actually have produced output for the containment check to mean
			// anything.
			t.Fatal("no log output captured at Debug level; the containment assertion below would be vacuous")
		}
		if strings.Contains(logged, secret) {
			t.Fatalf("secretKey value leaked into SDK logs:\n%s", logged)
		}
	}

	t.Run("grpc", func(t *testing.T) {
		h := newHarness(t)
		h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
		h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{})
		// The gRPC harness authenticates SecretKey against testSecret, so the
		// secret under test is testSecret itself.
		run(t, testSecret, h.baseConfig([]string{"ns1"}))
	})
	t.Run("http", func(t *testing.T) {
		h := newHTTPHarness(t)
		h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
		h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{})
		run(t, "hush-3f9a-do-not-log", h.baseHTTPConfig([]string{"ns1"}))
	})
}
