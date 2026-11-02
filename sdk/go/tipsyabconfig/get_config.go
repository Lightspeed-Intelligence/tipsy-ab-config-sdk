package tipsyabconfig

import "context"

// GetConfigStatic returns the value for (ns, key) at its active full-release
// version. Pure cache read; no abtest call; no exposure event. Use it for
// service-level / no-user-context paths. Returns (defaultValue, false) on a
// cache miss.
//
// Per design §10.5 the empty string is a valid cached value. Callers MUST
// gate on the second return value, not on len(value).
func (c *Client) GetConfigStatic(ns, key, defaultValue string) (string, bool) {
	if c == nil {
		return defaultValue, false
	}
	versionID, ok := c.cache.fullReleaseVersion(ns, key)
	if !ok {
		return defaultValue, false
	}
	val, ok := c.cache.valueOf(ns, key, versionID)
	if !ok {
		return defaultValue, false
	}
	c.logger.Info("tipsyabconfig: get_config_static hit",
		"ns", ns, "key", key, "version", versionID, "source", "full_static")
	return val, true
}

// GetConfig resolves the dynamic config (ns, key) for a specific user, honoring
// abtest hits (whitelist > experiment > full release) per design 04 §B.3.
//
// ns resolution (design 04 §B.1, decision A-3): an empty ns falls back to the
// project default namespace (Config.DefaultNamespace override or the
// `PROJECT_DEFAULT_NAMESPACE` env var read once at Init). If neither is set,
// GetConfig returns ErrNamespaceRequired. A resolved-but-unsubscribed ns
// returns ErrNamespaceNotSubscribed.
//
// abctx must be non-nil; pass EmptyAbtestContext() if there is no user
// identity. The per-ns abtest result is memoised into abctx on first access so
// the whole request link issues AT MOST ONE GetExperimentResult RPC per ns
// (design 04 §B.3). When abtest is unavailable or the per-ns call failed,
// GetConfig falls back to the full-release version silently — emitting a
// fallback metric tick but not an error.
//
// M6 (design 04 §B.3): after obtaining the abtest key→version result (merged
// locally from the per-group response) the SDK ALWAYS preserves the
// full-release fallback. A key absent from the map is the common
// "no experiment hit" case and resolves to the full-release version, NOT the
// default. The default is only returned when neither an abtest hit nor a
// full-release version exists; that return still emits one Info hit line
// (reason=default + default_cause, see defaultCauseFor) and stays
// (defaultValue, nil).
func (c *Client) GetConfig(ctx context.Context, abctx *AbtestContext, ns, key, defaultValue string) (string, error) {
	return c.getConfigResolved(ctx, abctx, ns, key, defaultValue)
}

// GetConfigDefault is the ns-optional convenience form of GetConfig (design 04
// §B.5): it resolves the namespace from the project default namespace. It is
// exactly GetConfig with an empty ns argument, so it returns
// ErrNamespaceRequired when no default namespace is configured.
func (c *Client) GetConfigDefault(ctx context.Context, abctx *AbtestContext, key, defaultValue string) (string, error) {
	return c.getConfigResolved(ctx, abctx, "", key, defaultValue)
}

