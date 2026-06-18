package tipsyabconfig

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
	"github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyauth"
)

const testSecret = "tipsyabconfig-sdk-test-secret"

// fakeConfigServer is a programmable ConfigService used by Go SDK tests.
//
// All fields are read+written under mu; tests interact with it through the
// helper methods (PushSnapshot / SetPullResponse / WaitForSubscribe).
type fakeConfigServer struct {
	configv1.UnimplementedConfigServiceServer

	mu sync.Mutex

	// PullAll knobs.
	pullResponses map[string]*configv1.NamespaceSnapshot // ns -> snapshot
	pullErr       error
	pullCalls     int

	// Subscribe knobs.
	subscribeReqs []*configv1.SubscribeRequest
	// pushCh is the channel each Subscribe call drains; tests feed it via
	// PushSnapshot. Closing pushCh signals EOF; cancelling ctx returns
	// Canceled.
	pushCh chan *configv1.NamespaceSnapshot
	// If subscribeErr is non-nil, Subscribe returns it immediately after
	// the first frame (or before, if there is no first frame).
	subscribeErrFn func() error
	subscribeCalls int
	subscribeWake  chan struct{}
}

func newFakeConfigServer() *fakeConfigServer {
	return &fakeConfigServer{
		pullResponses: map[string]*configv1.NamespaceSnapshot{},
		pushCh:        make(chan *configv1.NamespaceSnapshot, 16),
		subscribeWake: make(chan struct{}, 64),
	}
}

func (f *fakeConfigServer) SetPullSnapshot(s *configv1.NamespaceSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullResponses[s.Namespace] = s
}

func (f *fakeConfigServer) SetPullError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pullErr = err
}

func (f *fakeConfigServer) PullCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pullCalls
}

func (f *fakeConfigServer) SubscribeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subscribeCalls
}

func (f *fakeConfigServer) SetSubscribeErrFn(fn func() error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribeErrFn = fn
}

// PushSnapshot enqueues a snapshot that the active Subscribe stream will
// send to the client. Non-blocking from the test's view.
func (f *fakeConfigServer) PushSnapshot(s *configv1.NamespaceSnapshot) {
	f.pushCh <- s
}

// LastSubscribeReq returns the most recent SubscribeRequest received by the
// server (used to verify known_seqs).
func (f *fakeConfigServer) LastSubscribeReq() *configv1.SubscribeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.subscribeReqs) == 0 {
		return nil
	}
	return f.subscribeReqs[len(f.subscribeReqs)-1]
}

func (f *fakeConfigServer) PullAll(_ context.Context, req *configv1.PullAllRequest) (*configv1.PullAllResponse, error) {
	f.mu.Lock()
	f.pullCalls++
	pullErr := f.pullErr
	out := []*configv1.NamespaceSnapshot{}
	for _, ns := range req.GetNamespaces() {
		if snap, ok := f.pullResponses[ns]; ok {
			out = append(out, snap)
		}
	}
	f.mu.Unlock()
	if pullErr != nil {
		return nil, pullErr
	}
	return &configv1.PullAllResponse{Snapshots: out}, nil
}

func (f *fakeConfigServer) Subscribe(req *configv1.SubscribeRequest, stream grpc.ServerStreamingServer[configv1.ConfigUpdateEvent]) error {
	f.mu.Lock()
	f.subscribeCalls++
	f.subscribeReqs = append(f.subscribeReqs, req)
	errFn := f.subscribeErrFn
	f.mu.Unlock()
	select {
	case f.subscribeWake <- struct{}{}:
	default:
	}
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case snap, ok := <-f.pushCh:
			if !ok {
				return nil
			}
			ev := &configv1.ConfigUpdateEvent{
				Payload: &configv1.ConfigUpdateEvent_Snapshot{Snapshot: snap},
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
			if errFn != nil {
				if e := errFn(); e != nil {
					return e
				}
			}
		}
	}
}

// fakeAbtestServer is a programmable AbtestService.
type fakeAbtestServer struct {
	abtestv1.UnimplementedAbtestServiceServer

	mu        sync.Mutex
	resp      *abtestv1.GetExperimentResultResponse
	err       error
	delay     time.Duration
	respByNS  map[string]*abtestv1.GetExperimentResultResponse
	errByNS   map[string]error
	calls     int
	callsByNS map[string]int
	lastReq   *abtestv1.GetExperimentResultRequest
}

