package tipsyabconfig

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
)

// httpHarness mirrors testHarness for the HTTP transport. It stands up an
// httptest.Server whose handlers speak the exact publicread contract
// (internal/api/http/publicread/publicread.go):
//
//   - POST /api/v1/config/pull_all          → ConfigService.PullAll
//   - POST /api/v1/abtest/experiment_result → AbtestService.GetExperimentResult
//
// Request bodies are protojson-decoded (DiscardUnknown:true); responses are
// protojson-encoded (UseProtoNames:true, EmitUnpopulated:true), Content-Type
// application/json, status 200. Non-2xx replies carry {"error": msg}. The
// per-route business logic reuses the same fakeConfigServer / fakeAbtestServer
// the gRPC harness drives, so HTTP tests share the exact same knobs
// (SetPullSnapshot / SetPullError / SetResponse / SetError / Calls ...).
type httpHarness struct {
	t         *testing.T
	cfgServer *fakeConfigServer
	abServer  *fakeAbtestServer
	srv       *httptest.Server
	token     string

	// authMu guards the recorded Authorization headers.
	authMu      sync.Mutex
	pullAuth    []string // Authorization header seen on each pull_all request
	abtestAuth  []string // Authorization header seen on each experiment_result request
	pullURLPath []string // raw request path seen on each pull_all request

	// statusOverride, when set !=0, makes the route reply with that status
	// and an {"error": ...} body BEFORE touching the fakes (failure injection
	// for the non-2xx error-path tests). Guarded by authMu.
	pullStatusOverride   int
	abtestStatusOverride int
}

func newHTTPHarness(t *testing.T) *httpHarness {
	t.Helper()
	h := &httpHarness{
		t:         t,
		cfgServer: newFakeConfigServer(),
		abServer:  newFakeAbtestServer(),
		token:     "http-harness-token",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/config/pull_all", h.handlePullAll)
	mux.HandleFunc("/api/v1/abtest/experiment_result", h.handleExperimentResult)
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *httpHarness) baseURL() string { return h.srv.URL }

func (h *httpHarness) setPullStatus(code int) {
	h.authMu.Lock()
	h.pullStatusOverride = code
	h.authMu.Unlock()
}

func (h *httpHarness) setAbtestStatus(code int) {
	h.authMu.Lock()
	h.abtestStatusOverride = code
	h.authMu.Unlock()
}

func (h *httpHarness) lastPullAuth() string {
	h.authMu.Lock()
	defer h.authMu.Unlock()
	if len(h.pullAuth) == 0 {
		return ""
	}
	return h.pullAuth[len(h.pullAuth)-1]
}

func (h *httpHarness) lastAbtestAuth() string {
	h.authMu.Lock()
	defer h.authMu.Unlock()
	if len(h.abtestAuth) == 0 {
		return ""
	}
	return h.abtestAuth[len(h.abtestAuth)-1]
}

func (h *httpHarness) pullPaths() []string {
	h.authMu.Lock()
	defer h.authMu.Unlock()
	out := make([]string, len(h.pullURLPath))
	copy(out, h.pullURLPath)
	return out
}

// decodeReq reads a protojson request body into msg, mirroring publicread's
// decodeProto (empty body → zero-value request, DiscardUnknown).
func decodeHTTPReq(w http.ResponseWriter, r *http.Request, msg proto.Message) bool {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeHTTPErr(w, http.StatusBadRequest, "body read failed")
		return false
	}
	if len(body) == 0 {
		return true
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, msg); err != nil {
		writeHTTPErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return false
	}
	return true
}

