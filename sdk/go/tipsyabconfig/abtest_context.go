package tipsyabconfig

import (
	"context"
	"errors"
	"sync"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
	"github.com/google/uuid"
)

// AbtestContext is the per-request handle the SDK uses to memoise abtest
// GetExperimentResult results per namespace across one request link. Construct
// one per inbound HTTP / RPC request via NewAbtestContext (or
// EmptyAbtestContext for "no user context" paths), pass it through to every
// GetConfig call within the request, then let it go out of scope at request
// end.
//
// v2 semantics (design 04 §B.2/§B.3): construction does NOT fan out to every
// subscribed namespace. It eagerly pre-fetches AT MOST the prefetch namespace
// (the explicit construction ns, else the client defaultNamespace); other
// namespaces are fetched lazily on first dynamic GetConfig for that ns and
// memoised into results so the whole request link issues AT MOST ONE
// GetExperimentResult RPC per namespace.
//
// AbtestContext is safe for concurrent use by all goroutines participating in
// the same request: the per-ns lazy fetch deduplicates concurrent first-access
// via a shared computeStatus done channel (exactly one RPC, the rest wait).
type AbtestContext struct {
	userID    string
	userAttrs map[string]any

	// parentCtx is the request ctx captured at construction. Lazy per-ns
	// fetches (resultFor) inherit its deadline / cancellation so a request
	// abort propagates to any in-flight GetExperimentResult RPC.
	parentCtx context.Context

	mu      sync.Mutex
	results map[string]*computeStatus

	// empty marks an identity-less / mock ctx: resultFor short-circuits every
	// not-yet-resolved ns to the empty result without issuing any RPC. Set by
	// EmptyAbtestContext / MockAbtestContext.
	empty bool

	// owner is the Client that issued this ctx. AbtestContext is bound to
	// one Client because the cache lookup (in GetConfig) must use the same
	// per-process cache that issued the GetExperimentResult call.
	owner *Client

	// traceID is the per-request trace identifier propagated to every
	// GetExperimentResult RPC issued from this ctx (eager prefetch + lazy
	// resultFor). Always non-empty post-construction: the constructor falls
	// back to uuid.New().String() when the caller passes "".
	//
	// Concurrency note: traceID is assigned exactly once inside
	// newAbtestContext before the eager-prefetch goroutine is started AND
	// before the constructor returns, so the lazy resultFor goroutines have a
	// happens-before edge on the read. No mutex needed.
	traceID string
}

// UserInfo is the SDK-stable view of the user identity carried by an
// AbtestContext. Business code retrieves it via
// AbtestContextFromContext(ctx).UserInfo() (design 04 §B.4). Attrs is the same
// map the AbtestContext was constructed with (may be nil); callers MUST treat
// it as read-only.
type UserInfo struct {
	UID   string
	Attrs map[string]any
}

// computeStatus is the shared result slot for one (request, namespace) pair.
// done is closed once result+err have been populated. It is the dedup
// primitive: concurrent first-accessors of a not-yet-fetched ns share one
// computeStatus and block on done, so only the goroutine that created it
// issues the RPC.
type computeStatus struct {
	done   chan struct{}
	result *abtestComputeResult
	err    error
}

// abtestComputeResult is the SDK-local view of a GetExperimentResult response:
// the config_flat_kv key→version map consumed by the dynamic getConfig fast
// path. keyVersions key is the config_key name (not id).
type abtestComputeResult struct {
	keyVersions map[string]int64
}

// emptyAbtestResult is the sentinel "no abtest hits" result. We never close
// over it; callers must construct fresh AbtestContext instances per request.
var emptyAbtestResult = &abtestComputeResult{keyVersions: map[string]int64{}}

// NewAbtestContext creates a fresh per-request AbtestContext and, when the
// client has a project default namespace, eagerly pre-fetches ONLY that
// namespace's config_version flat_kv result in the background (design 04
// §B.2). Other namespaces are fetched lazily and memoised on first dynamic
// GetConfig (design 04 §B.3). Returns synchronously; the eager result is
// awaited lazily via GetConfig / WaitForAbtest.
//
// To pre-fetch a specific namespace (rather than the default), use
// NewAbtestContextForNamespace.
//
// parentCtx is the parent ctx whose deadline / cancel signal propagates to the
// eager pre-request AND every lazy per-ns GetExperimentResult RPC. Pass the
// request ctx.
//
// userAttrs is converted to abtestv1.Value entries on the wire. Supported
// concrete types: string, int, int32, int64, float32, float64, bool.
// Unsupported values are skipped with a WARN log.
func (c *Client) NewAbtestContext(parentCtx context.Context, userID string, userAttrs map[string]any) *AbtestContext {
	return c.newAbtestContext(parentCtx, "", userID, userAttrs, "")
}

