package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import ch.qos.logback.classic.Level;
import ch.qos.logback.classic.Logger;
import ch.qos.logback.classic.spi.ILoggingEvent;
import ch.qos.logback.core.read.ListAppender;
import io.github.lightspeedintelligence.abconfig.AbtestTestSupport.NsCache;
import io.grpc.Status;
import io.grpc.StatusException;
import java.time.Duration;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentLinkedQueue;
import java.util.stream.Collectors;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.slf4j.LoggerFactory;

/**
 * Issue #15 (phase 2): a caller-context cancellation (gRPC {@code CANCELLED})
 * is expected termination, NOT a fault. These tests pin the whole contract:
 *
 * <ul>
 *   <li>{@link TipsyAbConfigClient#isCancelled} truth table (deliberately
 *       narrow: exact {@link StatusRuntimeException} + {@code CANCELLED};
 *       {@code UNAVAILABLE} — the shape {@code close()}'s
 *       {@code channel.shutdownNow()} produces — stays false and remains
 *       covered by the {@code closed.get()} guards).</li>
 *   <li>subscribe / periodic-pull loops: an injected CANCELLED exits silently —
 *       no ERROR log, failure-counter delta == 0, no
 *       {@link BackgroundErrorEvent} (so {@code subscribeConnected} is never
 *       misreported false).</li>
 *   <li>abtest fetch: CANCELLED logs at INFO, bumps the NEW
 *       {@code abtestCanceled} counter, does NOT bump {@code abtestFallback},
 *       and the value semantics are unchanged (degrade to full release).</li>
 *   <li>real-error regression: a non-CANCELLED failure keeps the historical
 *       WARN + {@code abtestFallback} behaviour byte-for-byte (loop-level real
 *       errors are already pinned by {@code GrpcClientLifecycleTest}).</li>
 * </ul>
 *
 * <p>Log assertions reuse the {@code ListAppender} capture pattern from
 * {@code GetConfigHitLogTest}, including its snapshot() discipline (never
 * iterate {@code appender.list} directly: background threads keep appending and
 * ArrayList iterators are fail-fast).
 *
 * <p>Convention: every scenario observing a fetch outcome uses a FRESH
 * {@link AbtestContext} — per-ns results are memoised per ctx.
 */
final class CtxCancelHandlingTest {

    private static final String NS = "checkout";
    private static final long POLL_BUDGET_MS = 5_000;

    // ------------------------------------------------------------------
    // Log capture (GetConfigHitLogTest pattern, snapshot() discipline).
    // ------------------------------------------------------------------

    private static final class LogCapture implements AutoCloseable {
        final Logger logger =
                (Logger) LoggerFactory.getLogger(TipsyAbConfigClient.class);
        final ListAppender<ILoggingEvent> appender = new ListAppender<>();

        LogCapture() {
            appender.start();
            logger.addAppender(appender);
        }

        /** Point-in-time copy (toArray-backed, cannot throw CME). */
        List<ILoggingEvent> snapshot() {
            return new java.util.ArrayList<>(appender.list);
        }

        List<ILoggingEvent> atLevel(Level level) {
            return snapshot().stream()
                    .filter(e -> e.getLevel() == level)
                    .collect(Collectors.toList());
        }

        @Override
        public void close() {
            logger.detachAppender(appender);
            appender.stop();
        }
    }

    // ------------------------------------------------------------------
    // Background-thread observation helpers.
    // ------------------------------------------------------------------

    /** Alive threads with exactly this name, minus a pre-existing set. */
    private static List<Thread> aliveThreadsNamed(String name, Set<Thread> excluding) {
        return Thread.getAllStackTraces().keySet().stream()
                .filter(t -> name.equals(t.getName()) && t.isAlive() && !excluding.contains(t))
                .collect(Collectors.toList());
    }

    private Set<Thread> preexistingThreads;
    private InProcessConfigServiceHarness harness;
    private TipsyAbConfigClient client;

    @BeforeEach
    void setUp() throws Exception {
        // Threads leaked-but-dying from other test classes must not confuse the
        // "our loop thread exited" observations below.
        preexistingThreads = Thread.getAllStackTraces().keySet();
        harness = new InProcessConfigServiceHarness();
    }

