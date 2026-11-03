package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assertions.fail;

import ch.qos.logback.classic.Level;
import ch.qos.logback.classic.Logger;
import ch.qos.logback.classic.spi.ILoggingEvent;
import ch.qos.logback.core.read.ListAppender;
import io.github.lightspeedintelligence.abconfig.AbtestTestSupport.NsCache;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ExperimentGroupResult;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ExperimentType;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.time.Duration;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.HashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.TreeSet;
import java.util.concurrent.CountDownLatch;
import java.util.stream.Collectors;
import java.util.stream.Stream;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.DynamicTest;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.TestFactory;
import org.slf4j.LoggerFactory;

/**
 * Default-value hit log tests (real-enrollment-dashboard design part-c §C1 /
 * §C6).
 *
 * <p><b>Contract under test</b>: when dynamic {@code getConfig} /
 * {@code getConfigDefault} (and the typed accessors built on them) resolve NO
 * value and return the caller's default, the SDK emits exactly ONE INFO line
 * per call: the fixed template {@code "tipsyabconfig: get_config hit (default) "}
 * followed by comma-separated pairs, {@code reason=default} first, the field
 * set being exactly {reason, default_cause, ns, key, uid, trace_id} — never
 * version / experiment_id / group_id / release_id, never the default value.
 * {@code default_cause} is the FIRST matching row of the C1 table:
 * unknown_key &gt; fast_path &gt; no_user &gt; abtest_unavailable &gt;
 * ab_version_missing &gt; no_full_release &gt; full_release_value_missing.
 * Exceptions (closed client, null ctx, namespace errors) are unchanged and emit
 * no line.
 *
 * <p><b>Cross-language alignment</b>: the cause scenarios come from the
 * repo-shared list {@code test/sdk-alignment/default_cause_scenarios.txt},
 * which the Go and Python suites also execute row by row with the same
 * expected cause. Edit that file, not a Java-local table.
 *
 * <p><b>Observation</b>: a ListAppender on the SDK package logger (so the line
 * is captured whichever SDK class logs it). Every "no line" assertion is paired
 * with a positive control on the same client and capture.
 *
 * <p><b>Not proven here</b>: a real server's empty-parameter-group payload
 * (dev-e2e scenario under test/dev-e2e/default-hit).
 */
final class DefaultHitLogTest {

    private static final String NS = "ns1";

    /** Repo-shared list; surefire runs with user.dir = this module's basedir. */
    private static final Path SCENARIO_FILE = Paths.get(System.getProperty("user.dir"))
            .resolve("../../../test/sdk-alignment/default_cause_scenarios.txt").normalize();

    private static final Set<String> C1_CAUSES = Set.of(
            "unknown_key", "fast_path", "no_user", "abtest_unavailable",
            "ab_version_missing", "no_full_release", "full_release_value_missing");

    private static final Set<String> DEFAULT_LINE_FIELDS =
            Set.of("reason", "default_cause", "ns", "key", "uid", "trace_id");

    private static final Map<String, Set<String>> COLUMN_VALUES = Map.of(
            "snapshot", Set.of("none", "key_absent", "key"),
            "full", Set.of("-", "absent", "zero", "uncached"),
            "hdr", Set.of("-", "absent", "true", "false"),
            "ctx", Set.of("new", "empty", "mock_seeded", "mock_unseeded"),
            "uid", Set.of("real", "empty", "zero"),
            "abtest", Set.of("ok_empty", "ok_empty_group", "ok_other", "ok_v0",
                    "ok_uncached", "rpc_error", "timeout", "not_configured"));

    /** One row of the shared scenario list (see the file header for columns). */
    record Scenario(String id, String expect, String snapshot, String full, String hdr,
            String ctx, String uid, String abtest) {

        String uidValue() {
            switch (uid) {
                case "empty":
                    return "";
                case "zero":
                    return "0";
                default:
                    return "u-sc";
            }
        }
    }