func newFakeAbtestServer() *fakeAbtestServer {
	return &fakeAbtestServer{
		respByNS:  map[string]*abtestv1.GetExperimentResultResponse{},
		errByNS:   map[string]error{},
		callsByNS: map[string]int{},
	}
}

func (f *fakeAbtestServer) SetResponse(ns string, r *abtestv1.GetExperimentResultResponse) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respByNS[ns] = r
}

func (f *fakeAbtestServer) SetError(ns string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errByNS[ns] = err
}

func (f *fakeAbtestServer) SetDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

func (f *fakeAbtestServer) Calls(ns string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callsByNS[ns]
}

func (f *fakeAbtestServer) TotalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeAbtestServer) LastRequest() *abtestv1.GetExperimentResultRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

func (f *fakeAbtestServer) GetExperimentResult(ctx context.Context, req *abtestv1.GetExperimentResultRequest) (*abtestv1.GetExperimentResultResponse, error) {
	f.mu.Lock()
	f.calls++
	f.callsByNS[req.Namespace]++
	f.lastReq = req
	delay := f.delay
	err := f.errByNS[req.Namespace]
	if err == nil {
		err = f.err
	}
	resp := f.respByNS[req.Namespace]
	if resp == nil {
		resp = f.resp
	}
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return &abtestv1.GetExperimentResultResponse{}, nil
	}
	return resp, nil
}

// testHarness wires bufconn + both fakes behind a real *grpc.Server with a
// self-contained HS256 auth interceptor (see unaryServerInterceptor /
// streamServerInterceptor below). Tests build a Client through
// harnessClient().
type testHarness struct {
	t         *testing.T
	cfgServer *fakeConfigServer
	abServer  *fakeAbtestServer
	grpc      *grpc.Server
	cfgLis    *bufconn.Listener
	abLis     *bufconn.Listener
	signer    *tipsyauth.Signer
	token     string
	cleanup   func()
}

func newHarness(t *testing.T) *testHarness {
	t.Helper()
	signer, err := tipsyauth.NewSigner(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	// Token contract (see design doc Proposed Design 4): namespaces must
	// contain "*" so it covers every namespace used by the SDK tests; TTL
	// must be > 0. This preserves the "good token succeeds / bad token
	// returns Unauthenticated" semantics that TestPullAuth_BadToken*
	// relies on.
	token, err := signer.Issue(tipsyauth.IssueOptions{
		Subject:    "tipsyabconfig-sdk-test",
		Namespaces: []string{"*"},
		TTL:        365 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	cfgFake := newFakeConfigServer()
	abFake := newFakeAbtestServer()

	gs := grpc.NewServer(
		grpc.ChainUnaryInterceptor(unaryServerInterceptor(testSecret)),
		grpc.ChainStreamInterceptor(streamServerInterceptor(testSecret)),
	)
	configv1.RegisterConfigServiceServer(gs, cfgFake)
	abtestv1.RegisterAbtestServiceServer(gs, abFake)

	cfgLis := bufconn.Listen(1024 * 1024)
	abLis := bufconn.Listen(1024 * 1024)

	go func() { _ = gs.Serve(cfgLis) }()
	go func() { _ = gs.Serve(abLis) }()

	cleanup := func() {
		gs.Stop()
	}
	h := &testHarness{
		t:         t,
		cfgServer: cfgFake,
		abServer:  abFake,
		grpc:      gs,
		cfgLis:    cfgLis,
		abLis:     abLis,
		signer:    signer,
		token:     token,
		cleanup:   cleanup,
	}
	t.Cleanup(cleanup)
	return h
}

// configForHarness returns a baseline Config wired against both listeners.
//
// Tests typically override pull/sub timing knobs after the call.
func (h *testHarness) baseConfig(namespaces []string) Config {
	// The Go SDK's dial() helper uses cfg.DialOptions for BOTH cfg + abtest
	// conns. Because we need different listeners, we pass a dispatching
	// dialer that switches on the requested target.
	muxDialer := func(ctx context.Context, addr string) (net.Conn, error) {
		switch addr {
		case "passthrough:///bufnet-config", "bufnet-config":
			return h.cfgLis.DialContext(ctx)
		case "passthrough:///bufnet-abtest", "bufnet-abtest":
			return h.abLis.DialContext(ctx)
		}
		// grpc resolver strips "passthrough:///" before handing the
		// address to the dialer in current grpc-go versions; accept
		// both forms for safety. Fallback to cfgLis (tests that don't
		// use the abtest channel should never hit this branch).
		return h.cfgLis.DialContext(ctx)
	}
	return Config{
		Namespaces:        namespaces,
		ConfigServiceAddr: "passthrough:///bufnet-config",
		AbtestServiceAddr: "passthrough:///bufnet-abtest",
		Token:             h.token,
		PullInterval:      50 * time.Millisecond, // compressed fallback timer
		PullTimeout:       2 * time.Second,
		PullRetries:       1,
		AbtestTimeout:     200 * time.Millisecond,
		StartupFailOpen:   false,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		DialOptions:       []grpc.DialOption{grpc.WithContextDialer(muxDialer)},
	}
}

// withoutAbtest returns a baseConfig with AbtestServiceAddr cleared.
func (h *testHarness) baseConfigNoAbtest(namespaces []string) Config {
	cfg := h.baseConfig(namespaces)
	cfg.AbtestServiceAddr = ""
	return cfg
}

// helper to wait for a predicate with a deadline. Returns true on success,
// false when the deadline elapsed.
func waitFor(t *testing.T, deadline time.Duration, predicate func() bool) bool {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if predicate() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return predicate()
}

// errBadAuth is returned by gRPC servers when auth interceptor rejects.
var errBadAuth = errors.New("bad auth")

// mustStatusCode is a helper available to tests that need to assert a
// gRPC status code on a returned error.
func mustStatusCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %v, got nil", want)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a status error: %v", err)
	}
	if st.Code() != want {
		t.Fatalf("status code = %v, want %v (err=%v)", st.Code(), want, err)
	}
}