    @AfterEach
    void tearDown() {
        if (client != null) {
            client.close();
            client = null;
        }
        if (harness != null) {
            harness.close();
            harness = null;
        }
    }

    /** gRPC-mode builder wired to the harness (GrpcClientLifecycleTest shape). */
    private Config.Builder cfg(String... namespaces) {
        return Config.builder()
                .namespaces(namespaces)
                .configServiceAddr("passthrough:///x")
                .token("tok")
                .transport(Transport.GRPC)
                .channelConfigurator(harness.channelConfigurator())
                .pullTimeout(Duration.ofSeconds(2));
    }

    // ------------------------------------------------------------------
    // isCancelled truth table.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("isCancelled: true only for StatusRuntimeException(CANCELLED)")
    void isCancelledTruthTable() {
        assertTrue(TipsyAbConfigClient.isCancelled(
                        Status.CANCELLED.withDescription("Context cancelled").asRuntimeException()),
                "SRE CANCELLED (upstream ctx cancel) must be recognised");
        assertTrue(TipsyAbConfigClient.isCancelled(
                        Status.CANCELLED.withDescription("Thread interrupted").asRuntimeException()),
                "SRE CANCELLED (executor shutdownNow interrupt) must be recognised");

        assertFalse(TipsyAbConfigClient.isCancelled(
                        Status.UNAVAILABLE.withDescription("Channel shutdownNow invoked").asRuntimeException()),
                "UNAVAILABLE (close()'s channel.shutdownNow shape) is NOT a ctx-cancel: "
                        + "the closed.get() guards own that path");
        assertFalse(TipsyAbConfigClient.isCancelled(
                        Status.DEADLINE_EXCEEDED.asRuntimeException()),
                "DeadlineExceeded is a real timeout, out of #15 scope");
        assertFalse(TipsyAbConfigClient.isCancelled(Status.INTERNAL.asRuntimeException()),
                "other status codes stay real errors");
        assertFalse(TipsyAbConfigClient.isCancelled(new StatusException(Status.CANCELLED)),
                "the checked StatusException is outside the narrow contract "
                        + "(blocking stubs throw the runtime form)");
        assertFalse(TipsyAbConfigClient.isCancelled(new RuntimeException("boom")),
                "a plain exception is not a ctx-cancel");
        assertFalse(TipsyAbConfigClient.isCancelled(null), "null is not a ctx-cancel");
    }

