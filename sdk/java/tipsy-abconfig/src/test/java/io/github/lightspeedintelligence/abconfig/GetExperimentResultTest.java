package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.github.lightspeedintelligence.abconfig.AbtestTestSupport.NsCache;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ExperimentGroupResult;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.Value;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * {@link TipsyAbConfigClient#getExperimentResult} tests (design 05
 * §GetExperimentResult; covers point 9, plus the wire-encode transparency of
 * point 10 and the trace_id propagation of point 11).
 *
 * <p>Contract under test: ns resolution mirrors getConfig; abtest unconfigured
 * throws; layerIds/type/displayType/user_attrs/trace_id all pass through to the
 * server verbatim; the raw proto response is returned unchanged; and the call is
 * NOT memoised (two calls -> two RPCs).
 */
final class GetExperimentResultTest {

    private static final String NS = "checkout";

    private static AbtestTestSupport.Builder base() {
        return AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(1, 1).key("k", 4L, Map.of(4L, "v")));
    }

    // ------------------------------------------------------------------
    // Point 9: abtest unconfigured -> throw.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("getExperimentResult with no AbtestService configured throws")
    void abtestUnconfiguredThrows() {
        try (AbtestTestSupport h = base().withoutAbtest().build()) {
            ExperimentResultRequest req = ExperimentResultRequest.builder()
                    .namespace(NS).userInfo("u-1", Map.of()).build();
            assertThrows(TipsyConfigException.class, () -> h.client.getExperimentResult(req));
        }
    }

    // ------------------------------------------------------------------
    // Point 9: unsubscribed ns -> throw (resolution mirrors getConfig).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("getExperimentResult on an unsubscribed ns throws NamespaceNotSubscribedException")
    void unsubscribedNsThrows() {
        try (AbtestTestSupport h = base().build()) {
            ExperimentResultRequest req = ExperimentResultRequest.builder()
                    .namespace("nope").userInfo("u-1", Map.of()).build();
            assertThrows(NamespaceNotSubscribedException.class,
                    () -> h.client.getExperimentResult(req));
        }
    }

    @Test
    @DisplayName("getExperimentResult with no ns and no default ns throws NamespaceRequiredException")
    void noNsNoDefaultThrows() {
        try (AbtestTestSupport h = base().build()) {
            ExperimentResultRequest req = ExperimentResultRequest.builder()
                    .userInfo("u-1", Map.of()).build();
            assertThrows(NamespaceRequiredException.class,
                    () -> h.client.getExperimentResult(req));
        }
    }

    @Test
    @DisplayName("getExperimentResult after close throws SdkClosedException")
    void closedClientThrows() {
        AbtestTestSupport h = base().build();
        h.client.close();
        try {
            ExperimentResultRequest req = ExperimentResultRequest.builder()
                    .namespace(NS).userInfo("u-1", Map.of()).build();
            assertThrows(SdkClosedException.class, () -> h.client.getExperimentResult(req));
        } finally {
            h.close();
        }
    }

    // ------------------------------------------------------------------
    // Point 9: layerIds / type / displayType pass through to the server.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("layerIds / type / displayType are forwarded verbatim to the server")
    void requestParamsForwardedToServer() {
        try (AbtestTestSupport h = base().build()) {
            ExperimentResultRequest req = ExperimentResultRequest.builder()
                    .namespace(NS)
                    .userInfo("u-9", Map.of())
                    .layerIds(List.of("layerA", "layerB"))
                    .type(io.github.lightspeedintelligence.abconfig.ExperimentType.ALL)
                    .displayType(io.github.lightspeedintelligence.abconfig.ResultDisplayType.EACH_EXPERIMENT_GROUP)
                    .traceId("tr-1")
                    .build();

            h.client.getExperimentResult(req);

            GetExperimentResultRequest seen = h.abtest.requests.peek();
            assertTrue(seen != null, "the server must have received a request");
            assertEquals(NS, seen.getNamespace());
            assertEquals("u-9", seen.getUserId());
            assertEquals(List.of("layerA", "layerB"), seen.getLayerIdsList());
            assertEquals(io.github.lightspeedintelligence.abconfig.ExperimentType.ALL.wireValue(),
                    seen.getExperimentTypeValue(), "experiment_type passes through");
            assertEquals(io.github.lightspeedintelligence.abconfig.ResultDisplayType.EACH_EXPERIMENT_GROUP.wireValue(),
                    seen.getDisplayTypeValue(), "display_type passes through");
            assertEquals("tr-1", seen.getTraceId(), "explicit trace_id passes through");
        }
    }

    // ------------------------------------------------------------------
    // Point 9: raw proto response is returned unchanged (groups + config_flat_kv).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("the raw proto response is returned verbatim (groups + config_flat_kv readable)")
    void rawResponseReturnedVerbatim() {
        GetExperimentResultResponse canned = GetExperimentResultResponse.newBuilder()
                .putConfigFlatKv("layout", 12L)
                .addGroups(ExperimentGroupResult.newBuilder()
                        .setLayerId("L1").setExperimentId("E1").setGroupId("G1"))
                .build();

        try (AbtestTestSupport h = base().build()) {
            h.abtest.setFullResponse(NS, canned);

            ExperimentResultRequest req = ExperimentResultRequest.builder()
                    .namespace(NS).userInfo("u-1", Map.of()).build();
            GetExperimentResultResponse resp = h.client.getExperimentResult(req);

            assertEquals(12L, resp.getConfigFlatKvMap().get("layout"));
            assertEquals(1, resp.getGroupsCount());
            assertEquals("L1", resp.getGroups(0).getLayerId());
            assertEquals("E1", resp.getGroups(0).getExperimentId());
            assertEquals("G1", resp.getGroups(0).getGroupId());
        }
    }

    // ------------------------------------------------------------------
    // Point 9: NOT memoised — two calls issue two RPCs.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("getExperimentResult is NOT memoised: two calls -> two RPCs")
    void notMemoisedTwoCallsTwoRpcs() {
        try (AbtestTestSupport h = base().build()) {
            ExperimentResultRequest req = ExperimentResultRequest.builder()
                    .namespace(NS).userInfo("u-1", Map.of()).build();
            h.client.getExperimentResult(req);
            h.client.getExperimentResult(req);
            assertEquals(2, h.abtest.callsFor(NS),
                    "getExperimentResult does not memoise; each call hits the wire");
        }
    }

    // ------------------------------------------------------------------
    // Point 10 (wire transparency): user_attrs encoding observed at the server.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("user_attrs are encoded per type at the wire; unsupported keys are dropped")
    void userAttrsEncodedOnWire() {
        Map<String, Object> attrs = new LinkedHashMap<>();
        attrs.put("country", "FR");      // s
        attrs.put("vip", Boolean.TRUE);  // b (before Number)
        attrs.put("age", 30);            // i
        attrs.put("score", 4.5d);        // d
        attrs.put("bad", new Object());  // dropped

        try (AbtestTestSupport h = base().build()) {
            ExperimentResultRequest req = ExperimentResultRequest.builder()
                    .namespace(NS).userInfo("u-1", attrs).build();
            h.client.getExperimentResult(req);

            GetExperimentResultRequest seen = h.abtest.requests.peek();
            assertTrue(seen != null);
            Map<String, Value> ua = seen.getUserAttrsMap();
            assertEquals(4, ua.size(), "the unsupported attr must be dropped on the wire");
            assertFalse(ua.containsKey("bad"));

            assertEquals(Value.VCase.S, ua.get("country").getVCase());
            assertEquals("FR", ua.get("country").getS());
            assertEquals(Value.VCase.B, ua.get("vip").getVCase());
            assertTrue(ua.get("vip").getB());
            assertEquals(Value.VCase.I, ua.get("age").getVCase());
            assertEquals(30L, ua.get("age").getI());
            assertEquals(Value.VCase.D, ua.get("score").getVCase());
            assertEquals(4.5d, ua.get("score").getD(), 0.0);
        }
    }

    // ------------------------------------------------------------------
    // Point 11: empty trace_id -> a UUID is generated and sent on the wire.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("empty trace_id on the request -> SDK generates a non-empty trace_id on the wire")
    void emptyTraceIdGeneratedOnWire() {
        try (AbtestTestSupport h = base().build()) {
            ExperimentResultRequest req = ExperimentResultRequest.builder()
                    .namespace(NS).userInfo("u-1", Map.of()).build(); // no traceId
            h.client.getExperimentResult(req);

            GetExperimentResultRequest seen = h.abtest.requests.peek();
            assertTrue(seen != null);
            assertFalse(seen.getTraceId().isEmpty(),
                    "an empty request trace_id must be replaced with a generated UUID");
        }
    }
}
