package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import ch.qos.logback.classic.Level;
import ch.qos.logback.classic.Logger;
import ch.qos.logback.classic.spi.ILoggingEvent;
import ch.qos.logback.core.read.ListAppender;
import io.github.lightspeedintelligence.abconfig.AbtestTestSupport.NsCache;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ExperimentGroupResult;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ExperimentType;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GrayReleaseHit;
import java.util.List;
import java.util.Map;
import java.util.stream.Collectors;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.slf4j.LoggerFactory;

/**
 * getConfig hit-log assertions (design "actual-enrollment-log" §4, SLS parsing
 * contract F7): every dynamic getConfig hit logs at INFO with a structured
 * {@code reason} (value-hit reasons: full_release / experiment / gray_whitelist /
 * abtest_unattributed); conditional fields ({@code experiment_id},
 * {@code group_id}, {@code release_id}) are OMITTED (key absent from the line)
 * when not applicable. Java uses one FIXED kv text template per reason, so the
 * whole formatted line is asserted with FULL equality — substring containment
 * is banned here because the line embeds several ids and containment would let
 * "renders the version" and "renders the reason" pass on each other's content.
 *
 * <p>The fifth reason {@code default} (real-enrollment part-c §C1, the
 * {@code get_config hit (default)} line) is tested in {@link DefaultHitLogTest}.
 * Adding it must leave every template here byte-identical (part-c §C6); the
 * hit-line filter ({@link HitLogLines#HIT_PREFIX}) counts the default variant
 * too, so a value hit that also emitted a default line fails these tests.
 *
 * <p><b>Run {@link #smokeListAppenderCapturesHitLine} first</b> (design §4
 * implementation-order note). It is the injected-degradation check for this
 * file's observation point: it proves the {@code ListAppender} attached to
 * {@code LoggerFactory.getLogger(TipsyAbConfigClient.class)} actually captures
 * the hit line. If the smoke test fails, every other green/red in this file is
 * meaningless — fix the capture (logback-classic test-scope dependency, design
 * §4 / pom) before reading the template assertions.
 *
 * <p><b>What these tests do NOT prove</b>: the reason values are asserted on
 * EMITTED lines (never on an enum declaration), but only for the scenarios
 * fabricated here — they say nothing about a real server's attribution content
 * (Goal 6 review + AC10 live test carry that). The mock-context case proves
 * only that the unattributed fallback works; it is NOT attribution-mechanism
 * coverage (design no-op form 3).
 *
 * <p><b>Test convention</b>: cases observing "second and later call" behaviour
 * (RPC counts) use a FRESH {@code AbtestContext} — per-ns results are memoised
 * per ctx ({@code AbtestContext} ensureFetch).
 */
final class GetConfigHitLogTest {

    private static final String NS = "checkout";

    // ------------------------------------------------------------------
    // Log capture helper.
    // ------------------------------------------------------------------

    /**
     * Attaches a ListAppender to the client's logger for one test's scope.
     *
     * <p>Create it BEFORE {@code AbtestTestSupport.build()} (first resource in
     * the try header). Attaching after the client exists races its background
     * threads' logging against logback's copy-on-write appender-list refresh,
     * and an event logged in that window can miss the new appender (review
     * NB-ST5-r2-2).
     */
    private static final class LogCapture implements AutoCloseable {
        final Logger logger =
                (Logger) LoggerFactory.getLogger(TipsyAbConfigClient.class);
        final ListAppender<ILoggingEvent> appender = new ListAppender<>();

        LogCapture() {
            appender.start();
            logger.addAppender(appender);
        }

        /**
         * A point-in-time snapshot of the captured events. NEVER stream/iterate
         * {@code appender.list} directly: it is a plain ArrayList that the
         * client's BACKGROUND threads (subscribe reconnect, periodic pull) keep
         * appending to while the test thread reads, and ArrayList iterators are
         * fail-fast — a mid-iteration append throws
         * ConcurrentModificationException (observed once as a real flake in
         * fastPathLogsFullReleaseWithoutRpc during the mutation run,
         * 2026-08-27). The copy constructor goes through {@code toArray}, which
         * is not fail-fast, so it cannot throw CME; a torn read of a
         * concurrently appended background line is harmless to these
         * assertions (they filter by message prefix/key).
         */
        List<ILoggingEvent> snapshot() {
            return new java.util.ArrayList<>(appender.list);
        }

