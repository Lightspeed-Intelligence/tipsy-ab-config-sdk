package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;

import io.github.lightspeedintelligence.abconfig.AbtestTestSupport.NsCache;
import java.util.Map;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * uid empty-value static short-circuit (design §1 + Testing Plan 6).
 *
 * <p>When a context is built via {@code newAbtestContext} with a uid of
 * {@code ""}, {@code "0"} or {@code null} (the constructor normalises null to
 * ""), the lazy-fetch layer resolves every not-yet-resolved ns to the empty
 * result WITHOUT a {@code GetExperimentResult} RPC — the same short-circuit as
 * {@code emptyAbtestContext}. So getConfig, getAllConfigs and prefetch all issue
 * zero RPC and return pure full-release resolution. uid {@code "1"} is a real
 * identity and keeps the one-RPC path (regression). A pre-seeded
 * {@code mockAbtestContext("", ...)} still wins because the short-circuit only
 * governs the not-yet-resolved lazy path.
 */
final class NoUserUidShortcutTest {

    private static final String NS = "checkout";

    /** Cache: full release v7=blue, ab v9=gold; ab would steer color->v9 if asked. */
    private static NsCache steeredCache() {
        return new NsCache(2, 2).key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
    }

    // ------------------------------------------------------------------
    // uid "" -> zero RPC, pure full release (getConfig + getAllConfigs).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("uid \"\": getConfig resolves full release with ZERO RPC")
    void emptyUidGetConfigZeroRpc() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, steeredCache())
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("", Map.of());
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "empty uid skips abtest -> full release, never the steered ab value");
            assertEquals(0, h.abtest.totalCalls.get(),
                    "empty uid must issue ZERO GetExperimentResult RPC");
        }
    }

    @Test
    @DisplayName("uid \"\": getAllConfigs resolves pure full release with ZERO RPC")
    void emptyUidGetAllConfigsZeroRpc() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, steeredCache())
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("", Map.of());
            assertEquals(Map.of("color", "blue"), h.client.getAllConfigs(ctx, NS),
                    "empty uid -> every key resolves to full release");
            assertEquals(0, h.abtest.totalCalls.get(),
                    "empty uid get-all must issue ZERO RPC");
        }
    }

    // ------------------------------------------------------------------
    // uid "0" -> same short-circuit.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("uid \"0\": getConfig + getAllConfigs resolve full release with ZERO RPC")
    void zeroStringUidZeroRpc() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, steeredCache())
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("0", Map.of());
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"));
            assertEquals(Map.of("color", "blue"), h.client.getAllConfigs(ctx, NS));
            assertEquals(0, h.abtest.totalCalls.get(),
                    "uid \"0\" is a no-user sentinel -> ZERO RPC");
        }
    }

    // ------------------------------------------------------------------
    // null uid -> constructor normalises to "" -> short-circuit.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("uid null (normalised to \"\"): getConfig + getAllConfigs ZERO RPC")
    void nullUidNormalisesAndShortCircuits() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, steeredCache())
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext(null, Map.of());
            assertEquals("", ctx.experimentHashId(), "null uid is normalised to the empty string");
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"));
            assertEquals(Map.of("color", "blue"), h.client.getAllConfigs(ctx, NS));
            assertEquals(0, h.abtest.totalCalls.get(),
                    "null uid takes the no-user short-circuit -> ZERO RPC");
        }
    }

    // ------------------------------------------------------------------
    // uid "1" -> real identity, unchanged one-RPC path (regression).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("uid \"1\": real identity, still issues exactly ONE RPC and resolves the ab hit")
    void nonEmptyUidStillIssuesRpc() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, steeredCache())
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("1", Map.of());
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "uid \"1\" is a real identity -> the ab hit still resolves");
            assertEquals(Map.of("color", "gold"), h.client.getAllConfigs(ctx, NS));
            assertEquals(1, h.abtest.callsFor(NS),
                    "a real uid keeps the one-RPC path; get-all reuses the same memoised result");
            assertEquals(1, h.abtest.totalCalls.get());
        }
    }

    // ------------------------------------------------------------------
    // prefetch under the empty uid short-circuits inside ensureFetch.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("uid \"\": prefetch short-circuits inside ensureFetch -> ZERO RPC")
    void emptyUidPrefetchZeroRpc() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, steeredCache())
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("", Map.of());
            ctx.prefetchConfigVersionFlatKvForNamespace(NS);

            // A subsequent getConfig must still be RPC-free and full-release.
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"));
            assertEquals(0, h.abtest.totalCalls.get(),
                    "empty-uid prefetch must never open the abtest RPC");
        }
    }

    // ------------------------------------------------------------------
    // Pre-seeded mock("") still wins: the short-circuit only governs the
    // not-yet-resolved lazy path, not pre-seeded results.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("mockAbtestContext(\"\", seeded): pre-seeded result still resolves, no RPC")
    void mockEmptyUidPreSeededStillResolves() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 7L)) // real server would steer differently
                .build()) {

            // Empty uid AND a pre-seeded ab result for NS -> the seed wins.
            AbtestContext ctx = h.client.mockAbtestContext("", Map.of(NS, Map.of("color", 9L)));
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "a pre-seeded mock result resolves even when uid is empty");
            assertEquals(Map.of("color", "gold"), h.client.getAllConfigs(ctx, NS),
                    "get-all also honours the pre-seeded mock result");
            assertEquals(0, h.abtest.totalCalls.get(),
                    "mock ctx never calls the AbtestService regardless of uid");
        }
    }
}