// drainExposureSink is a simple channel-backed ExposureSink for tests.
type drainExposureSink struct {
	mu     sync.Mutex
	events []ExposureEvent
	notify chan struct{}
}

func newDrainExposureSink() *drainExposureSink {
	return &drainExposureSink{notify: make(chan struct{}, 256)}
}

func (s *drainExposureSink) Sink(ev ExposureEvent) {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *drainExposureSink) Events() []ExposureEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ExposureEvent, len(s.events))
	copy(out, s.events)
	return out
}

// silence "unused" linter when these helpers aren't referenced from one of
// the smaller test files; they are part of the public test API.
var _ = io.EOF
var _ = errBadAuth

// --- self-contained HS256 auth interceptor for the SDK test harness ---
//
// The SDK module cannot import the server's internal/auth package (it now
// lives in a sibling module under github.com/Lightspeed-Intelligence/tipsy-ab-config/internal/...,
// which is unreachable across module boundaries). The harness therefore
// ships its own minimal HS256 verifier. Per design doc Proposed Design 4:
//
//   - Read the gRPC `authorization` metadata key (grpc-go lower-cases all
//     header names on the server side).
//   - Strip the "Bearer " prefix (case-insensitive).
//   - Parse the JWT with HS256 + the shared testSecret; the jwt/v5 parser
//     validates iat/exp automatically.
//   - On any failure return codes.Unauthenticated.
//
// Namespace authorization is intentionally NOT replicated here: the real
// server-side interceptor (internal/auth/interceptor_grpc.go) also does not
// enforce ns at this layer (only signature + iat/exp), and SDK tests rely
// on that same shape. Full server contract is covered by the main module's
// internal/auth/tipsyauth_contract_test.go.

const authorizationKey = "authorization"

func unaryServerInterceptor(secret string) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := harnessAuthenticate(ctx, secret); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

func streamServerInterceptor(secret string) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := harnessAuthenticate(ss.Context(), secret); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}

func harnessAuthenticate(ctx context.Context, secret string) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	values := md.Get(authorizationKey)
	if len(values) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization metadata")
	}
	raw := strings.TrimSpace(values[0])
	if len(raw) >= 7 && strings.EqualFold(raw[:7], "Bearer ") {
		raw = strings.TrimSpace(raw[7:])
	}
	if raw == "" {
		return status.Error(codes.Unauthenticated, "malformed authorization metadata")
	}
	parser := jwt.NewParser(jwt.WithValidMethods([]string{"HS256"}))
	_, err := parser.Parse(raw, func(t *jwt.Token) (any, error) {
		return []byte(secret), nil
	})
	if err != nil {
		return status.Error(codes.Unauthenticated, "invalid token")
	}
	return nil
}
