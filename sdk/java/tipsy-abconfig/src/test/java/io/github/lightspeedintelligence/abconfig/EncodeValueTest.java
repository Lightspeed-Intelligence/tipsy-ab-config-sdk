package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.Value;
import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.Map;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * White-box unit tests for {@link AbtestContext#encodeValue(Object)} /
 * {@link AbtestContext#encodeUserAttrs(Map, org.slf4j.Logger)} (design 05
 * §encodeUserAttrs/encodeValue, covers point 10).
 *
 * <p>Type mapping under test (the Boolean branch MUST precede the Number branch
 * so a {@code Boolean} never resolves to {@code i}):
 * <ul>
 *   <li>{@code String}        -> {@code s}</li>
 *   <li>{@code Boolean}       -> {@code b}</li>
 *   <li>{@code Integer/Long/Short/Byte} -> {@code i} (as long)</li>
 *   <li>{@code Float/Double}  -> {@code d} (as double)</li>
 *   <li>everything else       -> dropped (null from encodeValue; absent from map)</li>
 * </ul>
 */
final class EncodeValueTest {

    // ------------------------------------------------------------------
    // encodeValue: per-type oneof selection.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("String -> s oneof")
    void stringEncodesToS() {
        Value v = AbtestContext.encodeValue("hello");
        assertEquals(Value.VCase.S, v.getVCase());
        assertEquals("hello", v.getS());
    }

    @Test
    @DisplayName("empty String is still a valid s value (existence over isEmpty)")
    void emptyStringEncodesToS() {
        Value v = AbtestContext.encodeValue("");
        assertEquals(Value.VCase.S, v.getVCase());
        assertEquals("", v.getS());
    }

    @Test
    @DisplayName("Boolean -> b oneof (checked before Number, so it never becomes i)")
    void booleanEncodesToB() {
        Value vt = AbtestContext.encodeValue(Boolean.TRUE);
        assertEquals(Value.VCase.B, vt.getVCase(), "Boolean must map to b, not i");
        assertTrue(vt.getB());

        Value vf = AbtestContext.encodeValue(Boolean.FALSE);
        assertEquals(Value.VCase.B, vf.getVCase());
        assertFalse(vf.getB());
    }

    @Test
    @DisplayName("Integer/Long/Short/Byte -> i oneof (as long)")
    void integralTypesEncodeToI() {
        assertEquals(Value.VCase.I, AbtestContext.encodeValue(42).getVCase());
        assertEquals(42L, AbtestContext.encodeValue(42).getI());

        assertEquals(Value.VCase.I, AbtestContext.encodeValue(9_000_000_000L).getVCase());
        assertEquals(9_000_000_000L, AbtestContext.encodeValue(9_000_000_000L).getI());

        assertEquals(Value.VCase.I, AbtestContext.encodeValue((short) 7).getVCase());
        assertEquals(7L, AbtestContext.encodeValue((short) 7).getI());

        assertEquals(Value.VCase.I, AbtestContext.encodeValue((byte) 3).getVCase());
        assertEquals(3L, AbtestContext.encodeValue((byte) 3).getI());
    }

    @Test
    @DisplayName("Float/Double -> d oneof (as double)")
    void floatingTypesEncodeToD() {
        Value vf = AbtestContext.encodeValue(1.5f);
        assertEquals(Value.VCase.D, vf.getVCase());
        assertEquals(1.5d, vf.getD(), 0.0);

        Value vd = AbtestContext.encodeValue(2.25d);
        assertEquals(Value.VCase.D, vd.getVCase());
        assertEquals(2.25d, vd.getD(), 0.0);
    }

    @Test
    @DisplayName("unsupported types -> null (dropped)")
    void unsupportedTypesEncodeToNull() {
        assertNull(AbtestContext.encodeValue(new Object()));
        assertNull(AbtestContext.encodeValue(java.util.List.of(1, 2, 3)));
        assertNull(AbtestContext.encodeValue(new int[] {1, 2}));
        assertNull(AbtestContext.encodeValue('c'), "char is not a supported scalar");
        assertNull(AbtestContext.encodeValue(null), "null value is unsupported");
    }

    // ------------------------------------------------------------------
    // encodeUserAttrs: map-level behaviour (drop unsupported, empty inputs).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("null / empty attrs -> empty map")
    void nullOrEmptyAttrsYieldEmptyMap() {
        assertTrue(AbtestContext.encodeUserAttrs(null, null).isEmpty());
        assertTrue(AbtestContext.encodeUserAttrs(new HashMap<>(), null).isEmpty());
    }

    @Test
    @DisplayName("mixed attrs: supported entries encoded, unsupported dropped (key absent)")
    void mixedAttrsEncodeAndDrop() {
        Map<String, Object> attrs = new LinkedHashMap<>();
        attrs.put("country", "FR");
        attrs.put("vip", Boolean.TRUE);
        attrs.put("age", 30);
        attrs.put("ratio", 0.75d);
        attrs.put("bad", new Object()); // unsupported -> dropped

        Map<String, Value> out = AbtestContext.encodeUserAttrs(attrs, null);

        assertEquals(4, out.size(), "the unsupported entry must be dropped");
        assertFalse(out.containsKey("bad"), "unsupported key must be absent from the encoded map");

        assertEquals(Value.VCase.S, out.get("country").getVCase());
        assertEquals("FR", out.get("country").getS());
        assertEquals(Value.VCase.B, out.get("vip").getVCase());
        assertTrue(out.get("vip").getB());
        assertEquals(Value.VCase.I, out.get("age").getVCase());
        assertEquals(30L, out.get("age").getI());
        assertEquals(Value.VCase.D, out.get("ratio").getVCase());
        assertEquals(0.75d, out.get("ratio").getD(), 0.0);
    }
}
