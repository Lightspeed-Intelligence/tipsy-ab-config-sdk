package io.github.lightspeedintelligence.abconfig.deve2e;

import com.google.protobuf.ListValue;
import com.google.protobuf.Struct;
import com.google.protobuf.Value;
import io.github.lightspeedintelligence.abconfig.Config;
import io.github.lightspeedintelligence.abconfig.ExperimentResultRequest;
import io.github.lightspeedintelligence.abconfig.ExperimentType;
import io.github.lightspeedintelligence.abconfig.Health;
import io.github.lightspeedintelligence.abconfig.ResultDisplayType;
import io.github.lightspeedintelligence.abconfig.TipsyAbConfigClient;
import io.github.lightspeedintelligence.abconfig.Transport;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import java.nio.file.Path;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * Java SDK client-correctness driver for the DEV environment — the Java
 * counterpart of {@code test/dev-e2e/clients/{go,py}}. One invocation tests BOTH
 * transports (internal loop). For each transport it:
 * <ul>
 *   <li>{@code create()}s the SDK once (one client per transport),</li>
 *   <li>builds an {@code AbtestContext} per (user, attrs) via
 *       {@code newAbtestContext} (the SDK encodes raw attr values into the typed
 *       Value envelope itself, so we pass RAW values — {@code "US"}, not
 *       {@code {"s":"US"}}, decoded back from the fixture via
 *       {@link Expectation#rawAttrs(Map)}),</li>
 *   <li>calls {@code getConfig} (config-version rows) /
 *       {@code getExperimentResult} (custom rows) and asserts the resolved value
 *       against {@code expectations.json}.</li>
 * </ul>
 *
 * <p>gRPC transport: dev exposes a Cloudflare-proxied gRPC domain with standard
 * TLS. The SDK address is just {@code grpcs://dev-ab-config-grpc.infra.fantacy.live:443}
 * — no :authority override, no skip-verify. The deprecated IP-direct form
 * {@code grpcs://IP:443?authority=...&insecure=true} is kept only as a fallback
 * when {@code AB_CONFIG_GRPC_AUTHORITY} overrides the default. If gRPC mode fails
 * to connect, the driver WARNs, marks gRPC degraded in the summary, and continues
 * HTTP mode rather than hard-crashing (mirrors the Go driver).
 *
 * <p>Env vars (never hard-code secrets):
 * <pre>
 *   AB_CONFIG_HTTP_BASE   (default https://dev-ab-config.infra.fantacy.live)
 *   AB_CONFIG_GRPC_ADDR   (default dev-ab-config-grpc.infra.fantacy.live:443)
 *   AB_CONFIG_TOKEN       (REQUIRED — missing → FATAL + exit 2)
 *   AB_CONFIG_GRPC_AUTHORITY  (optional legacy IP-direct override)
 * </pre>
 *
 * <p>Run: {@code AB_CONFIG_TOKEN=... java -jar target/tipsy-dev-e2e-java.jar}
 */
public final class Main {

    private static final List<String> NAMESPACES = List.of("demo-test", "for_dev_agent_test");

    private static final String DEFAULT_HTTP_BASE = "https://dev-ab-config.infra.fantacy.live";
    private static final String DEFAULT_GRPC_ADDR = "dev-ab-config-grpc.infra.fantacy.live:443";

    private Main() {
    }

    public static void main(String[] args) {
        String fixturesFlag = null;
        String transportFlag = "both";
        for (int i = 0; i < args.length; i++) {
            switch (args[i]) {
                case "--fixtures", "-fixtures" -> {
                    if (i + 1 < args.length) {
                        fixturesFlag = args[++i];
                    }
                }
                case "--transport", "-transport" -> {
                    if (i + 1 < args.length) {
                        transportFlag = args[++i];
                    }
                }
                default -> {
                    // ignore unknown args (parity with the Go flag set is minimal)
                }
            }
        }

        String token = System.getenv("AB_CONFIG_TOKEN");
        if (token == null || token.isEmpty()) {
            System.err.println(
                    "FATAL: AB_CONFIG_TOKEN env var is required (see docs/dev-http-token.md)");
            System.exit(2);
            return;
        }
        String httpBase = envOr("AB_CONFIG_HTTP_BASE", DEFAULT_HTTP_BASE);
        String grpcAddr = envOr("AB_CONFIG_GRPC_ADDR", DEFAULT_GRPC_ADDR);
        // Legacy override: when AB_CONFIG_GRPC_AUTHORITY is set (e.g. an operator
        // is debugging via the direct origin IP), fall back to the old IP-direct
        // SDK addr form. Empty (default) → use the standard TLS domain above.
        String grpcAuthority = System.getenv("AB_CONFIG_GRPC_AUTHORITY");

        Path fixturesPath = Expectation.resolveFixturesPath(fixturesFlag);
        List<Expectation> exps;
        try {
            exps = Expectation.load(fixturesPath);
        } catch (Exception e) {
            System.err.println("FATAL: load expectations: " + e);
            System.exit(2);
            return;
        }

        System.out.println("================================================================");
        System.out.println("Java SDK client-correctness driver (dev-e2e)");
        System.out.println("  http base    : " + httpBase);
        System.out.println("  grpc addr    : " + grpcAddr);
        if (grpcAuthority != null && !grpcAuthority.isEmpty()) {
            System.out.println("  grpc authority: " + grpcAuthority + " (legacy IP-direct fallback)");
        }
        System.out.println("  fixtures     : " + fixturesPath + " (" + exps.size() + " rows)");
        System.out.println("  WARNING      : hitting the SHARED dev environment");
        System.out.println("================================================================");

        Results r = new Results();

        if (transportFlag.equals("both") || transportFlag.equals("http")) {
            Config httpCfg = Config.builder()
                    .namespaces(NAMESPACES)
                    .configServiceAddr(httpBase)
                    .abtestServiceAddr(httpBase)
                    .token(token)
                    .transport(Transport.HTTP)
                    // Fail-open so a transient startup pull failure does not abort
                    // create(); we want per-case failures, not a single hard crash.
                    .startupFailOpen(true)
                    .build();
            runTransport(r, "java_sdk_http", httpCfg, exps);
        }

        if (transportFlag.equals("both") || transportFlag.equals("grpc")) {
            // Default: standard TLS to the gRPC domain — no :authority override,
            // no skip-verify (Cloudflare-proxied DNS, cert matches).
            // Legacy: when AB_CONFIG_GRPC_AUTHORITY is set, fall back to the
            // IP-direct form with authority override + skip-verify.
            String grpcTarget;
            if (grpcAuthority == null || grpcAuthority.isEmpty()) {
                grpcTarget = "grpcs://" + grpcAddr;
            } else {
                grpcTarget = "grpcs://" + grpcAddr + "?authority=" + grpcAuthority + "&insecure=true";
            }
            Config grpcCfg = Config.builder()
                    .namespaces(NAMESPACES)
                    .configServiceAddr(grpcTarget)
                    .abtestServiceAddr(grpcTarget)
                    .token(token)
                    .transport(Transport.GRPC)
                    .startupFailOpen(true)
                    .build();
            runTransport(r, "java_sdk_grpc", grpcCfg, exps);
        }

        System.out.println("----------------------------------------------------------------");
        System.out.printf("SUMMARY: %d passed, %d failed (of %d checks)%n",
                r.passed, r.failed, r.passed + r.failed);
        if (r.grpcDegraded) {
            System.out.println(
                    "NOTE: gRPC transport was DEGRADED (connect/init failed) — see WARNING above.");
        }
        if (r.failed > 0 || r.grpcDegraded) {
            System.exit(1);
        }
    }

    private static String envOr(String key, String def) {
        String v = System.getenv(key);
        return (v != null && !v.isEmpty()) ? v : def;
    }

    /**
     * Creates the SDK with {@code cfg}, runs all applicable expectations for the
     * client tag, then closes the SDK. A gRPC create/connect failure (or an empty
     * startup cache) is caught, marked degraded, and does not crash the run
     * (HTTP mode still proceeds) — mirroring the Go driver.
     */
    private static void runTransport(
            Results r, String client, Config cfg, List<Expectation> exps) {
        System.out.println();
        System.out.println("=== transport: " + client + " ===");

        boolean isGrpc = client.equals("java_sdk_grpc");

        TipsyAbConfigClient cli;
        try {
            cli = TipsyAbConfigClient.create(cfg);
        } catch (RuntimeException e) {
            if (isGrpc) {
                r.grpcDegraded = true;
                System.out.println(
                        "WARNING: gRPC SDK create failed; marking gRPC degraded and skipping: " + e);
                return;
            }
            r.fail("[%s] create failed: %s", client, e);
            return;
        }

        try {
            // A quick health check surfaces an empty cache (startup pull failed
            // under fail-open) so we don't report misleading per-case FAILs.
            Health h = cli.health();
            if (h.startupCacheEmpty()) {
                if (isGrpc) {
                    r.grpcDegraded = true;
                    System.out.println(
                            "WARNING: gRPC SDK started with EMPTY cache (startup pull failed); "
                                    + "marking degraded");
                    return;
                }
                r.fail("[%s] SDK started with empty cache (startup pull failed)", client);
                return;
            }

            warmup(cli, client, r);

            for (Expectation e : exps) {
                if (!e.appliesTo(client)) {
                    continue;
                }
                assertExpectation(cli, client, e, r);
            }
        } finally {
            cli.close();
        }
    }

    /**
     * Warms up the abtest path once before the asserted loop. The first abtest
     * RPC over a freshly-dialed cross-internet connection (esp. gRPC) can be slow;
     * priming it avoids a one-off slow first call skewing a single assertion. The
     * result is intentionally ignored. The warmup also exercises the explicit
     * trace-id path on every dev-e2e run via a recognisable synthetic id.
     */
    private static void warmup(TipsyAbConfigClient cli, String client, Results r) {
        String warmupTrace = "dev-e2e-java-warmup-" + client + "-" + System.nanoTime();
        var abctx = cli.newAbtestContext("warmup-probe", null, warmupTrace);
        if (!warmupTrace.equals(abctx.traceId())) {
            r.fail("[%s] warmup AbtestContext.traceId() = %s, want %s",
                    client, abctx.traceId(), warmupTrace);
        }
        try {
            cli.getConfig(abctx, NAMESPACES.get(0), "welcome_text", "");
        } catch (RuntimeException ignored) {
            // Warmup result is intentionally ignored; real failures surface in the
            // asserted loop below.
        }
    }

    /**
     * Drives one expectation row through the Java SDK. Config-version rows use
     * {@code getConfig} (dynamic, abtest-aware); custom rows use
     * {@code getExperimentResult} (custom_flat_kv). Mirrors the Go driver's
     * {@code assertExpectation} / {@code assertCustom}.
     */
    private static void assertExpectation(
            TipsyAbConfigClient cli, String client, Expectation e, Results r) {
        if (Expectation.CUSTOM_KEY.equals(e.key)) {
            assertCustom(cli, client, e, r);
            return;
        }

        if (!(e.expectedValue instanceof String expectedVal)) {
            r.fail("[%s] %s/%s/%s: expected_value not a string (%s)",
                    client, e.ns, e.userId, e.key,
                    e.expectedValue == null ? "null" : e.expectedValue.getClass().getSimpleName());
            return;
        }

        var abctx = cli.newAbtestContext(e.userId, Expectation.rawAttrs(e.userAttrs));
        String got;
        try {
            got = cli.getConfig(abctx, e.ns, e.key, Expectation.DEFAULT_SENTINEL);
        } catch (RuntimeException ex) {
            r.fail("[%s] getConfig %s/%s/%s: %s", client, e.ns, e.userId, e.key, ex);
            return;
        }
        if (!got.equals(expectedVal)) {
            r.fail("[%s] getConfig %s/%s/%s: got %s want %s (%s)",
                    client, e.ns, e.userId, e.key, quote(got), quote(expectedVal), e.source);
        } else {
            r.pass("[%s] getConfig %s/%s/%s = %s (%s)",
                    client, e.ns, e.userId, e.key, quote(got), e.source);
        }
    }

    /**
     * Exercises the SDK {@code getExperimentResult} wrapper for custom_params rows
     * and deep-compares {@code custom_flat_kv} to the expected KV object. Mirrors
     * the Go driver's {@code assertCustom}.
     */
    @SuppressWarnings("unchecked")
    private static void assertCustom(
            TipsyAbConfigClient cli, String client, Expectation e, Results r) {
        if (!(e.expectedValue instanceof Map<?, ?> wantRaw)) {
            r.fail("[%s] %s/%s custom: expected_value not an object", client, e.ns, e.userId);
            return;
        }
        Map<String, Object> want = (Map<String, Object>) wantRaw;

        GetExperimentResultResponse resp;
        try {
            // Explicit trace_id so the SDK call, the server compute log and any
            // downstream exposure-report row all join on a recognisable id.
            String traceId = "dev-e2e-java-" + client + "-" + e.ns + "-" + e.userId;
            resp = cli.getExperimentResult(ExperimentResultRequest.builder()
                    .namespace(e.ns)
                    .userInfo(e.userId, Expectation.rawAttrs(e.userAttrs))
                    .type(ExperimentType.CUSTOM_PARAMS)
                    .displayType(ResultDisplayType.FLAT_KV)
                    .traceId(traceId)
                    .build());
        } catch (RuntimeException ex) {
            r.fail("[%s] getExperimentResult(custom) %s/%s: %s", client, e.ns, e.userId, ex);
            return;
        }

        Map<String, Object> got = new LinkedHashMap<>();
        if (resp.hasCustomFlatKv()) {
            got = structToMap(resp.getCustomFlatKv());
        }
        if (!Expectation.kvEqual(got, want)) {
            r.fail("[%s] custom %s/%s: custom_flat_kv %s want %s",
                    client, e.ns, e.userId, got, want);
            return;
        }
        r.pass("[%s] custom %s/%s custom_flat_kv = %s (%s)",
                client, e.ns, e.userId, want, e.source);
    }

    // ------------------------------------------------------------------
    // Struct → Java map (parity with Go google.protobuf Struct.AsMap)
    // ------------------------------------------------------------------

    /**
     * Converts a {@link Struct} into a plain {@code Map<String,Object>}, mirroring
     * Go's {@code structpb.Struct.AsMap}: number → {@code Double}, string →
     * {@code String}, bool → {@code Boolean}, null → {@code null}, nested struct →
     * {@code Map}, list → {@code List}. Numeric values stay as {@code Double} so
     * {@link Expectation#kvEqual(Map, Map)}'s numeric tolerance applies.
     */
    static Map<String, Object> structToMap(Struct s) {
        Map<String, Object> out = new LinkedHashMap<>();
        for (Map.Entry<String, Value> e : s.getFieldsMap().entrySet()) {
            out.put(e.getKey(), valueToJava(e.getValue()));
        }
        return out;
    }

    private static Object valueToJava(Value v) {
        return switch (v.getKindCase()) {
            case NULL_VALUE -> null;
            case NUMBER_VALUE -> v.getNumberValue();
            case STRING_VALUE -> v.getStringValue();
            case BOOL_VALUE -> v.getBoolValue();
            case STRUCT_VALUE -> structToMap(v.getStructValue());
            case LIST_VALUE -> listToJava(v.getListValue());
            case KIND_NOT_SET -> null;
        };
    }

    private static List<Object> listToJava(ListValue lv) {
        List<Object> out = new ArrayList<>(lv.getValuesCount());
        for (Value v : lv.getValuesList()) {
            out.add(valueToJava(v));
        }
        return out;
    }

    private static String quote(String s) {
        return "\"" + s + "\"";
    }
}