    /** Parses + validates the shared list; a missing file fails (never skips). */
    static List<Scenario> loadScenarios() {
        List<String> lines;
        try {
            lines = Files.readAllLines(SCENARIO_FILE);
        } catch (IOException e) {
            throw new AssertionError("read shared scenario list " + SCENARIO_FILE, e);
        }
        List<Scenario> out = new ArrayList<>();
        Set<String> seen = new HashSet<>();
        for (int n = 0; n < lines.size(); n++) {
            String line = lines.get(n).strip();
            if (line.isEmpty() || line.startsWith("#")) {
                continue;
            }
            String[] c = line.split("\\s+");
            if (c.length != 8) {
                throw new AssertionError(SCENARIO_FILE + ":" + (n + 1) + ": want 8 columns: " + line);
            }
            Scenario s = new Scenario(c[0], c[1], c[2], c[3], c[4], c[5], c[6], c[7]);
            if (!seen.add(s.id())) {
                throw new AssertionError("duplicate scenario id " + s.id());
            }
            if (!C1_CAUSES.contains(s.expect())) {
                throw new AssertionError(s.id() + ": " + s.expect() + " is not a C1 cause");
            }
            Map<String, String> cols = Map.of("snapshot", s.snapshot(), "full", s.full(),
                    "hdr", s.hdr(), "ctx", s.ctx(), "uid", s.uid(), "abtest", s.abtest());
            for (Map.Entry<String, String> e : cols.entrySet()) {
                if (!COLUMN_VALUES.get(e.getKey()).contains(e.getValue())) {
                    throw new AssertionError(s.id() + ": column " + e.getKey()
                            + " has unknown value " + e.getValue());
                }
            }
            boolean key = s.snapshot().equals("key");
            if (key == s.full().equals("-") || key == s.hdr().equals("-")) {
                throw new AssertionError(s.id() + ": full/hdr must be set iff snapshot=key");
            }
            if (s.ctx().equals("empty") && !s.uid().equals("empty")) {
                throw new AssertionError(s.id() + ": ctx=empty implies uid=empty");
            }
            out.add(s);
        }
        if (out.isEmpty()) {
            throw new AssertionError("shared scenario list " + SCENARIO_FILE + " has no rows");
        }
        return out;
    }

    // ------------------------------------------------------------------
    // Log capture.
    // ------------------------------------------------------------------

    /**
     * ListAppender on the SDK package logger for one test's scope. Create it
     * BEFORE the harness's {@code build()} (first resource in the try header):
     * attaching after the client exists races its background threads' logging
     * against logback's copy-on-write appender-list refresh (review
     * NB-ST5-r2-2).
     */
    private static final class LogCapture implements AutoCloseable {
        final Logger logger = (Logger) LoggerFactory.getLogger("io.github.lightspeedintelligence.abconfig");
        final ListAppender<ILoggingEvent> appender = new ListAppender<>();

        LogCapture() {
            appender.start();
            logger.addAppender(appender);
        }

        /** Non-fail-fast copy (background threads append concurrently). */
        List<ILoggingEvent> snapshot() {
            return new ArrayList<>(appender.list);
        }

        /** Every get_config hit line, any variant. */
        List<ILoggingEvent> hitLines() {
            return snapshot().stream()
                    .filter(e -> HitLogLines.variant(e.getFormattedMessage()) != null)
                    .collect(Collectors.toList());
        }

        List<ILoggingEvent> defaultLines() {
            return hitLines().stream()
                    .filter(e -> "default".equals(HitLogLines.variant(e.getFormattedMessage())))
                    .collect(Collectors.toList());
        }

        @Override
        public void close() {
            logger.detachAppender(appender);
            appender.stop();
        }
    }

