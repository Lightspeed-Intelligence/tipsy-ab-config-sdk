package io.github.lightspeedintelligence.abconfig;

import static java.nio.charset.StandardCharsets.UTF_8;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.google.protobuf.Struct;
import com.google.protobuf.Timestamp;
import com.google.protobuf.Value;
import com.google.protobuf.util.JsonFormat;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.KeyState;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSnapshot;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllRequest;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllResponse;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.net.http.HttpClient;
import java.time.Duration;
import java.util.concurrent.atomic.AtomicReference;
import java.util.function.Supplier;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/**
 * Unit tests for {@link HttpConfigTransport} / {@link HttpAbtestTransport} using
 * a local JDK {@link HttpServer} stub bound to an ephemeral port (port 0). No
 * external network and no real DEV dependency, so the suite is deterministic and
 * repeatable.
 *
 * <p>Design refs: 04-transport-and-cache.md §"HTTP 传输" and 06-auth-web-testing.md
 * §"缓存与传输（ST2）" + R3 (custom_flat_kv Struct + Timestamp protojson round-trip).
 */
final class HttpTransportTest {

    private static final Duration TIMEOUT = Duration.ofSeconds(5);
    private static final int MAX_RECV = 1 << 20; // 1 MiB default cap for most tests

    private HttpServer server;
    private String baseUrl;
    private HttpClient client;

    /** Captures the headers and body of the last request the stub received. */
    private final AtomicReference<RecordedRequest> lastRequest = new AtomicReference<>();

    private record RecordedRequest(String authorization, String contentType, String body) {}

    @BeforeEach
    void setUp() throws IOException {
        server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        baseUrl = "http://127.0.0.1:" + server.getAddress().getPort();
        client = HttpClient.newHttpClient();
    }

    @AfterEach
    void tearDown() {
        if (server != null) {
            server.stop(0);
        }
    }

    /** Registers a handler that records the request then returns {@code status}/{@code body}. */
    private void stub(String path, int status, byte[] body) {
        server.createContext(path, exchange -> {
            recordRequest(exchange);
            respond(exchange, status, body);
        });
        server.start();
    }

