package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertTrue;

import java.time.Duration;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicInteger;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.api.condition.EnabledIfEnvironmentVariable;

/**
 * OPT-IN integration wiring check for the subscribe backoff reset (ST5 Java, S4).
 *
 * <p>Design ref: {@code design-parity.md} Testing Plan Java "接线测试（S4）" — this
 * guards against the silent regression where {@link
 * TipsyAbConfigClient#resetBackoffIfStable} is correct in isolation but is never
 * actually wired into {@code runSubscribe}'s catch path (uptime computed from
 * {@code startNanos} and fed to the reset). It injects a tiny
 * {@code stableResetThresholdMs}, makes each Subscribe connection stay alive just
 * past the threshold and then error, and asserts the reconnect gaps stay flat
 * (~1s, i.e. reset each cycle) rather than escalating (1s → 2s → 4s).
 *
 * <p><b>SKIPPED BY DEFAULT — ran-or-skip登记: SKIP.</b> This test is
 * {@code @EnabledIfEnvironmentVariable(TIPSY_RUN_TIMING_TESTS=1)} so a plain
 * {@code mvn clean test} reports it as skipped (never silently omitted). Reasons
 * it is not part of the default gate:
 * <ol>
 *   <li><b>Wall-clock discrimination.</b> The reconnect floor is {@code
 *       Thread.sleep(backoffMs)} with a 1s initial backoff that is NOT
 *       injectable, so distinguishing reset (~1.06s gap) from escalation
 *       (~2.06s gap) needs multi-second real sleeps and a timing-window
 *       assertion — inherently flaky under CI load/GC pauses.</li>
 *   <li><b>Injection data race.</b> {@code stableResetThresholdMs} is a plain
 *       (non-volatile) field with no pre-start injection seam; {@code create()}
 *       starts the subscribe thread before the test can set it, so the injected
 *       value's visibility to the subscribe thread is not guaranteed by the JMM.
 *       A truly deterministic wiring test would need implementationAgent to
 *       expose a pre-start seam (set the threshold before {@code
 *       startBackgroundLoops()}), e.g. via the Builder/Config.</li>
 * </ol>
 * The authoritative coverage of the reset logic is the pure-function
 * {@link ResetBackoffIfStableTest}; run this opt-in check locally with
 * {@code TIPSY_RUN_TIMING_TESTS=1 mvn -Dtest=SubscribeBackoffResetWiringTest test}
 * to sanity-check the wiring after touching {@code runSubscribe}.
 */
@EnabledIfEnvironmentVariable(named = "TIPSY_RUN_TIMING_TESTS", matches = "1")
final class SubscribeBackoffResetWiringTest {

    private static final String NS = "checkout";
    /** Tiny threshold so a ~60ms connection counts as "stable" and resets the backoff. */
    private static final long TINY_THRESHOLD_MS = 20L;
    private static final long ALIVE_MS = 60L;
    /** Error this many attempts, then leave the stream open to stop the churn. */
    private static final int ERRORING_ATTEMPTS = 4;
    /** Reset gap ≈ ALIVE + 1000ms; escalation gap(2->3) ≈ ALIVE + 2000ms. Split in the middle. */
    private static final long GAP_DISCRIMINATOR_MS = 1_600L;

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

    @Test
    void stableConnections_resetBackoff_soReconnectGapsStayFlat() {
        harness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshot(NS, 1, 0, "color", 11, "blue")));

        // Record the wall-clock arrival of each subscribe attempt.
        CopyOnWriteArrayList<Long> attemptNanos = new CopyOnWriteArrayList<>();
        AtomicInteger attempts = new AtomicInteger();
        harness.setSubscribeHandler((req, obs) -> {
            int n = attempts.incrementAndGet();
            attemptNanos.add(System.nanoTime());
            if (n <= ERRORING_ATTEMPTS) {
                // Stay alive past the threshold, then error (a real, non-cancel
                // status) so runSubscribe treats the connection as "stable" and
                // resets the backoff before sleeping.
                Thread t = new Thread(() -> {
                    sleepQuietly(ALIVE_MS);
                    obs.onError(io.grpc.Status.UNAVAILABLE
                            .withDescription("induced stable-then-error").asRuntimeException());
                }, "test-stable-then-error");
                t.setDaemon(true);
                t.start();
            }
            // n > ERRORING_ATTEMPTS: leave the stream open.
        });

        client = TipsyAbConfigClient.create(cfg(NS)
                .pullInterval(Duration.ofMinutes(10))
                .build());
        // Inject the tiny threshold (best-effort; see class javadoc on the race).
        client.stableResetThresholdMs = TINY_THRESHOLD_MS;

        // Wait for at least 3 attempts so we can measure gap(attempt2 -> attempt3).
        boolean enough = InProcessConfigServiceHarness.awaitTrue(
                () -> attemptNanos.size() >= 3, 8_000);
        assertTrue(enough, "expected at least 3 subscribe attempts within the budget");

        long gap23Ms = (attemptNanos.get(2) - attemptNanos.get(1)) / 1_000_000L;
        assertTrue(gap23Ms < GAP_DISCRIMINATOR_MS,
                "with the backoff reset wired in, a stable connection resets to ~1s so the "
                        + "gap between attempts 2 and 3 (" + gap23Ms + "ms) must stay well under "
                        + "the ~2s that escalation would produce");
    }

    private static void sleepQuietly(long ms) {
        try {
            Thread.sleep(ms);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

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