// NewAbtestContextWithTraceID is NewAbtestContext with an explicit per-request
// trace_id (sdk-trace-id §4). Empty traceID ⇒ the SDK generates a fresh
// uuid.New().String(); non-empty ⇒ passed through verbatim. Every
// GetExperimentResult RPC issued from this ctx (eager prefetch + lazy per-ns
// fetch) carries this trace_id.
//
// Use this in Gin / net/http middleware to propagate an inbound X-Trace-Id /
// X-Request-Id from the upstream request; see Middleware / GinMiddleware.
func (c *Client) NewAbtestContextWithTraceID(parentCtx context.Context, userID string, userAttrs map[string]any, traceID string) *AbtestContext {
	return c.newAbtestContext(parentCtx, "", userID, userAttrs, traceID)
}

// NewAbtestContextForNamespace is NewAbtestContext with an explicit prefetch
// namespace. When ns is non-empty AND subscribed, the eager pre-request
// targets ns instead of the client defaultNamespace. When ns is empty it
// behaves exactly like NewAbtestContext.
func (c *Client) NewAbtestContextForNamespace(parentCtx context.Context, ns, userID string, userAttrs map[string]any) *AbtestContext {
	return c.newAbtestContext(parentCtx, ns, userID, userAttrs, "")
}

// NewAbtestContextForNamespaceWithTraceID combines NewAbtestContextForNamespace
// (explicit prefetch ns) with the explicit trace_id of
// NewAbtestContextWithTraceID. See those two helpers for semantics.
func (c *Client) NewAbtestContextForNamespaceWithTraceID(parentCtx context.Context, ns, userID string, userAttrs map[string]any, traceID string) *AbtestContext {
	return c.newAbtestContext(parentCtx, ns, userID, userAttrs, traceID)
}

func (c *Client) newAbtestContext(parentCtx context.Context, prefetchNs, userID string, userAttrs map[string]any, traceID string) *AbtestContext {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	// trace_id: empty ⇒ generate locally so SDK-side and server-side log
	// lines for this request share the same id (server-side normalization
	// would otherwise produce a fresh id we never see here).
	if traceID == "" {
		traceID = uuid.New().String()
	}
	ctx := &AbtestContext{
		userID:    userID,
		userAttrs: userAttrs,
		parentCtx: parentCtx,
		results:   make(map[string]*computeStatus, 1),
		owner:     c,
		traceID:   traceID,
	}
	// Resolve the prefetch ns: explicit construction ns wins, else the client
	// default namespace. Empty (or unsubscribed) ⇒ no eager pre-request; the
	// first dynamic GetConfig lazily fetches its ns instead (design 04 §B.2).
	if prefetchNs == "" {
		prefetchNs = c.defaultNamespace
	}
	if prefetchNs == "" || !c.isSubscribed(prefetchNs) {
		return ctx
	}
	st := &computeStatus{done: make(chan struct{})}
	ctx.results[prefetchNs] = st
	go func() {
		st.result, st.err = c.getExperimentResultForNamespace(parentCtx, prefetchNs, userID, userAttrs, ctx.traceID)
		close(st.done)
	}()
	return ctx
}

// EmptyAbtestContext returns a ctx whose abtest results resolve to the empty
// result. Use it on paths with no user identity (cron jobs, internal
// pipelines) so GetConfig still works and never fires a GetExperimentResult
// RPC. Per design §B.2: with the no-fan-out change the empty ctx pre-resolves
// nothing eagerly; instead resultFor short-circuits to the empty result for an
// identity-less ctx, so no RPC is ever issued.
//
// A fresh trace_id is generated even for the empty ctx so any downstream log
// / report channel stays internally consistent (sdk-trace-id §4).
func (c *Client) EmptyAbtestContext() *AbtestContext {
	return &AbtestContext{
		results: make(map[string]*computeStatus),
		owner:   c,
		empty:   true,
		traceID: uuid.New().String(),
	}
}

