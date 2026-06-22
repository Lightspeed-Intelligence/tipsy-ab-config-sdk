package io.tipsy.abconfig;

/**
 * Thrown by {@link TipsyAbConfigClient#getConfig} /
 * {@link TipsyAbConfigClient#getConfigDefault} when the caller passes a
 * {@code null} {@link AbtestContext}.
 *
 * <p>Mirrors the Go SDK's {@code ErrAbtestContextMissing}: callers must
 * explicitly pass either a {@code newAbtestContext(...)} result or
 * {@link TipsyAbConfigClient#emptyAbtestContext()}.
 */
public class AbtestContextMissingException extends TipsyConfigException {

    private static final long serialVersionUID = 1L;

    /**
     * Creates the exception with the given message.
     *
     * @param message the detail message (should start with {@code tipsyabconfig:})
     */
    public AbtestContextMissingException(String message) {
        super(message);
    }
}
