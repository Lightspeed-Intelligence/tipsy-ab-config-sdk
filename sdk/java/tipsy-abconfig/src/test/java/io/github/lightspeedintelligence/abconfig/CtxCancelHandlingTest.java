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
 *   <li>subscribe / periodic-pull loops: only the SDK's own {@code close()}
 *       is a silent exit (no ERROR log, failure-counter delta == 0, no
 *       {@link BackgroundErrorEvent}). The loops run on the ROOT Context, so a
 *       CANCELLED they receive was sent by the server or a proxy: it is a
 *       fault — reported, and the loop keeps reconnecting / ticking. (Until
 *       2026-10 the loops exited on it, stopping pull/subscribe for the
 *       process lifetime with no signal; the {@code *RemoteCancelled*} tests
 *       pin the fix.)</li>
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
    // Subscribe loop: a server-sent CANCELLED is a fault -> report + reconnect.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("subscribe remote CANCELLED: reported (ERROR, metric, event) and reconnects")
    void subscribeRemoteCancelled_reportsAndReconnects() {
        harness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshot(NS, 1, 0, "color", 11, "blue")));
        harness.setSubscribeHandler((req, obs) -> obs.onError(
                Status.CANCELLED.withDescription("server/proxy cancelled the stream").asRuntimeException()));

        ConcurrentLinkedQueue<BackgroundErrorEvent> events = new ConcurrentLinkedQueue<>();
        try (LogCapture cap = new LogCapture()) {
            client = TipsyAbConfigClient.create(cfg(NS)
                    .pullInterval(Duration.ofMinutes(10)) // keep the pull loop out of the way
                    .onBackgroundError(events::add)
                    .build());

            // Reconnect after the initial 1s backoff (pre-fix: the loop
            // returned and subscribeCalls stayed at 1 forever).
            assertTrue(InProcessConfigServiceHarness.awaitTrue(
                            () -> harness.subscribeCalls.get() >= 2, POLL_BUDGET_MS),
                    "a remote CANCELLED must not stop the subscribe loop (calls="
                            + harness.subscribeCalls.get() + ")");
            assertTrue(client.metrics().subscribeDisconnectTotal(NS) > 0,
                    "a remote CANCELLED must count as a disconnect");
            assertTrue(events.stream().anyMatch(e -> "subscribe".equals(e.phase())),
                    "a remote CANCELLED must fire a subscribe BackgroundErrorEvent");
            assertFalse(cap.atLevel(Level.ERROR).isEmpty(),
                    "a remote CANCELLED must log the reconnect ERROR");
        }
    }

    // ------------------------------------------------------------------
    // Periodic pull loop: a server-sent CANCELLED is a fault -> report + tick on.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("periodic pull remote CANCELLED: reported (ERROR, metric, event) and keeps ticking")
    void periodicPullRemoteCancelled_reportsAndKeepsTicking() {
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

            handler.set(req -> {
                throw Status.CANCELLED.withDescription("server/proxy cancelled the call").asRuntimeException();
            });

            assertTrue(InProcessConfigServiceHarness.awaitTrue(
                            () -> client.metrics().pullFailureTotal(NS) > 0, POLL_BUDGET_MS),
                    "a remote CANCELLED must count as a pull failure");
            // The loop must keep ticking (pre-fix: it returned after one hit).
            int afterFirstFailure = harness.pullCalls.get();
            assertTrue(InProcessConfigServiceHarness.awaitTrue(
                            () -> harness.pullCalls.get() > afterFirstFailure + 2, POLL_BUDGET_MS),
                    "the pull loop must keep ticking after a remote CANCELLED");
            assertFalse(aliveThreadsNamed("tipsyabconfig-pull", preexistingThreads).isEmpty(),
                    "the pull thread must still be alive");
            assertTrue(events.stream().anyMatch(e -> "periodic_pull".equals(e.phase())),
                    "a remote CANCELLED must fire a periodic_pull BackgroundErrorEvent");
            assertFalse(cap.atLevel(Level.ERROR).isEmpty(),
                    "a remote CANCELLED must log the periodic PullAll ERROR");
        }
    }

    // ------------------------------------------------------------------
    // close() with a pull in flight and the stream attached: silent exit.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("close() mid-pull with stream attached: both loops exit silently")
    void closeMidFlight_exitsSilently() throws Exception {
        PullAllResponseHolder ok = new PullAllResponseHolder();
        InProcessConfigServiceHarness.SwitchablePullHandler handler =
                new InProcessConfigServiceHarness.SwitchablePullHandler(req -> ok.response);
        harness.setPullHandler(handler);
        harness.setSubscribeHandler((req, obs) -> { /* keep open */ });

        ConcurrentLinkedQueue<BackgroundErrorEvent> events = new ConcurrentLinkedQueue<>();
        java.util.concurrent.CountDownLatch entered = new java.util.concurrent.CountDownLatch(1);
        java.util.concurrent.CountDownLatch release = new java.util.concurrent.CountDownLatch(1);
        try (LogCapture cap = new LogCapture()) {
            client = TipsyAbConfigClient.create(cfg(NS)
                    .pullInterval(Duration.ofMillis(120))
                    .pullTimeout(Duration.ofSeconds(5))
                    .onBackgroundError(events::add)
                    .build());
            assertTrue(InProcessConfigServiceHarness.awaitTrue(
                            () -> harness.subscribeCalls.get() >= 1, POLL_BUDGET_MS),
                    "the subscribe stream must be attached");

            // Park the next periodic pull on the server so close() lands mid-call.
            handler.set(req -> {
                entered.countDown();
                try {
                    release.await(5, java.util.concurrent.TimeUnit.SECONDS);
                } catch (InterruptedException ie) {
                    Thread.currentThread().interrupt();
                }
                return ok.response;
            });
            assertTrue(entered.await(POLL_BUDGET_MS, java.util.concurrent.TimeUnit.MILLISECONDS),
                    "a periodic pull must be in flight");

            client.close();
            release.countDown();

            assertEquals(0L, client.metrics().pullFailureTotal(NS), "close() is not a pull failure");
            assertEquals(0L, client.metrics().subscribeDisconnectTotal(NS), "close() is not a disconnect");
            assertTrue(events.isEmpty(), "close() must not fire BackgroundErrorEvents, got: " + events);
            List<ILoggingEvent> errors = cap.atLevel(Level.ERROR);
            assertTrue(errors.isEmpty(),
                    "close() must not log ERROR, got: " + errors.stream()
                            .map(ILoggingEvent::getFormattedMessage).collect(Collectors.toList()));
        } finally {
            release.countDown();
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
