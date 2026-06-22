package io.github.lightspeedintelligence.abconfig.deve2e;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.ArrayList;
import java.util.Iterator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * One row of {@code test/dev-e2e/fixtures/expectations.json}, plus the fixture
 * load / path-resolve / attr-decode helpers. This mirrors the Go driver's
 * {@code assert.go} {@code Expectation} type and free functions
 * ({@code rawAttrs}, {@code kvEqual}, {@code resolveFixturesPath},
 * {@code loadExpectations}) so the Java driver asserts the exact same golden
 * fixture identically.
 */
final class Expectation {

    /** The {@code key} value marking a custom_params row (vs a config-version key). */
    static final String CUSTOM_KEY = "__custom__";

    /** Sentinel default passed to {@code getConfig} so a fall-through is visible as a FAIL. */
    static final String DEFAULT_SENTINEL = "<DEFAULT>";

    final String ns;
    final String userId;
    /** Typed-Value envelope from the fixture, e.g. {@code {"country":{"s":"US"}}}. */
    final Map<String, Object> userAttrs;
    final String key;
    final String expectedVersionId;
    /** Either a {@code String} (config-version rows) or a {@code Map} (custom rows). */
    final Object expectedValue;
    final String source;
    final String note;
    final List<String> appliesTo;

    private Expectation(
            String ns,
            String userId,
            Map<String, Object> userAttrs,
            String key,
            String expectedVersionId,
            Object expectedValue,
            String source,
            String note,
            List<String> appliesTo) {
        this.ns = ns;
        this.userId = userId;
        this.userAttrs = userAttrs;
        this.key = key;
        this.expectedVersionId = expectedVersionId;
        this.expectedValue = expectedValue;
        this.source = source;
        this.note = note;
        this.appliesTo = appliesTo;
    }

    /** Whether this expectation row applies to the given client tag. */
    boolean appliesTo(String client) {
        return appliesTo != null && appliesTo.contains(client);
    }

    // ------------------------------------------------------------------
    // Fixture loading
    // ------------------------------------------------------------------

    private static final ObjectMapper MAPPER = new ObjectMapper();

    /** Loads and parses the expectations fixture from {@code path}. */
    static List<Expectation> load(Path path) throws IOException {
        byte[] buf = Files.readAllBytes(path);
        JsonNode root = MAPPER.readTree(buf);
        if (!root.isArray()) {
            throw new IOException("parse " + path + ": top-level JSON is not an array");
        }
        List<Expectation> out = new ArrayList<>(root.size());
        for (JsonNode row : root) {
            out.add(fromJson(row));
        }
        return out;
    }

    private static Expectation fromJson(JsonNode row) {
        List<String> applies = new ArrayList<>();
        JsonNode at = row.get("applies_to");
        if (at != null && at.isArray()) {
            for (JsonNode c : at) {
                applies.add(c.asText());
            }
        }
        return new Expectation(
                text(row, "ns"),
                text(row, "user_id"),
                toJavaMap(row.get("user_attrs")),
                text(row, "key"),
                text(row, "expected_version_id"),
                toJavaValue(row.get("expected_value")),
                text(row, "source"),
                text(row, "note"),
                applies);
    }

    private static String text(JsonNode row, String field) {
        JsonNode n = row.get(field);
        return n == null || n.isNull() ? "" : n.asText();
    }

    /**
     * Converts a JSON object node into a {@code Map<String,Object>} preserving
     * the value types we care about (string / boolean / number / nested object).
     * Numbers come back as {@code Long} for integral values and {@code Double}
     * otherwise, matching what {@code kvEqual} expects.
     */
    private static Map<String, Object> toJavaMap(JsonNode node) {
        Map<String, Object> out = new LinkedHashMap<>();
        if (node == null || !node.isObject()) {
            return out;
        }
        Iterator<Map.Entry<String, JsonNode>> it = node.fields();
        while (it.hasNext()) {
            Map.Entry<String, JsonNode> e = it.next();
            out.put(e.getKey(), toJavaValue(e.getValue()));
        }
        return out;
    }

    private static Object toJavaValue(JsonNode n) {
        if (n == null || n.isNull()) {
            return null;
        }
        if (n.isObject()) {
            return toJavaMap(n);
        }
        if (n.isBoolean()) {
            return n.asBoolean();
        }
        if (n.isIntegralNumber()) {
            return n.asLong();
        }
        if (n.isNumber()) {
            return n.asDouble();
        }
        // String (and anything else) falls back to text.
        return n.asText();
    }