// writeHTTPProto mirrors publicread.writeProto exactly (snake_case names,
// zero-value fields emitted, int64 string-encoded).
func writeHTTPProto(w http.ResponseWriter, msg proto.Message) {
	b, err := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}.Marshal(msg)
	if err != nil {
		writeHTTPErr(w, http.StatusInternalServerError, "encode response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func writeHTTPErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, `{"error":`+jsonQuote(msg)+`}`)
}

// jsonQuote is a tiny helper so the harness never imports encoding/json just to
// quote a single string (keeps the test file dependency surface obvious).
func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (h *httpHarness) handlePullAll(w http.ResponseWriter, r *http.Request) {
	h.authMu.Lock()
	h.pullAuth = append(h.pullAuth, r.Header.Get("Authorization"))
	h.pullURLPath = append(h.pullURLPath, r.URL.Path)
	override := h.pullStatusOverride
	h.authMu.Unlock()

	if r.Method != http.MethodPost {
		writeHTTPErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if override != 0 {
		writeHTTPErr(w, override, "injected failure")
		return
	}
	var req configv1.PullAllRequest
	if !decodeHTTPReq(w, r, &req) {
		return
	}
	// Reuse the fake's PullAll business logic (snapshot map + injected error).
	res, err := h.cfgServer.PullAll(r.Context(), &req)
	if err != nil {
		writeGRPCStatusErr(w, err)
		return
	}
	writeHTTPProto(w, res)
}

func (h *httpHarness) handleExperimentResult(w http.ResponseWriter, r *http.Request) {
	h.authMu.Lock()
	h.abtestAuth = append(h.abtestAuth, r.Header.Get("Authorization"))
	override := h.abtestStatusOverride
	h.authMu.Unlock()

	if r.Method != http.MethodPost {
		writeHTTPErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if override != 0 {
		writeHTTPErr(w, override, "injected failure")
		return
	}
	var req abtestv1.GetExperimentResultRequest
	if !decodeHTTPReq(w, r, &req) {
		return
	}
	res, err := h.abServer.GetExperimentResult(r.Context(), &req)
	if err != nil {
		writeGRPCStatusErr(w, err)
		return
	}
	writeHTTPProto(w, res)
}

// writeGRPCStatusErr maps an injected gRPC status error onto an HTTP error
// response, mirroring publicread.writeGRPCErr (so the SDK sees the same shape
// for a server-side PullAll/GetExperimentResult failure).
func writeGRPCStatusErr(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	msg := err.Error()
	if st, ok := status.FromError(err); ok {
		switch st.Code() {
		case codes.InvalidArgument:
			code = http.StatusBadRequest
		case codes.NotFound:
			code = http.StatusNotFound
		case codes.PermissionDenied:
			code = http.StatusForbidden
		case codes.Unauthenticated:
			code = http.StatusUnauthorized
		case codes.Unavailable:
			code = http.StatusServiceUnavailable
		}
		msg = st.Message()
	}
	writeHTTPErr(w, code, msg)
}

// baseHTTPConfig returns a Config wired to the HTTP harness in HTTP transport
// mode. Tests override timing knobs after the call (mirrors baseConfig).
func (h *httpHarness) baseHTTPConfig(namespaces []string) Config {
	return Config{
		Namespaces:        namespaces,
		Transport:         TransportHTTP,
		ConfigServiceAddr: h.baseURL(),
		AbtestServiceAddr: h.baseURL(),
		Token:             h.token,
		PullInterval:      50 * time.Millisecond,
		PullTimeout:       2 * time.Second,
		PullRetries:       1,
		AbtestTimeout:     500 * time.Millisecond,
		StartupFailOpen:   false,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (h *httpHarness) baseHTTPConfigNoAbtest(namespaces []string) Config {
	cfg := h.baseHTTPConfig(namespaces)
	cfg.AbtestServiceAddr = ""
	return cfg
}

// ---- C.1 startup PullAll assembles the cache ------------------------------

func TestHTTP_StartupPullAll_PopulatesCache(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k1": {full: 10, versions: map[int64]string{10: "v10"}}}))

	cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http): %v", err)
	}
	defer cli.Close()

	if v, ok := cli.GetConfigStatic("ns1", "k1", "def"); !ok || v != "v10" {
		t.Fatalf("HTTP startup PullAll did not populate cache: got (%q,%v)", v, ok)
	}
	if h.cfgServer.PullCalls() == 0 {
		t.Fatal("expected at least one HTTP pull_all call")
	}
}

// ---- C.2 periodic polling refreshes the cache -----------------------------

func TestHTTP_PeriodicPull_RefreshesCache(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "v1"}}}))

	cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"})
	cfg.PullInterval = 50 * time.Millisecond
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http): %v", err)
	}
	defer cli.Close()

	if v, ok := cli.GetConfigStatic("ns1", "k", "def"); !ok || v != "v1" {
		t.Fatalf("initial cache value wrong: got (%q,%v)", v, ok)
	}

	// Mutate the mock server; the next poll must refresh the cache.
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 2, 2, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 2, versions: map[int64]string{2: "v2"}}}))

	if !waitFor(t, 2*time.Second, func() bool {
		v, ok := cli.GetConfigStatic("ns1", "k", "def")
		return ok && v == "v2"
	}) {
		t.Fatalf("periodic HTTP pull did not refresh cache; pullCalls=%d", h.cfgServer.PullCalls())
	}
}

// ---- C.3 GetExperimentResult over HTTP ------------------------------------

