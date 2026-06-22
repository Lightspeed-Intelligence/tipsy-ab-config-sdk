package io.tipsy.abconfig;

import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.tipsy.abconfig.proto.config.v1.NamespaceSnapshot;
import io.tipsy.abconfig.proto.config.v1.PullAllResponse;
import java.time.Duration;
import java.util.List;
import java.util.Optional;
import java.util.concurrent.ConcurrentLinkedQueue;
import java.util.concurrent.atomic.AtomicBoolean;
import java.util.concurrent.atomic.AtomicInteger;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/**
 * Lifecycle + background-loop tests for {@link TipsyAbConfigClient} in gRPC mode,
 * driven through the in-process channel injection seam (no sockets, no DEV).
 *
 * <p>Design refs:
 * <ul>
 *   <li>03-core-client-api.md §"TipsyAbConfigClient" / §"Health" / §"Metrics"
 *       (startup contract, fail-open, health snapshot semantics).</li>
 *   <li>04-transport-and-cache.md §"PullAll" (startup sweep + retries + periodic
 *       loop), §"Subscribe" (server-stream, three branches), §"fireBackgroundError"
 *       (callback resilience).</li>
 * </ul>
 *
 * <p>All asynchronous assertions use bounded polling
 * ({@link InProcessConfigServiceHarness#awaitTrue}) rather than fixed sleeps to
 * stay deterministic and avoid flakiness.
 */
final class GrpcClientLifecycleTest {

    private static final long POLL_BUDGET_MS = 3_000;

    private InProcessConfigServiceHarness harness;
    private TipsyAbConfigClient client;

    @BeforeEach
    void setUp() throws Exception {
        harness = new InProcessConfigServiceHarness();
    }

    @AfterEach
    void tearDown() {
        if (client != null) {
            client.close();
        }
        if (harness != null) {
            harness.close();
        }
    }

    // ------------------------------------------------------------------
    // startup pull success
    // ------------------------------------------------------------------

