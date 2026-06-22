package io.tipsy.abconfig;

import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.List;
import org.junit.jupiter.api.Test;

/**
 * Parameter / address validation for {@link TipsyAbConfigClient#create(Config)}.
 *
 * <p>Design refs: 03-core-client-api.md §"校验顺序" (the seven ordered checks, all
 * throwing {@link ConfigValidationException} and never absorbed by
 * {@code startupFailOpen}) and 04-transport-and-cache.md §"地址解析" / §"HTTP 传输".
 *
 * <p>These cases never reach a transport: each one trips a validation gate that
 * fires before any channel is dialed or PullAll is issued, so no in-process /
 * HTTP stub is needed and the tests are fully deterministic.
 */
final class ConfigValidationTest {

    // ---- 1. namespaces non-empty -----------------------------------------

    @Test
    void emptyNamespaces_throwsConfigValidation() {
        Config cfg = baseGrpc()
                .namespaces(List.of())
                .build();
        ConfigValidationException ex =
                assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
        assertTrue(ex.getMessage().contains("Namespaces"),
                "message must name the offending field: " + ex.getMessage());
    }

    @Test
    void namespacesEmptyAfterDedup_throwsConfigValidation() {
        // Only null / empty entries survive build() (varargs preserves them);
        // sort+dedup drops them all, leaving an empty subscribed set -> error.
        Config cfg = baseGrpc()
                .namespaces("", "", "")
                .build();
        ConfigValidationException ex =
                assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
        assertTrue(ex.getMessage().contains("Namespaces"),
                "message must name the offending field: " + ex.getMessage());
    }

    // ---- 3. configServiceAddr non-empty ----------------------------------

    @Test
    void emptyConfigServiceAddr_throwsConfigValidation() {
        Config cfg = Config.builder()
                .namespaces("checkout")
                .configServiceAddr("")
                .token("tok")
                .transport(Transport.GRPC)
                .build();
        ConfigValidationException ex =
                assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
        assertTrue(ex.getMessage().contains("ConfigServiceAddr"),
                "message must name the offending field: " + ex.getMessage());
    }

    @Test
    void nullConfigServiceAddr_throwsConfigValidation() {
        Config cfg = Config.builder()
                .namespaces("checkout")
                .token("tok")
                .transport(Transport.GRPC)
                .build(); // configServiceAddr never set -> null
        assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
    }

    // ---- 4. token or tokenProvider at least one --------------------------

    @Test
    void neitherTokenNorProvider_throwsConfigValidation() {
        Config cfg = Config.builder()
                .namespaces("checkout")
                .configServiceAddr("passthrough:///x")
                .transport(Transport.GRPC)
                .build(); // no token, no tokenProvider
        ConfigValidationException ex =
                assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
        assertTrue(ex.getMessage().contains("Token") || ex.getMessage().contains("TokenProvider"),
                "message must mention token requirement: " + ex.getMessage());
    }

    @Test
    void emptyTokenAndNoProvider_throwsConfigValidation() {
        Config cfg = Config.builder()
                .namespaces("checkout")
                .configServiceAddr("passthrough:///x")
                .token("") // empty string does not satisfy the token requirement
                .transport(Transport.GRPC)
                .build();
        assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
    }

    // ---- 5. HTTP mode base URL must be http(s):// -------------------------

    @Test
    void httpMode_nonHttpBaseUrl_throwsConfigValidation() {
        Config cfg = Config.builder()
                .namespaces("checkout")
                .configServiceAddr("grpc://localhost:9000") // not http(s)
                .token("tok")
                .transport(Transport.HTTP)
                .build();
        ConfigValidationException ex =
                assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
        assertTrue(ex.getMessage().contains("http"),
                "message must mention the http(s) requirement: " + ex.getMessage());
    }

    @Test
    void httpMode_bareHostBaseUrl_throwsConfigValidation() {
        Config cfg = Config.builder()
                .namespaces("checkout")
                .configServiceAddr("localhost:8080") // bare host:port is invalid in HTTP mode
                .token("tok")
                .transport(Transport.HTTP)
                .build();
        assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
    }

    @Test
    void httpMode_abtestNonHttpBaseUrl_throwsConfigValidation() {
        // config addr is valid http, but abtest addr is not -> still a param error.
        Config cfg = Config.builder()
                .namespaces("checkout")
                .configServiceAddr("http://127.0.0.1:65535")
                .abtestServiceAddr("grpc://localhost:9000")
                .token("tok")
                .transport(Transport.HTTP)
                .build();
        assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
    }

    // ---- 7. gRPC target parse errors surface before dialing --------------

    @Test
    void grpcMode_httpSchemeAddr_throwsConfigValidation() {
        // http:// in gRPC mode is a parameter error (design 04 rule 5).
        Config cfg = Config.builder()
                .namespaces("checkout")
                .configServiceAddr("http://localhost:9000")
                .token("tok")
                .transport(Transport.GRPC)
                .build();
        assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
    }

    @Test
    void grpcMode_grpcsMissingPort_throwsConfigValidation() {
        // grpcs:// requires a numeric port (design 04 rule 4).
        Config cfg = Config.builder()
                .namespaces("checkout")
                .configServiceAddr("grpcs://localhost")
                .token("tok")
                .transport(Transport.GRPC)
                .build();
        assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
    }

    @Test
    void grpcMode_grpcWithAuthorityQuery_throwsConfigValidation() {
        // grpc:// (plaintext) must not carry an authority/insecure query.
        Config cfg = Config.builder()
                .namespaces("checkout")
                .configServiceAddr("grpc://host:9000?authority=foo")
                .token("tok")
                .transport(Transport.GRPC)
                .build();
        assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
    }

    // ---- startupFailOpen never absorbs parameter errors ------------------

    @Test
    void startupFailOpen_doesNotAbsorbEmptyNamespaces() {
        Config cfg = baseGrpc()
                .namespaces(List.of())
                .startupFailOpen(true) // must NOT swallow a parameter error
                .build();
        assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
    }

    @Test
    void startupFailOpen_doesNotAbsorbMissingToken() {
        Config cfg = Config.builder()
                .namespaces("checkout")
                .configServiceAddr("passthrough:///x")
                .transport(Transport.GRPC)
                .startupFailOpen(true)
                .build();
        assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
    }

    @Test
    void startupFailOpen_doesNotAbsorbBadGrpcTarget() {
        Config cfg = Config.builder()
                .namespaces("checkout")
                .configServiceAddr("grpcs://localhost") // missing port
                .token("tok")
                .transport(Transport.GRPC)
                .startupFailOpen(true)
                .build();
        assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
    }

    @Test
    void startupFailOpen_doesNotAbsorbBadHttpBaseUrl() {
        Config cfg = Config.builder()
                .namespaces("checkout")
                .configServiceAddr("ftp://localhost")
                .token("tok")
                .transport(Transport.HTTP)
                .startupFailOpen(true)
                .build();
        assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
    }

    /** A gRPC-mode builder pre-loaded with a valid token + in-process-style addr. */
    private static Config.Builder baseGrpc() {
        return Config.builder()
                .namespaces("checkout")
                .configServiceAddr("passthrough:///x")
                .token("tok")
                .transport(Transport.GRPC);
    }
}
