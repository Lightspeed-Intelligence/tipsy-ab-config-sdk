package io.github.lightspeedintelligence.abconfig;

import static java.nio.charset.StandardCharsets.UTF_8;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import ch.qos.logback.classic.spi.ILoggingEvent;
import ch.qos.logback.core.read.ListAppender;
import com.google.protobuf.util.JsonFormat;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.ConfigServiceGrpc;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.ConfigUpdateEvent;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.KeyState;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSnapshot;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllRequest;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllResponse;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.SubscribeRequest;
import io.grpc.CallCredentials;
import io.grpc.ManagedChannelBuilder;
import io.grpc.Metadata;
import io.grpc.Server;
import io.grpc.ServerCall;
import io.grpc.ServerCallHandler;
import io.grpc.ServerInterceptor;
import io.grpc.ServerInterceptors;
import io.grpc.inprocess.InProcessChannelBuilder;
import io.grpc.inprocess.InProcessServerBuilder;
import io.grpc.stub.StreamObserver;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.net.http.HttpClient;
import java.time.Duration;
import java.util.List;
import java.util.Optional;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.Executor;
import java.util.function.UnaryOperator;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.slf4j.LoggerFactory;

/**
 * Issue #16 — secretKey-only authentication.
 *
 * <p>Contract under test (design-phase2 §"#16 SDK 侧"):
 * <ul>
 *   <li>credential precedence is <b>SecretKey &gt; TokenProvider &gt; Token</b>,
 *       decided at the single value point {@code TokenSource.authHeaderValue()}
 *       which feeds BOTH the gRPC {@code CallCredentials} and the HTTP
 *       {@code Supplier<String>} seam — hence the dual-path (gRPC metadata +
 *       HTTP header) exact-literal assertions below;</li>
 *   <li>the wire literal is exactly {@code "SecretKey <secret>"} (the server
 *       dispatches on the scheme prefix; assertions use string equality, not
 *       {@code contains}, so a scheme typo or missing space fails);</li>
 *   <li>Init accepts any one of secretKey / token / tokenProvider; with none
 *       of the three it fails with the unified message
 *       {@code "tipsyabconfig: SecretKey, Token or TokenProvider must be set"};</li>
 *   <li>the secret value never appears in log output.</li>
 * </ul>
 */
final class SecretKeyAuthTest {

    /** Unique per-suite literal so the log-scrub assertion cannot cross-match. */
    private static final String SECRET = "sk-secret-0016-e5f1";

    private static final Metadata.Key<String> AUTHORIZATION =
            Metadata.Key.of("authorization", Metadata.ASCII_STRING_MARSHALLER);

    private static final Executor DIRECT_EXECUTOR = Runnable::run;

    // ------------------------------------------------------------------
    // TokenSource unit: the single value point.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("仅 secretKey: authHeaderValue() 恒等于精确字面量 \"SecretKey <secret>\"")
    void secretKeyOnly_authHeaderValueExactLiteral() throws Exception {
        TokenSource ts = TokenSource.of(SECRET, null, null);
        assertEquals("SecretKey " + SECRET, ts.authHeaderValue());
        assertEquals("SecretKey " + SECRET, ts.httpAuthHeaderSupplier().get());
    }

    @Test
    @DisplayName("优先级: secretKey + token + provider 三者同配 -> SecretKey 赢（HTTP supplier 路径）")
    void secretKeyBeatsProviderAndToken_httpPath() throws Exception {
        TokenProvider provider = () -> "provider-tok";
        TokenSource ts = TokenSource.of(SECRET, "static-tok", provider);
        assertEquals("SecretKey " + SECRET, ts.authHeaderValue());
        assertEquals("SecretKey " + SECRET, ts.httpAuthHeaderSupplier().get());
    }

    @Test
    @DisplayName("优先级: secretKey + token + provider 三者同配 -> SecretKey 赢（gRPC CallCredentials 路径）")
    void secretKeyBeatsProviderAndToken_grpcPath() {
        TokenProvider provider = () -> "provider-tok";
        TokenSource ts = TokenSource.of(SECRET, "static-tok", provider);

        CapturingApplier applier = new CapturingApplier();
        CallCredentials cc = ts.toCallCredentials();
        cc.applyRequestMetadata(null, DIRECT_EXECUTOR, applier);

        assertEquals(1, applier.applyCalls, "成功路径 apply 恰一次");
        assertEquals(0, applier.failCalls);
        assertEquals("SecretKey " + SECRET, applier.appliedHeaders.get(AUTHORIZATION),
                "gRPC metadata 必须是精确的 SecretKey scheme 字面量");
    }

