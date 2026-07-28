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
	c.logger.Debug("tipsyabconfig: get_config_static hit",
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
// M6 (design 04 §B.3): after obtaining config_flat_kv the SDK ALWAYS preserves
// the full-release fallback. A key absent from the map is the common
// "no experiment hit" case and resolves to the full-release version, NOT the
// default. The default is only returned when neither an abtest hit nor a
// full-release version exists.
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
		return defaultValue, nil
	}
	msg := "tipsyabconfig: get_config hit (full)"
	if res.source == keySourceAbtest {
		msg = "tipsyabconfig: get_config hit (abtest)"
	}
	c.logger.Debug(msg,
		"ns", resolvedNs, "key", key, "version", res.version, "uid", abctx.userID, "trace_id", abctx.traceID)
	return res.value, nil
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
type keyResolution struct {
	value   string
	source  keySource
	version int64
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
				return keyResolution{value: val, source: keySourceAbtest, version: abVersion}
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
