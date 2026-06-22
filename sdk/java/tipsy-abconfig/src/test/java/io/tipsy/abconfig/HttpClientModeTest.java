package io.tipsy.abconfig;

import static java.nio.charset.StandardCharsets.UTF_8;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.util.JsonFormat;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import io.tipsy.abconfig.proto.config.v1.KeyState;
import io.tipsy.abconfig.proto.config.v1.NamespaceSnapshot;
import io.tipsy.abconfig.proto.config.v1.PullAllResponse;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.net.http.HttpClient;
import java.time.Duration;
import java.util.Optional;
import java.util.concurrent.atomic.AtomicInteger;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/**
 * HTTP transport-mode lifecycle for {@link TipsyAbConfigClient}, driven by a
 * local JDK {@link HttpServer} stub on an ephemeral port (port 0). No external
 * network, no DEV.
 *
 * <p>Design refs: 04-transport-and-cache.md §"HTTP 传输" ("HTTP 模式不启动 Subscribe")
 * and 03-core-client-api.md §"Health" ("HTTP 模式：subscribeConnected() 恒 false").
 */
final class HttpClientModeTest {

    private HttpServer server;
    private String baseUrl;
    private HttpClient httpClient;
    private TipsyAbConfigClient client;

    private final AtomicInteger pullAllCalls = new AtomicInteger();

    @BeforeEach
    void setUp() throws IOException {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        baseUrl = "http://127.0.0.1:" + server.getAddress().getPort();
        httpClient = HttpClient.newHttpClient();
    }

    @AfterEach
    void tearDown() {
        if (client != null) {
            client.close();
        }
        if (server != null) {
            server.stop(0);
        }
    }

    /** Registers the pull_all endpoint returning the given protojson snapshot. */
    private void stubPullAll(NamespaceSnapshot... snapshots) throws Exception {
        PullAllResponse.Builder resp = PullAllResponse.newBuilder();
        for (NamespaceSnapshot s : snapshots) {
            resp.addSnapshots(s);
        }
        byte[] body = JsonFormat.printer().print(resp.build()).getBytes(UTF_8);
        server.createContext(HttpConfigTransport.PATH_PULL_ALL, exchange -> {
            pullAllCalls.incrementAndGet();
            drainRequest(exchange);
            respond(exchange, 200, body);
        });
        // A catch-all for any other path (e.g. a stray subscribe attempt) so an
        // accidental request fails loudly with 404 instead of hanging.
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

    private Config.Builder httpCfg(String... namespaces) {
        return Config.builder()
                .namespaces(namespaces)
                .configServiceAddr(baseUrl)
                .token("tok")
                .transport(Transport.HTTP)
                .httpClient(httpClient)
                .pullInterval(Duration.ofMinutes(10)) // keep the periodic loop out of the way
                .pullTimeout(Duration.ofSeconds(3));
    }

    // ------------------------------------------------------------------

    @Test
    void httpStartupPull_populatesCache() throws Exception {
        stubPullAll(NamespaceSnapshot.newBuilder()
                .setNamespace("checkout")
                .setBusinessSnapshotSeq(2)
                .setExperimentSnapshotSeq(1)
                .addKeys(KeyState.newBuilder()
                        .setKey("color")
                        .setFullReleaseVersion(11)
                        .putVersions(11L, "blue"))
                .build());

        client = TipsyAbConfigClient.create(httpCfg("checkout").build());

        Optional<String> v = client.getConfigStatic("checkout", "color");
        assertTrue(v.isPresent(), "HTTP startup pull must populate the cache");
        assertEquals("blue", v.get());
        assertFalse(client.health().startupCacheEmpty());
        assertTrue(pullAllCalls.get() >= 1, "the HTTP pull_all endpoint must have been hit");
    }

    @Test
    void httpMode_neverStartsSubscribe_subscribeConnectedAlwaysFalse() throws Exception {
        stubPullAll(NamespaceSnapshot.newBuilder()
                .setNamespace("checkout")
                .setBusinessSnapshotSeq(1)
                .addKeys(KeyState.newBuilder()
                        .setKey("k").setFullReleaseVersion(3).putVersions(3L, "v"))
                .build());

        client = TipsyAbConfigClient.create(httpCfg("checkout").build());

        // subscribeConnected is constant false in HTTP mode: assert immediately
        // and again after a small window (there is no stream to ever flip it).
        assertFalse(client.health().subscribeConnected());
        assertFalse(InProcessConfigServiceHarness.awaitTrue(
                        () -> client.health().subscribeConnected(), 500),
                "HTTP mode must never establish a subscribe stream");
        // No subscribe events ever, and lastSubscribeErr stays empty.
        assertEquals(0, client.metrics().subscribeEventReceivedTotal("checkout"));
        assertTrue(client.health().lastSubscribeErr().isEmpty(),
                "HTTP mode must never record a subscribe error");
    }

    @Test
    void httpMode_startupPullFailure_failClose_throws() {
        // pull_all returns 500: a non-2xx is a transport failure.
        server.createContext(HttpConfigTransport.PATH_PULL_ALL, exchange -> {
            drainRequest(exchange);
            respond(exchange, 500, "{\"error\":\"down\"}".getBytes(UTF_8));
        });
        server.start();

        Config c = httpCfg("checkout")
                .pullRetries(1)
                .pullTimeout(Duration.ofMillis(500))
                .startupFailOpen(false)
                .build();
        org.junit.jupiter.api.Assertions.assertThrows(
                StartupPullFailedException.class, () -> TipsyAbConfigClient.create(c));
    }

    @Test
    void httpMode_startupPullFailure_failOpen_startsEmpty_noSubscribe() {
        server.createContext(HttpConfigTransport.PATH_PULL_ALL, exchange -> {
            drainRequest(exchange);
            respond(exchange, 500, "{\"error\":\"down\"}".getBytes(UTF_8));
        });
        server.start();

        client = TipsyAbConfigClient.create(httpCfg("checkout")
                .pullRetries(1)
                .pullTimeout(Duration.ofMillis(500))
                .startupFailOpen(true)
                .build());

        assertTrue(client.health().startupCacheEmpty());
        assertFalse(client.health().subscribeConnected(),
                "even under fail-open, HTTP mode never opens a subscribe stream");
        assertNotNull(client.namespaces());
    }
}