    private void recordRequest(HttpExchange exchange) throws IOException {
        String authz = exchange.getRequestHeaders().getFirst("Authorization");
        String ctype = exchange.getRequestHeaders().getFirst("Content-Type");
        byte[] req;
        try (InputStream in = exchange.getRequestBody()) {
            req = in.readAllBytes();
        }
        lastRequest.set(new RecordedRequest(authz, ctype, new String(req, UTF_8)));
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

    private static Supplier<String> bearer(String token) {
        return () -> "Bearer " + token;
    }

    // ---- PullAll happy path ----------------------------------------------

    @Test
    void pullAll_decodesProtojsonResponseIncludingSnapshot() throws Exception {
        PullAllResponse stubResp = PullAllResponse.newBuilder()
                .addSnapshots(NamespaceSnapshot.newBuilder()
                        .setNamespace("checkout")
                        .setBusinessSnapshotSeq(7)
                        .setExperimentSnapshotSeq(3)
                        .addKeys(KeyState.newBuilder()
                                .setKey("color")
                                .setFullReleaseVersion(11)
                                .putVersions(11L, "blue")
                                .putVersions(12L, "")) // empty-string value must round-trip
                        .build())
                .build();
        stub(HttpConfigTransport.PATH_PULL_ALL, 200,
                JsonFormat.printer().print(stubResp).getBytes(UTF_8));

        HttpConfigTransport transport = new HttpConfigTransport(
                client, bearer("tok-123"), MAX_RECV, baseUrl);

        PullAllResponse resp = transport.pullAll(
                PullAllRequest.newBuilder().addNamespaces("checkout").setTraceId("t-1").build(),
                TIMEOUT);

        assertEquals(1, resp.getSnapshotsCount());
        NamespaceSnapshot s = resp.getSnapshots(0);
        assertEquals("checkout", s.getNamespace());
        assertEquals(7, s.getBusinessSnapshotSeq());
        assertEquals(3, s.getExperimentSnapshotSeq());
        assertEquals(1, s.getKeysCount());
        KeyState k = s.getKeys(0);
        assertEquals("color", k.getKey());
        assertTrue(k.hasFullReleaseVersion());
        assertEquals(11, k.getFullReleaseVersion());
        assertEquals("blue", k.getVersionsMap().get(11L));
        assertEquals("", k.getVersionsMap().get(12L), "empty-string value must round-trip");
    }

    @Test
    void pullAll_sendsExpectedRequestHeadersAndBody() throws Exception {
        stub(HttpConfigTransport.PATH_PULL_ALL, 200,
                JsonFormat.printer().print(PullAllResponse.getDefaultInstance()).getBytes(UTF_8));

        HttpConfigTransport transport = new HttpConfigTransport(
                client, bearer("tok-abc"), MAX_RECV, baseUrl);
        transport.pullAll(
                PullAllRequest.newBuilder().addNamespaces("checkout").build(), TIMEOUT);

        RecordedRequest rec = lastRequest.get();
        assertNotNull(rec, "stub must have recorded the request");
        assertEquals("Bearer tok-abc", rec.authorization(),
                "Authorization header must carry the supplier value");
        assertEquals("application/json", rec.contentType(),
                "Content-Type must be application/json");
        // Body is protojson of the request.
        assertTrue(rec.body().contains("checkout"),
                "request body must be protojson carrying the namespace");
    }

    // ---- R3: custom_flat_kv Struct + Timestamp round-trip ----------------

    @Test
    void getExperimentResult_roundTripsStructAndTimestamp() throws Exception {
        // custom_flat_kv: mixed string / number / bool / nested object.
        Struct nested = Struct.newBuilder()
                .putFields("inner_s", Value.newBuilder().setStringValue("deep").build())
                .putFields("inner_n", Value.newBuilder().setNumberValue(3.5).build())
                .build();
        Struct custom = Struct.newBuilder()
                .putFields("name", Value.newBuilder().setStringValue("alpha").build())
                .putFields("count", Value.newBuilder().setNumberValue(42).build())
                .putFields("enabled", Value.newBuilder().setBoolValue(true).build())
                .putFields("nested", Value.newBuilder().setStructValue(nested).build())
                .build();
        Timestamp computedAt = Timestamp.newBuilder()
                .setSeconds(1_781_254_817L)
                .setNanos(123_000_000)
                .build();

        GetExperimentResultResponse stubResp = GetExperimentResultResponse.newBuilder()
                .putConfigFlatKv("layout", 5L)              // int64 map (config_flat_kv)
                .putConfigFlatKv("theme", 9L)
                .setCustomFlatKv(custom)
                .setComputedAt(computedAt)
                .build();

        stub(HttpAbtestTransport.PATH_EXPERIMENT_RESULT, 200,
                JsonFormat.printer().print(stubResp).getBytes(UTF_8));

        HttpAbtestTransport transport = new HttpAbtestTransport(
                client, bearer("tok"), MAX_RECV, baseUrl);

        GetExperimentResultResponse resp = transport.getExperimentResult(
                GetExperimentResultRequest.newBuilder()
                        .setNamespace("checkout")
                        .setUserId("u-1")
                        .build(),
                TIMEOUT);

        // config_flat_kv int64 map preserved.
        assertEquals(5L, resp.getConfigFlatKvMap().get("layout"));
        assertEquals(9L, resp.getConfigFlatKvMap().get("theme"));

        // Struct preserved field-by-field, including the nested object.
        Struct got = resp.getCustomFlatKv();
        assertEquals("alpha", got.getFieldsMap().get("name").getStringValue());
        assertEquals(42.0, got.getFieldsMap().get("count").getNumberValue(), 0.0);
        assertTrue(got.getFieldsMap().get("enabled").getBoolValue());
        Struct gotNested = got.getFieldsMap().get("nested").getStructValue();
        assertEquals("deep", gotNested.getFieldsMap().get("inner_s").getStringValue());
        assertEquals(3.5, gotNested.getFieldsMap().get("inner_n").getNumberValue(), 0.0);

        // Whole Struct equality as a stronger round-trip assertion.
        assertEquals(custom, got, "custom_flat_kv Struct must round-trip unchanged");

        // Timestamp preserved exactly (seconds + nanos).
        assertEquals(1_781_254_817L, resp.getComputedAt().getSeconds());
        assertEquals(123_000_000, resp.getComputedAt().getNanos());
        assertEquals(computedAt, resp.getComputedAt(),
                "computed_at Timestamp must round-trip unchanged");
    }

    // ---- non-2xx error handling ------------------------------------------

    @Test
    void non2xx_withErrorField_throwsWithStatusAndMessage() {
        stub(HttpConfigTransport.PATH_PULL_ALL, 500,
                "{\"error\":\"boom\"}".getBytes(UTF_8));

        HttpConfigTransport transport = new HttpConfigTransport(
                client, bearer("tok"), MAX_RECV, baseUrl);

        Exception ex = assertThrows(Exception.class, () ->
                transport.pullAll(PullAllRequest.newBuilder().addNamespaces("x").build(), TIMEOUT));

        String msg = ex.getMessage();
        assertNotNull(msg);
        assertTrue(msg.contains("500"), "message must carry the HTTP status code: " + msg);
        assertTrue(msg.contains("boom"), "message must carry the parsed error field: " + msg);
    }

    @Test
    void non2xx_withNonStandardBody_fallsBackToRawText() {
        // Body is NOT the {"error": msg} shape -> fall back to raw trimmed text.
        stub(HttpConfigTransport.PATH_PULL_ALL, 503,
                "  service unavailable  ".getBytes(UTF_8));

        HttpConfigTransport transport = new HttpConfigTransport(
                client, bearer("tok"), MAX_RECV, baseUrl);

        Exception ex = assertThrows(Exception.class, () ->
                transport.pullAll(PullAllRequest.newBuilder().addNamespaces("x").build(), TIMEOUT));

        String msg = ex.getMessage();
        assertTrue(msg.contains("503"), "message must carry the status code: " + msg);
        assertTrue(msg.contains("service unavailable"),
                "message must fall back to the raw body text: " + msg);
    }

    // ---- oversized response cap ------------------------------------------

    @Test
    void responseExceedingMaxRecvBytes_throwsTransportException() {
        int smallCap = 64; // bytes
        byte[] big = new byte[smallCap + 4096];
        java.util.Arrays.fill(big, (byte) 'a');
        stub(HttpConfigTransport.PATH_PULL_ALL, 200, big);

        HttpConfigTransport transport = new HttpConfigTransport(
                client, bearer("tok"), smallCap, baseUrl);

        Exception ex = assertThrows(Exception.class, () ->
                transport.pullAll(PullAllRequest.newBuilder().addNamespaces("x").build(), TIMEOUT));

        // The cap breach is surfaced as the package-private TransportException.
        assertTrue(ex instanceof TransportException,
                "oversized response must throw TransportException, got " + ex.getClass());
        assertTrue(ex.getMessage().contains("MaxRecvMessageSize")
                        || ex.getMessage().contains(String.valueOf(smallCap)),
                "message should mention the cap: " + ex.getMessage());
    }

    // ---- token supplier failure ------------------------------------------

    @Test
    void tokenSupplierThrows_isWrappedAsTransportException() {
        // No stub needed: the request must fail before any HTTP call is made.
        Supplier<String> failing = () -> {
            throw new RuntimeException("token source down");
        };
        HttpConfigTransport transport = new HttpConfigTransport(
                client, failing, MAX_RECV, baseUrl);

        Exception ex = assertThrows(Exception.class, () ->
                transport.pullAll(PullAllRequest.newBuilder().addNamespaces("x").build(), TIMEOUT));

        assertTrue(ex instanceof TransportException,
                "a token supplier failure must surface as TransportException, got " + ex.getClass());
        assertTrue(ex.getMessage().contains("token"),
                "message should mention token acquisition: " + ex.getMessage());
        assertNotNull(ex.getCause(), "the original supplier exception must be chained as the cause");
        assertEquals("token source down", ex.getCause().getMessage());
    }

    // ---- header assertions on the abtest endpoint too --------------------

    @Test
    void getExperimentResult_sendsAuthAndJsonHeaders() throws Exception {
        stub(HttpAbtestTransport.PATH_EXPERIMENT_RESULT, 200,
                JsonFormat.printer().print(GetExperimentResultResponse.getDefaultInstance())
                        .getBytes(UTF_8));

        HttpAbtestTransport transport = new HttpAbtestTransport(
                client, bearer("xyz"), MAX_RECV, baseUrl);
        transport.getExperimentResult(
                GetExperimentResultRequest.newBuilder().setNamespace("ns").setUserId("u").build(),
                TIMEOUT);

        RecordedRequest rec = lastRequest.get();
        assertNotNull(rec);
        assertEquals("Bearer xyz", rec.authorization());
        assertEquals("application/json", rec.contentType());
        assertFalse(rec.body().isBlank(), "request body must be non-empty protojson");
    }
}
