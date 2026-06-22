package io.tipsy.abconfig;

import java.time.Instant;

/**
 * Describes a single background-link failure delivered to the host's
 * {@code onBackgroundError} callback.
 *
 * <p>Mirrors Go {@code BackgroundErrorEvent}. Immutable. The callback is invoked
 * synchronously on the SDK's background thread (see the client configuration
 * contract).
 */
public final class BackgroundErrorEvent {

    private final String phase;
    private final String namespace;
    private final Throwable error;
    private final Instant time;

    /**
     * @param phase     the background link that failed; one of
     *                  {@code "startup_pull"} (the startup PullAll sweep failed
     *                  and was absorbed by {@code startupFailOpen}; aggregate,
     *                  namespace {@code ""}), {@code "periodic_pull"} (a periodic
     *                  fallback PullAll for {@code namespace} failed), or
     *                  {@code "subscribe"} (the Subscribe stream errored and will
     *                  reconnect; namespace {@code ""} as the stream is
     *                  multi-namespace).
     * @param namespace the namespace associated with the failure, or {@code ""}
     *                  for aggregate startup / subscribe failures.
     * @param error     the underlying error.
     * @param time      when the SDK observed the failure.
     */
    public BackgroundErrorEvent(String phase, String namespace, Throwable error, Instant time) {
        this.phase = phase;
        this.namespace = namespace;
        this.error = error;
        this.time = time;
    }

    /** {@code "startup_pull" | "periodic_pull" | "subscribe"}. */
    public String phase() {
        return phase;
    }

    /** The failing namespace, or {@code ""} for aggregate startup / subscribe failures. */
    public String namespace() {
        return namespace;
    }

    /** The underlying error. */
    public Throwable error() {
        return error;
    }

    /** When the SDK observed the failure. */
    public Instant time() {
        return time;
    }
}