    /**
     * One well-formed default line: INFO, fixed prefix with {@code reason=default}
     * first, EXACT field set, exact values, and — when {@code defaultValue} is
     * non-null — the caller's default nowhere in the line.
     */
    static void assertDefaultLine(ILoggingEvent e, String ns, String key, String cause,
            String uid, String traceId, String defaultValue) {
        String line = e.getFormattedMessage();
        assertEquals(Level.INFO, e.getLevel(), line);
        assertTrue(line.startsWith(HitLogLines.DEFAULT_PREFIX + "reason=default, "),
                "default line must start with the fixed prefix and reason first: " + line);
        Map<String, String> f = HitLogLines.fields(line);
        assertEquals(new TreeSet<>(DEFAULT_LINE_FIELDS), new TreeSet<>(f.keySet()),
                "default line field set (no version/experiment_id/group_id/release_id/default value): " + line);
        assertEquals("default", f.get("reason"), line);
        assertEquals(cause, f.get("default_cause"), line);
        assertEquals(ns, f.get("ns"), line);
        assertEquals(key, f.get("key"), line);
        assertEquals(uid, f.get("uid"), line);
        assertEquals(traceId, f.get("trace_id"), line);
        if (defaultValue != null) {
            assertFalse(line.contains(defaultValue), "default line leaks the caller's default: " + line);
        }
    }

    private static GetExperimentResultResponse group(Map<String, Long> params) {
        return GetExperimentResultResponse.newBuilder()
                .addGroups(ExperimentGroupResult.newBuilder()
                        .setExperimentId("exp-sc")
                        .setGroupId("grp-sc")
                        .setExperimentType(ExperimentType.EXPERIMENT_TYPE_CONFIG_VERSION)
                        .putAllParamsVersions(params))
                .build();
    }

    private static NsCache scenarioCache(Scenario s) {
        NsCache c = new NsCache(1, 1).key("k2", 3L, Map.of(3L, "full-k2"));
        if (s.snapshot().equals("key")) {
            c.key("k", s.full().equals("uncached") ? 7L : 0L, Map.of(0L, "stray-v0", 5L, "v5"));
            if (s.full().equals("zero")) {
                c.fullReleaseExplicitZero("k");
            }
            if (!s.hdr().equals("absent")) {
                c.hasDynamicResolution("k", Boolean.valueOf(s.hdr()));
            }
        }
        return c;
    }

    private static void runScenario(Scenario s) {
        AbtestTestSupport.Builder b = AbtestTestSupport.newBuilder().namespaces(NS);
        if (!s.snapshot().equals("none")) {
            b.snapshot(NS, scenarioCache(s));
        }
        if (s.abtest().equals("not_configured")) {
            b.withoutAbtest();
        }
        if (s.abtest().equals("timeout")) {
            b.abtestTimeout(Duration.ofMillis(50));
        }
        CountDownLatch gate = null;
        try (LogCapture cap = new LogCapture(); AbtestTestSupport h = b.build()) {
            switch (s.abtest()) {
                case "ok_empty":
                    h.abtest.setFullResponse(NS, GetExperimentResultResponse.getDefaultInstance());
                    break;
                case "ok_empty_group":
                    h.abtest.setFullResponse(NS, group(Map.of()));
                    break;
                case "ok_other":
                    h.abtest.setFullResponse(NS, group(Map.of("k2", 3L)));
                    break;
                case "ok_v0":
                    h.abtest.setFullResponse(NS, group(Map.of("k", 0L)));
                    break;
                case "ok_uncached":
                case "timeout":
                    // timeout: content that would yield ab_version_missing if the
                    // timeout did not fire.
                    h.abtest.setFullResponse(NS, group(Map.of("k", 99L)));
                    if (s.abtest().equals("timeout")) {
                        gate = new CountDownLatch(1);
                        h.abtest.releaseGate = gate;
                    }
                    break;
                case "rpc_error":
                    h.abtest.failFor(NS);
                    break;
                default:
                    break;
            }
            if (s.snapshot().equals("none")) {
                assertTrue(h.client.cache().snapshot(NS).isEmpty(), "fixture: ns1 must have no snapshot");
            }
            String uid = s.uidValue();
            AbtestContext ctx;
            switch (s.ctx()) {
                case "empty":
                    ctx = h.client.emptyAbtestContext();
                    break;
                case "mock_seeded":
                    ctx = h.client.mockAbtestContext(uid, Map.of(NS, Map.of()));
                    break;
                case "mock_unseeded":
                    ctx = h.client.mockAbtestContext(uid, Map.of());
                    break;
                default:
                    ctx = h.client.newAbtestContext(uid, Map.of(), "tr-" + s.id());
                    break;
            }
            String def = "DEF-" + s.id();
            assertEquals(def, h.client.getConfig(ctx, NS, "k", def),
                    "the caller's default must be returned unchanged");
            List<ILoggingEvent> hits = cap.hitLines();
            assertEquals(1, hits.size(), "exactly one hit line expected, got: " + hits);
            assertDefaultLine(hits.get(0), NS, "k", s.expect(), uid, ctx.traceId(), def);
        } finally {
            if (gate != null) {
                gate.countDown();
            }
        }
    }

