package tipsyabconfig

// SubTask B test file (sdk-trace-id design §4 PullAll + §3 C SDK Debug log).
//
// Covers Client.pullOnce trace_id behaviour:
//   - Every background PullAll generates a FRESH UUID v4 (design F6: "trace_id
//     is per-request, not per-instance"), so the captured PullAllRequest must
//     carry a non-empty TraceId that matches the UUID v4 shape.
//   - A Debug log line MUST be emitted with the same `trace_id` field so
//     operators can correlate "why this PullAll fired" between SDK and server.
//
// Implementation: we stub the configTransport (capturePullTransport) and
// invoke pullOnce directly via the in-package test seam. This bypasses Init /
// dial / loop goroutines and isolates the unit under test.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
)

// capturePullTransport implements configTransport and records every PullAll
// request under a mutex.
type capturePullTransport struct {
	mu   sync.Mutex
	reqs []*configv1.PullAllRequest
	resp *configv1.PullAllResponse
	err  error
}

func newCapturePullTransport() *capturePullTransport {
	return &capturePullTransport{
		resp: &configv1.PullAllResponse{},
	}
}

func (c *capturePullTransport) PullAll(_ context.Context, req *configv1.PullAllRequest) (*configv1.PullAllResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := &configv1.PullAllRequest{
		Namespaces: append([]string(nil), req.GetNamespaces()...),
		TraceId:    req.GetTraceId(),
	}
	c.reqs = append(c.reqs, cp)
	if c.err != nil {
		return nil, c.err
	}
	return c.resp, nil
}

func (c *capturePullTransport) LastRequest() *configv1.PullAllRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reqs) == 0 {
		return nil
	}
	return c.reqs[len(c.reqs)-1]
}

func (c *capturePullTransport) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

// newClientWithCapturePull wires a *Client whose configTr captures pull calls.
// A JSON slog handler is plumbed so the test can inspect the Debug log line.
func newClientWithCapturePull(t *testing.T, ns string) (*Client, *capturePullTransport, *bytes.Buffer) {
	t.Helper()
	pull := newCapturePullTransport()
	logBuf := &bytes.Buffer{}
	logger := slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cli := &Client{
		cfg: Config{
			PullTimeout: 500 * time.Millisecond,
		},
		metrics:              newMetrics(),
		cache:                newConfigCache(newMetrics()),
		logger:               logger,
		subscribedNamespaces: []string{ns},
		configTr:             pull,
	}
	return cli, pull, logBuf
}

func TestPullOnce_TraceID_NonEmptyUUID(t *testing.T) {
	cli, pull, _ := newClientWithCapturePull(t, "ns1")

	if err := cli.pullOnce(context.Background(), "ns1"); err != nil {
		t.Fatalf("pullOnce: %v", err)
	}
	if pull.Calls() != 1 {
		t.Fatalf("expected exactly 1 PullAll, got %d", pull.Calls())
	}
	req := pull.LastRequest()
	if req == nil {
		t.Fatal("no PullAll request captured")
	}
	if req.GetTraceId() == "" {
		t.Fatal("expected non-empty TraceId on PullAllRequest")
	}
	if !uuidV4Pattern.MatchString(req.GetTraceId()) {
		t.Fatalf("expected UUID-shaped TraceId, got %q", req.GetTraceId())
	}
}

func TestPullOnce_TraceID_FreshPerCall(t *testing.T) {
	// Design F6 / open-question: every background PullAll fires a fresh
	// trace_id so the server-side trace remains "per-request, not
	// per-instance". Assert two consecutive pullOnce calls yield two
	// DIFFERENT TraceIds.
	cli, pull, _ := newClientWithCapturePull(t, "ns1")

	if err := cli.pullOnce(context.Background(), "ns1"); err != nil {
		t.Fatalf("pullOnce#1: %v", err)
	}
	if err := cli.pullOnce(context.Background(), "ns1"); err != nil {
		t.Fatalf("pullOnce#2: %v", err)
	}
	if pull.Calls() != 2 {
		t.Fatalf("expected 2 PullAll calls, got %d", pull.Calls())
	}
	pull.mu.Lock()
	first := pull.reqs[0].GetTraceId()
	second := pull.reqs[1].GetTraceId()
	pull.mu.Unlock()
	if first == "" || second == "" {
		t.Fatalf("expected non-empty TraceIds on both calls; got (%q,%q)", first, second)
	}
	if first == second {
		t.Fatalf("expected DIFFERENT trace_ids per call (per-request semantics); both = %q", first)
	}
}

func TestPullOnce_TraceID_DebugLogCarriesField(t *testing.T) {
	cli, pull, logBuf := newClientWithCapturePull(t, "ns1")

	if err := cli.pullOnce(context.Background(), "ns1"); err != nil {
		t.Fatalf("pullOnce: %v", err)
	}
	req := pull.LastRequest()
	if req == nil {
		t.Fatal("no PullAll request captured")
	}
	wantTraceID := req.GetTraceId()

	// Scan JSON log lines for a Debug-level entry on the PullAll path that
	// carries `trace_id` equal to the proto TraceId actually sent. The exact
	// message wording is design-driven ("tipsyabconfig: PullAll") but the
	// substring match keeps this resilient to small wording tweaks.
	if !findLogLineWithField(t, logBuf, "PullAll", "trace_id", wantTraceID) {
		t.Fatalf("expected DEBUG log line containing %q with trace_id=%q;\nlogs=\n%s",
			"PullAll", wantTraceID, logBuf.String())
	}
}

// bufStringer is satisfied by anything that can snapshot its contents to a
// string. Both *bytes.Buffer and *syncBuffer satisfy it, so the same log-line
// scanner helper works whether or not the underlying buffer is shared with a
// production background goroutine.
type bufStringer interface {
	String() string
}

// findLogLineWithField scans the JSON-log buffer for a line whose message
// contains `msgSubstr` AND whose top-level `field` equals `wantValue`. Returns
// true on a hit. This is the standard slog assertion helper used across the
// trace_id test suite.
//
// The buf argument is taken as a bufStringer (not *bytes.Buffer) so tests whose
// production goroutine writes the log buffer concurrently with the test
// goroutine reading it can pass a mutex-guarded *syncBuffer instead. See
// syncBuffer below.
func findLogLineWithField(t *testing.T, buf bufStringer, msgSubstr, field, wantValue string) bool {
	t.Helper()
	for _, line := range strings.Split(buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		msg, _ := rec["msg"].(string)
		if !strings.Contains(msg, msgSubstr) {
			continue
		}
		v, ok := rec[field].(string)
		if !ok {
			continue
		}
		if v == wantValue {
			return true
		}
	}
	return false
}

// syncBuffer is a mutex-guarded wrapper around bytes.Buffer used by trace_id
// tests where the slog handler writes from a production background goroutine
// while the test goroutine reads the buffered output via String(). Without the
// lock `go test -race` flags the unsynchronised Write/Read on bytes.Buffer's
// internal slice. The wrapper exposes only the minimum surface the slog
// handler and the findLogLineWithField helper need.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newSyncBuffer() *syncBuffer { return &syncBuffer{} }

// Write satisfies io.Writer so *syncBuffer can be plugged into
// slog.NewJSONHandler directly.
func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// String returns a snapshot of the buffer's contents under the lock so it is
// safe to call from the test goroutine while a producer goroutine is writing.
func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
