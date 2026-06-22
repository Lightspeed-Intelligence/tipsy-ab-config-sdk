package io.tipsy.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.tipsy.abconfig.proto.abtest.v1.ExperimentType;
import io.tipsy.abconfig.proto.abtest.v1.ResultDisplayType;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * Pure unit tests for the SDK-stable enums, {@link UserInfo}, and
 * {@link ExperimentResultRequest} builder (design 03 §enums / UserInfo /
 * ExperimentResultRequest; covers points 12 and 13). No client / RPC needed.
 */
final class EnumsAndValueTypesTest {

    // ------------------------------------------------------------------
    // Point 13: enum wireValue() corresponds to the proto enum numbers.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("ExperimentType wireValue() matches proto numbers")
    void experimentTypeWireValuesMatchProto() {
        assertEquals(0, io.tipsy.abconfig.ExperimentType.UNSPECIFIED.wireValue());
        assertEquals(1, io.tipsy.abconfig.ExperimentType.CONFIG_VERSION.wireValue());
        assertEquals(2, io.tipsy.abconfig.ExperimentType.CUSTOM_PARAMS.wireValue());
        assertEquals(3, io.tipsy.abconfig.ExperimentType.ALL.wireValue());

        assertEquals(ExperimentType.EXPERIMENT_TYPE_UNSPECIFIED.getNumber(),
                io.tipsy.abconfig.ExperimentType.UNSPECIFIED.wireValue());
        assertEquals(ExperimentType.EXPERIMENT_TYPE_CONFIG_VERSION.getNumber(),
                io.tipsy.abconfig.ExperimentType.CONFIG_VERSION.wireValue());
        assertEquals(ExperimentType.EXPERIMENT_TYPE_CUSTOM_PARAMS.getNumber(),
                io.tipsy.abconfig.ExperimentType.CUSTOM_PARAMS.wireValue());
        assertEquals(ExperimentType.EXPERIMENT_TYPE_ALL.getNumber(),
                io.tipsy.abconfig.ExperimentType.ALL.wireValue());
    }

    @Test
    @DisplayName("ResultDisplayType wireValue() matches proto numbers")
    void resultDisplayTypeWireValuesMatchProto() {
        assertEquals(0, io.tipsy.abconfig.ResultDisplayType.UNSPECIFIED.wireValue());
        assertEquals(1, io.tipsy.abconfig.ResultDisplayType.FLAT_KV.wireValue());
        assertEquals(2, io.tipsy.abconfig.ResultDisplayType.EACH_EXPERIMENT_GROUP.wireValue());

        assertEquals(ResultDisplayType.RESULT_DISPLAY_TYPE_UNSPECIFIED.getNumber(),
                io.tipsy.abconfig.ResultDisplayType.UNSPECIFIED.wireValue());
        assertEquals(ResultDisplayType.RESULT_DISPLAY_TYPE_FLAT_KV.getNumber(),
                io.tipsy.abconfig.ResultDisplayType.FLAT_KV.wireValue());
        assertEquals(ResultDisplayType.RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP.getNumber(),
                io.tipsy.abconfig.ResultDisplayType.EACH_EXPERIMENT_GROUP.wireValue());
    }

    // ------------------------------------------------------------------
    // Point 12: UserInfo read-only view.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("UserInfo exposes uid + read-only attrs; null attrs -> empty map")
    void userInfoExposesUidAndReadOnlyAttrs() {
        Map<String, Object> attrs = new LinkedHashMap<>();
        attrs.put("k", "v");
        UserInfo ui = new UserInfo("u-1", attrs);
        assertEquals("u-1", ui.uid());
        assertEquals("v", ui.attrs().get("k"));

        assertThrows(UnsupportedOperationException.class, () -> ui.attrs().put("x", "y"),
                "attrs() must be an unmodifiable view");

        UserInfo nullAttrs = new UserInfo("u-2", null);
        assertTrue(nullAttrs.attrs().isEmpty(), "null attrs -> empty map (never null)");
        assertEquals("u-2", nullAttrs.uid());

        UserInfo nullUid = new UserInfo(null, null);
        assertEquals("", nullUid.uid(), "null uid -> empty string (never null)");
    }

    // ------------------------------------------------------------------
    // ExperimentResultRequest builder defaults + setters.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("ExperimentResultRequest builder defaults are the documented zero values")
    void requestBuilderDefaults() {
        ExperimentResultRequest req = ExperimentResultRequest.builder().build();
        assertEquals("", req.namespace());
        assertEquals("", req.userInfo().uid());
        assertTrue(req.userInfo().attrs().isEmpty());
        assertTrue(req.layerIds().isEmpty());
        assertEquals(io.tipsy.abconfig.ExperimentType.UNSPECIFIED, req.type());
        assertEquals(io.tipsy.abconfig.ResultDisplayType.UNSPECIFIED, req.displayType());
        assertEquals("", req.traceId());
    }

    @Test
    @DisplayName("ExperimentResultRequest builder retains all set fields (userInfo(uid,attrs) overload)")
    void requestBuilderRetainsFields() {
        Map<String, Object> attrs = Map.of("country", "FR");
        ExperimentResultRequest req = ExperimentResultRequest.builder()
                .namespace("checkout")
                .userInfo("u-7", attrs)
                .layerIds(List.of("L1", "L2"))
                .type(io.tipsy.abconfig.ExperimentType.ALL)
                .displayType(io.tipsy.abconfig.ResultDisplayType.EACH_EXPERIMENT_GROUP)
                .traceId("trace-xyz")
                .build();

        assertEquals("checkout", req.namespace());
        assertEquals("u-7", req.userInfo().uid());
        assertEquals("FR", req.userInfo().attrs().get("country"));
        assertEquals(List.of("L1", "L2"), req.layerIds());
        assertSame(io.tipsy.abconfig.ExperimentType.ALL, req.type());
        assertSame(io.tipsy.abconfig.ResultDisplayType.EACH_EXPERIMENT_GROUP, req.displayType());
        assertEquals("trace-xyz", req.traceId());
    }

    @Test
    @DisplayName("ExperimentResultRequest layerIds is an immutable copy")
    void requestLayerIdsImmutable() {
        ExperimentResultRequest req = ExperimentResultRequest.builder()
                .layerIds(List.of("L1"))
                .build();
        assertThrows(UnsupportedOperationException.class, () -> req.layerIds().add("L2"));
    }
}
