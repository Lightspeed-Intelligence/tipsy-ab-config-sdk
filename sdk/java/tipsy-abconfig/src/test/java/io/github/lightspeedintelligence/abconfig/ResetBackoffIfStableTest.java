package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;

import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.CsvSource;

/**
 * Unit tests for the pure backoff-reset decision
 * {@link TipsyAbConfigClient#resetBackoffIfStable(long, long, long)} (ST5 Java
 * parity with the Go SDK's ST3 stable-reset logic).
 *
 * <p>Design ref: {@code design-parity.md} §"退避重置（对齐 Go，两端同构）" /
 * Testing Plan Java + Acceptance Criteria 2. The contract is:
 * <pre>
 *   (thresholdMs &gt; 0 &amp;&amp; uptimeMs &gt;= thresholdMs) ? 1000L : backoffMs
 * </pre>
 * i.e. a connection that stayed alive at least {@code thresholdMs} before it
 * dropped resets the reconnect backoff to its 1s initial value; anything else
 * keeps the current backoff (which the caller then escalates). A non-positive
 * threshold NEVER treats a connection as stable (defensive branch mirroring Go
 * SG-a, S5).
 *
 * <p>This is the authoritative coverage for the reset logic; the integration
 * wiring into {@code runSubscribe} is exercised opt-in by
 * {@link SubscribeBackoffResetWiringTest} (see that file for the ran/skip
 * tradeoff).
 *
 * <p>Contract note for implementationAgent: {@code resetBackoffIfStable} must be
 * at least package-private {@code static} (the design specifies {@code static
 * long}). A {@code private} modifier would break this same-package direct call.
 */
final class ResetBackoffIfStableTest {

    private static final long INITIAL = 1000L;
    private static final long DEFAULT_THRESHOLD = 60_000L;

    // ------------------------------------------------------------------
    // uptime >= threshold (and threshold > 0) -> reset to 1000ms
    // ------------------------------------------------------------------

    @ParameterizedTest(name = "reset: backoff={0}ms uptime={1}ms threshold={2}ms -> 1000ms")
    @CsvSource({
        // boundary: uptime EXACTLY equal to the threshold still resets (>=).
        "8000,   60000,  60000",
        // uptime just past the threshold.
        "8000,   60001,  60000",
        // reset from the 30s cap after a long-lived connection.
        "30000,  120000, 60000",
        // reset with a tiny injected threshold (mirrors the wiring test seam).
        "2000,   1000,   50",
        // already at the initial value: reset is idempotent, stays 1000.
        "1000,   60000,  60000",
    })
    void stableConnection_resetsBackoffToInitial(long backoffMs, long uptimeMs, long thresholdMs) {
        assertEquals(INITIAL,
                TipsyAbConfigClient.resetBackoffIfStable(backoffMs, uptimeMs, thresholdMs),
                "uptime >= threshold (threshold > 0) must reset the backoff to 1000ms");
    }

    // ------------------------------------------------------------------
    // uptime < threshold -> keep the current backoff unchanged
    // ------------------------------------------------------------------

    @ParameterizedTest(name = "keep: backoff={0}ms uptime={1}ms threshold={2}ms -> {0}ms")
    @CsvSource({
        // just under the boundary.
        "8000,  59999,  60000",
        // a brand-new connection that dropped immediately.
        "2000,  0,      60000",
        // a short-lived connection under the default 60s threshold (S6 regression
        // safety: existing short-connection reconnects keep escalating as before).
        "4000,  100,    60000",
        "16000, 30000,  60000",
    })
    void shortLivedConnection_keepsCurrentBackoff(long backoffMs, long uptimeMs, long thresholdMs) {
        assertEquals(backoffMs,
                TipsyAbConfigClient.resetBackoffIfStable(backoffMs, uptimeMs, thresholdMs),
                "uptime < threshold must leave the backoff unchanged (caller escalates)");
    }

    // ------------------------------------------------------------------
    // threshold <= 0 -> NEVER reset, even for an arbitrarily long uptime
    // ------------------------------------------------------------------

    @ParameterizedTest(name = "disabled: backoff={0}ms uptime={1}ms threshold={2}ms -> {0}ms")
    @CsvSource({
        // threshold == 0 disables the reset even at the maximum possible uptime.
        "8000,  9223372036854775807, 0",
        // negative threshold likewise never resets.
        "8000,  9223372036854775807, -1",
        "16000, 1000000000,          -60000",
        // uptime == threshold == 0: threshold>0 guard fails -> keep.
        "5000,  0,                   0",
    })
    void nonPositiveThreshold_neverResets(long backoffMs, long uptimeMs, long thresholdMs) {
        assertEquals(backoffMs,
                TipsyAbConfigClient.resetBackoffIfStable(backoffMs, uptimeMs, thresholdMs),
                "a non-positive threshold must never treat the connection as stable");
    }

    // ------------------------------------------------------------------
    // Explicit, unmissable statements of the two guarantees the design calls out.
    // ------------------------------------------------------------------

    @Test
    void boundaryUptimeEqualsThreshold_resets() {
        assertEquals(INITIAL,
                TipsyAbConfigClient.resetBackoffIfStable(8000L, DEFAULT_THRESHOLD, DEFAULT_THRESHOLD),
                "uptime == threshold is inclusive (>=) and must reset");
    }

    @Test
    void hugeUptimeButZeroThreshold_doesNotReset() {
        assertEquals(8000L,
                TipsyAbConfigClient.resetBackoffIfStable(8000L, Long.MAX_VALUE, 0L),
                "threshold <= 0 disables the reset regardless of uptime");
    }
}
