package tipsyabconfig

import (
	"context"
	"errors"
	"sync"
	"time"

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
// Construction is pure-create: NewAbtestContext does NOT issue any
// GetExperimentResult RPC (no eager pre-fetch of any namespace). Every
// namespace — including the client defaultNamespace — is fetched lazily on
// first dynamic GetConfig for that ns and memoised into results so the whole
// request link issues AT MOST ONE GetExperimentResult RPC per namespace. To
// warm a namespace ahead of first GetConfig, call the explicit, opt-in
// PrefetchConfigVersionFlatKvForNamespace.
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
	// GetExperimentResult RPC issued from this ctx (lazy resultFor / explicit
	// prefetch). Always non-empty post-construction: the constructor falls
	// back to uuid.New().String() when the caller passes "".
	//
	// Concurrency note: traceID is assigned exactly once inside
	// newAbtestContext before the constructor returns, so the lazy resultFor /
	// prefetch goroutines have a happens-before edge on the read. No mutex
	// needed.
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

// attributionSource classifies where a key's abtest hit came from, for the
// getConfig hit-log `reason` field. The zero value is deliberately
// attributionUnattributed so a missing keyAttributions entry (MockAbtestContext
// seeds, legacy results) degrades to "value valid, attribution unknown" instead
// of a bogus experiment / gray claim.
type attributionSource uint8

const (
	attributionUnattributed  attributionSource = iota // value hit, attribution unknown (empty-id group, release_id=0, mock seed)
	attributionExperiment                             // experiment-group hit (experimentID + groupID populated)
	attributionGrayWhitelist                          // gray-release whitelist hit (releaseID populated)
)

// keyAttribution records why one key's abtest versionId won, alongside (never
// instead of) the keyVersions value. Value resolution and attribution are two
// independent tracks (design F3 invariant): attribution being absent or
// partial only degrades the log reason, it never changes which versionId is
// served.
type keyAttribution struct {
	source       attributionSource
	experimentID string // non-empty only when source == attributionExperiment
	groupID      string // non-empty only when source == attributionExperiment
	releaseID    int64  // non-zero only when source == attributionGrayWhitelist
}

// abtestComputeResult is the SDK-local view of a GetExperimentResult response:
// the key→versionId map consumed by the dynamic getConfig path, merged locally
// from the per-group response shape (groups + gray_hits), plus the per-key
// attribution used by the hit log. keyVersions key is the config_key name
// (not id).
//
// Every key present in keyVersions produced by the local merge also has a
// keyAttributions entry (possibly unattributed). keyAttributions may be nil on
// results seeded outside the merge (MockAbtestContext); look up via
// attributionFor which degrades to unattributed.
type abtestComputeResult struct {
	keyVersions     map[string]int64
	keyAttributions map[string]keyAttribution
}

// attributionFor returns the attribution for key, degrading to the zero value
// (unattributed) when the map is nil or has no entry — e.g. MockAbtestContext
// seeds, which only populate keyVersions.
func (r *abtestComputeResult) attributionFor(key string) keyAttribution {
	if r == nil || r.keyAttributions == nil {
		return keyAttribution{}
	}
	return r.keyAttributions[key]
}

// emptyAbtestResult is the sentinel "no abtest hits" result. We never close
// over it; callers must construct fresh AbtestContext instances per request.
var emptyAbtestResult = &abtestComputeResult{keyVersions: map[string]int64{}}

// NewAbtestContext creates a fresh per-request AbtestContext. Construction is
// pure-create: it issues NO GetExperimentResult RPC. Every namespace is
// fetched lazily and memoised on first dynamic GetConfig (design 04 §B.3); to
// warm a specific namespace ahead of time, call the opt-in
// PrefetchConfigVersionFlatKvForNamespace.
//
// No-user uid: when userID is "" or "0" the ctx is treated as identity-less —
// every not-yet-resolved namespace short-circuits to the empty result without
// an RPC (equivalent to EmptyAbtestContext), so GetConfig / GetAllConfigs
// resolve purely from full release. A real uid keeps the lazy per-ns fetch.
//
// parentCtx is the parent ctx whose deadline / cancel signal propagates to
// every lazy per-ns GetExperimentResult RPC (and any explicit prefetch). Pass
// the request ctx.
//
// userAttrs is converted to abtestv1.Value entries on the wire. Supported
// concrete types: string, int, int32, int64, float32, float64, bool.
// Unsupported values are skipped with a WARN log.
func (c *Client) NewAbtestContext(parentCtx context.Context, userID string, userAttrs map[string]any) *AbtestContext {
	return c.newAbtestContext(parentCtx, userID, userAttrs, "")
}

// NewAbtestContextWithTraceID is NewAbtestContext with an explicit per-request
// trace_id (sdk-trace-id §4). Empty traceID ⇒ the SDK generates a fresh
// uuid.New().String(); non-empty ⇒ passed through verbatim. Every
// GetExperimentResult RPC issued from this ctx (lazy per-ns fetch / explicit
// prefetch) carries this trace_id.
//
// Use this in Gin / net/http middleware to propagate an inbound X-Trace-Id /
// X-Request-Id from the upstream request; see Middleware / GinMiddleware.
func (c *Client) NewAbtestContextWithTraceID(parentCtx context.Context, userID string, userAttrs map[string]any, traceID string) *AbtestContext {
	return c.newAbtestContext(parentCtx, userID, userAttrs, traceID)
}

func (c *Client) newAbtestContext(parentCtx context.Context, userID string, userAttrs map[string]any, traceID string) *AbtestContext {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	// trace_id: empty ⇒ generate locally so SDK-side and server-side log
	// lines for this request share the same id (server-side normalization
	// would otherwise produce a fresh id we never see here).
	if traceID == "" {
		traceID = uuid.New().String()
	}
	// Pure-create: build the struct only. No GetExperimentResult RPC is issued
	// at construction; the first dynamic GetConfig (or an explicit
	// PrefetchConfigVersionFlatKvForNamespace) lazily fetches the needed ns.
	return &AbtestContext{
		userID:    userID,
		userAttrs: userAttrs,
		parentCtx: parentCtx,
		results:   make(map[string]*computeStatus, 1),
		owner:     c,
		traceID:   traceID,
	}
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

// isNoUserUID reports whether uid denotes "no real user identity". Both the
// empty string and the string zero "0" mean identity-less: neither can be
// bucketed into an experiment or matched against a whitelist server-side, so
// the SDK skips the GetExperimentResult RPC entirely and resolves statically
// (full release / default), matching an EmptyAbtestContext.
func isNoUserUID(uid string) bool {
	return uid == "" || uid == "0"
}

// ensureFetch guarantees that ns is being fetched (at most once) into this
// ctx's results map and returns its computeStatus. It owns the full critical
// section: it takes a.mu itself, double-checks the per-ns computeStatus, and —
// only when the ns is not yet present — creates it (with an open done channel)
// and either short-circuits to the empty result (identity-less / mock ctx, a
// no-user uid, or an unsubscribed ns; no RPC) or spawns the single fetch
// goroutine. Idempotent: when ns is already present it returns the existing
// computeStatus without issuing a new RPC. This is the shared dedup primitive
// behind both the lazy resultFor wait path and the explicit
// PrefetchConfigVersionFlatKvForNamespace warm path, so concurrent
// first-accessors of the same ns share one RPC.
func (a *AbtestContext) ensureFetch(ns string) *computeStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, ok := a.results[ns]
	if ok {
		return st
	}
	st = &computeStatus{done: make(chan struct{})}
	a.results[ns] = st
	switch {
	case a.empty || a.owner == nil:
		// Identity-less / mock ctx: resolve to empty without an RPC.
		st.result = emptyAbtestResult
		close(st.done)
	case isNoUserUID(a.userID):
		// No-user uid ("" / "0"): identity-less, resolve statically without an
		// RPC (same effect as an EmptyAbtestContext). This is a deliberate
		// short-circuit, NOT a degraded fallback, so it does not bump the
		// abtestFallback metric. owner is non-nil here (the arm above catches
		// owner == nil), so logging never dereferences a nil owner.
		a.owner.logger.Debug("tipsyabconfig: skip abtest: no-user uid",
			"ns", ns, "uid", a.userID, "trace_id", a.traceID)
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
			st.result, st.err = a.owner.fetchConfigVersionFlatKvForNamespace(parent, ns, a.userID, a.userAttrs, a.traceID)
			close(st.done)
		}()
	}
	return st
}

