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

	// Fast-path (design §3): the server sets has_dynamic_resolution=false on a
	// key only when it has NO gray/experiment attached, so abtest can never hit
	// it — skip the GetExperimentResult RPC entirely and resolve directly to the
	// full-release/default value. We gate strictly on present && val == false:
	// an absent field (old server) or true keeps the existing abtest path, so a
	// new SDK against an old server never silently skips a live experiment.
	if hdr, present := c.cache.hasDynamicResolution(resolvedNs, key); present && !hdr {
		return c.resolveFullOrDefault(resolvedNs, key, defaultValue, abctx)
	}

	// Per-ns memoised abtest result (at-most-once RPC per request link).
	abresult, err := abctx.resultFor(ctx, resolvedNs)
	if err != nil {
		// ctx canceled / deadline — surface to caller so they can abort.
		return defaultValue, err
	}

	// abtest hit path: key present in config_flat_kv with a non-zero version.
	if abresult != nil {
		if abVersion, ok := abresult.keyVersions[key]; ok && abVersion != 0 {
			if val, ok := c.cache.valueOf(resolvedNs, key, abVersion); ok {
				c.logger.Debug("tipsyabconfig: get_config hit (abtest)",
					"ns", resolvedNs, "key", key, "version", abVersion, "uid", abctx.userID, "trace_id", abctx.traceID)
				return val, nil
			}
			// ab→full fallback: local cache missing the ab version. Log WARN
			// (design §B.3 / M6).
			c.metrics.abtestFallback.inc(resolvedNs)
			c.logger.Warn("tipsyabconfig: ab version missing in local cache; falling back to full",
				"ns", resolvedNs, "key", key, "ab_version", abVersion, "trace_id", abctx.traceID)
		}
	}

	// Full-release fallback (M6): key not in config_flat_kv, or ab→full.
	return c.resolveFullOrDefault(resolvedNs, key, defaultValue, abctx)
}

// resolveFullOrDefault returns the full-release value for (resolvedNs, key), or
// defaultValue when no full-release version exists or its value is missing from
// the cache. This is the M6 full-release/default branch shared by the abtest
// path (key not in config_flat_kv / ab→full) and the has_dynamic_resolution
// fast-path. It performs no abtest RPC. Semantics are identical to the original
// inline branch; resolvedNs MUST already be resolved.
func (c *Client) resolveFullOrDefault(resolvedNs, key, defaultValue string, abctx *AbtestContext) (string, error) {
	fullVersion, ok := c.cache.fullReleaseVersion(resolvedNs, key)
	if !ok {
		return defaultValue, nil
	}
	val, ok := c.cache.valueOf(resolvedNs, key, fullVersion)
	if !ok {
		return defaultValue, nil
	}
	c.logger.Debug("tipsyabconfig: get_config hit (full)",
		"ns", resolvedNs, "key", key, "version", fullVersion, "uid", abctx.userID, "trace_id", abctx.traceID)
	return val, nil
}
