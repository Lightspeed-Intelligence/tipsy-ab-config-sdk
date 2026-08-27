package io.github.lightspeedintelligence.abconfig;

import io.grpc.CallCredentials;
import io.grpc.Metadata;
import io.grpc.Status;
import java.util.Objects;
import java.util.concurrent.Executor;
import java.util.function.Supplier;

/**
 * Shared credential abstraction used by both transports.
 *
 * <p>Mirrors the Go {@code tokenSource}: it resolves a credential with the
 * fixed precedence <b>SecretKey &gt; TokenProvider &gt; Token</b> and exposes
 * it in the two shapes the SDK needs:
 * <ul>
 *   <li>{@link #toCallCredentials()} — a gRPC {@link CallCredentials} that adds
 *       the {@code authorization} metadata ({@code Bearer <token>} or
 *       {@code SecretKey <secret>}) to every RPC; a provider failure fails the
 *       RPC with {@code UNAUTHENTICATED}.</li>
 *   <li>{@link #authHeaderValue()} — the full HTTP {@code Authorization} header
 *       value; {@link #httpAuthHeaderSupplier()} wraps it as a {@link Supplier}
 *       for wiring into the HTTP transport's {@code Supplier<String>} auth seam
 *       (ST3 passes this to ST2's HTTP transport).</li>
 * </ul>
 *
 * <p>{@link #authHeaderValue()} is the single point that decides which
 * credential is sent: both the gRPC {@code CallCredentials} and the HTTP
 * supplier evaluate it per request, so the SecretKey-first precedence and the
 * exact {@code "SecretKey <secret>"} literal hold on both transports by
 * construction.
 *
 * <p>Like the Go {@code tokenSource}, this does not require transport security:
 * the credential is attached even on plaintext h2c. The metadata key is the
 * lower-case {@code authorization} per the grpc-metadata convention.
 */
final class TokenSource {

    private static final Metadata.Key<String> AUTHORIZATION =
            Metadata.Key.of("authorization", Metadata.ASCII_STRING_MARSHALLER);

    private final String secretKey;
    private final String staticToken;
    private final TokenProvider provider;

    private TokenSource(String secretKey, String staticToken, TokenProvider provider) {
        this.secretKey = secretKey;
        this.staticToken = staticToken;
        this.provider = provider;
    }

    /**
     * Two-credential variant of {@link #of(String, String, TokenProvider)}
     * (no secretKey); kept for call sites and tests that predate secretKey.
     */
    static TokenSource of(String token, TokenProvider provider) {
        return of(null, token, provider);
    }

    /**
     * Builds a {@link TokenSource} from the three credential config knobs,
     * mirroring Go's {@code bearerCredentialsFromConfig}: a non-empty
     * {@code secretKey} takes precedence over everything; otherwise a
     * non-{@code null} {@link TokenProvider} takes precedence over the static
     * token.
     *
     * @param secretKey the raw service secret; may be {@code null}/empty
     * @param token     the static token (used when {@code secretKey} is absent
     *                  and {@code provider} is null); may be {@code null}/empty
     * @param provider  the dynamic provider; may be {@code null}
     * @return a token source
     */
    static TokenSource of(String secretKey, String token, TokenProvider provider) {
        String sk = (secretKey == null || secretKey.isEmpty()) ? null : secretKey;
        if (provider != null) {
            return new TokenSource(sk, null, provider);
        }
        return new TokenSource(sk, token, null);
    }

    /**
     * Resolves the current token (provider first, then the static value).
     *
     * @return the token string
     * @throws Exception if a configured {@link TokenProvider} throws
     */
    String token() throws Exception {
        if (provider != null) {
            return provider.getToken();
        }
        return staticToken;
    }

    /**
     * Returns the full {@code Authorization} header value. This is the single
     * credential-selection point for both transports: a configured secretKey
     * wins ({@code "SecretKey <secret>"}, exact literal — the server matches on
     * the scheme prefix); otherwise the bearer path applies
     * ({@code "Bearer <token>"}, provider first, then the static value).
     *
     * @return the header value
     * @throws Exception if a configured {@link TokenProvider} throws
     */
    String authHeaderValue() throws Exception {
        if (secretKey != null) {
            return "SecretKey " + secretKey;
        }
        return "Bearer " + token();
    }

    /**
     * Returns a {@link Supplier} that yields the {@code Authorization} header
     * value ({@link #authHeaderValue()}), for wiring into the HTTP transport's
     * {@code Supplier<String>} auth seam. A {@link TokenProvider} failure is
     * rethrown wrapped in a {@link RuntimeException} (the supplier contract is
     * unchecked); ST3's HTTP transport surfaces that to the call site.
     *
     * @return a supplier of the HTTP auth header value
     */
    Supplier<String> httpAuthHeaderSupplier() {
        return () -> {
            try {
                return authHeaderValue();
            } catch (RuntimeException e) {
                throw e;
            } catch (Exception e) {
                throw new RuntimeException(e);
            }
        };
    }

    /**
     * Returns a gRPC {@link CallCredentials} that attaches the credential as
     * the {@code authorization} metadata on every RPC (same value as
     * {@link #authHeaderValue()}). A {@link TokenProvider} failure fails the
     * in-flight RPC with {@code UNAUTHENTICATED}.
     *
     * @return the per-RPC call credentials
     */
    CallCredentials toCallCredentials() {
        return new BearerCallCredentials(this);
    }

    /**
     * gRPC {@link CallCredentials} backed by a {@link TokenSource}. Mirrors the
     * Go {@code tokenSource.GetRequestMetadata}: it adds the lower-case
     * {@code authorization} metadata with the {@link TokenSource#authHeaderValue()}
     * value — {@code "SecretKey <secret>"} or {@code "Bearer <token>"}, so both
     * transports share the one credential-selection point — and fails the RPC
     * with {@code UNAUTHENTICATED} when a dynamic provider throws. (The name
     * predates secretKey support and is kept for API-shape stability.)
     */
    static final class BearerCallCredentials extends CallCredentials {

        private final TokenSource source;

        BearerCallCredentials(TokenSource source) {
            this.source = Objects.requireNonNull(source, "source");
        }

        @Override
        public void applyRequestMetadata(RequestInfo requestInfo, Executor appExecutor,
                MetadataApplier applier) {
            final String headerValue;
            try {
                headerValue = source.authHeaderValue();
            } catch (Exception e) {
                applier.fail(Status.UNAUTHENTICATED
                        .withDescription("tipsyabconfig: token provider failed")
                        .withCause(e));
                return;
            }
            Metadata headers = new Metadata();
            headers.put(AUTHORIZATION, headerValue);
            applier.apply(headers);
        }

        @Override
        public void thisUsesUnstableApi() {
            // Intentionally empty: required acknowledgement of the unstable
            // CallCredentials API (mirrors the no-op in other grpc-java clients).
        }
    }
}
