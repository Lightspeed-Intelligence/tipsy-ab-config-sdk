package io.tipsy.abconfig;

/**
 * Thrown when a resolved namespace is not one of the namespaces the client
 * subscribed to at construction. The SDK only serves configuration for its
 * subscribed namespaces.
 *
 * <p>Mirrors the Go SDK's {@code ErrNamespaceNotSubscribed}.
 */
public class NamespaceNotSubscribedException extends TipsyConfigException {

    private static final long serialVersionUID = 1L;

    /**
     * Creates the exception with the given message.
     *
     * @param message the detail message (should start with {@code tipsyabconfig:})
     */
    public NamespaceNotSubscribedException(String message) {
        super(message);
    }
}
