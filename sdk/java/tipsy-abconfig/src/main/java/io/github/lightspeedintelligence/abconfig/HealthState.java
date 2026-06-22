package io.github.lightspeedintelligence.abconfig;

import java.time.Instant;
import java.util.concurrent.locks.ReentrantLock;

/**
 * Mutable, lock-protected backing store for the client's {@link Health}.
 *
 * <p>Mirrors Go {@code healthState}. Owned by the client and accessed from the
 * pull loop, the subscribe loop, startup, and external {@code health()} calls;
 * all access goes through these helpers so it is data-race free.
 *
 * <p>Errors are sticky: {@link #recordPullErr} / {@link #recordSubscribeErr}
 * overwrite the last error/time but nothing clears them on a subsequent
 * success. Only {@link #setSubscribeConnected} (and the implicit false set by
 * {@link #recordSubscribeErr}) flips the connected flag. Times default to
 * {@link Instant#EPOCH} until the first error.
 */
final class HealthState {

    private final ReentrantLock lock = new ReentrantLock();

    private boolean startupCacheEmpty;
    private Throwable lastPullErr;
    private Instant lastPullErrTime = Instant.EPOCH;
    private Throwable lastSubscribeErr;
    private Instant lastSubscribeErrTime = Instant.EPOCH;
    private boolean subscribeConnected;

    /** Returns an immutable snapshot of the current health. */
    Health snapshot() {
        lock.lock();
        try {
            return new Health(
                    startupCacheEmpty,
                    lastPullErr,
                    lastPullErrTime,
                    lastSubscribeErr,
                    lastSubscribeErrTime,
                    subscribeConnected);
        } finally {
            lock.unlock();
        }
    }

    /** Marks the client as having started with an empty cache (startup_pull absorbed). */
    void setStartupCacheEmpty() {
        lock.lock();
        try {
            startupCacheEmpty = true;
        } finally {
            lock.unlock();
        }
    }

    /** Records a periodic PullAll error (sticky). */
    void recordPullErr(Throwable err, Instant t) {
        lock.lock();
        try {
            lastPullErr = err;
            lastPullErrTime = t;
        } finally {
            lock.unlock();
        }
    }

    /** Records a Subscribe stream error (sticky) and flips {@code subscribeConnected} to false. */
    void recordSubscribeErr(Throwable err, Instant t) {
        lock.lock();
        try {
            lastSubscribeErr = err;
            lastSubscribeErrTime = t;
            subscribeConnected = false;
        } finally {
            lock.unlock();
        }
    }

    /** Sets whether the Subscribe stream is currently established. */
    void setSubscribeConnected(boolean connected) {
        lock.lock();
        try {
            subscribeConnected = connected;
        } finally {
            lock.unlock();
        }
    }
}