    @Test
    @DisplayName("优先级: secretKey 在场时 provider 完全不被求值（不会因 provider 抛异常而失败）")
    void secretKeyPresent_providerNeverEvaluated() throws Exception {
        TokenProvider explodes = () -> {
            throw new IllegalStateException("provider must not be consulted when secretKey is set");
        };
        TokenSource ts = TokenSource.of(SECRET, null, explodes);
        assertEquals("SecretKey " + SECRET, ts.authHeaderValue(),
                "secretKey 在场时不应触发 provider 求值");
    }

    @Test
    @DisplayName("空串 secretKey 不激活 SecretKey scheme: 回落 Bearer（回归）")
    void emptySecretKey_fallsBackToBearer() throws Exception {
        TokenSource ts = TokenSource.of("", "tok-1", null);
        assertEquals("Bearer tok-1", ts.authHeaderValue());
    }

    // ------------------------------------------------------------------
    // Init validation.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("三者皆无 -> ConfigValidationException，统一文案精确等值")
    void noCredentialAtAll_throwsWithUnifiedMessage() {
        Config cfg = Config.builder()
                .namespaces("checkout")
                .configServiceAddr("passthrough:///x")
                .transport(Transport.GRPC)
                .build();
        ConfigValidationException ex =
                assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
        assertEquals("tipsyabconfig: SecretKey, Token or TokenProvider must be set", ex.getMessage(),
                "三端统一的 Init 错误文案（精确等值，防漂移）");
    }

    @Test
    @DisplayName("空串 secretKey + 空串 token + 无 provider -> 仍拒绝 Init（空串不算凭据）")
    void emptyStringsDoNotSatisfyCredentialCheck() {
        Config cfg = Config.builder()
                .namespaces("checkout")
                .configServiceAddr("passthrough:///x")
                .secretKey("")
                .token("")
                .transport(Transport.GRPC)
                .build();
        assertThrows(ConfigValidationException.class, () -> TipsyAbConfigClient.create(cfg));
    }

    // ------------------------------------------------------------------
    // Full client, gRPC path: only secretKey -> Init succeeds, and the
    // wire metadata is the exact SecretKey literal. Secret never logged.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("gRPC 全链路: 仅 secretKey Init 成功，authorization metadata 精确等于 SecretKey <v>，且 secret 不入日志")
    void grpcFullClient_secretKeyOnly_initSucceeds_metadataExact_noSecretInLogs() throws Exception {
        ListAppender<ILoggingEvent> logs = attachRootListAppender();
        try (RecordingAuthServer srv = new RecordingAuthServer()) {
            Config cfg = Config.builder()
                    .namespaces("checkout")
                    .configServiceAddr("passthrough:///x")
                    .secretKey(SECRET) // the ONLY credential
                    .transport(Transport.GRPC)
                    .channelConfigurator(srv.channelConfigurator())
                    .pullInterval(Duration.ofMinutes(10))
                    .build();

            TipsyAbConfigClient client = TipsyAbConfigClient.create(cfg);
            try {
                Optional<String> v = client.getConfigStatic("checkout", "color");
                assertTrue(v.isPresent(), "startup PullAll must succeed with secretKey-only auth");
                assertEquals("blue", v.get());
            } finally {
                client.close();
            }

            assertFalse(srv.authValues.isEmpty(), "server must have observed at least one RPC");
            for (String authz : srv.authValues) {
                assertEquals("SecretKey " + SECRET, authz,
                        "every RPC (PullAll/Subscribe) must carry the exact SecretKey literal");
            }
        } finally {
            assertNoSecretInLogs(detachRootListAppender(logs));
        }
    }

    @Test
    @DisplayName("gRPC 全链路: secretKey 与 token/provider 同配 -> 线上发送的是 SecretKey scheme")
    void grpcFullClient_secretKeyWinsOverTokenAndProvider() throws Exception {
        try (RecordingAuthServer srv = new RecordingAuthServer()) {
            Config cfg = Config.builder()
                    .namespaces("checkout")
                    .configServiceAddr("passthrough:///x")
                    .secretKey(SECRET)
                    .token("legacy-token")
                    .tokenProvider(() -> "provider-token")
                    .transport(Transport.GRPC)
                    .channelConfigurator(srv.channelConfigurator())
                    .pullInterval(Duration.ofMinutes(10))
                    .build();

            TipsyAbConfigClient client = TipsyAbConfigClient.create(cfg);
            client.close();

            assertFalse(srv.authValues.isEmpty());
            for (String authz : srv.authValues) {
                assertEquals("SecretKey " + SECRET, authz,
                        "secretKey 优先：不得出现 Bearer 值");
            }
        }
    }