// MockAbtestContext is the test helper described in abtest-platform-sdk.md
// §9.4. Each entry in keyVersionsByNS pre-resolves the abtest result for that
// namespace; namespaces not in the map resolve lazily — for a mock ctx the
// lazy path short-circuits to the empty result (no RPC), matching the prior
// "namespaces not in the map resolve to the empty result" behaviour.
//
// A fresh trace_id is generated so any downstream log / report channel stays
// internally consistent (sdk-trace-id §4).
func (c *Client) MockAbtestContext(userID string, keyVersionsByNS map[string]map[string]int64) *AbtestContext {
	ctx := &AbtestContext{
		userID:  userID,
		results: make(map[string]*computeStatus, len(keyVersionsByNS)),
		owner:   c,
		empty:   true, // unspecified namespaces resolve to empty, no RPC.
		traceID: uuid.New().String(),
	}
	for ns, kv := range keyVersionsByNS {
		st := &computeStatus{done: make(chan struct{}), result: &abtestComputeResult{keyVersions: kv}}
		close(st.done)
		ctx.results[ns] = st
	}
	return ctx
}

// UserID returns the user_id this ctx was constructed with.
func (a *AbtestContext) UserID() string {
	if a == nil {
		return ""
	}
	return a.userID
}

// UserInfo returns the full user identity (uid + attrs) this ctx was
// constructed with (design 04 §B.4). Attrs aliases the constructor map and is
// read-only. Returns the zero UserInfo for a nil receiver.
func (a *AbtestContext) UserInfo() UserInfo {
	if a == nil {
		return UserInfo{}
	}
	return UserInfo{UID: a.userID, Attrs: a.userAttrs}
}

// TraceID returns the per-request trace id this ctx propagates to every
// GetExperimentResult RPC (sdk-trace-id §4). For a NewAbtestContext /
// EmptyAbtestContext / MockAbtestContext result it is the SDK-generated UUID
// (always non-empty); for a *WithTraceID constructor it is the
// caller-supplied value (or a fresh UUID when "" was passed). Returns "" for
// a nil receiver.
func (a *AbtestContext) TraceID() string {
	if a == nil {
		return ""
	}
	return a.traceID
}

