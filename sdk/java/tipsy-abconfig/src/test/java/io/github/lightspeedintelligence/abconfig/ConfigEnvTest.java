package io.github.lightspeedintelligence.abconfig;

import static java.nio.charset.StandardCharsets.UTF_8;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.util.JsonFormat;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import io.github.lightspeedintelligence.abconfig.AbtestTestSupport.NsCache;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllRequest;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllResponse;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.SubscribeRequest;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.net.http.HttpClient;
import java.time.Duration;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * Client-level {@code env} configuration tests (design D12): the {@code Config}
 * builder exposes an {@code env} knob defaulting to {@code ""}, and the resolved
 * value is stamped onto every outbound request the SDK issues — on BOTH the gRPC
 * and HTTP channels.
 *
 * <p>Covered request-construction sites:
 * <ul>
 *   <li>{@link TipsyAbConfigClient#getExperimentResult} (abtest RPC)</li>
 *   <li>{@code AbtestContext.fetchConfigVersionFlatKvForNamespace} (the
 *       {@code config_version flat_kv} fast path driven by {@code getConfig})</li>
 *   <li>{@code pullOnce} (startup PullAll)</li>
 *   <li>{@code subscribeOnce} (background Subscribe)</li>
 * </ul>
 *
 * <p>Default contract: when {@code env} is left unset it is {@code ""}; the gRPC
 * request carries the empty string, and the HTTP protojson body omits the field
 * entirely (JsonFormat drops zero-value scalars).
 */
final class ConfigEnvTest {

    private static final String NS = "checkout";
    private static final Duration WAIT = Duration.ofSeconds(5);

    // ==================================================================
    // gRPC channel
    // ==================================================================

    private static AbtestTestSupport.Builder grpcBase() {
        return AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("color", 7L, Map.of(7L, "blue", 9L, "gold")))
                .abtestConfigFlatKv(NS, Map.of("color", 9L));
    }

    @Test
    @DisplayName("gRPC: default (unset) env -> GetExperimentResult request carries env=\"\"")
    void grpcDefaultEnvEmptyOnExperimentResult() {
        try (AbtestTestSupport h = grpcBase().build()) {
            ExperimentResultRequest req = ExperimentResultRequest.builder()
                    .namespace(NS).userInfo("u-1", Map.of()).build();
            h.client.getExperimentResult(req);

            GetExperimentResultRequest seen = h.abtest.requests.peek();
            assertNotNull(seen, "the server must have received a request");
            assertEquals("", seen.getEnv(),
                    "an unset Config.env must send the empty string on the wire");
        }
    }

    @Test
    @DisplayName("gRPC: Config.env=\"prod\" -> GetExperimentResult request carries env=\"prod\"")
    void grpcEnvStampedOnExperimentResult() {
        try (AbtestTestSupport h = grpcBase().env("prod").build()) {
            ExperimentResultRequest req = ExperimentResultRequest.builder()
                    .namespace(NS).userInfo("u-1", Map.of()).build();
            h.client.getExperimentResult(req);

            GetExperimentResultRequest seen = h.abtest.requests.peek();
            assertNotNull(seen);
            assertEquals("prod", seen.getEnv(), "Config.env must travel on the abtest RPC");
        }
    }

    @Test
    @DisplayName("gRPC: Config.env=\"prod\" -> config_version flat_kv fast path (getConfig) carries env")
    void grpcEnvStampedOnConfigVersionFlatKvFastPath() {
        try (AbtestTestSupport h = grpcBase().env("prod").build()) {
            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            h.client.getConfig(ctx, NS, "color", "DEF");
            assertTrue(AbtestTestSupport.awaitTrue(() -> !h.abtest.requests.isEmpty(), WAIT),
                    "the lazy flat_kv fetch should issue the RPC");

            GetExperimentResultRequest seen = h.abtest.requests.peek();
            assertNotNull(seen);
            assertEquals("prod", seen.getEnv(),
                    "Config.env must travel on the AbtestContext flat_kv fetch");
        }
    }

    @Test
    @DisplayName("gRPC: Config.env=\"prod\" -> startup PullAll request carries env")
    void grpcEnvStampedOnPullAll() {
        try (AbtestTestSupport h = grpcBase().env("prod").build()) {
            assertTrue(AbtestTestSupport.awaitTrue(
                            () -> !h.config.pullAllRequests.isEmpty(), WAIT),
                    "startup PullAll should have run");
            PullAllRequest seen = h.config.pullAllRequests.peek();
            assertNotNull(seen);
            assertEquals("prod", seen.getEnv(), "Config.env must travel on the PullAll request");
        }
    }

    @Test
    @DisplayName("gRPC: Config.env=\"prod\" -> background Subscribe request carries env")
    void grpcEnvStampedOnSubscribe() {
        try (AbtestTestSupport h = grpcBase().env("prod").build()) {
            assertTrue(AbtestTestSupport.awaitTrue(
                            () -> !h.config.subscribeRequests.isEmpty(), WAIT),
                    "the background Subscribe stream should have connected");
            SubscribeRequest seen = h.config.subscribeRequests.peek();
            assertNotNull(seen);
            assertEquals("prod", seen.getEnv(), "Config.env must travel on the Subscribe request");
        }
    }

    @Test
    @DisplayName("gRPC: default (unset) env -> PullAll request carries env=\"\"")
    void grpcDefaultEnvEmptyOnPullAll() {
        try (AbtestTestSupport h = grpcBase().build()) {
            assertTrue(AbtestTestSupport.awaitTrue(
                            () -> !h.config.pullAllRequests.isEmpty(), WAIT),
                    "startup PullAll should have run");
            PullAllRequest seen = h.config.pullAllRequests.peek();
            assertNotNull(seen);
            assertEquals("", seen.getEnv(), "an unset Config.env sends the empty string");
        }
    }

    // ==================================================================
    // HTTP channel (protojson whole-message body)
    // ==================================================================

    private HttpServer server;
    private String baseUrl;
    private HttpClient httpClient;
    private TipsyAbConfigClient httpClientUnderTest;
    /** path -> last request body observed (protojson text). */
    private final ConcurrentHashMap<String, String> lastBodyByPath = new ConcurrentHashMap<>();

    @BeforeEach
    void setUpHttp() throws IOException {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        baseUrl = "http://127.0.0.1:" + server.getAddress().getPort();
        httpClient = HttpClient.newHttpClient();
        lastBodyByPath.clear();
    }

    @AfterEach
    void tearDownHttp() {
        if (httpClientUnderTest != null) {
            httpClientUnderTest.close();
            httpClientUnderTest = null;
        }
        if (server != null) {
            server.stop(0);
        }
    }

    /** Registers a handler that records the request body then returns protojson {@code body}. */
    private void stub(String path, byte[] body) {
        server.createContext(path, exchange -> {
            recordBody(path, exchange);
            respond(exchange, 200, body);
        });
    }

    private void recordBody(String path, HttpExchange exchange) throws IOException {
        byte[] req;
        try (InputStream in = exchange.getRequestBody()) {
            req = in.readAllBytes();
        }
        lastBodyByPath.put(path, new String(req, UTF_8));
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

    private Config.Builder httpCfg(String env) {
        Config.Builder b = Config.builder()
                .namespaces(NS)
                .configServiceAddr(baseUrl)
                .abtestServiceAddr(baseUrl)
                .token("tok")
                .transport(Transport.HTTP)
                .httpClient(httpClient)
                .pullInterval(Duration.ofMinutes(10)) // keep the periodic loop out of the way
                .pullTimeout(Duration.ofSeconds(3));
        if (env != null) {
            b.env(env);
        }
        return b;
    }

    @Test
    @DisplayName("HTTP: Config.env=\"prod\" -> PullAll JSON body carries \"env\":\"prod\"")
    void httpEnvStampedOnPullAllBody() throws Exception {
        stub(HttpConfigTransport.PATH_PULL_ALL,
                JsonFormat.printer().print(PullAllResponse.getDefaultInstance()).getBytes(UTF_8));
        server.start();

        httpClientUnderTest = TipsyAbConfigClient.create(httpCfg("prod").build());

        String body = lastBodyByPath.get(HttpConfigTransport.PATH_PULL_ALL);
        assertNotNull(body, "the pull_all endpoint must have been hit at startup");
        assertTrue(body.contains("\"env\""),
                "protojson body must carry the env field: " + body);
        assertTrue(body.contains("prod"),
                "protojson body must carry env=prod: " + body);
    }

    @Test
    @DisplayName("HTTP: default (unset) env -> PullAll JSON body omits the env field")
    void httpDefaultEnvOmittedFromPullAllBody() throws Exception {
        stub(HttpConfigTransport.PATH_PULL_ALL,
                JsonFormat.printer().print(PullAllResponse.getDefaultInstance()).getBytes(UTF_8));
        server.start();

        httpClientUnderTest = TipsyAbConfigClient.create(httpCfg(null).build());

        String body = lastBodyByPath.get(HttpConfigTransport.PATH_PULL_ALL);
        assertNotNull(body, "the pull_all endpoint must have been hit at startup");
        assertFalse(body.contains("\"env\""),
                "protojson must omit a zero-value env field: " + body);
    }

    @Test
    @DisplayName("HTTP: Config.env=\"prod\" -> GetExperimentResult JSON body carries \"env\":\"prod\"")
    void httpEnvStampedOnExperimentResultBody() throws Exception {
        stub(HttpConfigTransport.PATH_PULL_ALL,
                JsonFormat.printer().print(PullAllResponse.getDefaultInstance()).getBytes(UTF_8));
        stub(HttpAbtestTransport.PATH_EXPERIMENT_RESULT,
                JsonFormat.printer().print(GetExperimentResultResponse.getDefaultInstance())
                        .getBytes(UTF_8));
        server.start();

        httpClientUnderTest = TipsyAbConfigClient.create(httpCfg("prod").build());

        ExperimentResultRequest req = ExperimentResultRequest.builder()
                .namespace(NS).userInfo("u-1", Map.of()).build();
        httpClientUnderTest.getExperimentResult(req);

        String body = lastBodyByPath.get(HttpAbtestTransport.PATH_EXPERIMENT_RESULT);
        assertNotNull(body, "the experiment_result endpoint must have been hit");
        assertTrue(body.contains("\"env\""),
                "protojson body must carry the env field: " + body);
        assertTrue(body.contains("prod"),
                "protojson body must carry env=prod: " + body);
    }
}