    // ------------------------------------------------------------------
    // Full client, HTTP path: only secretKey -> Init succeeds, and the
    // Authorization header is the exact SecretKey literal. Secret never logged.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("HTTP 全链路: 仅 secretKey Init 成功，Authorization header 精确等于 SecretKey <v>，且 secret 不入日志")
    void httpFullClient_secretKeyOnly_initSucceeds_headerExact_noSecretInLogs() throws Exception {
        ListAppender<ILoggingEvent> logs = attachRootListAppender();
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        CopyOnWriteArrayList<String> authHeaders = new CopyOnWriteArrayList<>();
        try {
            stubPullAll(server, authHeaders);
            String baseUrl = "http://127.0.0.1:" + server.getAddress().getPort();

            Config cfg = Config.builder()
                    .namespaces("checkout")
                    .configServiceAddr(baseUrl)
                    .secretKey(SECRET) // the ONLY credential
                    .transport(Transport.HTTP)
                    .httpClient(HttpClient.newHttpClient())
                    .pullInterval(Duration.ofMinutes(10))
                    .pullTimeout(Duration.ofSeconds(3))
                    .build();

            TipsyAbConfigClient client = TipsyAbConfigClient.create(cfg);
            try {
                Optional<String> v = client.getConfigStatic("checkout", "color");
                assertTrue(v.isPresent(), "HTTP startup pull must succeed with secretKey-only auth");
                assertEquals("blue", v.get());
            } finally {
                client.close();
            }

            assertFalse(authHeaders.isEmpty(), "the pull_all endpoint must have been hit");
            for (String authz : authHeaders) {
                assertEquals("SecretKey " + SECRET, authz,
                        "HTTP Authorization header must be the exact SecretKey literal");
            }
        } finally {
            server.stop(0);
            assertNoSecretInLogs(detachRootListAppender(logs));
        }
    }

    @Test
    @DisplayName("HTTP 全链路: secretKey 与 token 同配 -> header 走 SecretKey scheme")
    void httpFullClient_secretKeyWinsOverToken() throws Exception {
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        CopyOnWriteArrayList<String> authHeaders = new CopyOnWriteArrayList<>();
        try {
            stubPullAll(server, authHeaders);
            String baseUrl = "http://127.0.0.1:" + server.getAddress().getPort();

            Config cfg = Config.builder()
                    .namespaces("checkout")
                    .configServiceAddr(baseUrl)
                    .secretKey(SECRET)
                    .token("legacy-token")
                    .transport(Transport.HTTP)
                    .httpClient(HttpClient.newHttpClient())
                    .pullInterval(Duration.ofMinutes(10))
                    .pullTimeout(Duration.ofSeconds(3))
                    .build();

            TipsyAbConfigClient client = TipsyAbConfigClient.create(cfg);
            client.close();

            assertFalse(authHeaders.isEmpty());
            for (String authz : authHeaders) {
                assertEquals("SecretKey " + SECRET, authz,
                        "secretKey 优先：header 不得为 Bearer legacy-token");
            }
        } finally {
            server.stop(0);
        }
    }

    // ------------------------------------------------------------------
    // helpers
    // ------------------------------------------------------------------

    private static final class CapturingApplier extends CallCredentials.MetadataApplier {
        Metadata appliedHeaders;
        int applyCalls;
        int failCalls;

        @Override
        public void apply(Metadata headers) {
            this.appliedHeaders = headers;
            this.applyCalls++;
        }

        @Override
        public void fail(io.grpc.Status status) {
            this.failCalls++;
        }
    }

    /**
     * In-process ConfigService whose server interceptor records the
     * {@code authorization} metadata of every inbound RPC (PullAll and
     * Subscribe alike). PullAll returns one "checkout/color=blue" snapshot;
     * Subscribe completes immediately (the reconnect loop backs off, and the
     * interceptor keeps recording whatever it re-sends).
     */
    private static final class RecordingAuthServer implements AutoCloseable {
        final CopyOnWriteArrayList<String> authValues = new CopyOnWriteArrayList<>();
        private final String name = InProcessServerBuilder.generateName();
        private final Server server;

