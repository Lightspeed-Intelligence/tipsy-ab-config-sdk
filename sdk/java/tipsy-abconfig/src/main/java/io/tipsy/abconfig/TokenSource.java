package io.tipsy.abconfig;

import io.grpc.CallCredentials;
import io.grpc.Metadata;
import io.grpc.Status;
import java.util.Objects;
import java.util.concurrent.Executor;
import java.util.function.Supplier;

/**
 * Shared bearer-token abstraction used by both transports.
 *
 * <p>Mirrors the Go {@code tokenSource}: it resolves a token from either a
 * static string or a dynamic {@link TokenProvider} (the provider, when present,
 * takes precedence) and exposes that token in the two shapes the SDK needs:
 * <ul>
 *   <li>{@link #toCallCredentials()} — a gRPC {@link CallCredentials} that adds
 *       the {@code authorization: Bearer <token>} metadata to every RPC; a
 *       provider failure fails the RPC with {@code UNAUTHENTICATED}.</li>
 *   <li>{@link #authHeaderValue()} — the {@code "Bearer <token>"} string for the
 *       HTTP {@code Authorization} header; {@link #httpAuthHeaderSupplier()}
 *       wraps it as a {@link Supplier} for wiring into the HTTP transport's
 *       {@code Supplier<String>} auth seam (ST3 passes this to ST2's HTTP
 *       transport).</li>
 * </ul>
 *
 * <p>Like the Go {@code tokenSource}, this does not require transport security:
 * the token is attached even on plaintext h2c. The metadata key is the
 * lower-case {@code authorization} per the grpc-metadata convention.
 */
final class TokenSource {

    private static final Metadata.Key<String> AUTHORIZATION =
            Metadata.Key.of("authorization", Metadata.ASCII_STRING_MARSHALLER);

    private final String staticToken;
    private final TokenProvider provider;

    private TokenSource(String staticToken, TokenProvider provider) {
        this.staticToken = staticToken;
        this.provider = provider;
    }

    /**
     * Builds a {@link TokenSource} from the static-token / dynamic-provider
     * config knobs, mirroring Go's {@code bearerCredentialsFromConfig}: a
     * non-{@code null} {@link TokenProvider} takes precedence; otherwise the
     * static token is used.
     *
     * @param token    the static token (used when {@code provider} is null); may
     *                 be {@code null}/empty if a provider is supplied
     * @param provider the dynamic provider; may be {@code null}
     * @return a token source
     */
    static TokenSource of(String token, TokenProvider provider) {
        if (provider != null) {
            return new TokenSource(null, provider);
        }
        return new TokenSource(token, null);
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
     * Returns the {@code Authorization} header value {@code "Bearer <token>"} for
     * HTTP-mode requests.
     *
     * @return the header value
     * @throws Exception if a configured {@link TokenProvider} throws
     */
    String authHeaderValue() throws Exception {
        return "Bearer " + token();
    }

    /**
     * Returns a {@link Supplier} that yields the {@code "Bearer <token>"} header
     * value, for wiring into the HTTP transport's {@code Supplier<String>} auth
     * seam. A {@link TokenProvider} failure is rethrown wrapped in a
     * {@link RuntimeException} (the supplier contract is unchecked); ST3's HTTP
     * transport surfaces that to the call site.
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
     * Returns a gRPC {@link CallCredentials} that attaches the bearer token as
     * {@code authorization: Bearer <token>} on every RPC. A {@link TokenProvider}
     * failure fails the in-flight RPC with {@code UNAUTHENTICATED}.
     *
     * @return the per-RPC call credentials
     */
    CallCredentials toCallCredentials() {
        return new BearerCallCredentials(this);
    }

    /**
     * gRPC {@link CallCredentials} backed by a {@link TokenSource}. Mirrors the
     * Go {@code tokenSource.GetRequestMetadata}: it adds the lower-case
     * {@code authorization} metadata with the {@code "Bearer <token>"} value and
     * fails the RPC with {@code UNAUTHENTICATED} when a dynamic provider throws.
     */
    static final class BearerCallCredentials extends CallCredentials {

        private final TokenSource source;

        BearerCallCredentials(TokenSource source) {
            this.source = Objects.requireNonNull(source, "source");
        }

        @Override
        public void applyRequestMetadata(RequestInfo requestInfo, Executor appExecutor,
                MetadataApplier applier) {
            final String token;
            try {
                token = source.token();
            } catch (Exception e) {
                applier.fail(Status.UNAUTHENTICATED
                        .withDescription("tipsyabconfig: token provider failed")
                        .withCause(e));
                return;
            }
            Metadata headers = new Metadata();
            headers.put(AUTHORIZATION, "Bearer " + token);
            applier.apply(headers);
        }

        @Override
        public void thisUsesUnstableApi() {
            // Intentionally empty: required acknowledgement of the unstable
            // CallCredentials API (mirrors the no-op in other grpc-java clients).
        }
    }
}
