package io.github.lightspeedintelligence.abconfig;

import java.time.Instant;
import java.util.Optional;

/**
 * Immutable snapshot of the SDK's background-link health.
 *
 * <p>Mirrors Go {@code Health}. Obtain a copy via the client's {@code health()}.
 * The "all healthy" state means no failures observed:
 * {@link #startupCacheEmpty()} false, both {@code lastErr} empty with their
 * times at {@link Instant#EPOCH}.
 *
 * <p>Error stickiness (mirrors Go): once {@link #lastPullErr()} /
 * {@link #lastSubscribeErr()} is recorded it is NOT cleared on a subsequent
 * success; only {@link #subscribeConnected()} flips with the stream connecting
 * / disconnecting. When no error has been recorded, the corresponding
 * {@code *Time()} returns {@link Instant#EPOCH} (a fixed zero value, never
 * {@code null}).
 *
 * <p>HTTP transport mode: the Subscribe stream is never opened, so
 * {@link #subscribeConnected()} is always false and {@link #lastSubscribeErr()}
 * is always empty.
 */
public final class Health {

    private final boolean startupCacheEmpty;
    private final Throwable lastPullErr;
    private final Instant lastPullErrTime;
    private final Throwable lastSubscribeErr;
    private final Instant lastSubscribeErrTime;
    private final boolean subscribeConnected;

    Health(boolean startupCacheEmpty,
           Throwable lastPullErr,
           Instant lastPullErrTime,
           Throwable lastSubscribeErr,
           Instant lastSubscribeErrTime,
           boolean subscribeConnected) {
        this.startupCacheEmpty = startupCacheEmpty;
        this.lastPullErr = lastPullErr;
        this.lastPullErrTime = lastPullErrTime;
        this.lastSubscribeErr = lastSubscribeErr;
        this.lastSubscribeErrTime = lastSubscribeErrTime;
        this.subscribeConnected = subscribeConnected;
    }

    /** True when {@code startupFailOpen} absorbed a startup PullAll failure and the client started empty. */
    public boolean startupCacheEmpty() {
        return startupCacheEmpty;
    }

    /** The most recent periodic PullAll error, or empty if none recorded (sticky). */
    public Optional<Throwable> lastPullErr() {
        return Optional.ofNullable(lastPullErr);
    }

    /** When {@link #lastPullErr()} was recorded, or {@link Instant#EPOCH} if none. */
    public Instant lastPullErrTime() {
        return lastPullErrTime;
    }

    /** The most recent Subscribe stream error, or empty if none (always empty in HTTP mode; sticky). */
    public Optional<Throwable> lastSubscribeErr() {
        return Optional.ofNullable(lastSubscribeErr);
    }

    /** When {@link #lastSubscribeErr()} was recorded, or {@link Instant#EPOCH} if none. */
    public Instant lastSubscribeErrTime() {
        return lastSubscribeErrTime;
    }

    /** Whether the Subscribe stream is currently established (always false in HTTP mode). */
    public boolean subscribeConnected() {
        return subscribeConnected;
    }
}
