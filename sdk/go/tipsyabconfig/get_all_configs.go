package tipsyabconfig

import "context"

// GetAllConfigs resolves EVERY dynamic config key under ns for a specific user
// and returns a fresh key→value map, using exactly the same per-key precedence
// as GetConfig (abtest whitelist / experiment hit > full release) — it just
// omits the single-key argument. It is the batched form of GetConfig, not a new
// resolution model: the two share resolveKeyFromSnapshot, so a key resolves to
// the identical value under either call.
//
// ns resolution mirrors GetConfig (design 04 §B.1): an empty ns falls back to
// the project default namespace; if none is configured GetAllConfigs returns
// ErrNamespaceRequired, and a resolved-but-unsubscribed ns returns
// ErrNamespaceNotSubscribed. abctx must be non-nil (pass EmptyAbtestContext()
// when there is no user identity); a no-user experimentHashID ("" / "0") resolves every key
// statically without a GetExperimentResult RPC.
//
// The whole namespace is resolved against ONE cache snapshot captured up front,
// so the result is snapshot-consistent (a concurrent cache replace can never
// tear the map across two snapshots). At most one GetExperimentResult RPC is
// issued per (request link, ns) and it is shared with any GetConfig on the same
// abctx+ns (memoised); when every key is pure full-release
// (has_dynamic_resolution explicitly false) no RPC is issued at all.
//
// Keys with neither an abtest hit nor a full-release value are OMITTED from the
// map (there is no per-key default in the batched form). An empty-string value
// is a valid value and is kept (§10.5). The returned map is freshly allocated
// and caller-owned. When ns is subscribed but its snapshot has not been pulled
// yet, GetAllConfigs returns a non-nil empty map with a nil error and issues no
// RPC.
func (c *Client) GetAllConfigs(ctx context.Context, abctx *AbtestContext, ns string) (map[string]string, error) {
	if c == nil {
		return nil, ErrClosed
	}
	if abctx == nil {
		return nil, ErrAbtestContextMissing
	}
	resolvedNs, err := c.resolveNamespace(ns)
	if err != nil {
		return nil, err
	}

	// Capture the snapshot ONCE (snapshot-consistency invariant, design §2):
	// every per-key read below uses this same immutable object, never a
	// cache-level accessor that would re-snapshot per key.
	snap := c.cache.snapshot(resolvedNs)
	out := make(map[string]string)
	if snap == nil {
		// Subscribed but not yet pulled: empty map, no RPC (design §2 step 3).
		c.logger.Debug("tipsyabconfig: get_all_configs (no snapshot)",
			"ns", resolvedNs, "uid", abctx.experimentHashID, "trace_id", abctx.traceID)
		return out, nil
	}

	// Whole-ns fast-path (design §2 step 4): if EVERY key is explicitly pure
	// full-release, no key can be an abtest hit, so skip the RPC entirely.
	// Otherwise fetch the memoised per-ns result (at-most-once RPC; a no-user /
	// empty ctx yields the empty result without an RPC).
	var abresult *abtestComputeResult
	if !allKeysStaticInSnapshot(snap) {
		abresult, err = abctx.resultFor(ctx, resolvedNs)
		if err != nil {
			// ctx canceled / deadline — surface to caller so they can abort.
			return nil, err
		}
	}

	var abHits, fullHits int
	for key := range snap.Keys {
		res := c.resolveKeyFromSnapshot(snap, resolvedNs, key, abresult, abctx.traceID)
		switch res.source {
		case keySourceAbtest:
			out[key] = res.value
			abHits++
		case keySourceFull:
			out[key] = res.value
			fullHits++
		}
	}

	c.logger.Debug("tipsyabconfig: get_all_configs",
		"ns", resolvedNs, "total_keys", len(snap.Keys), "ab_hits", abHits,
		"full_hits", fullHits, "dropped", len(snap.Keys)-len(out),
		"uid", abctx.experimentHashID, "trace_id", abctx.traceID)
	return out, nil
}

// GetAllConfigsDefault is the ns-optional convenience form of GetAllConfigs
// (design §2, decision D3): it resolves the namespace from the project default
// namespace. It is exactly GetAllConfigs with an empty ns argument, so it
// returns ErrNamespaceRequired when no default namespace is configured.
func (c *Client) GetAllConfigsDefault(ctx context.Context, abctx *AbtestContext) (map[string]string, error) {
	return c.GetAllConfigs(ctx, abctx, "")
}

// allKeysStaticInSnapshot reports whether EVERY key in snap is a pure
// full-release key (has_dynamic_resolution present AND explicitly false). Only
// then can GetAllConfigs skip the abtest RPC for the whole namespace. A
// zero-key snapshot is vacuously all-static (no key could be an abtest hit, so
// the RPC would be pointless — matches the Python/Java SDKs). Any key with an
// absent or true field keeps the abtest path, so a new SDK against an old
// server never silently skips a live experiment. It reads only the passed
// snapshot (no cache re-snapshot).
func allKeysStaticInSnapshot(snap *NamespaceSnapshot) bool {
	for _, ks := range snap.Keys {
		if ks.HasDynamicResolution == nil || *ks.HasDynamicResolution {
			return false
		}
	}
	return true
}
