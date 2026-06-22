package io.tipsy.abconfig;

import static java.nio.charset.StandardCharsets.UTF_8;
import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
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
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/**
 * HTTP-mode {@link HttpClient} ownership / lifecycle on {@link TipsyAbConfigClient#close()}.
 *
 * <p>Regression coverage for F1 (review finding): the SDK must release ONLY a
 * client it built itself ({@code ownsHttpClient == true} &rArr;
 * {@code httpClient.shutdownNow()}) and must NEVER touch an injected
 * {@code Config.httpClient} (the caller owns its lifecycle). This mirrors the Go
 * SDK's {@code ownsHTTPClient} gating in {@code Close}.
 *
 * <h2>How owned-vs-injected is observed (no production getters/fields added)</h2>
 * The two branches are distinguished purely by externally observable behaviour,
 * because JDK 21's {@link HttpClient} is {@link AutoCloseable} and a client that
 * has had {@code shutdownNow()} called on it rejects subsequent sends:
 * <ul>
 *   <li><b>Injected (the load-bearing case):</b> a caller-built {@link HttpClient}
 *       is passed via {@code httpClient(...)}. After {@code client.close()} the
 *       SAME injected client must still be able to send a request to the stub and
 *       get a 200. If the SDK had wrongly shut it down, the send would fail
 *       ({@code IOException} / rejected). A successful probe-after-close proves
 *       the SDK left the injected client alone.</li>
 *   <li><b>Owned:</b> the SDK builds its own client (no {@code httpClient(...)}).
 *       We cannot reach that internal client directly (by design — no test-only
 *       getter is added), so this branch is asserted indirectly: startup pull
 *       succeeds over the SDK-built client, and {@code close()} (which exercises
 *       the {@code ownsHttpClient → shutdownNow()} path) neither throws nor
 *       breaks idempotency. The injected-contrast test above is what actually
 *       pins down the gating direction.</li>
 * </ul>
 */
final class HttpClientOwnershipTest {

    private HttpServer server;
    private String baseUrl;

    @BeforeEach
    void setUp() throws IOException {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        baseUrl = "http://127.0.0.1:" + server.getAddress().getPort();
    }

    @AfterEach
    void tearDown() {
        if (server != null) {
            server.stop(0);
        }
    }

