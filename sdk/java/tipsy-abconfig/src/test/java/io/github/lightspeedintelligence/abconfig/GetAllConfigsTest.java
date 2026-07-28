package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.github.lightspeedintelligence.abconfig.AbtestTestSupport.NsCache;
import java.util.HashMap;
import java.util.Map;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * {@link TipsyAbConfigClient#getAllConfigs} / {@code getAllConfigsDefault}
 * tests (design "getAllConfigs" §2 + semantics table + Testing Plan 1-5,7-9).
 *
 * <p>get-all reuses the single-key resolution path per key over ONE captured
 * immutable snapshot: abtest hit (cache value) &gt; ab&rarr;full fallback (metric
 * + WARN) &gt; full-release fallback &gt; <b>key removed</b> (no full release; the
 * one deliberate divergence from single-key, which returns a default). The
 * empty string stays a valid value. Errors mirror {@code getConfig}
 * (closed / null abctx / unsubscribed / no-default-ns). A subscribed-but-empty
 * cache yields an empty map with zero RPC; at-most-once RPC holds across a
 * getConfig+getAllConfigs mix; the returned map is a fresh mutable copy.
 */
final class GetAllConfigsTest {

    private static final String NS = "checkout";

    // ------------------------------------------------------------------
    // Testing Plan 1: resolution matrix in one ns (ab-hit / ab->full fallback /
    // full-only / neither=removed / empty-string value), asserted as an exact map.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("resolution matrix: ab-hit, ab->full fallback, full-only, removed, empty-string value")
    void resolutionMatrixOneNamespace() {
        NsCache cache = new NsCache(2, 2)
                .key("abhit", 7L, Map.of(7L, "blue", 9L, "gold"))   // ab v9 cached -> gold
                .key("abmiss", 7L, Map.of(7L, "blue"))              // ab v99 NOT cached -> full + metric
                .key("fullonly", 7L, Map.of(7L, "teal"))            // no ab hit -> full
                .versionOnly("neither", 5L, "candidate")            // no ab, no full release -> removed
                .key("emptyval", 3L, Map.of(3L, ""));               // full release value is ""
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("abhit", 9L, "abmiss", 99L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            Map<String, String> got = h.client.getAllConfigs(ctx, NS);

            assertEquals(
                    Map.of("abhit", "gold", "abmiss", "blue", "fullonly", "teal", "emptyval", ""),
                    got,
                    "get-all resolves each key like single-key; the no-value key is removed");
            assertFalse(got.containsKey("neither"),
                    "a key with no ab hit and no full release is removed, not defaulted");
            // Only the ab->full cache miss (abmiss) bumps the counter; the clean
            // hit (abhit) and the plain no-hits do not -> exactly 1.
            assertEquals(1L, h.client.metrics().abtestFallbackTotal(NS),
                    "only the ab-version-cache-miss key bumps the fallback counter");
            assertEquals(1, h.abtest.callsFor(NS), "one memoised RPC serves the whole ns");
        }
    }

    // ------------------------------------------------------------------
    // Testing Plan 9: config_flat_kv version 0 is a sentinel -> full release,
    // no fallback metric (mirrors the single-key non-zero gate).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("flat_kv version=0 entry: no fallback metric, resolves to full release")
    void flatKvVersionZeroFallsToFullNoMetric() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 0L)) // 0 is a sentinel, not a hit
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals(Map.of("color", "blue"), h.client.getAllConfigs(ctx, NS));
            assertEquals(0L, h.client.metrics().abtestFallbackTotal(NS),
                    "a 0-version sentinel is a no-hit, not a fallback");
        }
    }

    // ------------------------------------------------------------------
    // Accepted divergence #2 (design §2): a key present in config_flat_kv but
    // ABSENT from the snapshot is silently omitted — no fallback metric bump
    // (single-key getConfig on that key WOULD warn + bump; get-all must not).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("flat_kv key absent from snapshot: silently omitted, NO fallback metric")
    void flatKvKeyAbsentFromSnapshotSilentlyOmitted() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue")); // "ghost" is NOT in the snapshot
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("ghost", 5L)) // ab steers a key the cache never had
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            Map<String, String> got = h.client.getAllConfigs(ctx, NS);

            assertEquals(Map.of("color", "blue"), got,
                    "get-all iterates snapshot keys only; a flat_kv-only key never appears");
            assertFalse(got.containsKey("ghost"));
            assertEquals(0L, h.client.metrics().abtestFallbackTotal(NS),
                    "the snapshot-absent flat_kv key is a silent omission, NOT a fallback");
        }
    }

    // ------------------------------------------------------------------
    // Testing Plan 2: getAllConfigsDefault default-ns fallback + no-default error.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("getAllConfigsDefault resolves to the configured default ns")
    void getAllConfigsDefaultUsesDefaultNs() {
        NsCache cache = new NsCache(2, 2)
                .key("k", 4L, Map.of(4L, "vDefault"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .defaultNamespace(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of())
                .build()) {

            AbtestContext ctx = h.client.emptyAbtestContext();
            assertEquals(Map.of("k", "vDefault"), h.client.getAllConfigsDefault(ctx));
        }
    }

    @Test
    @DisplayName("getAllConfigsDefault with no default ns throws NamespaceRequiredException")
    void getAllConfigsDefaultNoDefaultNsThrows() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS) // no default namespace configured
                .snapshot(NS, new NsCache(1, 1).key("k", 4L, Map.of(4L, "v")))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertThrows(NamespaceRequiredException.class,
                    () -> h.client.getAllConfigsDefault(ctx));
        }
    }

    // ------------------------------------------------------------------
    // Testing Plan 3: error paths mirror getConfig.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("getAllConfigs on an unsubscribed ns throws NamespaceNotSubscribedException")
    void unsubscribedNsThrows() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(1, 1).key("k", 4L, Map.of(4L, "v")))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertThrows(NamespaceNotSubscribedException.class,
                    () -> h.client.getAllConfigs(ctx, "not-subscribed"));
        }
    }

    @Test
    @DisplayName("getAllConfigs with null abctx throws AbtestContextMissingException")
    void nullAbctxThrows() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(1, 1).key("k", 4L, Map.of(4L, "v")))
                .build()) {

            assertThrows(AbtestContextMissingException.class,
                    () -> h.client.getAllConfigs(null, NS));
        }
    }

    @Test
    @DisplayName("getAllConfigs after close throws SdkClosedException")
    void closedClientThrows() {
        AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(1, 1).key("k", 4L, Map.of(4L, "v")))
                .build();
        AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
        h.client.close();
        try {
            assertThrows(SdkClosedException.class, () -> h.client.getAllConfigs(ctx, NS));
        } finally {
            h.close();
        }
    }

    // ------------------------------------------------------------------
    // Testing Plan 4: subscribed but no snapshot -> empty map, zero RPC.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("subscribed ns with no snapshot yet: empty map, zero RPC, no error")
    void subscribedButNoSnapshotReturnsEmptyMap() {
        // NS is subscribed but NO snapshot is registered, so PullAll returns
        // nothing for it and the cache stays empty for NS.
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .abtestConfigFlatKv(NS, Map.of("color", 9L)) // would steer if consulted
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            Map<String, String> got = h.client.getAllConfigs(ctx, NS);
            assertTrue(got.isEmpty(), "no snapshot -> empty map");
            assertEquals(0, h.abtest.totalCalls.get(),
                    "get-all short-circuits before resultFor when there is no snapshot -> zero RPC");
        }
    }

    // ------------------------------------------------------------------
    // Testing Plan 5: at-most-once RPC across a getConfig + getAllConfigs mix.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("getConfig then getAllConfigs on the same ctx/ns issues exactly ONE RPC")
    void getConfigThenGetAllConfigsReuseOneRpc() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"));
            assertEquals(1, h.abtest.callsFor(NS), "the first getConfig opens the one RPC");

            assertEquals(Map.of("color", "gold"), h.client.getAllConfigs(ctx, NS),
                    "get-all reuses the memoised ab result and resolves the same value");
            assertEquals(1, h.abtest.callsFor(NS),
                    "get-all must reuse the memoised future -> still exactly one RPC");
            assertEquals(1, h.abtest.totalCalls.get());
        }
    }

    // ------------------------------------------------------------------
    // Testing Plan 8: mixed / all-false fast-path RPC counts.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("all keys has_dynamic_resolution=false: zero RPC, all full release")
    void allExplicitFalseIssuesZeroRpc() {
        NsCache cache = new NsCache(2, 2)
                .key("a", 1L, Map.of(1L, "x")).hasDynamicResolution("a", Boolean.FALSE)
                .key("b", 2L, Map.of(2L, "y")).hasDynamicResolution("b", Boolean.FALSE);
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("a", 9L, "b", 9L)) // would steer if consulted
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals(Map.of("a", "x", "b", "y"), h.client.getAllConfigs(ctx, NS));
            assertEquals(0, h.abtest.totalCalls.get(),
                    "every key is pure full-rollout -> get-all skips resultFor -> zero RPC");
        }
    }

    @Test
    @DisplayName("mixed fast-path: one non-false key forces exactly ONE RPC for the whole ns")
    void mixedFastPathIssuesOneRpc() {
        NsCache cache = new NsCache(2, 2)
                .key("static", 7L, Map.of(7L, "blue")).hasDynamicResolution("static", Boolean.FALSE)
                .key("dynamic", 3L, Map.of(3L, "full", 9L, "gold")); // absent flag -> always-wait
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("dynamic", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals(Map.of("static", "blue", "dynamic", "gold"),
                    h.client.getAllConfigs(ctx, NS));
            assertEquals(1, h.abtest.callsFor(NS),
                    "one non-false key means the ns still needs the abtest result -> one RPC");
            assertEquals(1, h.abtest.totalCalls.get());
        }
    }

    // ------------------------------------------------------------------
    // Testing Plan 7: the returned map is a fresh, mutable, independent copy.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("returned map is mutable and independent: mutating it never affects a later call")
    void returnedMapIsMutableAndIndependent() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue"))
                .key("size", 4L, Map.of(4L, "large"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of())
                .build()) {

            AbtestContext ctx = h.client.emptyAbtestContext();
            Map<String, String> first = h.client.getAllConfigs(ctx, NS);
            assertEquals(Map.of("color", "blue", "size", "large"), first);

            // Mutate freely: put, overwrite, remove, clear.
            first.put("injected", "z");
            first.put("color", "tampered");
            first.remove("size");
            first.clear();

            Map<String, String> second = h.client.getAllConfigs(ctx, NS);
            assertEquals(Map.of("color", "blue", "size", "large"), second,
                    "a fresh map is allocated per call; caller mutation cannot leak into the cache");
        }
    }

    @Test
    @DisplayName("returned empty map is also mutable (no immutable Map.of view)")
    void returnedEmptyMapIsMutable() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            Map<String, String> got = h.client.getAllConfigs(ctx, NS);
            assertTrue(got.isEmpty());
            // A HashMap accepts a put; an immutable view would throw here.
            got.put("k", "v");
            assertEquals(new HashMap<>(Map.of("k", "v")), got);
        }
    }
}