// resultFor returns the memoised abtest result for ns within this request
// link, fetching it synchronously exactly once on first access (design 04
// §B.3). Concurrency: under a.mu we double-check the per-ns computeStatus; the
// first goroutine creates it (with an open done channel) and is responsible for
// the RPC, while every other goroutine that races on the same not-yet-fetched
// ns finds the existing computeStatus, releases the lock, and blocks on the
// SAME done channel. Net effect: AT MOST ONE GetExperimentResult RPC per ns
// per request link.
//
// The fetch runs under the ctx the AbtestContext captured at construction
// (parentCtx); the caller's ctx only governs the wait. A per-ns RPC failure
// degrades silently to the empty result (caller falls through to full
// release), mirroring WaitForAbtest.
func (a *AbtestContext) resultFor(ctx context.Context, ns string) (*abtestComputeResult, error) {
	if a == nil {
		return nil, ErrAbtestContextMissing
	}
	a.mu.Lock()
	st, ok := a.results[ns]
	if !ok {
		st = &computeStatus{done: make(chan struct{})}
		a.results[ns] = st
		switch {
		case a.empty || a.owner == nil:
			// Identity-less / mock ctx: resolve to empty without an RPC.
			st.result = emptyAbtestResult
			close(st.done)
		case !a.owner.isSubscribed(ns):
			// Unsubscribed ns: the SDK only consumes subscribed namespaces and
			// has no cache for it, so degrade to empty without an RPC. Dynamic
			// GetConfig rejects unsubscribed ns earlier via resolveNamespace;
			// this guards the low-level WaitForAbtest entry.
			st.result = emptyAbtestResult
			close(st.done)
		default:
			parent := a.parentCtx
			if parent == nil {
				parent = context.Background()
			}
			go func() {
				st.result, st.err = a.owner.getExperimentResultForNamespace(parent, ns, a.userID, a.userAttrs, a.traceID)
				close(st.done)
			}()
		}
	}
	a.mu.Unlock()

	select {
	case <-st.done:
		if st.err != nil || st.result == nil {
			// Degraded path: callers see "empty hits, no error" so the
			// surrounding GetConfig falls through to the full-release branch
			// silently (per-ns failure is isolated).
			return emptyAbtestResult, nil
		}
		return st.result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// WaitForAbtest blocks until the abtest result for ns is available (or the
// caller's ctx is cancelled). It triggers the same lazy per-ns memoise as
// dynamic GetConfig: a not-yet-fetched ns is fetched once and cached. Returns
// the empty result + nil error when the per-ns call failed — per design §B.3 a
// single-ns failure degrades that ns silently and the other ns are unaffected.
func (a *AbtestContext) WaitForAbtest(ctx context.Context, ns string) (*abtestComputeResult, error) {
	if a == nil {
		return nil, ErrAbtestContextMissing
	}
	if ns == "" {
		// No explicit ns at this low-level entry: keep the legacy "empty,
		// no error" contract rather than reaching for defaultNamespace here.
		// Dynamic GetConfig performs ns resolution before calling resultFor.
		return emptyAbtestResult, nil
	}
	return a.resultFor(ctx, ns)
}

// getExperimentResultForNamespace wraps AbtestService.GetExperimentResult with
// the per-call timeout for the config_version flat_kv shape the dynamic
// getConfig fast path consumes. On any error (including a missing abtest
// connection) it returns the empty result and bumps the per-ns fallback counter
// so the caller can monitor degraded mode.
//
// traceID is the per-request id stamped onto the proto request. It is assumed
// already-normalised by the caller (newAbtestContext / *WithTraceID
// constructors); empty here is technically valid wire-wise (server-side
// normalisation generates one), but the AbtestContext constructors never leave
// it empty.
func (c *Client) getExperimentResultForNamespace(parentCtx context.Context, ns, userID string, userAttrs map[string]any, traceID string) (*abtestComputeResult, error) {
	if c.abtestTr == nil {
		c.metrics.abtestFallback.inc(ns)
		return emptyAbtestResult, errors.New("abtest service not configured")
	}
	callCtx, cancel := context.WithTimeout(parentCtx, c.cfg.AbtestTimeout)
	defer cancel()
	req := &abtestv1.GetExperimentResultRequest{
		Namespace:      ns,
		UserId:         userID,
		UserAttrs:      encodeUserAttrs(userAttrs, c.logger),
		ExperimentType: abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION,
		DisplayType:    abtestv1.ResultDisplayType_RESULT_DISPLAY_TYPE_FLAT_KV,
		TraceId:        traceID,
	}
	resp, err := c.abtestTr.GetExperimentResult(callCtx, req)
	if err != nil {
		c.metrics.abtestFallback.inc(ns)
		c.logger.Warn("tipsyabconfig: AbtestService.GetExperimentResult failed; falling back to full release",
			"ns", ns, "trace_id", traceID, "err", err)
		return emptyAbtestResult, err
	}
	out := &abtestComputeResult{
		keyVersions: make(map[string]int64, len(resp.GetConfigFlatKv())),
	}
	for k, v := range resp.GetConfigFlatKv() {
		out.keyVersions[k] = v
	}
	return out, nil
}

func encodeUserAttrs(attrs map[string]any, logger interface{ Warn(string, ...any) }) map[string]*abtestv1.Value {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]*abtestv1.Value, len(attrs))
	for k, v := range attrs {
		val := encodeValue(v)
		if val == nil {
			if logger != nil {
				logger.Warn("tipsyabconfig: dropping unsupported user_attr value type", "key", k)
			}
			continue
		}
		out[k] = val
	}
	return out
}

func encodeValue(v any) *abtestv1.Value {
	switch x := v.(type) {
	case string:
		return &abtestv1.Value{V: &abtestv1.Value_S{S: x}}
	case bool:
		return &abtestv1.Value{V: &abtestv1.Value_B{B: x}}
	case int:
		return &abtestv1.Value{V: &abtestv1.Value_I{I: int64(x)}}
	case int32:
		return &abtestv1.Value{V: &abtestv1.Value_I{I: int64(x)}}
	case int64:
		return &abtestv1.Value{V: &abtestv1.Value_I{I: x}}
	case float32:
		return &abtestv1.Value{V: &abtestv1.Value_D{D: float64(x)}}
	case float64:
		return &abtestv1.Value{V: &abtestv1.Value_D{D: x}}
	default:
		return nil
	}
}
