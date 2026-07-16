package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.github.lightspeedintelligence.abconfig.proto.config.v1.ConfigUpdateEvent;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.Heartbeat;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSeqs;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSnapshot;
import java.lang.reflect.Method;
import java.time.Duration;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/**
 * Heartbeat parity tests for {@link TipsyAbConfigClient#handleEvent} (ST5 Java).
 *
 * <p>Design ref: {@code design-parity.md} §"心跳 parity" (Java) + Acceptance
 * Criteria 2/5. After the rebuild {@code PayloadCase.HEARTBEAT} is available and
 * {@code handleEvent} switches on {@code getPayloadCase()} with an explicit
 * HEARTBEAT no-op, keeping the {@code if (ev == null) return;} guard (S3, without
 * which {@code getPayloadCase()} would NPE). Behaviour must be identical to the
 * previous {@code !hasSnapshot()} skip: a heartbeat frame touches neither the
 * cache nor the subscribe-event metric, and the stream keeps draining.
 *
 * <p>Coverage split:
 * <ul>
 *   <li>Through the real Subscribe stream (in-process harness): a heartbeat
 *       interleaved with a snapshot proves the heartbeat is a no-op AND the
 *       stream survives it (the following snapshot still applies).</li>
 *   <li>Direct {@code handleEvent} calls via reflection: heartbeat no-op and the
 *       {@code null}-guard (cannot be driven through the stream, which never
 *       yields null).</li>
 * </ul>
 */
final class HeartbeatHandlingTest {

    private static final long POLL_BUDGET_MS = 3_000;
    private static final String NS = "checkout";

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
    // Real stream: heartbeat is a no-op, and the stream keeps draining so the
    // following snapshot still applies (exactly one subscribe-event counted).
    // ------------------------------------------------------------------

