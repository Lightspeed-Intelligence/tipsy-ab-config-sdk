package tipsyabconfig

// SubTask B test file (sdk-trace-id design §4 + Testing Plan §SDK Go bullet 5).
//
// Covers Client.GetExperimentResult trace_id behaviour:
//   - empty ExperimentResultRequest.TraceID ⇒ SDK generates a fresh
//     uuid.New().String() (UUID v4 shape) and writes it to the outbound
//     GetExperimentResultRequest.TraceId proto field.
//   - non-empty TraceID ⇒ the SDK passes it through verbatim (no rewrite, no
//     format check — design §Important Details).
//
// Implementation note: we do NOT spin up bufconn here. We construct a Client by
// hand with the in-memory captureAbtestTransport stub injected as abtestTr.
// This keeps the test laser-focused on the proto-level field assignment and is
// the smallest reliable surface; the bufconn-driven integration coverage
// already exists in get_config_test.go.

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"sync"
	"testing"
	"time"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// uuidV4Pattern matches the 36-char UUID v4 textual form (8-4-4-4-12 hex with
// dashes). We intentionally do NOT enforce the version nibble: uuid.New() is
// guaranteed UUID v4, but the assertion is "looks like a UUID" — the design
// allows future swap to v7 etc. without breaking the SDK callers.
var uuidV4Pattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// captureAbtestTransport is a mock abtestTransport that records every inbound
// GetExperimentResultRequest under a mutex and returns a canned response. It
// satisfies the package-internal `abtestTransport` interface so we can plug it
// into Client.abtestTr directly.
type captureAbtestTransport struct {
	mu   sync.Mutex
	reqs []*abtestv1.GetExperimentResultRequest
	resp *abtestv1.GetExperimentResultResponse
	err  error
}

func newCaptureAbtestTransport() *captureAbtestTransport {
	return &captureAbtestTransport{
		resp: &abtestv1.GetExperimentResultResponse{},
	}
}

func (c *captureAbtestTransport) GetExperimentResult(_ context.Context, req *abtestv1.GetExperimentResultRequest) (*abtestv1.GetExperimentResultResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Copy the proto so later in-place mutations on the request struct (none
	// expected today, but defensive) don't tamper with our capture.
	cp := &abtestv1.GetExperimentResultRequest{
		Namespace:      req.GetNamespace(),
		UserId:         req.GetUserId(),
		LayerIds:       append([]string(nil), req.GetLayerIds()...),
		ExperimentType: req.GetExperimentType(),
		DisplayType:    req.GetDisplayType(),
		TraceId:        req.GetTraceId(),
	}
	c.reqs = append(c.reqs, cp)
	if c.err != nil {
		return nil, c.err
	}
	return c.resp, nil
}

func (c *captureAbtestTransport) LastRequest() *abtestv1.GetExperimentResultRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reqs) == 0 {
		return nil
	}
	return c.reqs[len(c.reqs)-1]
}

func (c *captureAbtestTransport) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

// newClientWithCaptureAbtest builds a minimal *Client wired against a capture
// transport WITHOUT going through Init (which would dial / pull / spawn
// goroutines). This is the unit-test seam for trace_id assertions on the
// outbound proto.
//
// subscribedNs is treated as the sole subscribed namespace. The client owns no
// real config transport — tests that exercise pull/subscribe live in the
// dedicated _traceid_test.go files for those paths.
func newClientWithCaptureAbtest(t *testing.T, subscribedNs string) (*Client, *captureAbtestTransport) {
	t.Helper()
	tr := newCaptureAbtestTransport()
	cli := &Client{
		cfg: Config{
			AbtestTimeout: 500 * time.Millisecond,
		},
		metrics:              newMetrics(),
		cache:                newConfigCache(newMetrics()),
		logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		subscribedNamespaces: []string{subscribedNs},
		defaultNamespace:     subscribedNs,
		defaultNsSubscribed:  true,
		abtestTr:             tr,
	}
	return cli, tr
}

func TestGetExperimentResult_EmptyTraceID_GeneratesUUID(t *testing.T) {
	cli, tr := newClientWithCaptureAbtest(t, "ns1")

	_, err := cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
		Namespace: "ns1",
		UserInfo:  UserInfo{UID: "u1"},
		TraceID:   "",
	})
	if err != nil {
		t.Fatalf("GetExperimentResult: %v", err)
	}
	req := tr.LastRequest()
	if req == nil {
		t.Fatal("no request captured")
	}
	if req.GetTraceId() == "" {
		t.Fatal("expected non-empty TraceId on outbound proto when caller passed empty TraceID")
	}
	if !uuidV4Pattern.MatchString(req.GetTraceId()) {
		t.Fatalf("expected UUID-shaped TraceId, got %q", req.GetTraceId())
	}
}

func TestGetExperimentResult_CallerTraceID_PassedThrough(t *testing.T) {
	cli, tr := newClientWithCaptureAbtest(t, "ns1")

	const caller = "caller-id"
	_, err := cli.GetExperimentResult(context.Background(), ExperimentResultRequest{
		Namespace: "ns1",
		UserInfo:  UserInfo{UID: "u1"},
		TraceID:   caller,
	})
	if err != nil {
		t.Fatalf("GetExperimentResult: %v", err)
	}
	req := tr.LastRequest()
	if req == nil {
		t.Fatal("no request captured")
	}
	if got := req.GetTraceId(); got != caller {
		t.Fatalf("expected TraceId=%q passed through verbatim, got %q", caller, got)
	}
}
