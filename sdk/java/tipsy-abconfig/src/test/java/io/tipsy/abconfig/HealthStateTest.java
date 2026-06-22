package io.tipsy.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.time.Instant;
import org.junit.jupiter.api.Test;

/**
 * Unit tests for {@link HealthState} / {@link Health} and the immutable
 * {@link BackgroundErrorEvent} accessor contract.
 *
 * <p>Design ref: 04-transport-and-cache.md §"可观测内部（fireBackgroundError）".
 * Verifies: clean default state (EPOCH times, no errors); startupCacheEmpty;
 * sticky pull/subscribe errors; recordSubscribeErr flips connected to false;
 * a later setSubscribeConnected(true) does NOT clear the sticky error.
 */
final class HealthStateTest {

    @Test
    void defaultState_isHealthyWithEpochTimes() {
        Health h = new HealthState().snapshot();

        assertFalse(h.startupCacheEmpty());
        assertTrue(h.lastPullErr().isEmpty());
        assertTrue(h.lastSubscribeErr().isEmpty());
        assertFalse(h.subscribeConnected());
        assertEquals(Instant.EPOCH, h.lastPullErrTime(),
                "no error -> time must be the fixed EPOCH zero, never null");
        assertEquals(Instant.EPOCH, h.lastSubscribeErrTime());
    }

    @Test
    void setStartupCacheEmpty() {
        HealthState s = new HealthState();
        s.setStartupCacheEmpty();
        assertTrue(s.snapshot().startupCacheEmpty());
    }

    @Test
    void recordPullErr_isVisibleInSnapshotWithErrorAndTime() {
        HealthState s = new HealthState();
        RuntimeException err = new RuntimeException("pull boom");
        Instant t = Instant.parse("2026-06-22T10:15:30Z");

        s.recordPullErr(err, t);
        Health h = s.snapshot();

        assertTrue(h.lastPullErr().isPresent());
        assertSame(err, h.lastPullErr().orElseThrow(), "the recorded throwable is surfaced");
        assertEquals(t, h.lastPullErrTime());
        // Pull error does not touch subscribe state.
        assertTrue(h.lastSubscribeErr().isEmpty());
        assertEquals(Instant.EPOCH, h.lastSubscribeErrTime());
    }

    @Test
    void recordSubscribeErr_flipsConnectedToFalse() {
        HealthState s = new HealthState();
        s.setSubscribeConnected(true);
        assertTrue(s.snapshot().subscribeConnected());

        RuntimeException err = new RuntimeException("stream boom");
        Instant t = Instant.parse("2026-06-22T11:00:00Z");
        s.recordSubscribeErr(err, t);

        Health h = s.snapshot();
        assertFalse(h.subscribeConnected(),
                "recordSubscribeErr must flip subscribeConnected to false");
        assertSame(err, h.lastSubscribeErr().orElseThrow());
        assertEquals(t, h.lastSubscribeErrTime());
    }

    @Test
    void subscribeError_isSticky_acrossReconnect() {
        HealthState s = new HealthState();
        RuntimeException err = new RuntimeException("stream boom");
        Instant errTime = Instant.parse("2026-06-22T11:00:00Z");
        s.recordSubscribeErr(err, errTime);

        // Reconnect: connected flips back to true, but the last error stays.
        s.setSubscribeConnected(true);

        Health h = s.snapshot();
        assertTrue(h.subscribeConnected(), "reconnect sets connected=true");
        assertTrue(h.lastSubscribeErr().isPresent(),
                "the last subscribe error must remain sticky after reconnect");
        assertSame(err, h.lastSubscribeErr().orElseThrow());
        assertEquals(errTime, h.lastSubscribeErrTime(),
                "the error time must remain sticky after reconnect");
    }

    @Test
    void pullError_isSticky_overwrittenOnlyByNewerError() {
        HealthState s = new HealthState();
        RuntimeException first = new RuntimeException("first");
        RuntimeException second = new RuntimeException("second");
        Instant t1 = Instant.parse("2026-06-22T11:00:00Z");
        Instant t2 = Instant.parse("2026-06-22T12:00:00Z");

        s.recordPullErr(first, t1);
        s.recordPullErr(second, t2);

        Health h = s.snapshot();
        assertSame(second, h.lastPullErr().orElseThrow(), "newer pull error overwrites");
        assertEquals(t2, h.lastPullErrTime());
    }

    @Test
    void snapshotIsImmutableCopy_independentOfLaterMutation() {
        HealthState s = new HealthState();
        Health before = s.snapshot();

        s.setStartupCacheEmpty();
        s.recordPullErr(new RuntimeException("later"), Instant.parse("2026-06-22T13:00:00Z"));

        // The earlier snapshot is a frozen copy.
        assertFalse(before.startupCacheEmpty(),
                "an earlier Health snapshot must not observe later mutation");
        assertTrue(before.lastPullErr().isEmpty());
    }

    @Test
    void backgroundErrorEvent_accessorsReturnConstructorValues() {
        RuntimeException err = new RuntimeException("x");
        Instant t = Instant.parse("2026-06-22T14:00:00Z");
        BackgroundErrorEvent ev = new BackgroundErrorEvent("periodic_pull", "checkout", err, t);

        assertEquals("periodic_pull", ev.phase());
        assertEquals("checkout", ev.namespace());
        assertSame(err, ev.error());
        assertEquals(t, ev.time());
    }
}
