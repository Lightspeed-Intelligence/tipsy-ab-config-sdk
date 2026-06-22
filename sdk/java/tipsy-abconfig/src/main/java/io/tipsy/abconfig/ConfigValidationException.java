package io.tipsy.abconfig;

/**
 * Thrown for parameter- and address-validation failures detected before any
 * network connection is established.
 *
 * <p>Mirrors the Go SDK's parameter errors that are returned from {@code Init}
 * and are <em>never</em> absorbed by {@code StartupFailOpen}: an invalid
 * {@code Transport} value, an empty {@code Namespaces} list, a malformed gRPC
 * target ({@code 方案 Y} grammar), or a non-{@code http(s)://} base URL in HTTP
 * mode all surface as this exception.
 *
 * <p>Messages carry the {@code tipsyabconfig:} prefix for parity with the Go
 * SDK's error strings.
 */
public class ConfigValidationException extends TipsyConfigException {

    private static final long serialVersionUID = 1L;

    /**
     * Creates a validation exception with the given message.
     *
     * @param message the detail message (should start with {@code tipsyabconfig:})
     */
    public ConfigValidationException(String message) {
        super(message);
    }

    /**
     * Creates a validation exception with the given message and cause.
     *
     * @param message the detail message (should start with {@code tipsyabconfig:})
     * @param cause   the underlying cause (may be {@code null})
     */
    public ConfigValidationException(String message, Throwable cause) {
        super(message, cause);
    }
}