    @Test
    void heartbeatThenSnapshot_overStream_heartbeatIsNoOpAndStreamSurvives() {
        // Startup seeds business_seq=1 so the pushed snapshot (seq=2) is a strict
        // advance and is applied.
        harness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshot(NS, 1, 0, "color", 11, "blue")));

        NamespaceSnapshot pushed =
                InProcessConfigServiceHarness.snapshot(NS, 2, 0, "color", 12, "green");
        harness.setSubscribeHandler((req, obs) -> {
            Thread t = new Thread(() -> {
                // A heartbeat first (must be ignored), then a real snapshot. Then
                // leave the stream open so subscribeConnected stays true.
                obs.onNext(heartbeatEvent(123L));
                obs.onNext(InProcessConfigServiceHarness.snapshotEvent(pushed));
            }, "test-heartbeat-then-snapshot");
            t.setDaemon(true);
            t.start();
        });

        client = TipsyAbConfigClient.create(cfg(NS)
                .pullInterval(Duration.ofMinutes(10)) // isolate from periodic pull
                .build());

        boolean applied = InProcessConfigServiceHarness.awaitTrue(
                () -> client.getConfigStatic(NS, "color").map("green"::equals).orElse(false),
                POLL_BUDGET_MS);
        assertTrue(applied, "the snapshot after the heartbeat must still be applied "
                + "(the heartbeat must not break the stream)");

        // The heartbeat did NOT count as a subscribe event: exactly one event
        // (the snapshot) was received. If the heartbeat were mis-counted this
        // would be 2.
        assertEquals(1L, client.metrics().subscribeEventReceivedTotal(NS),
                "a heartbeat frame must not increment the subscribe-event counter");
        // Only the snapshot advanced the business seq (heartbeat carries no seq).
        assertEquals(2L, client.cache().knownSeqs(List.of(NS)).get(NS).getBusinessSnapshotSeq(),
                "only the snapshot may advance the business seq");
    }

    // ------------------------------------------------------------------
    // Direct handleEvent(heartbeat): no cache/seq mutation, no metric, no throw.
    // ------------------------------------------------------------------

    @Test
    void handleEvent_heartbeat_directCall_isNoOp() throws Exception {
        client = seededClientWithIdleStream();

        // Snapshot the observable state before the heartbeat.
        Optional<String> valueBefore = client.getConfigStatic(NS, "color");
        NamespaceSeqs seqsBefore = client.cache().knownSeqs(List.of(NS)).get(NS);
        long eventsBefore = client.metrics().subscribeEventReceivedTotal(NS);

        Method handleEvent = handleEventMethod();
        ConfigUpdateEvent hb = heartbeatEvent(456L);
        assertDoesNotThrow(() -> handleEvent.invoke(client, hb),
                "handleEvent(heartbeat) must not throw");

        assertEquals(valueBefore, client.getConfigStatic(NS, "color"),
                "a heartbeat must not mutate a cached value");
        NamespaceSeqs seqsAfter = client.cache().knownSeqs(List.of(NS)).get(NS);
        assertEquals(seqsBefore.getBusinessSnapshotSeq(), seqsAfter.getBusinessSnapshotSeq(),
                "a heartbeat must not advance the business seq");
        assertEquals(seqsBefore.getExperimentSnapshotSeq(), seqsAfter.getExperimentSnapshotSeq(),
                "a heartbeat must not advance the experiment seq");
        assertEquals(eventsBefore, client.metrics().subscribeEventReceivedTotal(NS),
                "a heartbeat must not increment the subscribe-event counter");
    }

    // ------------------------------------------------------------------
    // Direct handleEvent(null): the null guard (S3) must survive so
    // getPayloadCase() is never called on null.
    // ------------------------------------------------------------------

    @Test
    void handleEvent_null_directCall_doesNotThrow() throws Exception {
        client = seededClientWithIdleStream();

        Method handleEvent = handleEventMethod();
        long eventsBefore = client.metrics().subscribeEventReceivedTotal(NS);

        // Pass a genuine null argument (explicit array to avoid varargs ambiguity).
        Object[] args = new Object[] {null};
        assertDoesNotThrow(() -> handleEvent.invoke(client, args),
                "handleEvent(null) must be guarded and must not NPE (S3)");

        assertEquals(eventsBefore, client.metrics().subscribeEventReceivedTotal(NS),
                "a null event must not touch metrics");
    }

    // ------------------------------------------------------------------
    // helpers
    // ------------------------------------------------------------------

    private static ConfigUpdateEvent heartbeatEvent(long unixNanos) {
        return ConfigUpdateEvent.newBuilder()
                .setHeartbeat(Heartbeat.newBuilder().setUnixNanos(unixNanos))
                .build();
    }

    private static Method handleEventMethod() throws NoSuchMethodException {
        Method m = TipsyAbConfigClient.class.getDeclaredMethod("handleEvent", ConfigUpdateEvent.class);
        m.setAccessible(true);
        return m;
    }

    /**
     * A client whose startup pull seeded {@code checkout=blue} (business_seq=5),
     * with a Subscribe stream that stays open but pushes nothing — so the
     * background thread never mutates the cache/metrics while the reflection
     * tests inspect them.
     */
    private TipsyAbConfigClient seededClientWithIdleStream() {
        harness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshot(NS, 5, 0, "color", 11, "blue")));
        harness.setSubscribeHandler((req, obs) -> {
            // leave the stream open, emit nothing
        });
        TipsyAbConfigClient c = TipsyAbConfigClient.create(cfg(NS)
                .pullInterval(Duration.ofMinutes(10))
                .build());
        // Sanity: startup populated the cache.
        Map<String, NamespaceSeqs> seqs = c.cache().knownSeqs(List.of(NS));
        assertEquals(5L, seqs.get(NS).getBusinessSnapshotSeq(), "startup pull must seed business_seq=5");
        return c;
    }

    /** A gRPC-mode builder wired to the in-process harness (mirrors GrpcClientLifecycleTest). */
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