    /** Registers the pull_all endpoint returning a one-namespace snapshot (startup succeeds). */
    private void stubPullAll() throws Exception {
        PullAllResponse resp = PullAllResponse.newBuilder()
                .addSnapshots(NamespaceSnapshot.newBuilder()
                        .setNamespace("checkout")
                        .setBusinessSnapshotSeq(1)
                        .addKeys(KeyState.newBuilder()
                                .setKey("color").setFullReleaseVersion(11).putVersions(11L, "blue")))
                .build();
        byte[] body = JsonFormat.printer().print(resp).getBytes(UTF_8);
        server.createContext(HttpConfigTransport.PATH_PULL_ALL, exchange -> {
            drainRequest(exchange);
            respond(exchange, 200, body);
        });
        // Probe endpoint used to prove an injected client is still alive after close.
        server.createContext("/ping", exchange -> {
            drainRequest(exchange);
            respond(exchange, 200, "pong".getBytes(UTF_8));
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

    private Config.Builder httpCfg() {
        return Config.builder()
                .namespaces("checkout")
                .configServiceAddr(baseUrl)
                .token("tok")
                .transport(Transport.HTTP)
                .pullInterval(Duration.ofMinutes(10)) // keep the periodic loop out of the way
                .pullTimeout(Duration.ofSeconds(3));
    }

    /** A direct probe with the GIVEN client; returns true only on a clean 200. */
    private boolean canProbe(HttpClient probeClient) {
        try {
            HttpResponse<String> r = probeClient.send(
                    HttpRequest.newBuilder(URI.create(baseUrl + "/ping"))
                            .timeout(Duration.ofSeconds(3))
                            .GET()
                            .build(),
                    HttpResponse.BodyHandlers.ofString());
            return r.statusCode() == 200 && "pong".equals(r.body());
        } catch (IOException | InterruptedException e) {
            if (e instanceof InterruptedException) {
                Thread.currentThread().interrupt();
            }
            return false;
        }
    }

    // ------------------------------------------------------------------
    // 1. SDK-built (owned) client: create succeeds, close does not throw and is
    //    idempotent (exercises the ownsHttpClient -> shutdownNow() branch).
    // ------------------------------------------------------------------

    @Test
    void ownedHttpClient_createSucceeds_closeDoesNotThrow_andIsIdempotent() throws Exception {
        stubPullAll();

        // No httpClient(...) call -> the SDK builds (and owns) its own client.
        TipsyAbConfigClient c = TipsyAbConfigClient.create(httpCfg().build());

        // Startup pull went through the SDK-built client.
        assertTrue(c.getConfigStatic("checkout", "color").isPresent(),
                "startup pull over the SDK-built HttpClient must populate the cache");
        assertFalse(c.health().startupCacheEmpty());

        // close() drives shutdownConns() down the ownsHttpClient == true branch
        // (httpClient.shutdownNow()). It must neither throw nor break idempotency.
        assertDoesNotThrow(c::close);
        assertDoesNotThrow(c::close);
        assertDoesNotThrow(c::close);
    }

    // ------------------------------------------------------------------
    // 2. Injected client (the load-bearing contrast): the SDK must NOT shut it
    //    down; it stays usable after the SDK is closed.
    // ------------------------------------------------------------------

    @Test
    void injectedHttpClient_isNotShutDownByClose_remainsUsable() throws Exception {
        stubPullAll();

        HttpClient injected = HttpClient.newHttpClient();
        try {
            // Sanity: the injected client works before anything else.
            assertTrue(canProbe(injected), "precondition: the injected client must work before create");

            TipsyAbConfigClient c = TipsyAbConfigClient.create(
                    httpCfg().httpClient(injected).build());
            assertTrue(c.getConfigStatic("checkout", "color").isPresent(),
                    "startup pull over the injected HttpClient must populate the cache");

            // Closing the SDK must NOT shut down the caller-owned client.
            c.close();

            // The decisive assertion: the injected client still sends successfully.
            // If close() had wrongly called shutdownNow() on it, this probe would
            // fail (rejected / IOException) and the test would catch the bug.
            assertTrue(canProbe(injected),
                    "the SDK must not shut down an injected HttpClient; it must remain usable after close()");

            // Idempotent close still must not touch the injected client.
            c.close();
            assertTrue(canProbe(injected),
                    "a second close() must still leave the injected client usable");
        } finally {
            // The TEST owns the injected client; release it here, never the SDK.
            injected.shutdownNow();
        }
    }

    // ------------------------------------------------------------------
    // 3. HTTP-mode invariant preserved: no Subscribe stream regardless of who
    //    owns the client (owned path).
    // ------------------------------------------------------------------

    @Test
    void httpMode_ownedClient_neverStartsSubscribe() throws Exception {
        stubPullAll();
        TipsyAbConfigClient c = TipsyAbConfigClient.create(httpCfg().build());
        try {
            assertFalse(c.health().subscribeConnected(),
                    "HTTP mode must never establish a subscribe stream (owned client)");
            assertEquals(0, c.metrics().subscribeEventReceivedTotal("checkout"));
            assertTrue(c.health().lastSubscribeErr().isEmpty());
        } finally {
            c.close();
        }
    }

    @Test
    void httpMode_injectedClient_neverStartsSubscribe() throws Exception {
        stubPullAll();
        HttpClient injected = HttpClient.newHttpClient();
        TipsyAbConfigClient c = TipsyAbConfigClient.create(httpCfg().httpClient(injected).build());
        try {
            assertFalse(c.health().subscribeConnected(),
                    "HTTP mode must never establish a subscribe stream (injected client)");
            assertEquals(0, c.metrics().subscribeEventReceivedTotal("checkout"));
            assertTrue(c.health().lastSubscribeErr().isEmpty());
        } finally {
            c.close();
            injected.shutdownNow();
        }
    }
}