    @Test
    @DisplayName("shared list covers every C1 cause and the four §C6 scenarios; E4 differs only in full")
    void scenarioListShape() {
        List<Scenario> rows = loadScenarios();
        Map<String, Scenario> byId = new HashMap<>();
        Set<String> causes = new HashSet<>();
        for (Scenario s : rows) {
            byId.put(s.id(), s);
            causes.add(s.expect());
        }
        assertEquals(C1_CAUSES, causes, "every C1 cause needs a row");
        for (String id : List.of("E1.rpc_ok_empty_param_group", "E2.fast_path_valid_uid",
                "E3.merged_version_zero", "E4.full_id_absent", "E4.full_id_zero")) {
            assertTrue(byId.containsKey(id), "missing §C6 scenario " + id);
        }
        Scenario a = byId.get("E4.full_id_absent");
        Scenario b = byId.get("E4.full_id_zero");
        assertEquals("absent", a.full());
        assertEquals("zero", b.full());
        assertEquals(new Scenario("", a.expect(), a.snapshot(), "", a.hdr(), a.ctx(), a.uid(), a.abtest()),
                new Scenario("", b.expect(), b.snapshot(), "", b.hdr(), b.ctx(), b.uid(), b.abtest()),
                "E4 pair must differ only in full");
    }

    /** Every shared row, one dynamic test each (no skip path). */
    @TestFactory
    Stream<DynamicTest> sharedScenarios() {
        return loadScenarios().stream()
                .map(s -> DynamicTest.dynamicTest(s.id(), () -> runScenario(s)));
    }

    /**
     * The {@code null} arm of the cause helper: a {@code null} abtest result on
     * a non-fast-path key is {@code abtest_unavailable}, never "evaluated"
     * (part-c §C1 judgement point 1). {@code getConfig} cannot reach this arm
     * today (it initialises the result to {@code EMPTY_RESULT} and
     * {@code resultFor} never returns {@code null}), so it is asserted on the
     * pure helper directly.
     */
    @Test
    @DisplayName("cause helper: null abtest result on a non-fast-path key is abtest_unavailable")
    void causeHelperNullAbtestResult() {
        KeyState ks = new KeyState(0L, Map.of(5L, "v5"), null);
        assertEquals("abtest_unavailable", TipsyAbConfigClient.defaultCause(ks, "k", null, "u"));
    }

    // ------------------------------------------------------------------
    // Error exits, entry-point variants, call multiplicity, out-of-scope APIs.
    // ------------------------------------------------------------------

