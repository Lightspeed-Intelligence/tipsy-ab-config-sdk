package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.github.lightspeedintelligence.abconfig.AbtestTestSupport.NsCache;
import java.time.Duration;
import java.util.Map;
import java.util.concurrent.CountDownLatch;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * {@link TipsyAbConfigClient#getConfig} / {@code getConfigDefault} resolution
 * tests (design 05 §GetConfig 解析; covers points 3, 4, 6, 7, 8).
 *
 * <p>Resolution priority under test: abtest hit (with cache value) &gt; ab&rarr;full
 * fallback (cache miss on ab version) &gt; full-release fallback (key not in
 * config_flat_kv) &gt; default (no full release). Plus silent degrade on per-ns
 * abtest failure, the empty/mock no-RPC contracts, the unsubscribed-ns error,
 * and the null-abctx / closed-client guards.
 */
final class GetConfigResolutionTest {

    private static final String NS = "checkout";
    private static final Duration WAIT = Duration.ofSeconds(5);

    // ------------------------------------------------------------------
    // Point 3: abtest hit (key in config_flat_kv -> ab version cached).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("ab hit: returns the ab-version value from the cache")
    void abtestHitReturnsAbVersionValue() {
        // Cache holds two versions of "color": full release = 7 (blue), ab = 9 (gold).
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L)) // ab steers color -> v9
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"));
            // Exactly one RPC was needed for this ns.
            assertEquals(1, h.abtest.callsFor(NS));
            assertEquals(0L, h.client.metrics().abtestFallbackTotal(NS),
                    "a clean ab hit must NOT bump the fallback counter");
        }
    }

    @Test
    @DisplayName("ab hit with empty-string value is a hit (existence over isEmpty)")
    void abtestHitEmptyStringIsHit() {
        NsCache cache = new NsCache(2, 2)
                .key("banner", 3L, Map.of(3L, "full", 5L, "")); // ab v5 = empty string
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("banner", 5L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("", h.client.getConfig(ctx, NS, "banner", "DEF"),
                    "empty string is a valid ab hit, not a miss");
            assertEquals(0L, h.client.metrics().abtestFallbackTotal(NS));
        }
    }

    // ------------------------------------------------------------------
    // Point 3: ab version missing in cache -> fallback to full + counter, no throw.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("ab version not in local cache: fall back to full release + bump fallback, no throw")
    void abVersionCacheMissFallsBackToFull() {
        // Cache holds full release v7=blue, but NOT ab version v99.
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 99L)) // ab steers to a version we don't cache
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "ab version missing in cache -> full release value");
            assertEquals(1L, h.client.metrics().abtestFallbackTotal(NS),
                    "ab->full cache miss bumps the fallback counter once");
        }
    }

    // ------------------------------------------------------------------
    // Point 3: key absent from config_flat_kv -> full release (NOT default).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("key not in config_flat_kv (common no-hit case) -> full release, NOT default")
    void keyAbsentFromAbtestMapFallsToFullNotDefault() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                // ab map only steers "other_key"; "color" is a no-hit.
                .abtestConfigFlatKv(NS, Map.of("other_key", 5L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "no abtest hit is the common case -> full release, never default");
            assertEquals(0L, h.client.metrics().abtestFallbackTotal(NS),
                    "a plain no-hit is NOT a fallback (the metric stays 0)");
        }
    }

    // ------------------------------------------------------------------
    // Point 3: no ab hit AND no full release -> default.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("neither ab hit nor full release -> default")
    void noAbAndNoFullReturnsDefault() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue")); // unrelated key cached
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of()) // no ab hits at all
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("DEF", h.client.getConfig(ctx, NS, "unknown_key", "DEF"),
                    "key with no ab hit and no full release -> default");
        }
    }

    @Test
    @DisplayName("ab version == 0 sentinel is treated as no-hit -> full release")
    void abVersionZeroIsSentinelNotHit() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 0L)) // 0 is a sentinel, not a real version
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "ab version 0 is a sentinel -> fall through to full release");
            assertEquals(0L, h.client.metrics().abtestFallbackTotal(NS),
                    "a 0-sentinel no-hit is not a fallback");
        }
    }

    // ------------------------------------------------------------------
    // Point 4: single-ns abtest failure degrades silently to full release.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("abtest RPC fails for the ns: silent full-release fallback + counter, no business exception")
    void abtestFailureDegradesSilently() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .build()) {
            h.abtest.failFor(NS); // the AbtestService throws INTERNAL for this ns

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            // Does not throw; returns the full-release value.
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"));
            assertTrue(h.client.metrics().abtestFallbackTotal(NS) >= 1L,
                    "a failed abtest RPC bumps the fallback counter");
        }
    }

    // ------------------------------------------------------------------
    // Point 6: empty / mock contexts never issue an RPC.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("emptyAbtestContext: getConfig goes straight to full release, zero abtest RPCs")
    void emptyContextNeverHitsAbtest() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L)) // would steer, but empty ctx ignores abtest
                .build()) {

            AbtestContext ctx = h.client.emptyAbtestContext();
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"));
            assertEquals(0, h.abtest.callsFor(NS), "empty ctx must never call the AbtestService");
            assertEquals(0, h.abtest.totalCalls.get());
        }
    }

    @Test
    @DisplayName("mockAbtestContext: hits the pre-seeded ns, no RPC; unlisted ns -> full release, no RPC")
    void mockContextUsesPreseededValuesNoRpc() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS, "other")
                .snapshot(NS, cache)
                .snapshot("other", new NsCache(1, 1).key("k", 4L, Map.of(4L, "fullOther")))
                // even though the real abtest service would steer, mock ctx must not call it.
                .abtestConfigFlatKv(NS, Map.of("color", 7L))
                .build()) {

            // Mock pre-resolves NS color -> v9; "other" is unlisted.
            AbtestContext ctx = h.client.mockAbtestContext("u-1",
                    Map.of(NS, Map.of("color", 9L)));

            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "mocked ns resolves to the pre-seeded ab version");
            assertEquals("fullOther", h.client.getConfig(ctx, "other", "k", "DEF"),
                    "unlisted ns falls through to full release");
            assertEquals(0, h.abtest.totalCalls.get(), "mock ctx must never call the AbtestService");
        }
    }

    // ------------------------------------------------------------------
    // Point 7: unsubscribed ns / no default ns errors.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("getConfig on an unsubscribed ns throws NamespaceNotSubscribedException")
    void unsubscribedNsThrows() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(1, 1).key("k", 4L, Map.of(4L, "v")))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertThrows(NamespaceNotSubscribedException.class, () ->
                    h.client.getConfig(ctx, "not-subscribed", "k", "DEF"));
        }
    }

    @Test
    @DisplayName("getConfigDefault with no default ns throws NamespaceRequiredException")
    void getConfigDefaultNoDefaultNsThrows() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS) // no default namespace configured
                .snapshot(NS, new NsCache(1, 1).key("k", 4L, Map.of(4L, "v")))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertThrows(NamespaceRequiredException.class, () ->
                    h.client.getConfigDefault(ctx, "k", "DEF"));
        }
    }

    @Test
    @DisplayName("getConfigDefault resolves to the configured default ns")
    void getConfigDefaultUsesDefaultNs() {
        NsCache cache = new NsCache(2, 2).key("k", 4L, Map.of(4L, "vDefault"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .defaultNamespace(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of())
                .build()) {

            AbtestContext ctx = h.client.emptyAbtestContext();
            assertEquals("vDefault", h.client.getConfigDefault(ctx, "k", "DEF"));
        }
    }

    // ------------------------------------------------------------------
    // G1: construction is pure-create (no eager prefetch RPC). G2/G3:
    // explicit prefetch primitive (prefetchConfigVersionFlatKvForNamespace)
    // is non-blocking, at-most-once, idempotent, and short-circuits on
    // empty/mock ctx + unsubscribed ns.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("G1: newAbtestContext (no trace_id) issues ZERO RPC at construction")
    void constructionIssuesNoRpc() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .defaultNamespace(NS) // would have triggered the old eager default-ns prefetch
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertNotNull(ctx);
            // No getConfig / prefetch has run yet: the construction must be silent.
            assertEquals(0, h.abtest.totalCalls.get(),
                    "pure-create: construction must not issue any GetExperimentResult RPC");
            assertEquals(0, h.abtest.callsFor(NS));
        }
    }

    @Test
    @DisplayName("G1: newAbtestContext (explicit trace_id) issues ZERO RPC at construction")
    void constructionWithTraceIdIssuesNoRpc() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .defaultNamespace(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of(), "trace-xyz");
            assertNotNull(ctx);
            assertEquals(0, h.abtest.totalCalls.get(),
                    "pure-create with explicit trace_id: still zero RPC at construction");
        }
    }

    @Test
    @DisplayName("explicit prefetch issues exactly ONE RPC and returns immediately (non-blocking void)")
    void explicitPrefetchIssuesOneRpcNonBlocking() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                // Hold the RPC open so we can prove prefetch did NOT block on it.
                .abtestTimeout(Duration.ofSeconds(30))
                .build()) {

            CountDownLatch gate = new CountDownLatch(1);
            h.abtest.releaseGate = gate;

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());

            // Non-blocking: prefetch returns immediately even though the server's
            // RPC handler is still parked on the gate.
            ctx.prefetchConfigVersionFlatKvForNamespace(NS);

            // The single prefetch RPC reaches the server while we have NOT released
            // the gate, proving prefetch did not synchronously wait for completion.
            assertTrue(AbtestTestSupport.awaitTrue(() -> h.abtest.callsFor(NS) >= 1, WAIT),
                    "prefetch should trigger exactly one RPC asynchronously");
            assertEquals(1, h.abtest.callsFor(NS), "prefetch issues exactly one RPC");

            gate.countDown(); // let it complete; a later getConfig reuses the future.
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"));
            assertEquals(1, h.abtest.callsFor(NS),
                    "prefetch + subsequent getConfig reuse one future -> at-most-once");
            assertEquals(1, h.abtest.totalCalls.get());
        }
    }

    @Test
    @DisplayName("idempotent prefetch: two prefetch calls for the same ns issue only ONE RPC")
    void idempotentPrefetchSameNsIssuesOneRpc() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            ctx.prefetchConfigVersionFlatKvForNamespace(NS);
            ctx.prefetchConfigVersionFlatKvForNamespace(NS);

            assertTrue(AbtestTestSupport.awaitTrue(() -> h.abtest.callsFor(NS) >= 1, WAIT),
                    "the first prefetch should issue the RPC");
            // Give any (erroneous) duplicate a chance to land before asserting.
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"));
            assertEquals(1, h.abtest.callsFor(NS),
                    "a second prefetch for the same ns must reuse the memoised future");
            assertEquals(1, h.abtest.totalCalls.get());
        }
    }

    @Test
    @DisplayName("empty ctx prefetch short-circuits: ZERO RPC")
    void emptyContextPrefetchIssuesNoRpc() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.emptyAbtestContext();
            ctx.prefetchConfigVersionFlatKvForNamespace(NS);

            // Empty ctx short-circuits inside ensureFetch -> no RPC, even on
            // a subsequent getConfig.
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"));
            assertEquals(0, h.abtest.totalCalls.get(),
                    "empty ctx prefetch must never call the AbtestService");
        }
    }

    @Test
    @DisplayName("mock ctx prefetch short-circuits: ZERO RPC")
    void mockContextPrefetchIssuesNoRpc() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.mockAbtestContext("u-1", Map.of(NS, Map.of("color", 9L)));
            ctx.prefetchConfigVersionFlatKvForNamespace(NS);

            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "mock ctx resolves the pre-seeded value");
            assertEquals(0, h.abtest.totalCalls.get(),
                    "mock ctx prefetch must never call the AbtestService");
        }
    }

    @Test
    @DisplayName("unsubscribed-ns prefetch short-circuits: ZERO RPC")
    void unsubscribedNsPrefetchIssuesNoRpc() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(1, 1).key("k", 4L, Map.of(4L, "v")))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            // The low-level prefetch primitive guards an unsubscribed ns and
            // short-circuits to the empty result without an RPC.
            ctx.prefetchConfigVersionFlatKvForNamespace("not-subscribed");

            assertEquals(0, h.abtest.totalCalls.get(),
                    "prefetching an unsubscribed ns must not call the AbtestService");
            assertEquals(0, h.abtest.callsFor("not-subscribed"));
        }
    }

    // ------------------------------------------------------------------
    // Point 8: null abctx / closed client guards.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("getConfig with null abctx throws AbtestContextMissingException")
    void nullAbctxThrows() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(1, 1).key("k", 4L, Map.of(4L, "v")))
                .build()) {

            assertThrows(AbtestContextMissingException.class, () ->
                    h.client.getConfig(null, NS, "k", "DEF"));
        }
    }

    @Test
    @DisplayName("getConfig after close throws SdkClosedException")
    void closedClientThrows() {
        AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(1, 1).key("k", 4L, Map.of(4L, "v")))
                .build();
        AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
        h.client.close();
        try {
            assertThrows(SdkClosedException.class, () ->
                    h.client.getConfig(ctx, NS, "k", "DEF"));
        } finally {
            h.close(); // idempotent close of the harness (server/executor)
        }
    }

    // ------------------------------------------------------------------
    // Point 12: AbtestContext accessors return constructor values.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("AbtestContext userId()/userInfo()/traceId() return the constructed values")
    void contextAccessorsReturnConstructedValues() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(1, 1).key("k", 4L, Map.of(4L, "v")))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-42",
                    Map.of("country", "FR"), "trace-abc");
            assertEquals("u-42", ctx.userId());
            assertEquals("u-42", ctx.userInfo().uid());
            assertEquals("FR", ctx.userInfo().attrs().get("country"));
            assertEquals("trace-abc", ctx.traceId());
            assertNotNull(ctx.userInfo());
        }
    }
}
