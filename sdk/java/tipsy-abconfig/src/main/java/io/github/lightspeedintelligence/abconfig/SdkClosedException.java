package io.github.lightspeedintelligence.abconfig;

/**
 * Thrown when an operation is attempted on a {@link TipsyAbConfigClient} after
 * {@link TipsyAbConfigClient#close()} has been called.
 *
 * <p>Mirrors the Go SDK's {@code ErrClosed}.
 */
public class SdkClosedException extends TipsyConfigException {

    private static final long serialVersionUID = 1L;

    /**
     * Creates the exception with the given message.
     *
     * @param message the detail message (should start with {@code tipsyabconfig:})
     */
    public SdkClosedException(String message) {
        super(message);
    }
}