    /**
     * ns1 holds a no-full-release key {@code k}, a full-release key {@code k2}
     * and an unparsable full value {@code bad}; abtest succeeds without k, so
     * a valid call on k is a no_full_release default.
     */
    private static AbtestTestSupport plainHarness(String defaultNs) {
        AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .defaultNamespace(defaultNs)
                .snapshot(NS, new NsCache(1, 1)
                        .key("k", 0L, Map.of(5L, "v5"))
                        .key("k2", 3L, Map.of(3L, "full-k2"))
                        .key("bad", 4L, Map.of(4L, "not-a-number")))
                .build();
        h.abtest.setFullResponse(NS, group(Map.of("k2", 3L)));
        return h;
    }

    /** A valid call emits one no_full_release line and it is the ONLY hit line. */
    private static void positiveControl(AbtestTestSupport h, LogCapture cap) {
        AbtestContext ctx = h.client.newAbtestContext("u-ctl", Map.of(), "tr-ctl");
        assertEquals("DEF-ctl", h.client.getConfig(ctx, NS, "k", "DEF-ctl"));
        List<ILoggingEvent> hits = cap.hitLines();
        assertEquals(1, hits.size(), "an error exit emitted a hit line: " + hits);
        assertDefaultLine(hits.get(0), NS, "k", "no_full_release", "u-ctl", "tr-ctl", "DEF-ctl");
    }

    @Test
    @DisplayName("closed client: SdkClosedException, no line (control runs before close)")
    void closedClientEmitsNoLine() {
        try (LogCapture cap = new LogCapture(); AbtestTestSupport h = plainHarness("")) {
            positiveControl(h, cap);
            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            h.client.close();
            assertThrows(SdkClosedException.class, () -> h.client.getConfig(ctx, NS, "k", "DEF"));
            assertThrows(SdkClosedException.class, () -> h.client.getConfigDefault(ctx, "k", "DEF"));
            assertEquals(1, cap.hitLines().size(), "the closed-client exit emitted a line");
        }
    }

    @Test
    @DisplayName("null AbtestContext: AbtestContextMissingException, no line")
    void nullContextEmitsNoLine() {
        try (LogCapture cap = new LogCapture(); AbtestTestSupport h = plainHarness("")) {
            assertThrows(AbtestContextMissingException.class, () -> h.client.getConfig(null, NS, "k", "DEF"));
            assertEquals(0, cap.hitLines().size());
            positiveControl(h, cap);
        }
    }

    @Test
    @DisplayName("namespace required: NamespaceRequiredException, no line")
    void namespaceRequiredEmitsNoLine() {
        try (LogCapture cap = new LogCapture(); AbtestTestSupport h = plainHarness("")) {
            if (!h.client.defaultNamespace().isEmpty()) {
                fail("fixture: PROJECT_DEFAULT_NAMESPACE is set in this environment ("
                        + h.client.defaultNamespace() + "); the namespace-required exit cannot be reached");
            }
            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertThrows(NamespaceRequiredException.class, () -> h.client.getConfigDefault(ctx, "k", "DEF"));
            assertThrows(NamespaceRequiredException.class, () -> h.client.getConfig(ctx, "", "k", "DEF"));
            assertEquals(0, cap.hitLines().size());
            positiveControl(h, cap);
        }
    }

    @Test
    @DisplayName("namespace not subscribed: NamespaceNotSubscribedException, no line")
    void namespaceNotSubscribedEmitsNoLine() {
        try (LogCapture cap = new LogCapture(); AbtestTestSupport h = plainHarness("")) {
            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertThrows(NamespaceNotSubscribedException.class,
                    () -> h.client.getConfig(ctx, "not-subscribed", "k", "DEF"));
            assertEquals(0, cap.hitLines().size());
            positiveControl(h, cap);
        }
    }

    @Test
    @DisplayName("getConfigDefault emits the line with the RESOLVED namespace")
    void getConfigDefaultEmits() {
        try (LogCapture cap = new LogCapture(); AbtestTestSupport h = plainHarness(NS)) {
            AbtestContext ctx = h.client.newAbtestContext("u-dns", Map.of(), "tr-dns");
            assertEquals("DEF-dns", h.client.getConfigDefault(ctx, "k", "DEF-dns"));
            List<ILoggingEvent> hits = cap.hitLines();
            assertEquals(1, hits.size());
            assertDefaultLine(hits.get(0), NS, "k", "no_full_release", "u-dns", "tr-dns", "DEF-dns");
        }
    }