func (c *Client) getConfigResolved(ctx context.Context, abctx *AbtestContext, ns, key, defaultValue string) (string, error) {
	if c == nil {
		return defaultValue, ErrClosed
	}
	if abctx == nil {
		return defaultValue, ErrAbtestContextMissing
	}
	resolvedNs, err := c.resolveNamespace(ns)
	if err != nil {
		return defaultValue, err
	}

	// Capture the snapshot ONCE so every read below (fast-path gate, ab value,
	// full-release value) is snapshot-consistent — a concurrent cache replace
	// cannot tear this single lookup across two snapshots. A nil snapshot is
	// still fed through resultFor + resolveKeyFromSnapshot below so the
	// no-snapshot ns keeps its legacy "fire the at-most-once RPC, then return
	// default" behaviour (design Important Details difference #1).
	snap := c.cache.snapshot(resolvedNs)

	// Fast-path (design §3): the server sets has_dynamic_resolution=false on a
	// key only when it has NO gray/experiment attached, so abtest can never hit
	// it — skip the GetExperimentResult RPC entirely and resolve directly to the
	// full-release/default value. We gate strictly on present && val == false:
	// an absent field (old server) or true keeps the existing abtest path, so a
	// new SDK against an old server never silently skips a live experiment.
	var abresult *abtestComputeResult
	if !keyIsStaticInSnapshot(snap, key) {
		// Per-ns memoised abtest result (at-most-once RPC per request link).
		abresult, err = abctx.resultFor(ctx, resolvedNs)
		if err != nil {
			// ctx canceled / deadline — surface to caller so they can abort.
			return defaultValue, err
		}
	}

	res := c.resolveKeyFromSnapshot(snap, resolvedNs, key, abresult, abctx.traceID)
	if res.source == keySourceNone {
		// Default hit log (SLS contract): nothing resolved, so the caller's
		// default is served. Same msg family as the hit lines below with
		// reason=default plus default_cause; it never carries version /
		// experiment_id / group_id / release_id, nor the default value itself.
		c.logger.Info("tipsyabconfig: get_config hit (default)",
			"ns", resolvedNs, "key", key, "reason", reasonDefault,
			"default_cause", defaultCauseFor(snap, key, abresult, abctx.experimentHashID),
			"uid", abctx.experimentHashID, "trace_id", abctx.traceID)
		return defaultValue, nil
	}
	// Hit log (SLS contract, design §4): msg text unchanged; `reason` is always
	// present (one of the four resolved-value reasons here; the fifth,
	// reason=default, is emitted by the default branch above); experiment_id/
	// group_id/release_id are conditional fields with omit semantics — when
	// absent the key is not emitted at all (never an empty-string placeholder).
	// Downstream picks the conditional fields to read based on reason.
	msg := "tipsyabconfig: get_config hit (full)"
	reason := reasonFullRelease
	if res.source == keySourceAbtest {
		msg = "tipsyabconfig: get_config hit (abtest)"
		switch res.attribution.source {
		case attributionExperiment:
			reason = reasonExperiment
		case attributionGrayWhitelist:
			reason = reasonGrayWhitelist
		default:
			reason = reasonAbtestUnattributed
		}
	}
	attrs := []any{"ns", resolvedNs, "key", key, "version", res.version, "reason", reason}
	switch reason {
	case reasonExperiment:
		attrs = append(attrs, "experiment_id", res.attribution.experimentID, "group_id", res.attribution.groupID)
	case reasonGrayWhitelist:
		attrs = append(attrs, "release_id", res.attribution.releaseID)
	}
	attrs = append(attrs, "uid", abctx.experimentHashID, "trace_id", abctx.traceID)
	c.logger.Info(msg, attrs...)
	return res.value, nil
}

// reason enum for the getConfig hit log (SLS contract, design §4). Complete —
// exactly these five values are ever emitted:
//
//   - full_release: value came from the full-release branch (incl. the
//     fast-path and the ab→full fallback — the served value really is the
//     full-release version).
//   - experiment: abtest hit attributed to an experiment group; the log line
//     also carries experiment_id + group_id.
//   - gray_whitelist: abtest hit attributed to a gray release whitelist; the
//     log line also carries release_id.
//   - abtest_unattributed: abtest hit whose attribution is unknown (empty-id
//     group, release_id=0, MockAbtestContext seed) — value valid, attribution
//     missing.
//   - default: nothing resolved and the caller's default was returned
//     ("get_config hit (default)"); the line carries default_cause and has no
//     version.
//
// The contract covers dynamic GetConfig / GetConfigDefault only;
// GetConfigStatic keeps its unchanged source=full_static line with no reason.
const (
	reasonFullRelease        = "full_release"
	reasonExperiment         = "experiment"
	reasonGrayWhitelist      = "gray_whitelist"
	reasonAbtestUnattributed = "abtest_unattributed"
	reasonDefault            = "default"
)

// default_cause enum for the reason=default line. defaultCauseFor tests the
// rows in this order and returns the first match; downstream real-enrollment
// counts only no_full_release (abtest was evaluated, the user's groups do not
// configure the key, and the key has no full release).
const (
	defaultCauseUnknownKey              = "unknown_key"                // no snapshot for the ns, or key not in it
	defaultCauseFastPath                = "fast_path"                  // has_dynamic_resolution=false, abtest skipped
	defaultCauseNoUser                  = "no_user"                    // experimentHashID "" or "0"
	defaultCauseAbtestUnavailable       = "abtest_unavailable"         // abtest result is the degraded sentinel
	defaultCauseAbVersionMissing        = "ab_version_missing"         // ab version not in local cache, no full release
	defaultCauseNoFullRelease           = "no_full_release"            // key not in merged result (or version 0), no full release
	defaultCauseFullReleaseValueMissing = "full_release_value_missing" // full release id set but its value is absent
)