        /** All captured events whose formatted message starts with {@code prefix}. */
        List<ILoggingEvent> withPrefix(String prefix) {
            return snapshot().stream()
                    .filter(e -> e.getFormattedMessage().startsWith(prefix))
                    .collect(Collectors.toList());
        }

        /**
         * The single get_config hit line — of ANY variant (abtest / full /
         * default) — whose PARSED {@code key} field equals {@code key}. Parsing
         * replaces the old {@code contains("key=" + key + ",")} substring filter:
         * the default template's field order is not pinned, so a trailing-comma
         * substring could miss a default line for the key.
         */
        ILoggingEvent singleHitLineForKey(String key) {
            List<ILoggingEvent> hits = snapshot().stream()
                    .filter(e -> HitLogLines.variant(e.getFormattedMessage()) != null)
                    .filter(e -> key.equals(HitLogLines.fields(e.getFormattedMessage()).get("key")))
                    .collect(Collectors.toList());
            assertEquals(1, hits.size(),
                    "expected exactly one hit line for key=" + key + ", got: " + hits);
            return hits.get(0);
        }

        @Override
        public void close() {
            logger.detachAppender(appender);
            appender.stop();
        }
    }

    private static ExperimentGroupResult group(String expId, String grpId, Map<String, Long> pv) {
        return ExperimentGroupResult.newBuilder()
                .setExperimentId(expId)
                .setGroupId(grpId)
                .setExperimentType(ExperimentType.EXPERIMENT_TYPE_CONFIG_VERSION)
                .putAllParamsVersions(pv)
                .build();
    }

