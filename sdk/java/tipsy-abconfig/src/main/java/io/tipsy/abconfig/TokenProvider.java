package io.tipsy.abconfig;

/**
 * Supplies a fresh bearer token on every request.
 *
 * <p>Mirrors the Go SDK's {@code TokenProvider func(ctx) (string, error)}: when
 * configured it takes precedence over the static {@code token} and is consulted
 * for each RPC (gRPC per-call credentials) or HTTP request, enabling
 * short-lived / rotating tokens.
 *
 * <p>The Java signature omits the Go {@code context.Context} parameter — the
 * per-call deadline and cancellation are carried implicitly by the gRPC call
 * context / HTTP request, so providers do not need to thread a context through.
 * Implementations may throw any exception; the SDK fails the in-flight RPC with
 * {@code UNAUTHENTICATED} (gRPC) or surfaces the error to the HTTP call site.
 */
@FunctionalInterface
public interface TokenProvider {

    /**
     * Returns the bearer token to attach to the next request.
     *
     * @return a non-{@code null} token string (placed verbatim after
     *         {@code "Bearer "})
     * @throws Exception if a token cannot be obtained; the in-flight request is
     *                   failed
     */
    String getToken() throws Exception;
}