// defaultCauseFor classifies why GetConfig is returning the caller's default.
// Precondition: resolveKeyFromSnapshot(snap, ..., key, abresult, ...) resolved
// no value, so a non-zero ab version in abresult is necessarily missing from
// snap, and a full-release version (if any) has no value in snap. Pure: reads
// only its arguments.
//
// The fast path is decided from has_dynamic_resolution itself (the same gate
// GetConfig used to skip abtest), never from abresult — abresult is nil there.
// "abtest evaluated" means abresult is neither nil nor emptyAbtestResult,
// compared by pointer identity: a successful merge always allocates a fresh
// result (mergeEachExperimentGroupResult), so an evaluated-but-empty result
// (the user only lands in empty-param groups) yields no_full_release, never
// abtest_unavailable. "No full release" is FullReleaseVersion == 0.
func defaultCauseFor(snap *NamespaceSnapshot, key string, abresult *abtestComputeResult, experimentHashID string) string {
	var ks KeyState
	known := false
	if snap != nil {
		ks, known = snap.Keys[key]
	}
	switch {
	case !known:
		return defaultCauseUnknownKey
	case keyIsStaticInSnapshot(snap, key):
		return defaultCauseFastPath
	case isNoUserExperimentHashID(experimentHashID):
		return defaultCauseNoUser
	case abresult == nil || abresult == emptyAbtestResult:
		return defaultCauseAbtestUnavailable
	}
	noFullRelease := ks.FullReleaseVersion == 0
	switch {
	case abresult.keyVersions[key] != 0 && noFullRelease:
		return defaultCauseAbVersionMissing
	case noFullRelease:
		return defaultCauseNoFullRelease
	default:
		return defaultCauseFullReleaseValueMissing
	}
}

// keySource records where resolveKeyFromSnapshot found a key's value.
type keySource uint8

const (
	keySourceNone   keySource = iota // neither an abtest hit nor a full release
	keySourceAbtest                  // abtest whitelist / experiment hit
	keySourceFull                    // full-release version
)

// keyResolution is the outcome of resolving one key against a snapshot. source
// == keySourceNone means the key has no resolvable value (single key ⇒ return
// default; get-all ⇒ omit the key). value/version are meaningful only when
// source != keySourceNone; an empty-string value is a valid hit (§10.5).
// attribution is meaningful only when source == keySourceAbtest (zero value =
// unattributed otherwise); it feeds the hit-log reason and never influences
// which value/version was resolved (F3 decoupling).
type keyResolution struct {
	value       string
	source      keySource
	version     int64
	attribution keyAttribution
}

// keyIsStaticInSnapshot reports whether key is a pure full-release key in snap
// (has_dynamic_resolution present AND explicitly false). Only such a key may
// skip the abtest RPC. A nil snapshot, an absent key, or an absent/true field
// all return false so the abtest path is preserved (no silent skip against an
// old server). It reads only the passed snapshot (no cache re-snapshot).
func keyIsStaticInSnapshot(snap *NamespaceSnapshot, key string) bool {
	if snap == nil {
		return false
	}
	ks, ok := snap.Keys[key]
	return ok && ks.HasDynamicResolution != nil && !*ks.HasDynamicResolution
}

// resolveKeyFromSnapshot applies the single-key resolution precedence (abtest
// hit > full release) against ONE captured snapshot, issuing no RPC and never
// re-snapshotting (snapshot-consistency invariant, design §2). It is the shared
// per-key logic behind both GetConfig and GetAllConfigs.
//
// snap may be nil (single-key no-snapshot ns, difference #1) — reads on the
// zero KeyState are safe (nil Versions map), so an ab hit still emits the WARN +
// abtestFallback tick and falls through to a full-release miss ⇒ default,
// byte-identical to the pre-refactor single-key path. abresult may be nil
// (fast-path / short-circuited ctx) — then only the full-release branch is
// considered. ns is passed explicitly (not read from snap) so it is available
// even when snap is nil. traceID is stamped on the fallback WARN.
func (c *Client) resolveKeyFromSnapshot(snap *NamespaceSnapshot, ns, key string, abresult *abtestComputeResult, traceID string) keyResolution {
	var ks KeyState
	if snap != nil {
		ks = snap.Keys[key]
	}
	if abresult != nil {
		if abVersion, hit := abresult.keyVersions[key]; hit && abVersion != 0 {
			if val, ok := ks.Versions[abVersion]; ok {
				return keyResolution{value: val, source: keySourceAbtest, version: abVersion,
					attribution: abresult.attributionFor(key)}
			}
			// ab→full fallback: this snapshot is missing the ab version.
			c.metrics.abtestFallback.inc(ns)
			c.logger.Warn("tipsyabconfig: ab version missing in local cache; falling back to full",
				"ns", ns, "key", key, "ab_version", abVersion, "trace_id", traceID)
		}
	}
	if ks.FullReleaseVersion != 0 {
		if val, ok := ks.Versions[ks.FullReleaseVersion]; ok {
			return keyResolution{value: val, source: keySourceFull, version: ks.FullReleaseVersion}
		}
	}
	return keyResolution{}
}