    // ------------------------------------------------------------------
    // SMOKE — run this first. Gate for every template assertion below.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("SMOKE: the ListAppender attached to the client logger captures a get_config hit line")
    void smokeListAppenderCapturesHitLine() {
        try (LogCapture cap = new LogCapture();
             AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("color", 7L, Map.of(7L, "blue")))
                .abtestConfigFlatKv(NS, Map.of())
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-smoke", Map.of());
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"));

            // Deliberately LOOSE (contains, not equality): this test only proves
            // the observation point is live. The line's exact shape is pinned by
            // the template tests below, which are meaningless if this fails.
            assertTrue(cap.snapshot().stream()
                            .anyMatch(e -> e.getLevel() == Level.INFO
                                    && e.getFormattedMessage().contains("get_config hit")),
                    "the appender must capture at least one INFO get_config hit line; "
                            + "if this fails, fix the logback capture before trusting any "
                            + "template assertion in this file");
        }
    }

    // ------------------------------------------------------------------
    // reason=experiment (conditional fields: experiment_id + group_id).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("reason=experiment: fixed template line with experiment_id + group_id, no release_id")
    void experimentReasonTemplate() {
        GetExperimentResultResponse resp = GetExperimentResultResponse.newBuilder()
                .addGroups(group("exp-1", "grp-1", Map.of("color", 9L)))
                .build();
        try (LogCapture cap = new LogCapture();
             AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("color", 7L, Map.of(7L, "blue", 9L, "gold")))
                .build()) {
            h.abtest.setFullResponse(NS, resp);

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of(), "tr-exp");
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"));

            ILoggingEvent e = cap.singleHitLineForKey("color");
            assertEquals(Level.INFO, e.getLevel());
            // FULL equality (design §4 fixed template; containment banned here —
            // see the file header).
            assertEquals("tipsyabconfig: get_config hit (abtest) reason=experiment, "
                            + "ns=checkout, key=color, version=9, experiment_id=exp-1, "
                            + "group_id=grp-1, uid=u-1, trace_id=tr-exp",
                    e.getFormattedMessage());
            // Omit semantics, spelled out (already implied by the equality):
            assertFalse(e.getFormattedMessage().contains("release_id="),
                    "release_id must be omitted on an experiment hit");
        }
    }

    // ------------------------------------------------------------------
    // reason=gray_whitelist (conditional field: release_id).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("reason=gray_whitelist: fixed template line with release_id, no experiment_id/group_id")
    void grayWhitelistReasonTemplate() {
        GetExperimentResultResponse resp = GetExperimentResultResponse.newBuilder()
                .addGrayHits(GrayReleaseHit.newBuilder()
                        .setReleaseId(41L)
                        .putKeyVersions("color", 7L))
                .build();
        try (LogCapture cap = new LogCapture();
             AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("color", 3L, Map.of(3L, "full", 7L, "grayval")))
                .build()) {
            h.abtest.setFullResponse(NS, resp);

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of(), "tr-gray");
            assertEquals("grayval", h.client.getConfig(ctx, NS, "color", "DEF"));

            ILoggingEvent e = cap.singleHitLineForKey("color");
            assertEquals(Level.INFO, e.getLevel());
            assertEquals("tipsyabconfig: get_config hit (abtest) reason=gray_whitelist, "
                            + "ns=checkout, key=color, version=7, release_id=41, "
                            + "uid=u-1, trace_id=tr-gray",
                    e.getFormattedMessage());
            assertFalse(e.getFormattedMessage().contains("experiment_id="),
                    "experiment_id must be omitted on a gray hit");
            assertFalse(e.getFormattedMessage().contains("group_id="),
                    "group_id must be omitted on a gray hit");
        }
    }

    // ------------------------------------------------------------------
    // reason=abtest_unattributed: real merge path (empty-id group) + mock ctx.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("reason=abtest_unattributed (empty-id group): value resolves, no conditional fields; paired attributed key logs experiment")
    void unattributedReasonFromEmptyIdGroup() {
        // F3 pairing (design Testing Plan): the empty-id group rides with an
        // attributed group in the SAME response/capture, so the unattributed
        // line cannot pass vacuously in a build that never writes attribution
        // at all — the paired key must show reason=experiment in the same
        // capture, proving the attribution track is live.
        GetExperimentResultResponse resp = GetExperimentResultResponse.newBuilder()
                .addGroups(group("", "", Map.of("color", 9L)))
                .addGroups(group("exp-1", "grp-1", Map.of("banner", 12L)))
                .build();
        try (LogCapture cap = new LogCapture();
             AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2)
                        .key("color", 7L, Map.of(7L, "blue", 9L, "gold"))
                        .key("banner", 5L, Map.of(5L, "bfull", 12L, "bexp")))
                .build()) {
            h.abtest.setFullResponse(NS, resp);

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of(), "tr-unattr");
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "F3: the empty-id group's versionId must still resolve");
            assertEquals("bexp", h.client.getConfig(ctx, NS, "banner", "DEF"));

            assertEquals("tipsyabconfig: get_config hit (abtest) reason=abtest_unattributed, "
                            + "ns=checkout, key=color, version=9, uid=u-1, trace_id=tr-unattr",
                    cap.singleHitLineForKey("color").getFormattedMessage());
            assertEquals("tipsyabconfig: get_config hit (abtest) reason=experiment, "
                            + "ns=checkout, key=banner, version=12, experiment_id=exp-1, "
                            + "group_id=grp-1, uid=u-1, trace_id=tr-unattr",
                    cap.singleHitLineForKey("banner").getFormattedMessage());
        }
    }

    @Test
    @DisplayName("reason=abtest_unattributed (mock ctx): mock-seeded keyVersions log unattributed")
    void unattributedReasonFromMockContext() {
        // Scope note (design no-op form 3): a mock ctx seeds keyVersions with NO
        // attribution, so reason=abtest_unattributed here holds whether or not
        // the merge writes attribution correctly. This case proves only the
        // unattributed FALLBACK; it must not be counted as attribution coverage.
        try (LogCapture cap = new LogCapture();
             AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("color", 7L, Map.of(7L, "blue", 9L, "gold")))
                .build()) {

            AbtestContext ctx = h.client.mockAbtestContext("u-1", Map.of(NS, Map.of("color", 9L)));
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"));

            // mockAbtestContext generates its trace id; build the expectation
            // from the ctx accessor.
            assertEquals("tipsyabconfig: get_config hit (abtest) reason=abtest_unattributed, "
                            + "ns=checkout, key=color, version=9, uid=u-1, trace_id="
                            + ctx.traceId(),
                    cap.singleHitLineForKey("color").getFormattedMessage());
        }
    }

    // ------------------------------------------------------------------
    // reason=full_release: plain full hit, fast-path, and ab->full fallback.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("reason=full_release: fixed template line, no conditional fields")
    void fullReleaseReasonTemplate() {
        try (LogCapture cap = new LogCapture();
             AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("color", 7L, Map.of(7L, "blue")))
                .abtestConfigFlatKv(NS, Map.of()) // no ab hits
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of(), "tr-full");
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"));

            ILoggingEvent e = cap.singleHitLineForKey("color");
            assertEquals(Level.INFO, e.getLevel());
            assertEquals("tipsyabconfig: get_config hit (full) reason=full_release, "
                            + "ns=checkout, key=color, version=7, uid=u-1, trace_id=tr-full",
                    e.getFormattedMessage());
        }
    }

    @Test
    @DisplayName("fast-path (has_dynamic_resolution=false): reason=full_release AND zero abtest RPC")
    void fastPathLogsFullReleaseWithoutRpc() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue"))
                .hasDynamicResolution("color", Boolean.FALSE);
        try (LogCapture cap = new LogCapture();
             AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .abtestConfigFlatKv(NS, Map.of("color", 9L)) // would steer if (wrongly) asked
                .build()) {

            // FRESH ctx and ONLY the fast-path key queried on this link (see the
            // file-header convention): a reused ctx with a prior dynamic query
            // would make the RPC count trivially wrong in both directions.
            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of(), "tr-fast");
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"));

            assertEquals(0, h.abtest.callsFor(NS), "fast-path must not issue any abtest RPC");
            assertEquals("tipsyabconfig: get_config hit (full) reason=full_release, "
                            + "ns=checkout, key=color, version=7, uid=u-1, trace_id=tr-fast",
                    cap.singleHitLineForKey("color").getFormattedMessage());
        }
    }

    @Test
    @DisplayName("ab->full fallback (ab version not cached): WARN keeps trace_id; hit line is reason=full_release")
    void abToFullFallbackLogsFullReleaseAndWarn() {
        try (LogCapture cap = new LogCapture();
             AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("color", 7L, Map.of(7L, "blue")))
                .abtestConfigFlatKv(NS, Map.of("color", 99L)) // steers to an uncached version
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of(), "tr-fb");
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"));

            // The pre-existing WARN keeps its shape (design: 既有 WARN 不变) and
            // carries the trace_id.
            List<ILoggingEvent> warns = cap.withPrefix(
                    "tipsyabconfig: ab version missing in local cache");
            assertEquals(1, warns.size(), "exactly one ab->full fallback WARN");
            assertEquals(Level.WARN, warns.get(0).getLevel());
            assertEquals("tipsyabconfig: ab version missing in local cache; falling back to full "
                            + "(ns=checkout, key=color, ab_version=99, trace_id=tr-fb)",
                    warns.get(0).getFormattedMessage());

            // The hit line reports what actually took effect: the full release.
            assertEquals("tipsyabconfig: get_config hit (full) reason=full_release, "
                            + "ns=checkout, key=color, version=7, uid=u-1, trace_id=tr-fb",
                    cap.singleHitLineForKey("color").getFormattedMessage());
        }
    }

    @Test
    @DisplayName("RPC failure degrade: hit line is reason=full_release (value from the full release)")
    void rpcFailureDegradeLogsFullRelease() {
        try (LogCapture cap = new LogCapture();
             AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("color", 7L, Map.of(7L, "blue", 9L, "gold")))
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {
            h.abtest.failFor(NS);

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of(), "tr-deg");
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"));

            assertEquals("tipsyabconfig: get_config hit (full) reason=full_release, "
                            + "ns=checkout, key=color, version=7, uid=u-1, trace_id=tr-deg",
                    cap.singleHitLineForKey("color").getFormattedMessage());
        }
    }

    // ------------------------------------------------------------------
    // get_config_static: contract boundary — line unchanged, NO reason field.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("get_config_static keeps its legacy line: source=full_static, no reason, no uid (AC9)")
    void staticPathLineUnchanged() {
        // Design §4 contract boundary (r2-F7): the reason contract covers ONLY
        // dynamic getConfig/getConfigDefault. Downstream identifies enrollment
        // events by the PRESENCE of the reason field, so the static line must
        // keep not having one.
        try (LogCapture cap = new LogCapture();
             AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("color", 7L, Map.of(7L, "blue")))
                .build()) {

            assertEquals(java.util.Optional.of("blue"), h.client.getConfigStatic(NS, "color"));

            List<ILoggingEvent> lines = cap.withPrefix("tipsyabconfig: get_config_static hit");
            assertEquals(1, lines.size(), "exactly one static hit line");
            assertEquals("tipsyabconfig: get_config_static hit "
                            + "(ns=checkout, key=color, version=7, source=full_static)",
                    lines.get(0).getFormattedMessage());
            assertFalse(lines.get(0).getFormattedMessage().contains("reason="),
                    "static line must NOT grow a reason field (enrollment-event discriminator)");
        }
    }
}