        RecordingAuthServer() throws IOException {
            ServerInterceptor capture = new ServerInterceptor() {
                @Override
                public <ReqT, RespT> ServerCall.Listener<ReqT> interceptCall(
                        ServerCall<ReqT, RespT> call, Metadata headers,
                        ServerCallHandler<ReqT, RespT> next) {
                    authValues.add(headers.get(AUTHORIZATION));
                    return next.startCall(call, headers);
                }
            };
            this.server = InProcessServerBuilder.forName(name)
                    .directExecutor()
                    .addService(ServerInterceptors.intercept(new FakeConfigService(), capture))
                    .build()
                    .start();
        }

        UnaryOperator<ManagedChannelBuilder<?>> channelConfigurator() {
            return b -> InProcessChannelBuilder.forName(name).directExecutor();
        }

        @Override
        public void close() {
            server.shutdownNow();
        }

        private static final class FakeConfigService extends ConfigServiceGrpc.ConfigServiceImplBase {
            @Override
            public void pullAll(PullAllRequest request, StreamObserver<PullAllResponse> obs) {
                obs.onNext(PullAllResponse.newBuilder()
                        .addSnapshots(NamespaceSnapshot.newBuilder()
                                .setNamespace("checkout")
                                .setBusinessSnapshotSeq(1)
                                .addKeys(KeyState.newBuilder()
                                        .setKey("color")
                                        .setFullReleaseVersion(7)
                                        .putVersions(7L, "blue")))
                        .build());
                obs.onCompleted();
            }

            @Override
            public void subscribe(SubscribeRequest request, StreamObserver<ConfigUpdateEvent> obs) {
                obs.onCompleted();
            }
        }
    }

    /** Registers a pull_all stub that records each request's Authorization header. */
    private static void stubPullAll(HttpServer server, List<String> authHeaders) throws Exception {
        PullAllResponse resp = PullAllResponse.newBuilder()
                .addSnapshots(NamespaceSnapshot.newBuilder()
                        .setNamespace("checkout")
                        .setBusinessSnapshotSeq(1)
                        .addKeys(KeyState.newBuilder()
                                .setKey("color")
                                .setFullReleaseVersion(7)
                                .putVersions(7L, "blue")))
                .build();
        byte[] body = JsonFormat.printer().print(resp).getBytes(UTF_8);
        server.createContext(HttpConfigTransport.PATH_PULL_ALL, exchange -> {
            authHeaders.add(exchange.getRequestHeaders().getFirst("Authorization"));
            drainRequest(exchange);
            respond(exchange, 200, body);
        });
        server.createContext("/", exchange -> {
            drainRequest(exchange);
            respond(exchange, 404, "no such endpoint".getBytes(UTF_8));
        });
        server.start();
    }

    private static void drainRequest(HttpExchange exchange) throws IOException {
        try (InputStream in = exchange.getRequestBody()) {
            in.readAllBytes();
        }
    }

    private static void respond(HttpExchange exchange, int status, byte[] body) throws IOException {
        exchange.sendResponseHeaders(status, body.length == 0 ? -1 : body.length);
        if (body.length > 0) {
            try (OutputStream out = exchange.getResponseBody()) {
                out.write(body);
            }
        }
        exchange.close();
    }

    /** Attaches a fresh ListAppender to the ROOT logger (captures every SDK logger). */
    private static ListAppender<ILoggingEvent> attachRootListAppender() {
        ch.qos.logback.classic.Logger root =
                (ch.qos.logback.classic.Logger) LoggerFactory.getLogger(org.slf4j.Logger.ROOT_LOGGER_NAME);
        ListAppender<ILoggingEvent> appender = new ListAppender<>();
        appender.start();
        root.addAppender(appender);
        return appender;
    }

    /** Detaches the appender and returns a stable snapshot of what it captured. */
    private static List<ILoggingEvent> detachRootListAppender(ListAppender<ILoggingEvent> appender) {
        ch.qos.logback.classic.Logger root =
                (ch.qos.logback.classic.Logger) LoggerFactory.getLogger(org.slf4j.Logger.ROOT_LOGGER_NAME);
        root.detachAppender(appender);
        appender.stop();
        // toArray snapshot: background loops may still be appending concurrently.
        return List.of(appender.list.toArray(new ILoggingEvent[0]));
    }

    /**
     * The secret value must not appear in any formatted log line. Scoped to the
     * unique {@link #SECRET} literal so concurrent tests' events cannot
     * cross-match.
     */
    private static void assertNoSecretInLogs(List<ILoggingEvent> events) {
        for (ILoggingEvent e : events) {
            String line = e.getFormattedMessage();
            assertFalse(line != null && line.contains(SECRET),
                    "secret value leaked into log line: " + line);
        }
    }
}