    @Test
    @DisplayName("typed accessors emit one default line each; a resolved-but-unparsable value does not")
    void typedAccessorsEmit() {
        try (LogCapture cap = new LogCapture(); AbtestTestSupport h = plainHarness("")) {
            List<String> names = List.of("Bool", "Long", "Double", "String", "Json");
            for (int i = 0; i < names.size(); i++) {
                String name = names.get(i);
                String trace = "tr-typed-" + name;
                AbtestContext ctx = h.client.newAbtestContext("u-typed", Map.of(), trace);
                switch (name) {
                    case "Bool":
                        assertTrue(h.client.getConfigBool(ctx, NS, "k", true));
                        break;
                    case "Long":
                        assertEquals(42L, h.client.getConfigLong(ctx, NS, "k", 42L));
                        break;
                    case "Double":
                        assertEquals(1.5, h.client.getConfigDouble(ctx, NS, "k", 1.5));
                        break;
                    case "String":
                        assertEquals("DEF-s", h.client.getConfigString(ctx, NS, "k", "DEF-s"));
                        break;
                    default:
                        assertEquals(Map.of("d", 1), h.client.getConfigJson(ctx, NS, "k", Map.class, Map.of("d", 1)));
                        break;
                }
                List<ILoggingEvent> defaults = cap.defaultLines();
                assertEquals(i + 1, defaults.size(), name + ": one default line per call");
                assertDefaultLine(defaults.get(i), NS, "k", "no_full_release", "u-typed", trace, null);
            }
            int before = cap.hitLines().size();
            AbtestContext ctx = h.client.newAbtestContext("u-typed", Map.of());
            assertEquals(7L, h.client.getConfigLong(ctx, NS, "bad", 7L));
            List<ILoggingEvent> added = cap.hitLines().subList(before, cap.hitLines().size());
            assertEquals(1, added.size(), "parse-failure call: exactly its own hit line");
            assertEquals("full", HitLogLines.variant(added.get(0).getFormattedMessage()),
                    "a resolved value that fails to parse logs its full hit, not a default line");
        }
    }

    @Test
    @DisplayName("every call emits (no dedup); the abtest RPC stays memoised at one")
    void everyCallEmits() {
        try (LogCapture cap = new LogCapture(); AbtestTestSupport h = plainHarness("")) {
            int before = h.abtest.callsFor(NS);
            AbtestContext ctx = h.client.newAbtestContext("u-rep", Map.of(), "tr-rep");
            for (int i = 0; i < 3; i++) {
                assertEquals("DEF-rep", h.client.getConfig(ctx, NS, "k", "DEF-rep"));
            }
            List<ILoggingEvent> defaults = cap.defaultLines();
            assertEquals(3, defaults.size());
            for (ILoggingEvent e : defaults) {
                assertDefaultLine(e, NS, "k", "no_full_release", "u-rep", "tr-rep", "DEF-rep");
            }
            assertEquals(1, h.abtest.callsFor(NS) - before);
        }
    }

    @Test
    @DisplayName("getConfigStatic and getAllConfigs are out of scope: no default line")
    void staticAndGetAllDoNotEmit() {
        try (LogCapture cap = new LogCapture(); AbtestTestSupport h = plainHarness("")) {
            assertTrue(h.client.getConfigStatic(NS, "k").isEmpty());
            Map<String, String> all = h.client.getAllConfigs(h.client.newAbtestContext("u-all", Map.of()), NS);
            assertNull(all.get("k"));
            assertEquals(0, cap.hitLines().size());
            positiveControl(h, cap);
        }
    }
}