func TestHTTP_GetExperimentResult(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	expID := "e1"
	groupID := "g1"
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k": 7},
		Exposures: []*abtestv1.Exposure{
			{Key: "k", Version: 7, Source: "experiment_group", ExperimentId: &expID, GroupId: &groupID},
		},
	})

	cfg := h.baseHTTPConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http): %v", err)
	}
	defer cli.Close()

	res, err := cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
		Namespace: "ns1",
		UserInfo:  UserInfo{UID: "u1", Attrs: map[string]any{"country": "US"}},
	})
	if err != nil {
		t.Fatalf("GetExperimentResult (http): %v", err)
	}
	if got := res.GetConfigFlatKv()["k"]; got != 7 {
		t.Fatalf("config_flat_kv[k] = %d, want 7", got)
	}
	if len(res.GetExposures()) != 1 || res.GetExposures()[0].GetVersion() != 7 {
		t.Fatalf("unexpected exposures round-trip: %+v", res.GetExposures())
	}
	// The wire request must have carried the user id + attrs through protojson.
	if lr := h.abServer.LastRequest(); lr == nil || lr.GetUserId() != "u1" {
		t.Fatalf("server did not receive user id over HTTP: %+v", lr)
	}
}

// ---- C.4 full GetConfig chain over HTTP (experiment compute + value) -------

func TestHTTP_GetConfig_FullChain(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full-v1", 2: "ab-v2"}}}))
	expID := "101"
	groupID := "202"
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"k": 2},
		Exposures: []*abtestv1.Exposure{
			{Key: "k", Version: 2, Source: "experiment_group", ExperimentId: &expID, GroupId: &groupID},
		},
	})

	sink := newDrainExposureSink()
	cfg := h.baseHTTPConfig([]string{"ns1"})
	cfg.ExposureSink = sink
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http): %v", err)
	}
	defer cli.Close()

	abctx := cli.NewAbtestContext(context.Background(), "u1", map[string]any{"country": "US"})
	val, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
	if err != nil {
		t.Fatalf("GetConfig (http): %v", err)
	}
	if val != "ab-v2" {
		t.Fatalf("expected ab value over HTTP, got %q", val)
	}
	if !waitFor(t, 2*time.Second, func() bool { return len(sink.Events()) >= 1 }) {
		t.Fatalf("expected exposure over HTTP, got %d", len(sink.Events()))
	}
	ev := sink.Events()[0]
	if ev.ExperimentID != expID || ev.GroupID != groupID || ev.Key != "k" || ev.Version != 2 || ev.UserID != "u1" {
		t.Fatalf("unexpected exposure over HTTP: %+v", ev)
	}
}

// ---- C.5 Authorization header (static token + TokenProvider) --------------

func TestHTTP_AuthorizationHeader_StaticToken(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{})

	cfg := h.baseHTTPConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http): %v", err)
	}
	defer cli.Close()

	want := "Bearer " + h.token
	if got := h.lastPullAuth(); got != want {
		t.Fatalf("pull_all Authorization = %q, want %q", got, want)
	}

	// Force an abtest call so the header on that route is captured too.
	_, _ = cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
		Namespace: "ns1",
		UserInfo:  UserInfo{UID: "u1"},
	})
	if got := h.lastAbtestAuth(); got != want {
		t.Fatalf("experiment_result Authorization = %q, want %q", got, want)
	}
}

func TestHTTP_AuthorizationHeader_TokenProvider(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	var calls atomic.Int64
	const provided = "provider-issued-token"
	cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"})
	cfg.Token = ""
	cfg.TokenProvider = func(ctx context.Context) (string, error) {
		calls.Add(1)
		return provided, nil
	}
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http, token provider): %v", err)
	}
	defer cli.Close()

	if calls.Load() == 0 {
		t.Fatal("expected TokenProvider to be consulted for the HTTP request")
	}
	if got := h.lastPullAuth(); got != "Bearer "+provided {
		t.Fatalf("pull_all Authorization = %q, want %q", got, "Bearer "+provided)
	}
}

// ---- C.6a non-2xx on pull_all → periodic_pull background error + degrade ---

