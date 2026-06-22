package io.tipsy.abconfig;

import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicLong;

/**
 * Per-namespace last-write-wins gauge backing the SDK {@link Metrics}.
 *
 * <p>Mirrors Go {@code nsGauge}: a {@code map[string]*atomic.Uint64} written
 * with overwrite semantics (e.g. {@code local_cache_bytes}, set on every cache
 * replace). Unlike {@link NsCounter} this is NOT monotonic; {@link #set} stores
 * the latest value, replacing any previous one. Lookups for an unknown
 * namespace return {@code 0}.
 */
final class NsGauge {

    private final ConcurrentHashMap<String, AtomicLong> byNs = new ConcurrentHashMap<>();

    /** Overwrites the gauge for {@code ns} with {@code value}. */
    void set(String ns, long value) {
        byNs.computeIfAbsent(ns, k -> new AtomicLong()).set(value);
    }

    /** Returns the last value written for {@code ns}, or {@code 0} if never set. */
    long get(String ns) {
        AtomicLong v = byNs.get(ns);
        return v == null ? 0L : v.get();
    }
}
