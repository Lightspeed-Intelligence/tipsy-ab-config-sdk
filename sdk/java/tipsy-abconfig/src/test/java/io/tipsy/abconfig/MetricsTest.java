package io.tipsy.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;

import org.junit.jupiter.api.Test;

/**
 * Unit tests for {@link Metrics}, {@link NsCounter} and {@link NsGauge}.
 *
 * <p>Design ref: 04-transport-and-cache.md §"可观测". Verifies monotonic counters
 * inc/get, overwrite-gauge semantics, and that unknown namespaces read as 0
 * without throwing.
 */
final class MetricsTest {

    private static final String NS = "checkout";
    private static final String OTHER = "search";

    @Test
    void counters_incAndGet_perNamespaceIndependent() {
        Metrics m = new Metrics();

        m.pullFailure.inc(NS);
        m.pullFailure.inc(NS);
        m.subscribeDisc.inc(NS);
        m.subscribeEvent.inc(NS);
        m.subscribeEvent.inc(NS);
        m.subscribeEvent.inc(NS);
        m.abtestFallback.inc(NS);
        m.businessSeqMoved.inc(NS);
        m.experimentSeqMov.inc(NS);
        m.experimentSeqMov.inc(NS);

        assertEquals(2L, m.pullFailureTotal(NS));
        assertEquals(1L, m.subscribeDisconnectTotal(NS));
        assertEquals(3L, m.subscribeEventReceivedTotal(NS));
        assertEquals(1L, m.abtestFallbackTotal(NS));
        assertEquals(1L, m.businessSeqChangeTotal(NS));
        assertEquals(2L, m.experimentSeqChangeTotal(NS));

        // A different namespace shares nothing.
        assertEquals(0L, m.pullFailureTotal(OTHER));
        assertEquals(0L, m.subscribeEventReceivedTotal(OTHER));
    }

    @Test
    void globalCacheEmptyCounter() {
        Metrics m = new Metrics();
        assertEquals(0L, m.cacheEmptyTotal());
        m.cacheEmpty.incrementAndGet();
        m.cacheEmpty.incrementAndGet();
        assertEquals(2L, m.cacheEmptyTotal());
    }

    @Test
    void unknownNamespace_readsZeroAcrossAllReaders() {
        Metrics m = new Metrics();
        assertEquals(0L, m.pullFailureTotal("never-touched"));
        assertEquals(0L, m.subscribeDisconnectTotal("never-touched"));
        assertEquals(0L, m.subscribeEventReceivedTotal("never-touched"));
        assertEquals(0L, m.abtestFallbackTotal("never-touched"));
        assertEquals(0L, m.businessSeqChangeTotal("never-touched"));
        assertEquals(0L, m.experimentSeqChangeTotal("never-touched"));
        assertEquals(0L, m.localCacheBytes("never-touched"));
    }

    @Test
    void localCacheBytes_isOverwriteGaugeNotAccumulator() {
        Metrics m = new Metrics();

        m.localCacheBytes.set(NS, 100L);
        assertEquals(100L, m.localCacheBytes(NS));

        // Second set REPLACES, does not accumulate.
        m.localCacheBytes.set(NS, 40L);
        assertEquals(40L, m.localCacheBytes(NS),
                "localCacheBytes must keep the last written value (overwrite gauge)");

        m.localCacheBytes.set(NS, 0L);
        assertEquals(0L, m.localCacheBytes(NS), "gauge may be reset back to 0");
    }

    @Test
    void nsCounter_directInstanceContract() {
        NsCounter c = new NsCounter();
        assertEquals(0L, c.get("a"), "unknown ns -> 0");
        c.inc("a");
        c.inc("a");
        c.inc("b");
        assertEquals(2L, c.get("a"));
        assertEquals(1L, c.get("b"));
        assertEquals(0L, c.get("c"));
    }

    @Test
    void nsGauge_directInstanceContract() {
        NsGauge g = new NsGauge();
        assertEquals(0L, g.get("a"), "unknown ns -> 0");
        g.set("a", 7L);
        assertEquals(7L, g.get("a"));
        g.set("a", 3L);
        assertEquals(3L, g.get("a"), "set overwrites");
        assertEquals(0L, g.get("b"));
    }
}
