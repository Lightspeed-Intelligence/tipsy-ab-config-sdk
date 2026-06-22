package io.github.lightspeedintelligence.abconfig;

/**
 * Thrown when a namespace-optional entry point is called with no explicit
 * namespace and the client has no default namespace configured (neither
 * {@code Config.defaultNamespace} nor the {@code PROJECT_DEFAULT_NAMESPACE}
 * environment variable was set).
 *
 * <p>Mirrors the Go SDK's {@code ErrNamespaceRequired}.
 */
public class NamespaceRequiredException extends TipsyConfigException {

    private static final long serialVersionUID = 1L;

    /**
     * Creates the exception with the given message.
     *
     * @param message the detail message (should start with {@code tipsyabconfig:})
     */
    public NamespaceRequiredException(String message) {
        super(message);
    }
}
