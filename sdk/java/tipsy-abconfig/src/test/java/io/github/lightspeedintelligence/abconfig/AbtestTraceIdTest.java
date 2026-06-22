package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.github.lightspeedintelligence.abconfig.AbtestTestSupport.NsCache;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;
import java.time.Duration;
import java.util.Map;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * trace_id propagation tests for the abtest layer (design 05 §trace_id 策略;
 * covers point 11).
 *
 * <p>Contract: an empty trace_id at construction yields a fresh UUID (always
 * non-empty); an explicit trace_id is propagated verbatim to every
 * GetExperimentResult RPC the context issues; empty / mock contexts also carry a
 * non-empty trace_id for downstream-log consistency.
 */
final class AbtestTraceIdTest {

    private static final String NS = "checkout";
    private static final Duration WAIT = Duration.ofSeconds(5);

    private static AbtestTestSupport.Builder base() {
        return AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("color", 7L, Map.of(7L, "blue", 9L, "gold")))
                .abtestConfigFlatKv(NS, Map.of("color", 9L));
    }

    @Test
    @DisplayName("empty trace_id at construction -> non-empty generated UUID on the context")
    void emptyTraceIdGeneratesUuid() {
        try (AbtestTestSupport h = base().build()) {
            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertFalse(ctx.traceId().isEmpty(), "an empty trace_id must be replaced with a UUID");

            AbtestContext ctx2 = h.client.newAbtestContext("u-1", Map.of());
            assertNotEquals(ctx.traceId(), ctx2.traceId(),
                    "two contexts get distinct generated trace ids");
        }
    }

    @Test
    @DisplayName("explicit trace_id is propagated verbatim to the GetExperimentResult RPC")
    void explicitTraceIdPropagatedToRpc() {
        try (AbtestTestSupport h = base().build()) {
            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of(), "trace-explicit-123");
            assertEquals("trace-explicit-123", ctx.traceId());

            // Trigger the lazy per-ns RPC and inspect the trace_id the server saw.
            h.client.getConfig(ctx, NS, "color", "DEF");
            assertTrue(AbtestTestSupport.awaitTrue(() -> !h.abtest.requests.isEmpty(), WAIT));

            GetExperimentResultRequest seen = h.abtest.requests.peek();
            assertEquals("trace-explicit-123", seen.getTraceId(),
                    "the context trace_id must travel on the RPC");
        }
    }

    @Test
    @DisplayName("generated trace_id (empty at construction) also travels on the RPC")
    void generatedTraceIdPropagatedToRpc() {
        try (AbtestTestSupport h = base().build()) {
            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            String generated = ctx.traceId();
            assertFalse(generated.isEmpty());

            h.client.getConfig(ctx, NS, "color", "DEF");
            assertTrue(AbtestTestSupport.awaitTrue(() -> !h.abtest.requests.isEmpty(), WAIT));

            GetExperimentResultRequest seen = h.abtest.requests.peek();
            assertEquals(generated, seen.getTraceId(),
                    "the generated context trace_id must travel on the RPC");
        }
    }

    @Test
    @DisplayName("empty and mock contexts carry a non-empty trace_id")
    void emptyAndMockContextsHaveTraceId() {
        try (AbtestTestSupport h = base().build()) {
            AbtestContext empty = h.client.emptyAbtestContext();
            assertFalse(empty.traceId().isEmpty(), "empty ctx must have a generated trace_id");

            AbtestContext mock = h.client.mockAbtestContext("u-1",
                    Map.of(NS, Map.of("color", 9L)));
            assertFalse(mock.traceId().isEmpty(), "mock ctx must have a generated trace_id");
        }
    }

    @Test
    @DisplayName("newAbtestContextForNamespace with explicit trace_id propagates on the eager RPC")
    void forNamespaceExplicitTraceIdPropagates() {
        try (AbtestTestSupport h = base().build()) {
            AbtestContext ctx = h.client.newAbtestContextForNamespace(
                    NS, "u-1", Map.of(), "trace-ns-456");
            assertEquals("trace-ns-456", ctx.traceId());

            // The eager prefetch RPC should carry the trace id.
            assertTrue(AbtestTestSupport.awaitTrue(() -> !h.abtest.requests.isEmpty(), WAIT),
                    "eager prefetch should issue the RPC");
            GetExperimentResultRequest seen = h.abtest.requests.peek();
            assertEquals("trace-ns-456", seen.getTraceId());
        }
    }
}
