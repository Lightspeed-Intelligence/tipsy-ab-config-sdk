package io.tipsy.abconfig;

/**
 * Common superclass for all Tipsy AB-config SDK exceptions.
 *
 * <p>Unchecked ({@link RuntimeException}) by design: the Go SDK returns
 * {@code error} values that callers usually degrade on rather than propagate,
 * so forcing Java callers into pervasive checked-exception handling would be a
 * poor mirror of that contract. Specific failure modes are expressed by
 * subclasses (parameter / address validation via {@link ConfigValidationException},
 * the runtime/lifecycle exceptions added by later slices).
 *
 * <p>All SDK exception messages carry the {@code tipsyabconfig:} prefix to match
 * the Go SDK's error strings.
 */
public class TipsyConfigException extends RuntimeException {

    private static final long serialVersionUID = 1L;

    /**
     * Creates an exception with the given message. Callers are expected to
     * prefix human-facing messages with {@code tipsyabconfig:} for parity with
     * the Go SDK.
     *
     * @param message the detail message
     */
    public TipsyConfigException(String message) {
        super(message);
    }

    /**
     * Creates an exception with the given message and underlying cause.
     *
     * @param message the detail message
     * @param cause   the underlying cause (may be {@code null})
     */
    public TipsyConfigException(String message, Throwable cause) {
        super(message, cause);
    }
}
