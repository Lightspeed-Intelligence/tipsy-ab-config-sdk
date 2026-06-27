package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;

import io.github.lightspeedintelligence.abconfig.AbtestTestSupport.NsCache;
import java.util.Map;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * ST5 {@code has_dynamic_resolution} fast-path tests for
 * {@link TipsyAbConfigClient#getConfig}.
 *
 * <p>Semantics under test (design §3 + Acceptance Criteria 2):
 * <ul>
 *   <li><b>Explicit FALSE</b> (server reports the key as pure full-rollout):
 *       getConfig skips {@code resultFor} and therefore issues <b>ZERO</b>
 *       {@code GetExperimentResult} RPCs, returning the full-release value (or
 *       the default when the key has no full release). This is the core
 *       optimisation.</li>
 *   <li><b>Explicit TRUE</b> (key has gray-release / experiment): unchanged —
 *       still calls {@code resultFor}, issues exactly ONE RPC, and resolves the
 *       ab-hit value.</li>
 *   <li><b>Field ABSENT</b> (null, old server): safe default — still calls
 *       {@code resultFor}, issues exactly ONE RPC.</li>
 * </ul>
 *
 * <h2>RPC-count==0 mechanism</h2>
 * The {@link AbtestTestSupport.FakeAbtestService} increments a per-ns counter
 * ({@code callsFor(ns)}) and a global counter ({@code totalCalls}) inside its
 * {@code getExperimentResult} handler. A skipped {@code resultFor} never opens
 * the unary RPC, so the counter stays at 0. We assert BOTH {@code callsFor(NS)}
 * and {@code totalCalls} are 0 to prove no RPC was issued on any ns.
 *
 * <h2>Avoiding the memoization false-green (design §S5 caveat)</h2>
 * {@code resultFor} memoises per-ns per {@link AbtestContext}: the first query
 * for a ns issues the RPC, later queries in the same context reuse the future.
 * If a TRUE key for the same ns were queried first in the same context, the RPC
 * would already be in flight and a later FALSE-key query would observe a
 * non-zero count even though it correctly skipped — a false GREEN for the fast
 * path, or a false RED disguised. To make the count unambiguous each fast-path
 * test uses a <b>FRESH</b> {@code newAbtestContext} and queries <b>only the
 * one FALSE key</b> on the namespace, with NO prior true-key query in the same
 * link. The negative-control test below queries a TRUE key on a SEPARATE
 * context to show the same harness DOES count an RPC, proving the 0 is real.
 */
final class GetConfigFastPathTest {

    private static final String NS = "checkout";

    // ------------------------------------------------------------------
    // Core fast-path: explicit FALSE -> ZERO GetExperimentResult RPCs.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("has_dynamic_resolution=false: ZERO abtest RPC, returns full release")
    void explicitFalseSkipsRpcReturnsFullRelease() {
        // "color" is pure full-rollout (flag=false) with full release v7=blue.
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue"))
                .hasDynamicResolution("color", Boolean.FALSE);
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                // The ab server WOULD steer color -> v9 if asked; the fast-path
                // must skip the RPC so this steering is never consulted.
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            // FRESH context, ONLY the false-key queried on this ns/link: no prior
            // true-key query could have pre-triggered the per-ns memoised RPC.
            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "explicit-false key resolves to the full-release value");

            // The load-bearing assertion: resultFor was skipped -> no RPC at all.
            assertEquals(0, h.abtest.callsFor(NS),
                    "explicit false must skip resultFor -> ZERO GetExperimentResult RPC for the ns");
            assertEquals(0, h.abtest.totalCalls.get(),
                    "no RPC may be issued on ANY ns for a fast-path key");
        }
    }

    @Test
    @DisplayName("has_dynamic_resolution=false, no full release: ZERO abtest RPC, returns default")
    void explicitFalseNoFullReleaseReturnsDefault() {
        // "flag" has versions but NO active full release (full_release_version=0),
        // and is reported as pure full-rollout (false).
        NsCache cache = new NsCache(2, 2)
                .versionOnly("flag", 5L, "candidate")
                .hasDynamicResolution("flag", Boolean.FALSE);
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("flag", 5L)) // would steer if asked
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("DEF", h.client.getConfig(ctx, NS, "flag", "DEF"),
                    "explicit-false key with no full release -> default (fallback semantics unchanged)");

            assertEquals(0, h.abtest.callsFor(NS),
                    "explicit false must skip resultFor even when it ends up returning the default");
            assertEquals(0, h.abtest.totalCalls.get());
        }
    }

    @Test
    @DisplayName("fast-path does not poison the per-ns link: a later TRUE key on the SAME ctx still issues its RPC")
    void falseKeyDoesNotSuppressLaterTrueKeyRpcOnSameContext() {
        // Two keys on one ns: "static" is pure full-rollout (false), "dynamic"
        // needs abtest (true). Querying the false key first must NOT pre-trigger
        // (or suppress) the per-ns memoised RPC; the true key must still drive it.
        NsCache cache = new NsCache(2, 2)
                .key("static", 7L, Map.of(7L, "blue")).hasDynamicResolution("static", Boolean.FALSE)
                .key("dynamic", 3L, Map.of(3L, "full", 9L, "gold")).hasDynamicResolution("dynamic", Boolean.TRUE);
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("dynamic", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());

            // Query the FALSE key first: must be a no-RPC fast-path.
            assertEquals("blue", h.client.getConfig(ctx, NS, "static", "DEF"));
            assertEquals(0, h.abtest.callsFor(NS),
                    "the false key must not have opened the per-ns RPC");

            // Now the TRUE key on the SAME context: it must open the (single) RPC
            // and resolve the ab-hit value.
            assertEquals("gold", h.client.getConfig(ctx, NS, "dynamic", "DEF"));
            assertEquals(1, h.abtest.callsFor(NS),
                    "the true key drives exactly one RPC; the prior false key neither pre-triggered nor suppressed it");
            assertEquals(1, h.abtest.totalCalls.get());
        }
    }

    // ------------------------------------------------------------------
    // No-regression: explicit TRUE -> still calls resultFor (count == 1).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("has_dynamic_resolution=true: still calls resultFor, exactly ONE RPC, resolves ab-hit value")
    void explicitTrueStillIssuesRpcAndResolvesAbHit() {
        // "color": full release v7=blue, ab version v9=gold; flag=true.
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"))
                .hasDynamicResolution("color", Boolean.TRUE);
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "true key must still resolve the ab-hit value");

            assertEquals(1, h.abtest.callsFor(NS),
                    "true key must NOT skip resultFor -> exactly one RPC");
            assertEquals(1, h.abtest.totalCalls.get());
        }
    }

    // ------------------------------------------------------------------
    // No-regression: field ABSENT (null, old server) -> safe default path.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("has_dynamic_resolution absent (old server): still calls resultFor, exactly ONE RPC")
    void absentFieldStillIssuesRpc() {
        // "color" carries NO has_dynamic_resolution flag (null) -> safe default:
        // keep the always-wait path so an old server never mis-skips.
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        // (no .hasDynamicResolution(...) call -> field absent -> null)
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "absent flag keeps the always-wait path and resolves the ab hit");

            assertEquals(1, h.abtest.callsFor(NS),
                    "absent flag (null) must NOT skip resultFor -> exactly one RPC (safe default)");
            assertEquals(1, h.abtest.totalCalls.get());
        }
    }

    @Test
    @DisplayName("absent flag, no ab hit: still issues the RPC, then falls through to full release")
    void absentFieldNoAbHitStillIssuesRpcThenFullRelease() {
        // Old-server semantics with a plain no-hit: the RPC is still issued (the
        // safety guarantee), then the no-hit falls through to full release.
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of()) // no ab hits
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"));
            assertEquals(1, h.abtest.callsFor(NS),
                    "absent flag must still issue the abtest RPC even on a no-hit");
            assertEquals(1, h.abtest.totalCalls.get());
        }
    }
}
