package io.github.lightspeedintelligence.abconfig;

import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.atomic.AtomicLong;

/**
 * Per-namespace monotonic counter backing the SDK {@link Metrics}.
 *
 * <p>Mirrors Go {@code nsCounter}: a {@code map[string]*atomic.Uint64} of
 * monotonically increasing counters. Java uses {@code long} where Go uses
 * {@code uint64}; the magnitude difference is irrelevant for these counters.
 * Lookups for an unknown namespace return {@code 0} without allocating an
 * entry.
 */
final class NsCounter {

    private final ConcurrentHashMap<String, AtomicLong> byNs = new ConcurrentHashMap<>();

    /** Increments the counter for {@code ns}, creating it on first use. */
    void inc(String ns) {
        byNs.computeIfAbsent(ns, k -> new AtomicLong()).incrementAndGet();
    }

    /** Returns the current value for {@code ns}, or {@code 0} if never incremented. */
    long get(String ns) {
        AtomicLong v = byNs.get(ns);
        return v == null ? 0L : v.get();
    }
}
