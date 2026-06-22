package io.github.lightspeedintelligence.abconfig.example;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpHandler;
import com.sun.net.httpserver.HttpServer;
import io.github.lightspeedintelligence.abconfig.AbtestContext;
import io.github.lightspeedintelligence.abconfig.Config;
import io.github.lightspeedintelligence.abconfig.TipsyAbConfigClient;
import io.github.lightspeedintelligence.abconfig.UserInfo;
import io.github.lightspeedintelligence.abconfig.web.AbtestContextHolder;
import io.github.lightspeedintelligence.abconfig.web.HttpServerSupport;
import io.github.lightspeedintelligence.auth.IssueOptions;
import io.github.lightspeedintelligence.auth.JwtSigner;
import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.Arrays;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * Runnable example HTTP service for the Tipsy AB-config Java SDK, mirroring the
 * Go {@code sdk/go/example/main.go} but framework-agnostic: it runs on the JDK
 * built-in {@code com.sun.net.httpserver.HttpServer} (the same server the
 * surveyed consumer pine-java runs underneath), so it pulls in NO servlet /
 * Spring / Netty / gRPC-server / Jackson dependency.
 *
 * <p>Endpoints:
 * <ul>
 *   <li>{@code GET /static} — no user context; demonstrates
 *       {@link TipsyAbConfigClient#getConfigStatic(String, String)} with
 *       {@code .orElse(default)} (the Optional-ised static read).</li>
 *   <li>{@code GET /user} — user-scoped; uses
 *       {@link HttpServerSupport#wrap} to build a per-request
 *       {@link AbtestContext} from request headers, then resolves a dynamic key
 *       via {@link TipsyAbConfigClient#getConfig} and echoes
 *       {@code abctx.userId()} / {@code abctx.traceId()} / the value.</li>
 * </ul>
 *
 * <p>Run with (gRPC mode, the default):
 *
 * <pre>{@code
 * TIPSY_TOKEN=... CONFIG_ADDR=grpcs://config.example.com:443 \
 *   ABTEST_ADDR=grpcs://abtest.example.com:443 NAMESPACES=tipsy-chat \
 *   mvn -q -pl example exec:java
 * }</pre>
 *
 * Then:
 *
 * <pre>{@code
 * curl localhost:8080/static
 * curl -H 'X-User-Id: user-123' -H 'X-Country: JP' localhost:8080/user
 * }</pre>
 */
public final class Main {

    private static final Logger LOG = LoggerFactory.getLogger(Main.class);

    private static final String DEMO_KEY = "rerank.threshold";
    private static final String DEMO_DEFAULT = "0.5";

    private Main() {
        // no instances
    }

    public static void main(String[] args) throws IOException {
        String cfgAddr = envOr("CONFIG_ADDR", "localhost:50051");
        String abAddr = envOr("ABTEST_ADDR", "localhost:50051");
        String token = System.getenv("TIPSY_TOKEN");
        if (token == null || token.isEmpty()) {
            // The SDK requires a bearer token (or a token provider). Without one
            // we cannot dial the services; print guidance and exit non-zero (the
            // tipsy-auth snippet below shows how to mint one locally for Dev).
            LOG.error("TIPSY_TOKEN env var is required. Mint a Dev token with tipsy-auth — "
                    + "see demoMintToken() in this file.");
            System.exit(2);
            return;
        }
        List<String> namespaces = Arrays.asList(envOr("NAMESPACES", "tipsy-chat").split(","));
        String demoNamespace = namespaces.get(0).trim();

        // try-with-resources: TipsyAbConfigClient is AutoCloseable. create()
        // runs the startup PullAll and starts the background loops; close()
        // stops them and releases the channels / HTTP client.
        try (TipsyAbConfigClient client = TipsyAbConfigClient.create(Config.builder()
                .namespaces(namespaces)
                .configServiceAddr(cfgAddr)
                .abtestServiceAddr(abAddr)
                .token(token)
                .build())) {

            HttpServer server = HttpServer.create(new InetSocketAddress(8080), 0);

            // /static — pure cache read, no user identity. getConfigStatic
            // returns Optional<String>; the empty string is a valid value, so we
            // only fall back to the default on a genuine miss (Optional.empty()).
            server.createContext("/static", staticHandler(client, demoNamespace));

            // /user — user-scoped. HttpServerSupport.wrap extracts the trace id
            // (X-Trace-Id → X-Request-Id → fresh UUID), runs the user provider,
            // builds the AbtestContext and binds it on the AbtestContextHolder
            // ThreadLocal for the duration of the handler.
            //
            // FAN-OUT NOTE: the holder is read on the SAME thread the HttpServer
            // dispatched. If this handler fanned out to virtual threads, it would
            // have to read abctx here and pass it explicitly downstream — the
            // ThreadLocal does not cross fan-out.
            HttpHandler userInner = userHandler(client, demoNamespace);
            server.createContext("/user",
                    HttpServerSupport.wrap(client, Main::extractUser, userInner));

            server.setExecutor(null); // default executor: one dispatch thread.
            server.start();
            LOG.info("example listening on :8080 (namespaces={}, config_addr={}, abtest_addr={})",
                    namespaces, cfgAddr, abAddr);

            // Block until interrupted (Ctrl-C). A shutdown hook stops the server
            // and lets the try-with-resources close the SDK client.
            Runtime.getRuntime().addShutdownHook(new Thread(() -> {
                LOG.info("shutting down");
                server.stop(1);
            }));
            awaitForever();
        }
    }

    /** {@code GET /static} → {@code getConfigStatic(ns,key).orElse(default)}. */
    private static HttpHandler staticHandler(TipsyAbConfigClient client, String ns) {
        return exchange -> {
            Optional<String> hit = client.getConfigStatic(ns, DEMO_KEY);
            String value = hit.orElse(DEMO_DEFAULT);
            Map<String, Object> body = new LinkedHashMap<>();
            body.put("key", DEMO_KEY);
            body.put("value", value);
            body.put("from_cache", hit.isPresent());
            writeJson(exchange, 200, body);
        };
    }

    /**
     * {@code GET /user} → resolves a dynamic key for the per-request user. The
     * {@link AbtestContext} was bound by {@link HttpServerSupport#wrap}; read it
     * off the holder once at the top, then use it explicitly for the config read
     * (the holder is not consulted again — fan-out-safe pattern).
     */
    private static HttpHandler userHandler(TipsyAbConfigClient client, String ns) {
        return exchange -> {
            AbtestContext abctx = AbtestContextHolder.get();
            if (abctx == null) {
                // Defensive: should never happen behind wrap(); degrade rather
                // than NPE so the example never 500s on a wiring slip.
                abctx = client.emptyAbtestContext();
            }
            String value = client.getConfig(abctx, ns, DEMO_KEY, DEMO_DEFAULT);
            Map<String, Object> body = new LinkedHashMap<>();
            body.put("uid", abctx.userId());
            body.put("trace_id", abctx.traceId());
            body.put("key", DEMO_KEY);
            body.put("value", value);
            writeJson(exchange, 200, body);
        };
    }

    /**
     * The {@link HttpServerSupport.AbtestUserProvider}: pulls the uid + a single
     * {@code country} attr from request headers. In a real pine-java service the
     * uid comes from the JSON body's {@code common} map; here we keep the example
     * dependency-free by reading headers. Returning an empty uid is fine — the
     * SDK still resolves to the full-release path.
     */
    private static UserInfo extractUser(HttpExchange ex) {
        String uid = ex.getRequestHeaders().getFirst("X-User-Id");
        if (uid == null || uid.isBlank()) {
            uid = "anonymous";
        }
        Map<String, Object> attrs = new LinkedHashMap<>();
        String country = ex.getRequestHeaders().getFirst("X-Country");
        if (country != null && !country.isBlank()) {
            attrs.put("country", country.trim());
        }
        return UserInfo.of(uid, attrs);
    }

    /**
     * Demonstrates minting a Dev service token with {@code tipsy-auth}. Not wired
     * into a request path — call it from {@code main} or a REPL when you need a
     * local HS256 token. The secret is the shared {@code TIPSY_SERVICE_SECRET};
     * the claims shape ({@code roles}/{@code namespaces}/{@code sub}/{@code iat}/
     * {@code exp}) matches what the server's verifier accepts.
     */
    @SuppressWarnings("unused")
    private static String demoMintToken(String sharedSecret) {
        JwtSigner signer = JwtSigner.create(sharedSecret);
        return signer.issue(IssueOptions.builder()
                .subject("tipsy-example")
                .roles(List.of("business_sdk"))
                .namespaces(List.of("*"))
                .ttl(Duration.ofHours(1))
                .build());
    }

    // ------------------------------------------------------------------
    // Tiny JSON + plumbing helpers (no Jackson; the example does not need it).
    // ------------------------------------------------------------------

    private static String envOr(String key, String def) {
        String v = System.getenv(key);
        return (v == null || v.isEmpty()) ? def : v;
    }

    private static void awaitForever() {
        try {
            Thread.currentThread().join();
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    /** Writes {@code body} as a minimal JSON object (string-quoted values, booleans verbatim). */
    private static void writeJson(HttpExchange exchange, int status, Map<String, Object> body)
            throws IOException {
        byte[] payload = toJson(body).getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().set("Content-Type", "application/json; charset=utf-8");
        exchange.sendResponseHeaders(status, payload.length);
        try (OutputStream os = exchange.getResponseBody()) {
            os.write(payload);
        }
    }

    /**
     * Hand-rolled, minimal JSON serialization for a flat {@code Map}. Booleans
     * are emitted verbatim; everything else is quoted as a string (with minimal
     * escaping). Good enough for the example — production code should use a real
     * JSON library.
     */
    private static String toJson(Map<String, Object> body) {
        StringBuilder sb = new StringBuilder("{");
        boolean first = true;
        for (Map.Entry<String, Object> e : body.entrySet()) {
            if (!first) {
                sb.append(",");
            }
            first = false;
            sb.append(quote(e.getKey())).append(":");
            Object v = e.getValue();
            if (v instanceof Boolean b) {
                sb.append(b.toString());
            } else {
                sb.append(quote(String.valueOf(v)));
            }
        }
        return sb.append("}").toString();
    }

    private static String quote(String s) {
        StringBuilder sb = new StringBuilder(s.length() + 2);
        sb.append('"');
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"' -> sb.append("\\\"");
                case '\\' -> sb.append("\\\\");
                case '\n' -> sb.append("\\n");
                case '\r' -> sb.append("\\r");
                case '\t' -> sb.append("\\t");
                default -> sb.append(c);
            }
        }
        return sb.append('"').toString();
    }
}