func TestHTTP_PullAll_Non2xx_FiresBackgroundError(t *testing.T) {
	h := newHTTPHarness(t)
	// Start healthy so startup PullAll succeeds and Init returns a client.
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "v1"}}}))

	sink := &recordingErrSink{}
	cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"})
	cfg.PullInterval = 20 * time.Millisecond
	cfg.OnBackgroundError = sink.cb
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http): %v", err)
	}
	defer cli.Close()

	// Now make every pull_all return 500; the periodic loop must surface a
	// periodic_pull background error and bump pull-failure metrics.
	h.setPullStatus(http.StatusInternalServerError)

	if !waitFor(t, 2*time.Second, func() bool { return len(sink.byPhase("periodic_pull")) >= 1 }) {
		t.Fatalf("no periodic_pull event from HTTP 500; events=%d", sink.count())
	}
	ev := sink.byPhase("periodic_pull")[0]
	if ev.Namespace != "ns1" {
		t.Errorf("periodic_pull Namespace=%q want ns1", ev.Namespace)
	}
	if ev.Err == nil {
		t.Error("periodic_pull Err is nil on HTTP 500")
	}
	if !waitFor(t, time.Second, func() bool { return cli.Health().LastPullErr != nil }) {
		t.Fatal("Health().LastPullErr never populated on HTTP 500")
	}
	// The previous cache value must remain served (graceful degrade).
	if v, ok := cli.GetConfigStatic("ns1", "k", "def"); !ok || v != "v1" {
		t.Fatalf("cache should retain last-good value during HTTP 500; got (%q,%v)", v, ok)
	}
}

// ---- C.6b non-2xx on experiment_result → GetConfig degrades to full -------

func TestHTTP_GetExperimentResult_Non2xx_DegradesToFull(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "full-v1"}}}))
	// experiment_result always 403.
	h.setAbtestStatus(http.StatusForbidden)

	sink := newDrainExposureSink()
	cfg := h.baseHTTPConfig([]string{"ns1"})
	cfg.ExposureSink = sink
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http): %v", err)
	}
	defer cli.Close()

	abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
	val, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def")
	if err != nil {
		t.Fatalf("GetConfig (http): %v", err)
	}
	if val != "full-v1" {
		t.Fatalf("expected full fallback over HTTP error, got %q", val)
	}
	if cli.Metrics().AbtestFallbackTotal("ns1") == 0 {
		t.Fatal("expected abtest_fallback_total ns1 > 0 on HTTP experiment_result error")
	}
	time.Sleep(50 * time.Millisecond)
	if len(sink.Events()) != 0 {
		t.Fatalf("HTTP abtest error must not emit exposure; got %+v", sink.Events())
	}
}

// ---- C.7 StartupFailOpen governs HTTP startup-pull failure ----------------

func TestHTTP_StartupPullFail_FailClose(t *testing.T) {
	h := newHTTPHarness(t)
	h.setPullStatus(http.StatusServiceUnavailable) // startup pull fails immediately
	cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"})
	cfg.StartupFailOpen = false
	cfg.PullRetries = 1
	_, err := Init(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected Init to fail-close on HTTP startup pull failure")
	}
	if !errors.Is(err, ErrStartupPullFailed) {
		t.Fatalf("expected ErrStartupPullFailed, got %v", err)
	}
}

func TestHTTP_StartupPullFail_FailOpen(t *testing.T) {
	h := newHTTPHarness(t)
	h.setPullStatus(http.StatusServiceUnavailable)
	cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"})
	cfg.StartupFailOpen = true
	cfg.PullRetries = 1
	cfg.PullInterval = time.Hour // keep the periodic loop from polluting the window
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init fail-open (http) returned err: %v", err)
	}
	defer cli.Close()

	if !cli.Health().StartupCacheEmpty {
		t.Fatal("Health().StartupCacheEmpty=false after absorbed HTTP startup failure")
	}
	// Empty cache: lookups must miss and fall through to the default.
	if v, ok := cli.GetConfigStatic("ns1", "k", "def"); ok || v != "def" {
		t.Fatalf("expected empty cache miss under fail-open; got (%q,%v)", v, ok)
	}
	if cli.Metrics().CacheEmptyTotal() == 0 {
		t.Fatal("expected CacheEmptyTotal +1 under HTTP fail-open")
	}
}

// ---- C.8 HTTP mode never starts Subscribe; Close does not hang ------------

func TestHTTP_NoSubscribe_AndCleanClose(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))
	cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http): %v", err)
	}

	// SubscribeConnected must stay false in HTTP mode — there is no stream.
	// Give the SDK a moment in case any (incorrect) subscribe attempt would
	// flip it.
	time.Sleep(100 * time.Millisecond)
	if cli.Health().SubscribeConnected {
		t.Fatal("SubscribeConnected=true in HTTP mode; Subscribe must not run")
	}

	// Close must return promptly (no subscribe goroutine to deadlock on, and
	// the pull loop must drain cleanly).
	done := make(chan error, 1)
	go func() { done <- cli.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Close (http): %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return within 3s in HTTP mode")
	}

	// Idempotent second Close.
	if err := cli.Close(); err != nil {
		t.Fatalf("second Close (http): %v", err)
	}
}