    // ------------------------------------------------------------------
    // Subscribe loop: CANCELLED exits silently.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("subscribe CANCELLED: silent exit — no ERROR, no metric, no event")
    void subscribeCancelled_exitsSilently() {
        harness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshot(NS, 1, 0, "color", 11, "blue")));
        harness.setSubscribeHandler((req, obs) -> obs.onError(
                Status.CANCELLED.withDescription("upstream context cancelled").asRuntimeException()));

        ConcurrentLinkedQueue<BackgroundErrorEvent> events = new ConcurrentLinkedQueue<>();
        try (LogCapture cap = new LogCapture()) {
            client = TipsyAbConfigClient.create(cfg(NS)
                    .pullInterval(Duration.ofMinutes(10)) // keep the pull loop out of the way
                    .onBackgroundError(events::add)
                    .build());

            // The stream must have been attempted, then the loop must EXIT
            // (silent return), not reconnect.
            assertTrue(InProcessConfigServiceHarness.awaitTrue(
                            () -> harness.subscribeCalls.get() >= 1, POLL_BUDGET_MS),
                    "the subscribe stream must have been attempted");
            assertTrue(InProcessConfigServiceHarness.awaitTrue(
                            () -> aliveThreadsNamed("tipsyabconfig-subscribe", preexistingThreads).isEmpty(),
                            POLL_BUDGET_MS),
                    "a ctx-cancelled subscribe loop must exit (silent return)");

            // No reconnect was scheduled (a mis-classified real error would back
            // off 1s then redial — the thread would still be alive above, and
            // the call count would grow here).
            assertEquals(1, harness.subscribeCalls.get(),
                    "no reconnect after a ctx-cancel");
            // Failure surface: all three channels must stay silent.
            assertEquals(0L, client.metrics().subscribeDisconnectTotal(NS),
                    "ctx-cancel must not count as a disconnect");
            assertTrue(events.stream().noneMatch(e -> "subscribe".equals(e.phase())),
                    "ctx-cancel must not fire a subscribe BackgroundErrorEvent");
            assertTrue(client.health().lastSubscribeErr().isEmpty(),
                    "ctx-cancel must not record lastSubscribeErr");
            assertTrue(client.health().subscribeConnected(),
                    "ctx-cancel must not misreport subscribeConnected=false");
            List<ILoggingEvent> errors = cap.atLevel(Level.ERROR);
            assertTrue(errors.isEmpty(),
                    "ctx-cancel must not log ERROR, got: " + errors.stream()
                            .map(ILoggingEvent::getFormattedMessage).collect(Collectors.toList()));
        }
    }

    // ------------------------------------------------------------------
    // Periodic pull loop: CANCELLED exits silently.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("periodic pull CANCELLED: silent exit — no ERROR, no metric, no event")
    void periodicPullCancelled_exitsSilently() {
        PullAllResponseHolder ok = new PullAllResponseHolder();
        InProcessConfigServiceHarness.SwitchablePullHandler handler =
                new InProcessConfigServiceHarness.SwitchablePullHandler(req -> ok.response);
        harness.setPullHandler(handler);
        // Park the subscribe stream (leave it open) so only the pull loop acts.
        harness.setSubscribeHandler((req, obs) -> { /* keep open */ });

        ConcurrentLinkedQueue<BackgroundErrorEvent> events = new ConcurrentLinkedQueue<>();
        try (LogCapture cap = new LogCapture()) {
            client = TipsyAbConfigClient.create(cfg(NS)
                    .pullInterval(Duration.ofMillis(120))
                    .pullTimeout(Duration.ofMillis(500))
                    .onBackgroundError(events::add)
                    .build());
            int startupPulls = harness.pullCalls.get();

            // Break the backend with a ctx-cancel-shaped failure.
            handler.set(req -> {
                throw Status.CANCELLED.withDescription("caller context canceled").asRuntimeException();
            });

            // One tick must land on the CANCELLED handler, then the loop exits.
            assertTrue(InProcessConfigServiceHarness.awaitTrue(
                            () -> harness.pullCalls.get() > startupPulls, POLL_BUDGET_MS),
                    "a periodic tick must have hit the CANCELLED handler");
            assertTrue(InProcessConfigServiceHarness.awaitTrue(
                            () -> aliveThreadsNamed("tipsyabconfig-pull", preexistingThreads).isEmpty(),
                            POLL_BUDGET_MS),
                    "a ctx-cancelled pull loop must exit (silent return)");

            assertEquals(0L, client.metrics().pullFailureTotal(NS),
                    "ctx-cancel must not count as a pull failure");
            assertTrue(events.stream().noneMatch(e -> "periodic_pull".equals(e.phase())),
                    "ctx-cancel must not fire a periodic_pull BackgroundErrorEvent");
            assertTrue(client.health().lastPullErr().isEmpty(),
                    "ctx-cancel must not record lastPullErr");
            List<ILoggingEvent> errors = cap.atLevel(Level.ERROR);
            assertTrue(errors.isEmpty(),
                    "ctx-cancel must not log ERROR, got: " + errors.stream()
                            .map(ILoggingEvent::getFormattedMessage).collect(Collectors.toList()));
        }
    }

    /** Tiny holder so the initial pull handler stays a lambda-capturable final. */
    private static final class PullAllResponseHolder {
        final io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllResponse response =
                InProcessConfigServiceHarness.pullResponse(
                        InProcessConfigServiceHarness.snapshot(NS, 1, 0, "k", 9, "v"));
    }

    // ------------------------------------------------------------------
    // Abtest fetch: CANCELLED -> INFO + abtestCanceled; value degrades to full.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("abtest CANCELLED: INFO + abtestCanceled==1, abtestFallback==0, value = full release")
    void abtestCancelled_logsInfoAndBumpsCanceledCounterOnly() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue", 9L, "gold"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                     .namespaces(NS)
                     .snapshot(NS, cache)
                     .build();
             LogCapture cap = new LogCapture()) {
            h.abtest.failWithStatus(NS,
                    Status.CANCELLED.withDescription("Context cancelled"));

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of(), "trace-cancel-1");
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "value semantics unchanged: ctx-cancel degrades to the full release value");

            assertEquals(1L, h.client.metrics().abtestCanceledTotal(NS),
                    "ctx-cancel bumps the dedicated abtestCanceled counter exactly once");
            assertEquals(0L, h.client.metrics().abtestFallbackTotal(NS),
                    "ctx-cancel must NOT count as an abtest fallback failure");

            // Exactly one INFO line with the full fixed shape; no WARN fallback line.
            List<String> infoLines = cap.atLevel(Level.INFO).stream()
                    .map(ILoggingEvent::getFormattedMessage)
                    .filter(m -> m.startsWith("tipsyabconfig: abtest fetch canceled"))
                    .collect(Collectors.toList());
            assertEquals(List.of(
                            "tipsyabconfig: abtest fetch canceled by caller context; falling back to full release"
                                    + " (ns=checkout, trace_id=trace-cancel-1,"
                                    + " err=io.grpc.StatusRuntimeException: CANCELLED: Context cancelled)"),
                    infoLines,
                    "the ctx-cancel line is INFO with the full fixed shape");
            List<ILoggingEvent> warns = cap.atLevel(Level.WARN).stream()
                    .filter(e -> e.getFormattedMessage().contains("falling back to full release"))
                    .collect(Collectors.toList());
            assertTrue(warns.isEmpty(), "ctx-cancel must not produce the WARN fallback line, got: "
                    + warns.stream().map(ILoggingEvent::getFormattedMessage).collect(Collectors.toList()));

            // Memoisation note: a SECOND observation needs a FRESH ctx (and the
            // counter moves again), proving the accounting is per-fetch.
            AbtestContext ctx2 = h.client.newAbtestContext("u-1", Map.of(), "trace-cancel-2");
            assertEquals("blue", h.client.getConfig(ctx2, NS, "color", "DEF"));
            assertEquals(2L, h.client.metrics().abtestCanceledTotal(NS));
            assertEquals(0L, h.client.metrics().abtestFallbackTotal(NS));
        }
    }

    // ------------------------------------------------------------------
    // Abtest real error regression: WARN + abtestFallback, abtestCanceled==0.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("abtest real error (INTERNAL): WARN + abtestFallback==1, abtestCanceled==0 (regression)")
    void abtestRealError_keepsWarnAndFallbackCounter() {
        NsCache cache = new NsCache(2, 2)
                .key("color", 7L, Map.of(7L, "blue"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                     .namespaces(NS)
                     .snapshot(NS, cache)
                     .build();
             LogCapture cap = new LogCapture()) {
            h.abtest.failWithStatus(NS, Status.INTERNAL.withDescription("backend boom"));

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of(), "trace-real-1");
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"));

            assertEquals(1L, h.client.metrics().abtestFallbackTotal(NS),
                    "a real abtest failure keeps the historical fallback accounting");
            assertEquals(0L, h.client.metrics().abtestCanceledTotal(NS),
                    "a real abtest failure must not touch abtestCanceled");

            // The WARN line keeps its historical wording byte-for-byte.
            List<String> warnLines = cap.atLevel(Level.WARN).stream()
                    .map(ILoggingEvent::getFormattedMessage)
                    .filter(m -> m.contains("falling back to full release"))
                    .collect(Collectors.toList());
            assertEquals(List.of(
                            "tipsyabconfig: AbtestService.GetExperimentResult failed; falling back to full release"
                                    + " (ns=checkout, trace_id=trace-real-1)"),
                    warnLines,
                    "the real-error WARN line is unchanged");
            assertTrue(cap.atLevel(Level.INFO).stream()
                            .noneMatch(e -> e.getFormattedMessage()
                                    .startsWith("tipsyabconfig: abtest fetch canceled")),
                    "a real error must not produce the ctx-cancel INFO line");
        }
    }
}
