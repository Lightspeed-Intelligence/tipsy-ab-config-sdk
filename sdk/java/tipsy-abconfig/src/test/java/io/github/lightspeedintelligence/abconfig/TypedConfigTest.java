package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;

import com.google.gson.reflect.TypeToken;
import io.github.lightspeedintelligence.abconfig.AbtestTestSupport.NsCache;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * Typed config accessors (bool / long / double / string / json), static and
 * dynamic. Mirrors the Go SDK's {@code typed_config.go} semantics but with the
 * Java idiom the user chose: both static and dynamic forms take a typed default
 * and return the primitive/typed value; a miss OR a parse failure yields the
 * default and never throws for a bad value.
 *
 * <p>Static cases reuse the pure-cache-read harness ({@link
 * InProcessConfigServiceHarness}, as in {@code GetConfigStaticTest}); dynamic
 * cases reuse the abtest harness ({@link AbtestTestSupport}, as in
 * {@code GetConfigResolutionTest}) so they exercise the real
 * abtest-hit / full-release / default resolution.
 */
final class TypedConfigTest {

    private static final String NS = "checkout";

    /** A tiny POJO for the JSON round-trip. Public no-arg ctor for Gson. */
    static final class Widget {
        String name;
        int size;
        boolean on;

        Widget() {}
    }

    // ==================================================================
    // Static typed accessors (wrap getConfigStatic).
    // ==================================================================

    private InProcessConfigServiceHarness staticHarness;
    private TipsyAbConfigClient staticClient;

    @BeforeEach
    void setUp() throws Exception {
        staticHarness = new InProcessConfigServiceHarness();
    }

    @AfterEach
    void tearDown() {
        if (staticClient != null) {
            staticClient.close();
        }
        if (staticHarness != null) {
            staticHarness.close();
        }
    }