    @Test
    void startupPullSuccess_populatesCacheAndExposesNamespaces() {
        harness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshot("checkout", 2, 1, "color", 11, "blue")));
        client = TipsyAbConfigClient.create(cfg("checkout").build());

        // getConfigStatic hits the cache populated by the startup sweep.
        Optional<String> v = client.getConfigStatic("checkout", "color");
        assertTrue(v.isPresent());
        assertEquals("blue", v.get());

        // namespaces() is a sorted copy.
        assertEquals(List.of("checkout"), client.namespaces());
        // startup cache was NOT empty.
        assertFalse(client.health().startupCacheEmpty());
    }

    @Test
    void namespaces_returnsSortedDeduplicatedCopy() {
        harness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse());
        client = TipsyAbConfigClient.create(
                cfg("orders", "checkout", "checkout").startupFailOpen(true).build());
        // sorted + de-duped
        assertEquals(List.of("checkout", "orders"), client.namespaces());

        // defensive copy: mutating the returned list must not affect the client.
        List<String> ns = client.namespaces();
        ns.add("mutated");
        assertEquals(List.of("checkout", "orders"), client.namespaces());
    }

    @Test
    void defaultNamespace_isResolvedFromConfigOverride() {
        harness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse());
        client = TipsyAbConfigClient.create(
                cfg("checkout").defaultNamespace("checkout").startupFailOpen(true).build());
        assertEquals("checkout", client.defaultNamespace());
    }

    @Test
    void getConfigStatic_emptyStringValueIsAHit() {
        harness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshot("checkout", 1, 0, "flag", 5, "")));
        client = TipsyAbConfigClient.create(cfg("checkout").build());
        Optional<String> v = client.getConfigStatic("checkout", "flag");
        assertTrue(v.isPresent(), "empty string is a valid cached value");
        assertEquals("", v.get());
    }

    // ------------------------------------------------------------------
    // startup pull failure: fail-close vs fail-open
    // ------------------------------------------------------------------

    @Test
    void startupPullFailure_failClose_throwsStartupPullFailed() {
        harness.setPullHandler(req -> {
            throw new RuntimeException("backend down");
        });
        // pullRetries=1 + tiny timeout to keep the test fast.
        Config c = cfg("checkout")
                .pullRetries(1)
                .pullTimeout(Duration.ofMillis(200))
                .startupFailOpen(false)
                .build();
        assertThrows(StartupPullFailedException.class, () -> TipsyAbConfigClient.create(c));
    }

    @Test
    void startupPullFailure_failOpen_startsEmptyAndFiresStartupEvent() {
        harness.setPullHandler(req -> {
            throw new RuntimeException("backend down");
        });
        ConcurrentLinkedQueue<BackgroundErrorEvent> events = new ConcurrentLinkedQueue<>();
        Config c = cfg("checkout")
                .pullRetries(1)
                .pullTimeout(Duration.ofMillis(200))
                .startupFailOpen(true)
                .pullInterval(Duration.ofMinutes(10)) // keep periodic loop out of the way
                .onBackgroundError(events::add)
                .build();

        client = TipsyAbConfigClient.create(c); // must NOT throw under fail-open

        assertTrue(client.health().startupCacheEmpty(),
                "fail-open must leave startupCacheEmpty=true");
        // The aggregate startup_pull event must have been delivered.
        BackgroundErrorEvent startup = events.stream()
                .filter(e -> "startup_pull".equals(e.phase()))
                .findFirst()
                .orElse(null);
        assertNotNull(startup, "a startup_pull background-error event must be fired under fail-open");
        assertEquals("", startup.namespace(), "the aggregate startup event carries the empty namespace");
        assertNotNull(startup.error());
        // cache_empty metric incremented.
        assertTrue(client.metrics().cacheEmptyTotal() >= 1,
                "fail-open must increment cacheEmptyTotal");
        // and the cache really is empty.
        assertFalse(client.getConfigStatic("checkout", "color").isPresent());
    }

    // ------------------------------------------------------------------
    // startup retries: fail N-1 times, succeed on the Nth
    // ------------------------------------------------------------------

    @Test
    void startupPull_retriesUntilSuccess() {
        PullAllResponse success = InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshot("checkout", 1, 0, "k", 9, "v"));
        // Fail the first 2 attempts, succeed on the 3rd. With pullRetries=3 the
        // sweep succeeds. Backoff starts at 200ms so the test stays sub-second.
        InProcessConfigServiceHarness.FailNTimesThenSucceed handler =
                new InProcessConfigServiceHarness.FailNTimesThenSucceed(2, success);
        harness.setPullHandler(handler);

        Config c = cfg("checkout")
                .pullRetries(3)
                .pullTimeout(Duration.ofMillis(200))
                .startupFailOpen(false)
                .build();
        client = TipsyAbConfigClient.create(c); // must succeed via retry

        assertEquals(3, handler.callCount(), "exactly 3 pull attempts (2 failures + 1 success)");
        assertFalse(client.health().startupCacheEmpty());
        assertEquals(Optional.of("v"), client.getConfigStatic("checkout", "k"));
    }

    // ------------------------------------------------------------------
    // Subscribe: a pushed snapshot event updates the cache + metrics + health
    // ------------------------------------------------------------------

    @Test
    void subscribe_pushedSnapshotUpdatesCacheAndMetricsAndHealth() {
        // startup pull returns business_seq=1 so the subscribe event (seq=2) is
        // a strict advance and is applied.
        harness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshot("checkout", 1, 0, "color", 11, "blue")));

        // Subscribe handler: push one newer snapshot, then keep the stream open
        // (do not complete) so subscribeConnected stays true. Push from a daemon
        // thread to avoid blocking the RPC open path under directExecutor.
        NamespaceSnapshot pushed =
                InProcessConfigServiceHarness.snapshot("checkout", 2, 0, "color", 12, "green");
        harness.setSubscribeHandler((req, obs) -> {
            Thread t = new Thread(() -> {
                obs.onNext(InProcessConfigServiceHarness.snapshotEvent(pushed));
                // intentionally leave the stream open
            }, "test-subscribe-pusher");
            t.setDaemon(true);
            t.start();
        });

        client = TipsyAbConfigClient.create(cfg("checkout")
                .pullInterval(Duration.ofMinutes(10)) // isolate subscribe from periodic pull
                .build());

        // Poll until the subscribe event was received + the cache was updated.
        boolean updated = InProcessConfigServiceHarness.awaitTrue(
                () -> client.metrics().subscribeEventReceivedTotal("checkout") >= 1
                        && client.getConfigStatic("checkout", "color")
                                 .map("green"::equals).orElse(false),
                POLL_BUDGET_MS);
        assertTrue(updated, "subscribe event must update the cache and bump the event metric");

        // subscribeConnected flips true once the stream is draining.
        assertTrue(InProcessConfigServiceHarness.awaitTrue(
                        () -> client.health().subscribeConnected(), POLL_BUDGET_MS),
                "subscribeConnected must become true once the stream is established");
        // the new full-release version is what getConfigStatic returns.
        assertEquals(Optional.of("green"), client.getConfigStatic("checkout", "color"));
    }

    // ------------------------------------------------------------------
    // periodic pull loop: a failing tick bumps the failure metric + fires event
    // ------------------------------------------------------------------

    @Test
    void periodicPull_failingTick_bumpsMetricAndFiresPeriodicEvent() {
        PullAllResponse ok = InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshot("checkout", 1, 0, "k", 9, "v"));
        // Start healthy so the startup sweep succeeds, then break so the next
        // periodic tick fails.
        InProcessConfigServiceHarness.SwitchablePullHandler handler =
                new InProcessConfigServiceHarness.SwitchablePullHandler(req -> ok);
        harness.setPullHandler(handler);

        ConcurrentLinkedQueue<BackgroundErrorEvent> events = new ConcurrentLinkedQueue<>();
        client = TipsyAbConfigClient.create(cfg("checkout")
                .pullInterval(Duration.ofMillis(150)) // short interval to trigger ticks quickly
                .pullTimeout(Duration.ofMillis(500))
                .onBackgroundError(events::add)
                .build());

        // Now break the backend so the next periodic tick fails.
        handler.set(req -> {
            throw new RuntimeException("periodic boom");
        });

        boolean failed = InProcessConfigServiceHarness.awaitTrue(
                () -> client.metrics().pullFailureTotal("checkout") >= 1
                        && events.stream().anyMatch(e -> "periodic_pull".equals(e.phase())),
                POLL_BUDGET_MS);
        assertTrue(failed, "a failing periodic tick must bump pullFailureTotal and fire a periodic_pull event");

        // The periodic_pull health error is recorded (and sticky).
        assertTrue(InProcessConfigServiceHarness.awaitTrue(
                        () -> client.health().lastPullErr().isPresent(), POLL_BUDGET_MS),
                "a periodic pull failure must record lastPullErr");

        BackgroundErrorEvent ev = events.stream()
                .filter(e -> "periodic_pull".equals(e.phase()))
                .findFirst().orElseThrow();
        assertEquals("checkout", ev.namespace(),
                "a periodic_pull event carries the failing namespace");
    }

    // ------------------------------------------------------------------
    // Subscribe clean EOF: a cleanly-closed stream is reconnected immediately
    // (design 04 §Subscribe three-branch: clean EOF resets backoff + reconnects).
    // ------------------------------------------------------------------

    @Test
    void subscribe_cleanEof_reconnectsImmediately() {
        harness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshot("checkout", 1, 0, "color", 11, "blue")));
        // Each subscribe attempt completes cleanly at once (server-side EOF). The
        // SDK must reset its backoff to 1s and reconnect immediately, so the
        // subscribe call count climbs past the first attempt within the budget.
        harness.setSubscribeHandler((req, obs) -> obs.onCompleted());

        client = TipsyAbConfigClient.create(cfg("checkout")
                .pullInterval(Duration.ofMinutes(10))
                .build());

        boolean reconnected = InProcessConfigServiceHarness.awaitTrue(
                () -> harness.subscribeCalls.get() >= 2, POLL_BUDGET_MS);
        assertTrue(reconnected,
                "a clean EOF must reset the backoff and reconnect immediately (multiple attempts)");
        // A clean EOF is NOT an error: no subscribe disconnect metric, no sticky
        // subscribe error recorded.
        assertEquals(0, client.metrics().subscribeDisconnectTotal("checkout"),
                "a clean EOF must not count as a disconnect");
        assertTrue(client.health().lastSubscribeErr().isEmpty(),
                "a clean EOF must not record a subscribe error");
    }

    // ------------------------------------------------------------------
    // Subscribe real error: a non-cancel stream error bumps the disconnect
    // metric, fires a subscribe event, flips subscribeConnected false, then the
    // loop backs off and reconnects (design 04 §Subscribe three-branch: real error).
    // ------------------------------------------------------------------

    @Test
    void subscribe_realError_bumpsDisconnectMetricAndFiresSubscribeEvent() {
        harness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshot("checkout", 1, 0, "color", 11, "blue")));

        // First subscribe attempt errors with a real (non-CANCELLED) status;
        // later attempts keep the stream open so the error churn is bounded.
        AtomicInteger attempts = new AtomicInteger();
        harness.setSubscribeHandler((req, obs) -> {
            int n = attempts.incrementAndGet();
            if (n == 1) {
                obs.onError(io.grpc.Status.UNAVAILABLE
                        .withDescription("subscribe backend down").asRuntimeException());
            }
            // n >= 2: leave the stream open (no onNext / onComplete / onError).
        });

        ConcurrentLinkedQueue<BackgroundErrorEvent> events = new ConcurrentLinkedQueue<>();
        client = TipsyAbConfigClient.create(cfg("checkout")
                .pullInterval(Duration.ofMinutes(10))
                .onBackgroundError(events::add)
                .build());

        // The real error increments the per-ns disconnect counter and fires a
        // "subscribe" background event.
        boolean errored = InProcessConfigServiceHarness.awaitTrue(
                () -> client.metrics().subscribeDisconnectTotal("checkout") >= 1
                        && events.stream().anyMatch(e -> "subscribe".equals(e.phase())),
                POLL_BUDGET_MS);
        assertTrue(errored,
                "a real subscribe error must bump subscribeDisconnectTotal and fire a subscribe event");

        // The subscribe error is recorded (and is sticky).
        assertTrue(InProcessConfigServiceHarness.awaitTrue(
                        () -> client.health().lastSubscribeErr().isPresent(), POLL_BUDGET_MS),
                "a real subscribe error must record lastSubscribeErr");

        // After the backoff the loop reconnects (attempt 2 keeps the stream open),
        // so subscribeConnected flips back to true.
        assertTrue(InProcessConfigServiceHarness.awaitTrue(
                        () -> client.health().subscribeConnected(), POLL_BUDGET_MS),
                "the loop must reconnect after the error and re-establish the stream");

        BackgroundErrorEvent ev = events.stream()
                .filter(e -> "subscribe".equals(e.phase()))
                .findFirst().orElseThrow();
        assertEquals("", ev.namespace(),
                "the subscribe event carries the empty namespace (stream is multi-namespace)");
    }

    // ------------------------------------------------------------------
    // close(): idempotent, stops background threads, no exceptions
    // ------------------------------------------------------------------

    @Test
    void close_isIdempotentAndDoesNotThrow() {
        harness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshot("checkout", 1, 0, "k", 9, "v")));
        client = TipsyAbConfigClient.create(cfg("checkout").build());

        assertDoesNotThrow(client::close);
        assertDoesNotThrow(client::close); // second call must be a no-op
        assertDoesNotThrow(client::close); // and a third

        // closed() is observable via getConfig throwing SdkClosedException.
        assertThrows(SdkClosedException.class,
                () -> client.getConfig(client.emptyAbtestContext(), "checkout", "k", "d"));

        client = null; // already closed; avoid double close in tearDown
    }

    @Test
    void close_stopsPeriodicPullLoop_noFurtherPullsAfterClose() {
        AtomicInteger pulls = new AtomicInteger();
        harness.setPullHandler(req -> {
            pulls.incrementAndGet();
            return InProcessConfigServiceHarness.pullResponse(
                    InProcessConfigServiceHarness.snapshot("checkout", 1, 0, "k", 9, "v"));
        });
        TipsyAbConfigClient c = TipsyAbConfigClient.create(cfg("checkout")
                .pullInterval(Duration.ofMillis(100))
                .build());
        c.close();
        int afterClose = pulls.get();
        // Wait a couple of would-be intervals: no further pulls should land.
        try {
            Thread.sleep(400);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
        assertEquals(afterClose, pulls.get(),
                "no PullAll must be issued after close() stops the periodic loop");
        // client field left null: c is already closed.
    }

    // ------------------------------------------------------------------
    // onBackgroundError callback resilience: a throwing callback must not kill
    // the background thread (subsequent events still arrive).
    // ------------------------------------------------------------------

    @Test
    void throwingOnBackgroundError_doesNotKillBackgroundThread() {
        // Always-failing periodic pull so the callback fires repeatedly.
        PullAllResponse ok = InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshot("checkout", 1, 0, "k", 9, "v"));
        InProcessConfigServiceHarness.SwitchablePullHandler handler =
                new InProcessConfigServiceHarness.SwitchablePullHandler(req -> ok);
        harness.setPullHandler(handler);

        AtomicInteger callbackInvocations = new AtomicInteger();
        AtomicBoolean firstThrew = new AtomicBoolean(false);
        client = TipsyAbConfigClient.create(cfg("checkout")
                .pullInterval(Duration.ofMillis(120))
                .pullTimeout(Duration.ofMillis(500))
                .onBackgroundError(ev -> {
                    callbackInvocations.incrementAndGet();
                    // Throw from the callback every time: the SDK must swallow it.
                    firstThrew.set(true);
                    throw new RuntimeException("callback boom");
                })
                .build());

        handler.set(req -> {
            throw new RuntimeException("periodic boom");
        });

        // The callback must be invoked MORE THAN ONCE: if the first throw killed
        // the loop thread, the count would stick at 1.
        boolean survived = InProcessConfigServiceHarness.awaitTrue(
                () -> callbackInvocations.get() >= 2, POLL_BUDGET_MS);
        assertTrue(firstThrew.get(), "the callback must have been invoked + thrown at least once");
        assertTrue(survived,
                "a throwing onBackgroundError must not kill the pull loop; it must keep firing events");

        // The client is still usable.
        assertDoesNotThrow(() -> client.getConfigStatic("checkout", "k"));
    }

    // ------------------------------------------------------------------
    // helpers
    // ------------------------------------------------------------------

    /** A gRPC-mode builder wired to the in-process harness with a fast pull timeout. */
    private Config.Builder cfg(String... namespaces) {
        return Config.builder()
                .namespaces(namespaces)
                .configServiceAddr("passthrough:///x")
                .token("tok")
                .transport(Transport.GRPC)
                .channelConfigurator(harness.channelConfigurator())
                .pullTimeout(Duration.ofSeconds(2));
    }
}
