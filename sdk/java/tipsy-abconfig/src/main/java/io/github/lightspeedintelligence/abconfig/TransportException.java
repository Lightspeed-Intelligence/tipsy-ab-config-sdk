package io.github.lightspeedintelligence.abconfig;

/**
 * Internal checked exception for HTTP transport failures (oversized response,
 * non-2xx status, token-acquisition failure).
 *
 * <p>Package-private and intentionally distinct from the public configuration
 * exception hierarchy: the transport interfaces declare {@code throws Exception},
 * and the client's retry / degrade paths treat any transport failure uniformly.
 * Keeping this internal avoids coupling the transport layer to other subtasks'
 * public exception types.
 */
final class TransportException extends Exception {

    private static final long serialVersionUID = 1L;

    TransportException(String message) {
        super(message);
    }

    TransportException(String message, Throwable cause) {
        super(message, cause);
    }
}
