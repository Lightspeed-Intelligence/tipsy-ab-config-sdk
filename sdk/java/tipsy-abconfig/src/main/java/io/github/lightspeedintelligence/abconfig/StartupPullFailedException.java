package io.github.lightspeedintelligence.abconfig;

/**
 * Thrown from {@link TipsyAbConfigClient#create(Config)} when the startup
 * {@code PullAll} sweep failed for at least one namespace and
 * {@code startupFailOpen} is {@code false} (the default).
 *
 * <p>Mirrors the Go SDK's {@code ErrStartupPullFailed}. When
 * {@code startupFailOpen} is {@code true} the same failure is instead absorbed:
 * the client starts with an empty cache, {@link Health#startupCacheEmpty()}
 * reports {@code true}, and a {@link BackgroundErrorEvent} with phase
 * {@code "startup_pull"} is fired.
 *
 * <p>This exception covers the case where the dial succeeded but the per-RPC
 * PullAll calls failed (including auth failures surfaced via a
 * {@link TokenProvider}). It is distinct from {@link ConfigValidationException}
 * (parameter / address errors), which is never absorbed by
 * {@code startupFailOpen}.
 */
public class StartupPullFailedException extends TipsyConfigException {

    private static final long serialVersionUID = 1L;

    /**
     * Creates the exception with the given message.
     *
     * @param message the detail message (should start with {@code tipsyabconfig:})
     */
    public StartupPullFailedException(String message) {
        super(message);
    }

    /**
     * Creates the exception with the given message and the aggregate startup
     * failure as its cause.
     *
     * @param message the detail message (should start with {@code tipsyabconfig:})
     * @param cause   the underlying first per-namespace failure (may be {@code null})
     */
    public StartupPullFailedException(String message, Throwable cause) {
        super(message, cause);
    }
}