    /**
     * Resolves the fixtures path. When {@code flagVal} is set it is taken as-is
     * (absolutised); otherwise the working directory is walked UPWARD looking for
     * {@code test/dev-e2e/fixtures/expectations.json}, mirroring the Go driver's
     * {@code resolveFixturesPath}. This lets the driver run from the module dir
     * (where the cwd is {@code .../clients/java}) or from the repo root.
     */
    static Path resolveFixturesPath(String flagVal) {
        if (flagVal != null && !flagVal.isEmpty()) {
            return Paths.get(flagVal).toAbsolutePath().normalize();
        }
        final String rel = "test/dev-e2e/fixtures/expectations.json";
        Path dir = Paths.get("").toAbsolutePath();
        while (dir != null) {
            Path candidate = dir.resolve(rel);
            if (Files.exists(candidate)) {
                return candidate.normalize();
            }
            dir = dir.getParent();
        }
        return Paths.get(rel).toAbsolutePath().normalize();
    }

    // ------------------------------------------------------------------
    // Attr decoding (rawAttrs) + KV comparison (kvEqual)
    // ------------------------------------------------------------------

    /**
     * Converts the fixture's typed-Value envelope (e.g. {@code {"country":{"s":"US"}}})
     * back into the RAW attribute map the SDK expects (e.g. {@code {"country":"US"}}).
     * The SDK re-encodes raw values into the {@code abtestv1.Value} envelope on the
     * wire, so passing the envelope would double-wrap and break admission matching.
     * Mirrors the Go driver's {@code rawAttrs}:
     * <ul>
     *   <li>{@code s} → String</li>
     *   <li>{@code b} → Boolean</li>
     *   <li>{@code d} → Double</li>
     *   <li>{@code i} → Long (proto int64 is a JSON string in the envelope)</li>
     * </ul>
     *
     * @return a raw attr map, or {@code null} when the envelope is empty (so the
     *     SDK sees "no attrs" exactly like the Go driver passing {@code nil})
     */
    static Map<String, Object> rawAttrs(Map<String, Object> envelope) {
        if (envelope == null || envelope.isEmpty()) {
            return null;
        }
        Map<String, Object> out = new LinkedHashMap<>(envelope.size());
        for (Map.Entry<String, Object> e : envelope.entrySet()) {
            Object v = e.getValue();
            if (!(v instanceof Map<?, ?> inner)) {
                // Already raw (defensive); pass through.
                out.put(e.getKey(), v);
                continue;
            }
            if (inner.containsKey("s")) {
                out.put(e.getKey(), String.valueOf(inner.get("s")));
            } else if (inner.containsKey("b")) {
                out.put(e.getKey(), asBoolean(inner.get("b")));
            } else if (inner.containsKey("d")) {
                out.put(e.getKey(), asDouble(inner.get("d")));
            } else if (inner.containsKey("i")) {
                out.put(e.getKey(), asLong(inner.get("i")));
            } else {
                out.put(e.getKey(), v);
            }
        }
        return out;
    }

    private static boolean asBoolean(Object v) {
        if (v instanceof Boolean b) {
            return b;
        }
        return Boolean.parseBoolean(String.valueOf(v));
    }

    private static double asDouble(Object v) {
        if (v instanceof Number n) {
            return n.doubleValue();
        }
        try {
            return Double.parseDouble(String.valueOf(v));
        } catch (NumberFormatException ex) {
            return 0.0;
        }
    }

    private static long asLong(Object v) {
        if (v instanceof Number n) {
            return n.longValue();
        }
        try {
            return Long.parseLong(String.valueOf(v).trim());
        } catch (NumberFormatException ex) {
            return 0L;
        }
    }

    /**
     * Deep-compares two flat KV maps with numeric tolerance, mirroring the Go
     * driver's {@code kvEqual}: same key set; numbers compared as {@code double}
     * (so {@code 10} == {@code 10.0}); everything else compared by string form.
     */
    static boolean kvEqual(Map<String, Object> got, Map<String, Object> want) {
        if (got.size() != want.size()) {
            return false;
        }
        for (Map.Entry<String, Object> e : want.entrySet()) {
            if (!got.containsKey(e.getKey())) {
                return false;
            }
            Object gv = got.get(e.getKey());
            Object wv = e.getValue();
            Double gn = numberOrNull(gv);
            Double wn = numberOrNull(wv);
            if (gn != null && wn != null) {
                if (gn.doubleValue() != wn.doubleValue()) {
                    return false;
                }
                continue;
            }
            if (!stringForm(gv).equals(stringForm(wv))) {
                return false;
            }
        }
        return true;
    }

    private static Double numberOrNull(Object v) {
        if (v instanceof Number n) {
            return n.doubleValue();
        }
        return null;
    }

    /**
     * String form for the non-numeric comparison branch. Booleans and strings
     * use their plain {@code toString}; this matches the Go driver's
     * {@code fmt.Sprintf("%v", ...)} for the value types present in the fixture.
     */
    private static String stringForm(Object v) {
        return v == null ? "null" : String.valueOf(v);
    }
}
