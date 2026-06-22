package io.tipsy.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.tipsy.abconfig.AbtestTestSupport.NsCache;
import java.time.Duration;
import java.util.Map;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicReference;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * Concurrency + memoisation tests for {@link AbtestContext} (design 05 §并发正确性
 * 要点 1, 2 and the F5 invariant; covers points 1, 2, 5).
 *
 * <p>Core invariant: AT MOST ONE {@code GetExperimentResult} RPC per (ns, request)
 * even under heavy concurrent first-access. Concurrent first-accessors are forced
 * to pile up on the same in-flight RPC via a server-side {@code releaseGate}
 * latch and started simultaneously via a {@link CountDownLatch}.
 */
final class AbtestContextConcurrencyTest {

    private static final String NS = "checkout";
    private static final Duration WAIT = Duration.ofSeconds(10);

    // ------------------------------------------------------------------
    // Point 1: at-most-once RPC per ns under concurrent first access.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("16 threads, same ctx + ns, concurrent first access -> exactly ONE abtest RPC")
    void concurrentFirstAccessIssuesExactlyOneRpc() throws Exception {
        final int n = 16;
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .abtestTimeout(Duration.ofSeconds(30)) // never let the gated RPC deadline-fail
                .build()) {

            // Gate the single RPC so every first-accessor must wait on the same future.
            CountDownLatch gate = new CountDownLatch(1);
            h.abtest.releaseGate = gate;

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());

            CountDownLatch ready = new CountDownLatch(n);
            CountDownLatch go = new CountDownLatch(1);
            ExecutorService pool = Executors.newFixedThreadPool(n);
            AtomicReference<Throwable> firstError = new AtomicReference<>();
            String[] results = new String[n];
            try {
                for (int i = 0; i < n; i++) {
                    final int idx = i;
                    pool.submit(() -> {
                        ready.countDown();
                        try {
                            go.await();
                            results[idx] = h.client.getConfig(ctx, NS, "color", "DEF");
                        } catch (Throwable t) {
                            firstError.compareAndSet(null, t);
                        }
                    });
                }
                assertTrue(ready.await(5, TimeUnit.SECONDS), "all workers should be ready");
                go.countDown(); // release all threads at once

                // Wait until the single RPC has actually arrived at the server, then
                // confirm the count is pinned at 1 while the rest pile on the future.
                assertTrue(AbtestTestSupport.awaitTrue(() -> h.abtest.callsFor(NS) >= 1, WAIT),
                        "the single RPC should reach the server");
                // The gate is still closed; no second RPC can have been issued.
                assertEquals(1, h.abtest.callsFor(NS),
                        "while the first RPC is in flight, no second RPC may be issued");

                gate.countDown(); // let the RPC complete; all waiters drain off the future.
            } finally {
                pool.shutdown();
                assertTrue(pool.awaitTermination(15, TimeUnit.SECONDS), "workers should finish");
            }

            assertEquals(null, firstError.get(),
                    "no worker should see an exception: " + firstError.get());
            for (int i = 0; i < n; i++) {
                assertEquals("gold", results[i], "every worker resolves the same ab value");
            }
            // The decisive assertion: exactly one RPC for the whole request link.
            assertEquals(1, h.abtest.callsFor(NS),
                    "AT MOST ONE GetExperimentResult RPC per (ns, request)");
            assertEquals(1, h.abtest.totalCalls.get());
        }
    }

    @Test
    @DisplayName("repeated sequential getConfig on the same ctx+ns reuses the memoised result (no extra RPC)")
    void sequentialReuseIssuesNoExtraRpc() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"))
                .key("size", 4L, Map.of(4L, "M", 6L, "L"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L, "size", 6L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"));
            assertEquals("L", h.client.getConfig(ctx, NS, "size", "DEF"));
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"));
            // Two keys, same ns, three calls -> still ONE RPC (memoised per ns).
            assertEquals(1, h.abtest.callsFor(NS));
        }
    }

    // ------------------------------------------------------------------
    // Point 2: eager prefetch + lazy share one future (no duplicate RPC).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("eager prefetch ns shares the future with lazy getConfig -> still ONE RPC")
    void eagerPrefetchAndLazyShareFuture() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            // newAbtestContextForNamespace eagerly prefetches NS in the background.
            AbtestContext ctx = h.client.newAbtestContextForNamespace(NS, "u-1", Map.of());

            // Wait for the eager prefetch RPC to land.
            assertTrue(AbtestTestSupport.awaitTrue(() -> h.abtest.callsFor(NS) >= 1, WAIT),
                    "eager prefetch should issue the RPC");

            // A subsequent getConfig for the same ns must reuse the eager future.
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"));
            assertEquals(1, h.abtest.callsFor(NS),
                    "eager prefetch + lazy getConfig share one future -> no second RPC");
        }
    }

    @Test
    @DisplayName("default-namespace eager prefetch (newAbtestContext) shares the future with getConfig")
    void defaultNamespaceEagerPrefetchSharesFuture() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .defaultNamespace(NS) // makes newAbtestContext eagerly prefetch NS
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertTrue(AbtestTestSupport.awaitTrue(() -> h.abtest.callsFor(NS) >= 1, WAIT),
                    "default-ns eager prefetch should issue the RPC");

            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"));
            assertEquals(1, h.abtest.callsFor(NS),
                    "default-ns eager prefetch + lazy getConfig share one future");
        }
    }

    // ------------------------------------------------------------------
    // Point 5: F5 — eager future never completes exceptionally.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("F5: abtest not configured -> newAbtestContext eager path does NOT throw; getConfig falls back to full")
    void f5EagerFutureNeverThrowsWhenAbtestUnconfigured() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .defaultNamespace(NS)
                .withoutAbtest() // no AbtestService address -> degraded mode
                .snapshot(NS, cache)
                .build()) {

            // Construction (which eagerly prefetches the default ns) must not throw.
            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());

            // getConfig must not throw an ExecutionException; it degrades to full release.
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "abtest unconfigured -> silent full-release fallback, no thrown ExecutionException");
            assertEquals(0, h.abtest.totalCalls.get(),
                    "no AbtestService configured -> zero RPCs");
            // The degraded eager prefetch bumps the fallback counter for the ns.
            assertTrue(h.client.metrics().abtestFallbackTotal(NS) >= 1L,
                    "degraded mode (no abtest transport) bumps the fallback counter");
        }
    }

    @Test
    @DisplayName("F5: abtest RPC fails on the eager-prefetched ns -> getConfig still degrades silently")
    void f5EagerFutureSilentOnRpcFailure() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .defaultNamespace(NS)
                .snapshot(NS, cache)
                .build()) {
            h.abtest.failFor(NS);

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "eager prefetch RPC failure degrades silently to full release");
            assertTrue(h.client.metrics().abtestFallbackTotal(NS) >= 1L);
        }
    }
}
