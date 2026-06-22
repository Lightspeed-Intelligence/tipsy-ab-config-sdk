package io.github.lightspeedintelligence.abconfig;

import java.util.concurrent.atomic.AtomicLong;

/**
 * Per-process SDK counters exposed for host observability.
 *
 * <p>Mirrors Go {@code Metrics}. All counters except {@link #localCacheBytes}
 * are monotonic ({@link NsCounter} / {@link AtomicLong}); {@code localCacheBytes}
 * is a last-write-wins gauge ({@link NsGauge}) updated on every cache replace.
 * The host application polls the public {@code *Total} / {@code localCacheBytes}
 * methods and forwards them to its own metrics pipeline.
 *
 * <p>Thread-safe: the global counter is an {@link AtomicLong}; per-namespace
 * counters/gauges use concurrent maps. Lookups for an unknown namespace return
 * {@code 0}.
 *
 * <p>The package-private fields are the write side, mutated by the cache and the
 * background pull/subscribe loops. The public methods are the read side handed
 * to the host.
 */
public final class Metrics {

    /** Global {@code sdk_cache_empty_total} counter (incremented by the client on a cache miss). */
    final AtomicLong cacheEmpty = new AtomicLong();

    /** {@code sdk_pull_failure_total{namespace}} — incremented by the pull loop on a per-ns failure. */
    final NsCounter pullFailure = new NsCounter();

    /** {@code sdk_subscribe_disconnect_total{namespace}} — incremented by the subscribe loop on a stream error. */
    final NsCounter subscribeDisc = new NsCounter();

    /** {@code sdk_subscribe_event_received_total{namespace}} — incremented per applied subscribe snapshot. */
    final NsCounter subscribeEvent = new NsCounter();

    /** {@code sdk_local_cache_bytes{namespace}} — gauge set by the cache on every replace (overwrite, not accumulate). */
    final NsGauge localCacheBytes = new NsGauge();

    /** {@code sdk_abtest_fallback_total{namespace}} — incremented when a per-ns abtest call falls back to full release. */
    final NsCounter abtestFallback = new NsCounter();

    /** {@code sdk_business_seq_change_total{namespace}} — incremented by the cache when business_snapshot_seq advances. */
    final NsCounter businessSeqMoved = new NsCounter();

    /** {@code sdk_experiment_seq_change_total{namespace}} — incremented by the cache when experiment_snapshot_seq advances. */
    final NsCounter experimentSeqMov = new NsCounter();

    /** Package-private: the SDK constructs exactly one {@code Metrics} per client. */
    Metrics() {
    }

    /** {@code sdk_cache_empty_total}. */
    public long cacheEmptyTotal() {
        return cacheEmpty.get();
    }

    /** {@code sdk_pull_failure_total{namespace}}. */
    public long pullFailureTotal(String ns) {
        return pullFailure.get(ns);
    }

    /** {@code sdk_subscribe_disconnect_total{namespace}}. */
    public long subscribeDisconnectTotal(String ns) {
        return subscribeDisc.get(ns);
    }

    /** {@code sdk_subscribe_event_received_total{namespace}}. */
    public long subscribeEventReceivedTotal(String ns) {
        return subscribeEvent.get(ns);
    }

    /**
     * {@code sdk_local_cache_bytes{namespace}} — gauge: the byte size of the
     * most recent cache snapshot for {@code ns} (overwrite semantics, not
     * monotonic).
     */
    public long localCacheBytes(String ns) {
        return localCacheBytes.get(ns);
    }

    /** {@code sdk_abtest_fallback_total{namespace}}. */
    public long abtestFallbackTotal(String ns) {
        return abtestFallback.get(ns);
    }

    /** {@code sdk_business_seq_change_total{namespace}}. */
    public long businessSeqChangeTotal(String ns) {
        return businessSeqMoved.get(ns);
    }

    /** {@code sdk_experiment_seq_change_total{namespace}}. */
    public long experimentSeqChangeTotal(String ns) {
        return experimentSeqMov.get(ns);
    }
}