// ---- B (address validation, HTTP-mode specific) ---------------------------

func TestHTTP_AddrValidation(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	t.Run("config addr without scheme errors", func(t *testing.T) {
		cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"})
		cfg.ConfigServiceAddr = "lb.internal:8080" // missing http(s)://
		_, err := Init(context.Background(), cfg)
		if err == nil {
			t.Fatal("expected Init error for non-http(s) ConfigServiceAddr in HTTP mode")
		}
		if errors.Is(err, ErrStartupPullFailed) {
			t.Fatalf("URL validation must be a parameter error, not absorbed startup failure: %v", err)
		}
	})

	t.Run("empty config addr errors", func(t *testing.T) {
		cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"})
		cfg.ConfigServiceAddr = ""
		_, err := Init(context.Background(), cfg)
		if err == nil {
			t.Fatal("expected Init error for empty ConfigServiceAddr")
		}
	})

	t.Run("abtest addr without scheme errors", func(t *testing.T) {
		cfg := h.baseHTTPConfig([]string{"ns1"})
		cfg.AbtestServiceAddr = "lb.internal:8080"
		_, err := Init(context.Background(), cfg)
		if err == nil {
			t.Fatal("expected Init error for non-http(s) AbtestServiceAddr in HTTP mode")
		}
		if errors.Is(err, ErrStartupPullFailed) {
			t.Fatalf("URL validation must be a parameter error, not absorbed startup failure: %v", err)
		}
	})

	t.Run("empty abtest addr degrades (Init succeeds)", func(t *testing.T) {
		cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"}) // AbtestServiceAddr == ""
		cli, err := Init(context.Background(), cfg)
		if err != nil {
			t.Fatalf("empty AbtestServiceAddr should keep degraded mode (Init ok), got %v", err)
		}
		defer cli.Close()
		// GetConfig must degrade to full release (abtest permanently unavailable).
		abctx := cli.NewAbtestContext(context.Background(), "u1", nil)
		if _, err := cli.GetConfig(context.Background(), abctx, "ns1", "k", "def"); err != nil {
			t.Fatalf("GetConfig in degraded HTTP mode errored: %v", err)
		}
		// The exported GetExperimentResult must report abtest not configured.
		if _, err := cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
			Namespace: "ns1", UserInfo: UserInfo{UID: "u1"},
		}); err == nil {
			t.Fatal("expected error from GetExperimentResult when AbtestServiceAddr empty (degraded)")
		}
	})
}

// ---- HTTPClient injection seam --------------------------------------------

// countingRoundTripper wraps an http.RoundTripper to count requests, so a
// test can prove the SDK used the injected Config.HTTPClient rather than a
// self-built one.
type countingRoundTripper struct {
	inner http.RoundTripper
	calls atomic.Int64
}

func (c *countingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return c.inner.RoundTrip(r)
}

// TestHTTP_InjectedHTTPClientIsUsed asserts Config.HTTPClient, when set, is the
// client the SDK drives (the injection seam for tests / custom TLS / proxies).
func TestHTTP_InjectedHTTPClientIsUsed(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "v1"}}}))

	rt := &countingRoundTripper{inner: http.DefaultTransport}
	cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"})
	cfg.HTTPClient = &http.Client{Transport: rt}
	cfg.PullInterval = time.Hour // one startup pull is enough to observe
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http, injected client): %v", err)
	}
	defer cli.Close()

	if rt.calls.Load() == 0 {
		t.Fatal("injected Config.HTTPClient was not used for the HTTP transport")
	}
	if v, ok := cli.GetConfigStatic("ns1", "k", "def"); !ok || v != "v1" {
		t.Fatalf("startup pull via injected client did not populate cache: (%q,%v)", v, ok)
	}
}

// TestHTTP_TrailingSlashTrimmed asserts a base URL with a trailing "/" routes
// to the identical path as one without (the SDK must trim the trailing slash so
// it never produces "//api/v1/...").
func TestHTTP_TrailingSlashTrimmed(t *testing.T) {
	h := newHTTPHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, nil))

	cfg := h.baseHTTPConfigNoAbtest([]string{"ns1"})
	cfg.ConfigServiceAddr = h.baseURL() + "/" // trailing slash
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init (http, trailing slash): %v", err)
	}
	defer cli.Close()

	paths := h.pullPaths()
	if len(paths) == 0 {
		t.Fatal("no pull_all request observed")
	}
	for _, p := range paths {
		if p != "/api/v1/config/pull_all" {
			t.Fatalf("trailing slash not trimmed: request path = %q, want /api/v1/config/pull_all", p)
		}
	}
}