// resultFor returns the memoised abtest result for ns within this request
// link, fetching it synchronously exactly once on first access (design 04
// §B.3). Concurrency: ensureFetch owns a.mu and creates/looks up the per-ns
// computeStatus; the first goroutine to reach a not-yet-fetched ns is
// responsible for the RPC, while every other goroutine racing on the same ns
// finds the existing computeStatus and blocks on the SAME done channel. Net
// effect: AT MOST ONE GetExperimentResult RPC per ns per request link.
//
// The fetch runs under the ctx the AbtestContext captured at construction
// (parentCtx); the caller's ctx only governs the wait. A per-ns RPC failure
// degrades silently to the empty result (caller falls through to full
// release), mirroring WaitForAbtest.
func (a *AbtestContext) resultFor(ctx context.Context, ns string) (*abtestComputeResult, error) {
	if a == nil {
		return nil, ErrAbtestContextMissing
	}
	st := a.ensureFetch(ns)

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

// PrefetchConfigVersionFlatKvForNamespace warms the config_version experiment
// result for ns into this ctx ahead of the first GetConfig, at most once
// (idempotent). The name keeps "FlatKv" for API compatibility; internally the
// fetch now requests the per-group shape (EACH_EXPERIMENT_GROUP) and merges it
// locally into the same flat key→versionId map, preserving per-key attribution
// for the getConfig hit log. It is non-blocking: it triggers the shared
// ensureFetch primitive and returns immediately without waiting for the RPC to
// complete; a subsequent GetConfig / WaitForAbtest for the same ns reuses the
// in-flight or completed result rather than issuing a second RPC, preserving
// the at-most-once invariant.
//
// This is the explicit, opt-in prefetch API (construction itself never
// pre-fetches). A nil receiver, an empty / mock ctx, or an unsubscribed ns all
// short-circuit inside ensureFetch and issue NO RPC.
func (a *AbtestContext) PrefetchConfigVersionFlatKvForNamespace(ns string) {
	if a == nil {
		return
	}
	a.ensureFetch(ns)
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

// fetchConfigVersionFlatKvForNamespace wraps AbtestService.GetExperimentResult
// with the per-call timeout, hardwired to the config_version per-group shape
// the dynamic getConfig path consumes (ExperimentType_CONFIG_VERSION +
// RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP). The name keeps its historical
// "FlatKv" wording because the produced keyVersions map is the same flat
// key→versionId shape callers always consumed; since the attribution change the
// map is merged locally from groups + gray_hits (see
// mergeEachExperimentGroupResult) instead of read from the server-side
// config_flat_kv field — that field is not read at all, and there is NO flat
// fallback path (design §7: pre-v2 servers do not exist in our deployments).
// It is the internal per-ns fetch primitive and must not be confused with the
// public custom_params GetExperimentResult. On any error (including a missing
// abtest connection) it returns the empty result and bumps the per-ns fallback
// counter so the caller can monitor degraded mode.
//
// traceID is the per-request id stamped onto the proto request. It is assumed
// already-normalised by the caller (newAbtestContext / *WithTraceID
// constructors); empty here is technically valid wire-wise (server-side
// normalisation generates one), but the AbtestContext constructors never leave
// it empty.
func (c *Client) fetchConfigVersionFlatKvForNamespace(parentCtx context.Context, ns, userID string, userAttrs map[string]any, traceID string) (*abtestComputeResult, error) {
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
		DisplayType:    abtestv1.ResultDisplayType_RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP,
		TraceId:        traceID,
	}
	start := time.Now()
	resp, err := c.abtestTr.GetExperimentResult(callCtx, req)
	attrs := []any{"ns", ns, "trace_id", traceID,
		"duration_ms", float64(time.Since(start).Microseconds()) / 1000}
	if err != nil {
		attrs = append(attrs, "err", err)
	}
	c.logger.Debug("tipsyabconfig: GetExperimentResult rpc", attrs...)
	if err != nil {
		// Ctx cancellation is an expected termination, not a fault (issue
		// #15): count it in the dedicated abtestCanceled metric (NOT
		// abtestFallback, which stays a pure fault signal) and log at Info.
		// The return semantics are identical to the fault arm — empty result
		// + err, so the caller degrades to the full release exactly as before.
		if isContextCanceled(err) {
			c.metrics.abtestCanceled.inc(ns)
			c.logger.Info("tipsyabconfig: AbtestService.GetExperimentResult canceled; falling back to full release",
				"ns", ns, "trace_id", traceID, "err", err)
			return emptyAbtestResult, err
		}
		c.metrics.abtestFallback.inc(ns)
		c.logger.Warn("tipsyabconfig: AbtestService.GetExperimentResult failed; falling back to full release",
			"ns", ns, "trace_id", traceID, "err", err)
		return emptyAbtestResult, err
	}
	return c.mergeEachExperimentGroupResult(resp, ns, traceID), nil
}

// mergeEachExperimentGroupResult merges a per-group GetExperimentResult
// response (groups + gray_hits) into the flat key→versionId map plus per-key
// attribution. The merge rules are a line-by-line replica of the platform's
// server-side flat_kv assembly (platform repo
// internal/abtest/compute/engine.go:329-385 at HEAD dd3cf76), because "result
// equals what the old FLAT_KV-consuming SDK saw" is the only correctness
// criterion:
//
//  1. gray_hits first, in wire order (the platform emits them ascending by
//     release_id, guarded by its own tests): first-writer-wins per key. A
//     same-key collision between two gray_hits is structurally unreachable on
//     the wire (the platform folds multi-gray conflicts before assembling
//     gray_hits, topology.go:433-437 / :499-514), so the skip here is purely
//     defensive.
//  2. groups next, in wire order, CONFIG_VERSION groups only: a key already
//     owned by a gray hit is SKIPPED — gray beats experiment unconditionally
//     (replicates engine.go:355-356; this cross-source rule is a strict,
//     gate-level guarantee). Between experiment groups, later writes overwrite
//     earlier ones (last-write-wins, replicates engine.go:380).
//
// Same-source conflict contract (user decision 2025-08-26, verbatim):
// 正常情况下不会发生同类型 key 冲突，同类型 key 冲突是异常情况，此时平台 + SDK
// 只需保障至少返回可选值中的一个就算符合承诺。
// The SDK still replicates the platform's winner rules above (same cost as
// writing them any other way), but the same-source winner identity carries no
// guarantee and is not gated.
//
// Implementation constraints:
//   - Traverse the wire `repeated` fields (groups, gray_hits) in order; the
//     merge MUST NOT be driven by iterating a Go map (random order ⇒ flapping:
//     the same response could merge to different winners). Iterating a single
//     entry's params_versions / key_versions map is fine — keys are unique
//     within one map, so no conflict can be decided by that iteration order.
//   - Cross-repo coupling note (design R3): the platform has NO test pinning
//     the wire order of `groups` (adding a sort in assembleGroups leaves its
//     suite green). After the same-source relaxation the SDK no longer depends
//     on that order for any guaranteed behaviour — do not reintroduce a
//     dependency on it. gray_hits ascending-by-release_id IS test-guarded on
//     the platform side and step 1 relies on it.
//   - F3 invariant (value/attribution decoupling): writing keyVersions never
//     depends on the attribution fields being valid. A group with empty
//     experiment_id/group_id, or a gray hit with release_id == 0, still writes
//     its versionId and records an unattributed entry — skipping it would drop
//     a value and change resolution. The experiment_type filter in step 2 is a
//     deliberate exception to this direction: it is fail-closed (a group
//     without CONFIG_VERSION type contributes nothing), mirroring the
//     platform's own flat-path filter, which is test-guarded platform-side.
//
// Every key written to keyVersions gets a keyAttributions entry — possibly
// unattributed — so the hit-log reason is always decidable.
func (c *Client) mergeEachExperimentGroupResult(resp *abtestv1.GetExperimentResultResponse, ns, traceID string) *abtestComputeResult {
	out := &abtestComputeResult{
		keyVersions:     make(map[string]int64),
		keyAttributions: make(map[string]keyAttribution),
	}
	// Step 1: gray_hits, wire order (ascending release_id), first-writer-wins.
	grayOwned := make(map[string]struct{})
	for _, hit := range resp.GetGrayHits() {
		releaseID := hit.GetReleaseId()
		for key, versionID := range hit.GetKeyVersions() {
			if _, dup := grayOwned[key]; dup {
				// Same-source (gray vs gray) conflict: wire-unreachable,
				// defensive only. First writer wins; DEBUG, not WARN — the
				// platform treats conflict folding as deterministic normal
				// behaviour, not an anomaly.
				c.logger.Debug("tipsyabconfig: abtest merge: duplicate gray key, first writer wins",
					"ns", ns, "key", key, "kept_version", out.keyVersions[key],
					"kept_release_id", out.keyAttributions[key].releaseID,
					"skipped_version", versionID, "skipped_release_id", releaseID,
					"trace_id", traceID)
				continue
			}
			grayOwned[key] = struct{}{}
			out.keyVersions[key] = versionID
			if releaseID != 0 {
				out.keyAttributions[key] = keyAttribution{source: attributionGrayWhitelist, releaseID: releaseID}
			} else {
				// F3: value still written; only the attribution degrades.
				out.keyAttributions[key] = keyAttribution{source: attributionUnattributed}
			}
		}
	}
	// Step 2: groups, wire order, CONFIG_VERSION only; gray-owned keys are
	// skipped (gray wins unconditionally — strict guarantee); between
	// experiment groups the later write overwrites (last-write-wins).
	for _, group := range resp.GetGroups() {
		if group.GetExperimentType() != abtestv1.ExperimentType_EXPERIMENT_TYPE_CONFIG_VERSION {
			continue
		}
		experimentID := group.GetExperimentId()
		groupID := group.GetGroupId()
		for key, versionID := range group.GetParamsVersions() {
			if _, gray := grayOwned[key]; gray {
				// Cross-source conflict: gray beats experiment, always
				// (engine.go:355-356). DEBUG for observability, not WARN.
				c.logger.Debug("tipsyabconfig: abtest merge: key owned by gray release, experiment skipped",
					"ns", ns, "key", key, "kept_version", out.keyVersions[key],
					"skipped_version", versionID, "skipped_experiment_id", experimentID,
					"trace_id", traceID)
				continue
			}
			if prevVersion, dup := out.keyVersions[key]; dup {
				// Same-source (experiment vs experiment) conflict: abnormal
				// data, see the contract in the function comment. Last write
				// wins, replicating the platform. DEBUG, not WARN.
				c.logger.Debug("tipsyabconfig: abtest merge: experiment key conflict, last writer wins",
					"ns", ns, "key", key, "prev_version", prevVersion,
					"prev_experiment_id", out.keyAttributions[key].experimentID,
					"new_version", versionID, "new_experiment_id", experimentID,
					"trace_id", traceID)
			}
			out.keyVersions[key] = versionID
			if experimentID != "" && groupID != "" {
				out.keyAttributions[key] = keyAttribution{source: attributionExperiment, experimentID: experimentID, groupID: groupID}
			} else {
				// F3: an empty-id group still writes its value (skipping would
				// drop the versionId and change value resolution).
				out.keyAttributions[key] = keyAttribution{source: attributionUnattributed}
			}
		}
	}
	return out
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
