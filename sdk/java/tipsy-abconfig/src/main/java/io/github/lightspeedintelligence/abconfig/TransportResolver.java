package io.github.lightspeedintelligence.abconfig;

/**
 * Transport-mode and HTTP base-URL resolution helpers.
 *
 * <p>Port of the Go {@code resolveTransport} / {@code validateHTTPBaseURL}
 * ({@code sdk.go}). These run during {@code Config} validation (before dialing);
 * all failures are parameter errors ({@link ConfigValidationException}) that are
 * never absorbed by {@code startupFailOpen}.
 */
final class TransportResolver {

    /**
     * Environment variable consulted when {@code Config.transport} is unset, the
     * Java mirror of the Go {@code transportEnvVarName}.
     */
    static final String TRANSPORT_ENV_VAR = "TIPSY_SDK_TRANSPORT";

    private TransportResolver() {
    }

    /**
     * Resolves the transport mode: an explicit non-null {@code configured} value
     * wins; otherwise the {@code TIPSY_SDK_TRANSPORT} environment variable is
     * consulted (trimmed + lower-cased); an empty result defaults to
     * {@link Transport#GRPC}. Any value other than {@code grpc} / {@code http} is
     * a parameter error.
     *
     * @param configured the explicit {@code Config.transport}; may be {@code null}
     * @return the resolved transport
     * @throws ConfigValidationException if the env var holds an invalid value
     */
    static Transport resolveTransport(Transport configured) {
        if (configured != null) {
            return configured;
        }
        return resolveTransportFromEnv(System.getenv(TRANSPORT_ENV_VAR));
    }

    /**
     * Resolves the transport mode from a raw environment-variable value. Split
     * out from {@link #resolveTransport(Transport)} so it is unit-testable
     * without mutating the process environment.
     *
     * @param rawEnv the raw {@code TIPSY_SDK_TRANSPORT} value; may be {@code null}
     * @return the resolved transport
     * @throws ConfigValidationException if {@code rawEnv} holds an invalid value
     */
    static Transport resolveTransportFromEnv(String rawEnv) {
        String raw = rawEnv == null ? "" : rawEnv;
        String v = raw.trim().toLowerCase();
        if (v.isEmpty()) {
            return Transport.GRPC;
        }
        if (Transport.GRPC.wireValue().equals(v)) {
            return Transport.GRPC;
        }
        if (Transport.HTTP.wireValue().equals(v)) {
            return Transport.HTTP;
        }
        throw new ConfigValidationException(String.format(
                "tipsyabconfig: invalid transport \"%s\" (must be \"%s\" or \"%s\")",
                raw, Transport.GRPC.wireValue(), Transport.HTTP.wireValue()));
    }

    /**
     * Validates and normalises an HTTP-mode base URL: it must start with
     * {@code http://} or {@code https://}, and any trailing {@code /} is trimmed.
     *
     * @param field the offending {@code Config} field name, for the error message
     * @param addr  the configured base URL
     * @return the normalised base URL (trailing slashes removed)
     * @throws ConfigValidationException if {@code addr} is not an
     *                                   {@code http(s)://} URL
     */
    static String validateHttpBaseURL(String field, String addr) {
        if (addr == null || (!addr.startsWith("http://") && !addr.startsWith("https://"))) {
            throw new ConfigValidationException(String.format(
                    "tipsyabconfig: %s must start with http:// or https:// in HTTP transport mode, got \"%s\"",
                    field, addr));
        }
        int end = addr.length();
        while (end > 0 && addr.charAt(end - 1) == '/') {
            end--;
        }
        return addr.substring(0, end);
    }
}