    /**
     * Builds a static client whose "checkout" cache holds a spread of typed
     * string values plus deliberately unparseable ones.
     */
    private TipsyAbConfigClient buildStaticClient() {
        staticHarness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshotWithKeys(NS, 2, 1, Map.ofEntries(
                        Map.entry("flag_true", Map.entry(1L, "true")),
                        Map.entry("flag_false", Map.entry(2L, "false")),
                        Map.entry("count", Map.entry(3L, "42")),
                        Map.entry("big", Map.entry(4L, "9007199254740993")),   // > 2^53
                        Map.entry("maxlong", Map.entry(5L, "9223372036854775807")),
                        Map.entry("ratio", Map.entry(6L, "3.14")),
                        Map.entry("raw", Map.entry(7L, "hello")),
                        Map.entry("empty", Map.entry(8L, "")),                  // valid empty hit
                        Map.entry("bad_long", Map.entry(9L, "abc")),
                        Map.entry("bad_double", Map.entry(10L, "xyz")),
                        Map.entry("widget", Map.entry(11L, "{\"name\":\"btn\",\"size\":7,\"on\":true}")),
                        Map.entry("bad_json", Map.entry(12L, "{not valid json")),
                        Map.entry("list", Map.entry(13L, "[\"a\",\"b\",\"c\"]"))
                ))));
        return TipsyAbConfigClient.create(grpcConfig(NS));
    }

    @Test
    @DisplayName("static bool: hit parses, miss -> def; lenient never throws")
    void staticBool() {
        staticClient = buildStaticClient();
        assertTrue(staticClient.getConfigStaticBool(NS, "flag_true", false));
        assertFalse(staticClient.getConfigStaticBool(NS, "flag_false", true));
        // miss -> def (both polarities)
        assertTrue(staticClient.getConfigStaticBool(NS, "no-such-key", true));
        assertFalse(staticClient.getConfigStaticBool(NS, "no-such-key", false));
        // unknown ns is a pure miss, not an exception
        assertTrue(staticClient.getConfigStaticBool("no-ns", "flag_true", true));
    }

    @Test
    @DisplayName("static long: hit parses, big/max lossless, miss & parse-fail -> def")
    void staticLong() {
        staticClient = buildStaticClient();
        assertEquals(42L, staticClient.getConfigStaticLong(NS, "count", -1L));
        // > 2^53 survives exactly (no double round-trip).
        assertEquals(9007199254740993L, staticClient.getConfigStaticLong(NS, "big", -1L));
        assertEquals(Long.MAX_VALUE, staticClient.getConfigStaticLong(NS, "maxlong", -1L));
        // miss -> def
        assertEquals(-1L, staticClient.getConfigStaticLong(NS, "no-such-key", -1L));
        // parse-fail -> def (no throw)
        assertEquals(-1L, staticClient.getConfigStaticLong(NS, "bad_long", -1L));
        // a float string is NOT a valid long -> def
        assertEquals(-1L, staticClient.getConfigStaticLong(NS, "ratio", -1L));
    }

    @Test
    @DisplayName("static double: hit parses, miss & parse-fail -> def")
    void staticDouble() {
        staticClient = buildStaticClient();
        assertEquals(3.14, staticClient.getConfigStaticDouble(NS, "ratio", -1.0), 0.0);
        assertEquals(42.0, staticClient.getConfigStaticDouble(NS, "count", -1.0), 0.0);
        // miss -> def
        assertEquals(-1.0, staticClient.getConfigStaticDouble(NS, "no-such-key", -1.0), 0.0);
        // parse-fail -> def
        assertEquals(-1.0, staticClient.getConfigStaticDouble(NS, "bad_double", -1.0), 0.0);
    }

    @Test
    @DisplayName("static string: hit -> raw (incl empty), miss -> def")
    void staticString() {
        staticClient = buildStaticClient();
        assertEquals("hello", staticClient.getConfigStaticString(NS, "raw", "DEF"));
        // empty string is a valid hit, NOT the default.
        assertEquals("", staticClient.getConfigStaticString(NS, "empty", "DEF"));
        // miss -> def
        assertEquals("DEF", staticClient.getConfigStaticString(NS, "no-such-key", "DEF"));
    }

    @Test
    @DisplayName("static json: POJO round-trip, malformed -> def, miss -> def; List<T> via TypeToken")
    void staticJson() {
        staticClient = buildStaticClient();
        Widget def = new Widget();
        Widget w = staticClient.getConfigStaticJson(NS, "widget", Widget.class, def);
        assertEquals("btn", w.name);
        assertEquals(7, w.size);
        assertTrue(w.on);
        // malformed -> def (same instance, no throw)
        assertSame(def, staticClient.getConfigStaticJson(NS, "bad_json", Widget.class, def));
        // miss -> def
        assertSame(def, staticClient.getConfigStaticJson(NS, "no-such-key", Widget.class, def));
        // empty-string static hit -> Gson parses "" to null -> def (null-guard)
        assertSame(def, staticClient.getConfigStaticJson(NS, "empty", Widget.class, def));
        // generic/collection overload via TypeToken.
        java.lang.reflect.Type listType = new TypeToken<List<String>>() {}.getType();
        List<String> defList = List.of();
        List<String> got = staticClient.getConfigStaticJson(NS, "list", listType, defList);
        assertEquals(List.of("a", "b", "c"), got);
        assertSame(defList, staticClient.getConfigStaticJson(NS, "no-such-key", listType, defList));
    }

    // ==================================================================
    // Bool lenient matrix (static path is enough to exercise the shared helper).
    // ==================================================================

    @Test
    @DisplayName("bool lenient matrix: true/TRUE/True/1/' true ' -> true; else -> false; never throws")
    void boolLenientMatrix() {
        staticHarness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshotWithKeys(NS, 2, 1, Map.ofEntries(
                        Map.entry("t1", Map.entry(1L, "true")),
                        Map.entry("t2", Map.entry(2L, "TRUE")),
                        Map.entry("t3", Map.entry(3L, "True")),
                        Map.entry("t4", Map.entry(4L, "1")),
                        Map.entry("t5", Map.entry(5L, " true ")),
                        Map.entry("f1", Map.entry(6L, "false")),
                        Map.entry("f2", Map.entry(7L, "FALSE")),
                        Map.entry("f3", Map.entry(8L, "0")),
                        Map.entry("f4", Map.entry(9L, "yes")),
                        Map.entry("f5", Map.entry(10L, "garbage")),
                        Map.entry("f6", Map.entry(11L, ""))
                ))));
        staticClient = TipsyAbConfigClient.create(grpcConfig(NS));

        for (String truthy : List.of("t1", "t2", "t3", "t4", "t5")) {
            // def=false so only a genuine parse-to-true can flip it.
            assertTrue(staticClient.getConfigStaticBool(NS, truthy, false),
                    "expected true for key " + truthy);
        }
        for (String falsy : List.of("f1", "f2", "f3", "f4", "f5", "f6")) {
            // def=true so only a genuine parse-to-false can flip it (proves the hit
            // was parsed, not just the default returned).
            assertFalse(staticClient.getConfigStaticBool(NS, falsy, true),
                    "expected false for key " + falsy);
        }
        // Bool NEVER throws, whatever the stored value.
        assertDoesNotThrow(() -> staticClient.getConfigStaticBool(NS, "f5", false));
        assertDoesNotThrow(() -> staticClient.getConfigStaticBool(NS, "f6", false));
    }

    // ==================================================================
    // Dynamic typed accessors (wrap getConfig; full abtest resolution).
    // ==================================================================

    @Test
    @DisplayName("dynamic bool: full-release hit parses, miss/empty -> def, lenient never throws")
    void dynamicBool() {
        NsCache cache = new NsCache(2, 2)
                .key("flag", 7L, Map.of(7L, "true"))
                .key("empty", 8L, Map.of(8L, "")); // empty resolution -> treated as miss
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .build()) {
            AbtestContext ctx = h.client.emptyAbtestContext();
            assertTrue(h.client.getConfigBool(ctx, NS, "flag", false));
            // no full release + no ab hit -> "" resolution -> def
            assertTrue(h.client.getConfigBool(ctx, NS, "no-key", true));
            assertFalse(h.client.getConfigBool(ctx, NS, "no-key", false));
            // resolved-but-empty -> treated as miss -> def
            assertTrue(h.client.getConfigBool(ctx, NS, "empty", true));
        }
    }

    @Test
    @DisplayName("dynamic bool: abtest-hit path resolves the ab version then parses")
    void dynamicBoolAbtestHit() {
        NsCache cache = new NsCache(2, 2)
                .key("flag", 7L, Map.of(7L, "false", 9L, "true")); // full=false, ab=true
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("flag", 9L)) // steer to v9="true"
                .build()) {
            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertTrue(h.client.getConfigBool(ctx, NS, "flag", false),
                    "ab hit must resolve v9 then parse it as true");
            assertEquals(1, h.abtest.callsFor(NS));
        }
    }

    @Test
    @DisplayName("dynamic long: hit parses, big lossless, miss/empty/parse-fail -> def")
    void dynamicLong() {
        NsCache cache = new NsCache(2, 2)
                .key("count", 1L, Map.of(1L, "42"))
                .key("big", 2L, Map.of(2L, "9223372036854775807"))
                .key("bad", 3L, Map.of(3L, "abc"))
                .key("empty", 4L, Map.of(4L, ""));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .build()) {
            AbtestContext ctx = h.client.emptyAbtestContext();
            assertEquals(42L, h.client.getConfigLong(ctx, NS, "count", -1L));
            assertEquals(Long.MAX_VALUE, h.client.getConfigLong(ctx, NS, "big", -1L));
            // miss -> def
            assertEquals(-1L, h.client.getConfigLong(ctx, NS, "no-key", -1L));
            // empty resolution -> def
            assertEquals(-1L, h.client.getConfigLong(ctx, NS, "empty", -1L));
            // parse-fail -> def (no throw)
            assertEquals(-1L, h.client.getConfigLong(ctx, NS, "bad", -1L));
        }
    }

    @Test
    @DisplayName("dynamic double: hit parses, miss/empty/parse-fail -> def")
    void dynamicDouble() {
        NsCache cache = new NsCache(2, 2)
                .key("ratio", 1L, Map.of(1L, "2.5"))
                .key("bad", 2L, Map.of(2L, "xyz"))
                .key("empty", 3L, Map.of(3L, ""));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .build()) {
            AbtestContext ctx = h.client.emptyAbtestContext();
            assertEquals(2.5, h.client.getConfigDouble(ctx, NS, "ratio", -1.0), 0.0);
            assertEquals(-1.0, h.client.getConfigDouble(ctx, NS, "no-key", -1.0), 0.0);
            assertEquals(-1.0, h.client.getConfigDouble(ctx, NS, "empty", -1.0), 0.0);
            assertEquals(-1.0, h.client.getConfigDouble(ctx, NS, "bad", -1.0), 0.0);
        }
    }

    @Test
    @DisplayName("dynamic string: hit -> raw, empty/miss -> def")
    void dynamicString() {
        NsCache cache = new NsCache(2, 2)
                .key("raw", 1L, Map.of(1L, "hello"))
                .key("empty", 2L, Map.of(2L, ""));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .build()) {
            AbtestContext ctx = h.client.emptyAbtestContext();
            assertEquals("hello", h.client.getConfigString(ctx, NS, "raw", "DEF"));
            // Unlike static, the dynamic path treats "" as a miss -> def.
            assertEquals("DEF", h.client.getConfigString(ctx, NS, "empty", "DEF"));
            assertEquals("DEF", h.client.getConfigString(ctx, NS, "no-key", "DEF"));
        }
    }

    @Test
    @DisplayName("dynamic json: POJO round-trip, malformed/empty/miss -> def; List<T> via TypeToken")
    void dynamicJson() {
        NsCache cache = new NsCache(2, 2)
                .key("widget", 1L, Map.of(1L, "{\"name\":\"btn\",\"size\":7,\"on\":true}"))
                .key("bad", 2L, Map.of(2L, "{not valid"))
                .key("empty", 3L, Map.of(3L, ""))
                .key("list", 4L, Map.of(4L, "[1,2,3]"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .build()) {
            AbtestContext ctx = h.client.emptyAbtestContext();
            Widget def = new Widget();
            Widget w = h.client.getConfigJson(ctx, NS, "widget", Widget.class, def);
            assertEquals("btn", w.name);
            assertEquals(7, w.size);
            assertTrue(w.on);
            // malformed -> def
            assertSame(def, h.client.getConfigJson(ctx, NS, "bad", Widget.class, def));
            // empty resolution -> def
            assertSame(def, h.client.getConfigJson(ctx, NS, "empty", Widget.class, def));
            // miss -> def
            assertSame(def, h.client.getConfigJson(ctx, NS, "no-key", Widget.class, def));
            // generic overload
            java.lang.reflect.Type listType = new TypeToken<List<Integer>>() {}.getType();
            List<Integer> defList = List.of();
            assertEquals(List.of(1, 2, 3),
                    h.client.getConfigJson(ctx, NS, "list", listType, defList));
            assertSame(defList, h.client.getConfigJson(ctx, NS, "no-key", listType, defList));
        }
    }

    @Test
    @DisplayName("dynamic accessors propagate underlying getConfig exceptions (null abctx)")
    void dynamicPropagatesUnderlyingExceptions() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(1, 1).key("k", 1L, Map.of(1L, "true")))
                .build()) {
            // A null abctx must still throw AbtestContextMissingException, not be
            // swallowed into the default.
            org.junit.jupiter.api.Assertions.assertThrows(
                    AbtestContextMissingException.class,
                    () -> h.client.getConfigBool(null, NS, "k", false));
        }
    }

    // ------------------------------------------------------------------

    private Config grpcConfig(String... namespaces) {
        return Config.builder()
                .namespaces(namespaces)
                .configServiceAddr("passthrough:///x")
                .token("tok")
                .transport(Transport.GRPC)
                .channelConfigurator(staticHarness.channelConfigurator())
                .pullInterval(java.time.Duration.ofMinutes(10))
                .pullTimeout(java.time.Duration.ofSeconds(2))
                .build();
    }
}
