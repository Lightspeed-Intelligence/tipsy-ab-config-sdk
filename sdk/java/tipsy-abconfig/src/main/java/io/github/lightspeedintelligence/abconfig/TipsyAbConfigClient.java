package io.github.lightspeedintelligence.abconfig;

import com.google.gson.Gson;
import com.google.gson.JsonSyntaxException;
import io.grpc.ManagedChannel;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.AbtestServiceGrpc;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.ConfigServiceGrpc;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.ConfigUpdateEvent;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSnapshot;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllRequest;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllResponse;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.SubscribeRequest;
import java.net.http.HttpClient;
import java.lang.reflect.Type;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.Iterator;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.OptionalLong;
import java.util.UUID;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * The SDK handle. Construct via {@link #create(Config)}; tear down via
 * {@link #close()} (also usable with try-with-resources via
 * {@link AutoCloseable}).
 *
 * <p>Mirrors the Go SDK's {@code Client}. {@code create} resolves the transport,
 * validates parameters (parameter errors throw {@link ConfigValidationException}
 * and are never absorbed by {@code startupFailOpen}), dials the gRPC channels (or
 * builds the HTTP transports), runs the startup PullAll sweep, and starts the
 * background loops (a Subscribe stream in gRPC mode, plus a periodic fallback
 * PullAll loop in both modes).
 *
 * <p>This class is safe for concurrent use; all public methods may be called
 * from any thread.
 *
 * <p>This slice (ST3) implements lifecycle, the background loops, and Health /
 * Metrics wiring. The abtest resolution layer ({@code getConfig} /
 * {@code getExperimentResult} / {@code AbtestContext}) is layered on top of this
 * same handle by ST4 via the package-private internal accessors documented near
 * the bottom of this file.
 */
public final class TipsyAbConfigClient implements AutoCloseable {

    private static final Logger LOG = LoggerFactory.getLogger(TipsyAbConfigClient.class);

    /**
     * Shared, thread-safe {@link Gson} used by the JSON typed accessors
     * ({@link #getConfigStaticJson}/{@link #getConfigJson}). A single instance is
     * reused rather than constructed per call: {@code Gson} is immutable and
     * documented safe for concurrent use.
     */
    private static final Gson GSON = new Gson();

    /**
     * Environment variable read once at {@code create} to discover the project
     * default namespace (mirrors the Go {@code defaultNamespaceEnvVar}). The SDK
     * never hard-codes a default; an empty/unset value leaves the default
     * namespace at {@code ""}.
     */
    static final String DEFAULT_NAMESPACE_ENV_VAR = "PROJECT_DEFAULT_NAMESPACE";

    private final Config cfg;
    private final Metrics metrics;
    private final ConfigCache cache;
    private final HealthState health;

    /** Sorted, de-duplicated subscribed namespaces. */
    private final List<String> subscribedNamespaces;

    /** Resolved default namespace (Config override > env > ""). May be empty. */
    private final String defaultNamespace;

    private final Duration abtestTimeout;

    // Transport abstraction used by the RPC call sites. abtestTr is null when no
    // AbtestService address was configured (degraded mode).
    private final ConfigTransport configTr;
    private final AbtestTransport abtestTr;

    // gRPC-only: the channels (closed on shutdown) and the raw config stub used
    // for the Subscribe server-streaming call (Subscribe is not part of the
    // ConfigTransport interface). Null in HTTP mode.
    private final ManagedChannel configChannel;
    private final ManagedChannel abtestChannel;
    private final ConfigServiceGrpc.ConfigServiceBlockingStub subscribeStub;

    // HTTP-only: the SDK releases idle connections on close only if it built the
    // client itself (an injected Config.httpClient is the caller's to manage).
    private final HttpClient httpClient;
    private final boolean ownsHttpClient;

    /** True after the Subscribe stream is started (gRPC mode only). */
    private final boolean subscribeEnabled;

    // Lifecycle.
    private final AtomicBoolean closed = new AtomicBoolean(false);
    private final AtomicBoolean closeStarted = new AtomicBoolean(false);
    private final List<Thread> backgroundThreads = new ArrayList<>();

    /**
     * Subscribe-reconnect backoff is reset to its initial value once a
     * connection stayed alive for at least this many milliseconds before it
     * dropped (mirrors the Go SDK). A non-positive value never treats a
     * connection as stable. Package-private + mutable so tests can inject a
     * tiny threshold for deterministic wiring checks; defaults to 60s.
     */
    long stableResetThresholdMs = 60_000L;

    /**
     * Daemon executor handed to ST4 for the lazy abtest fan-out (lazy
     * {@code getConfig} fetch + explicit {@code AbtestContext} prefetch).
     * A virtual-thread-per-task executor keeps the fan-out cheap; it is shut down
     * on {@link #close()}.
     */
    private final ExecutorService abtestExecutor;

    private TipsyAbConfigClient(Builder b) {
        this.cfg = b.cfg;
        this.metrics = b.metrics;
        this.cache = b.cache;
        this.health = b.health;
        this.subscribedNamespaces = b.subscribedNamespaces;
        this.defaultNamespace = b.defaultNamespace;
        // Mirror Go applyDefaults: non-positive values clamp to the documented
        // defaults so ST4 and the loops always see a sane duration.
        this.abtestTimeout = isPositive(b.cfg.abtestTimeout())
                ? b.cfg.abtestTimeout() : Duration.ofMillis(1500);
        this.configTr = b.configTr;
        this.abtestTr = b.abtestTr;
        this.configChannel = b.configChannel;
        this.abtestChannel = b.abtestChannel;
        this.subscribeStub = b.subscribeStub;
        this.httpClient = b.httpClient;
        this.ownsHttpClient = b.ownsHttpClient;
        this.subscribeEnabled = b.subscribeEnabled;
        this.abtestExecutor = Executors.newVirtualThreadPerTaskExecutor();
    }

    // ------------------------------------------------------------------
    // Factory + construction
    // ------------------------------------------------------------------

    /**
     * Constructs the client: validates the {@link Config}, dials the
     * transports, runs the startup PullAll sweep, and starts the background
     * loops.
     *
     * <p>Failure contract (mirrors Go {@code Init}):
     * <ul>
     *   <li>Parameter / address errors (empty namespaces, empty
     *       {@code configServiceAddr}, neither token nor provider, an invalid
     *       transport, a malformed gRPC target, a non-{@code http(s)} base URL)
     *       always throw {@link ConfigValidationException} and are never absorbed
     *       by {@code startupFailOpen}.</li>
     *   <li>A failed startup PullAll throws {@link StartupPullFailedException}
     *       when {@code startupFailOpen} is false; when true it is absorbed
     *       (empty cache, {@code startupCacheEmpty=true}, a {@code "startup_pull"}
     *       background-error event).</li>
     * </ul>
     *
     * @param cfg the startup configuration
     * @return a ready-to-use client (startup pull complete, background loops running)
     * @throws ConfigValidationException on any parameter / address error
     * @throws StartupPullFailedException when startup PullAll fails and
     *                                    {@code startupFailOpen} is false
     */
    public static TipsyAbConfigClient create(Config cfg) {
        // 1. namespaces non-empty.
        if (cfg.namespaces().isEmpty()) {
            throw new ConfigValidationException("tipsyabconfig: Namespaces must be non-empty");
        }

        // 2. resolve transport (invalid value → ConfigValidationException).
        Transport transport = TransportResolver.resolveTransport(cfg.transport());

        // 3. configServiceAddr non-empty.
        if (cfg.configServiceAddr() == null || cfg.configServiceAddr().isEmpty()) {
            throw new ConfigValidationException("tipsyabconfig: ConfigServiceAddr must be set");
        }

        // 4. token or tokenProvider at least one.
        boolean hasToken = cfg.token() != null && !cfg.token().isEmpty();
        if (!hasToken && cfg.tokenProvider() == null) {
            throw new ConfigValidationException("tipsyabconfig: Token or TokenProvider must be set");
        }

        // 5. HTTP mode: validate + normalise base URL(s).
        String configBaseUrl = cfg.configServiceAddr();
        String abtestBaseUrl = cfg.abtestServiceAddr();
        boolean hasAbtest = cfg.abtestServiceAddr() != null && !cfg.abtestServiceAddr().isEmpty();
        if (transport == Transport.HTTP) {
            configBaseUrl = TransportResolver.validateHttpBaseURL("ConfigServiceAddr", cfg.configServiceAddr());
            if (hasAbtest) {
                abtestBaseUrl = TransportResolver.validateHttpBaseURL("AbtestServiceAddr", cfg.abtestServiceAddr());
            }
        }

        // 6. sort + de-dup namespaces; empty after de-dup → error.
        List<String> subs = sortDedupNamespaces(cfg.namespaces());
        if (subs.isEmpty()) {
            throw new ConfigValidationException(
                    "tipsyabconfig: Namespaces must contain at least one non-empty value");
        }

        // 7. gRPC mode: parse the target(s) BEFORE dialing (parse errors are
        // parameter errors, surfaced before any connection is opened).
        GrpcTarget configTarget = null;
        GrpcTarget abtestTarget = null;
        if (transport == Transport.GRPC) {
            configTarget = GrpcTarget.parseGrpcTarget(cfg.configServiceAddr());
            if (hasAbtest) {
                abtestTarget = GrpcTarget.parseGrpcTarget(cfg.abtestServiceAddr());
            }
        }

        // ---- construction (validation passed) ----
        Metrics metrics = newMetrics();
        ConfigCache cache = new ConfigCache(metrics);
        HealthState health = new HealthState();

        // Resolve the default namespace once (Config override > env > "").
        String defaultNamespace = resolveDefaultNamespace(cfg.defaultNamespace());

        TokenSource tokenSource = TokenSource.of(cfg.token(), cfg.tokenProvider());

        Builder b = new Builder();
        b.cfg = cfg;
        b.metrics = metrics;
        b.cache = cache;
        b.health = health;
        b.subscribedNamespaces = subs;
        b.defaultNamespace = defaultNamespace;

        if (transport == Transport.HTTP) {
            HttpClient client = cfg.httpClient();
            if (client == null) {
                client = HttpClient.newHttpClient();
                b.ownsHttpClient = true;
            }
            b.httpClient = client;
            b.configTr = new HttpConfigTransport(
                    client, tokenSource.httpAuthHeaderSupplier(), cfg.maxRecvMessageSize(), configBaseUrl);
            if (hasAbtest) {
                b.abtestTr = new HttpAbtestTransport(
                        client, tokenSource.httpAuthHeaderSupplier(), cfg.maxRecvMessageSize(), abtestBaseUrl);
            }
            b.subscribeEnabled = false;
        } else {
            // gRPC: dial channel(s), build credentialed + outbound-sized stubs.
            warnInsecureSkipVerify("ConfigServiceAddr", configTarget);
            ManagedChannel cfgChannel =
                    GrpcChannels.dial(configTarget, cfg.maxRecvMessageSize(), cfg.channelConfigurator());
            b.configChannel = cfgChannel;
            ConfigServiceGrpc.ConfigServiceBlockingStub configStub =
                    ConfigServiceGrpc.newBlockingStub(cfgChannel)
                            .withCallCredentials(tokenSource.toCallCredentials())
                            .withMaxOutboundMessageSize(cfg.maxSendMessageSize());
            b.subscribeStub = configStub;
            b.configTr = new GrpcConfigTransport(configStub);

            if (hasAbtest) {
                warnInsecureSkipVerify("AbtestServiceAddr", abtestTarget);
                ManagedChannel abChannel;
                try {
                    abChannel = GrpcChannels.dial(
                            abtestTarget, cfg.maxRecvMessageSize(), cfg.channelConfigurator());
                } catch (RuntimeException e) {
                    cfgChannel.shutdownNow();
                    throw e;
                }
                b.abtestChannel = abChannel;
                AbtestServiceGrpc.AbtestServiceBlockingStub abtestStub =
                        AbtestServiceGrpc.newBlockingStub(abChannel)
                                .withCallCredentials(tokenSource.toCallCredentials())
                                .withMaxOutboundMessageSize(cfg.maxSendMessageSize());
                b.abtestTr = new GrpcAbtestTransport(abtestStub);
            }
            b.subscribeEnabled = true;
        }

        TipsyAbConfigClient client = new TipsyAbConfigClient(b);

        // Startup PullAll: ns-serial, with retries. Fail-close vs fail-open per
        // config.
        try {
            client.startupPullAll();
        } catch (StartupPullFailedException startupErr) {
            if (!cfg.startupFailOpen()) {
                client.shutdownConns();
                throw startupErr;
            }
            metrics.cacheEmpty.incrementAndGet();
            LOG.error("tipsyabconfig: startup PullAll failed; running with empty cache (fail-open)",
                    startupErr);
            // Single aggregate startup_pull fire (namespace ""); startupPullAll
            // only logs per-ns to avoid double-firing.
            client.fireBackgroundError(new BackgroundErrorEvent(
                    "startup_pull", "", startupErr, Instant.now()));
        }

        client.startBackgroundLoops();
        return client;
    }

    /** Sorts + de-duplicates the configured namespaces, dropping empty entries. */
    private static List<String> sortDedupNamespaces(List<String> namespaces) {
        LinkedHashSet<String> seen = new LinkedHashSet<>();
        for (String ns : namespaces) {
            if (ns != null && !ns.isEmpty()) {
                seen.add(ns);
            }
        }
        List<String> out = new ArrayList<>(seen);
        out.sort(String::compareTo);
        return List.copyOf(out);
    }

    /** Resolves the default namespace: Config override > env var > "". */
    private static String resolveDefaultNamespace(String configured) {
        if (configured != null && !configured.isEmpty()) {
            return configured;
        }
        String env = System.getenv(DEFAULT_NAMESPACE_ENV_VAR);
        return env == null ? "" : env;
    }

    /** Package-private factory hook so a single {@code Metrics} is built per client. */
    private static Metrics newMetrics() {
        return new Metrics();
    }

    /** WARN once per service address when TLS verification is disabled (Dev-only switch). */
    private static void warnInsecureSkipVerify(String field, GrpcTarget tgt) {
        if (tgt != null && tgt.useTls() && tgt.insecureSkipVerify()) {
            LOG.warn("tipsyabconfig: TLS certificate verification DISABLED (insecure=true); "
                    + "Dev / Origin-Cert direct-IP only — never use in production "
                    + "(field={}, authority={})", field, tgt.authority());
        }
    }

    // ------------------------------------------------------------------
    // Public API (ST3 subset)
    // ------------------------------------------------------------------

    /** Returns a sorted copy of the namespaces this client subscribed to. */
    public List<String> namespaces() {
        return new ArrayList<>(subscribedNamespaces);
    }

    /**
     * Returns the project default namespace resolved once at create time
     * ({@code Config.defaultNamespace} override > {@code PROJECT_DEFAULT_NAMESPACE}
     * env). Empty when none was configured.
     */
    public String defaultNamespace() {
        return defaultNamespace;
    }

    /** Returns the per-process counter handle. Safe for concurrent use. */
    public Metrics metrics() {
        return metrics;
    }

    /** Returns an immutable snapshot of the SDK's background-link health. */
    public Health health() {
        return health.snapshot();
    }

    /**
     * Pure cache read of the full-release value for {@code (ns, key)}. Mirrors
     * Go {@code GetConfigStatic}: it does NOT resolve the namespace and never
     * throws a namespace error — an unsubscribed / unknown namespace simply
     * yields {@link Optional#empty()}. A hit returns {@link Optional#of} the
     * value (the empty string is a valid value); a miss returns
     * {@link Optional#empty()}.
     *
     * @param ns  the namespace (no resolution applied)
     * @param key the config key
     * @return the full-release value, or empty on a miss
     */
    public Optional<String> getConfigStatic(String ns, String key) {
        var frv = cache.fullReleaseVersion(ns, key);
        if (frv.isEmpty()) {
            return Optional.empty();
        }
        long version = frv.getAsLong();
        Optional<String> value = cache.valueOf(ns, key, version);
        value.ifPresent(v -> LOG.info("tipsyabconfig: get_config_static hit "
                + "(ns={}, key={}, version={}, source=full_static)",
                ns, key, version));
        return value;
    }

    // ------------------------------------------------------------------
    // Abtest layer (ST4): AbtestContext factories + getConfig / getExperimentResult.
    // ------------------------------------------------------------------

    /**
     * Creates a fresh per-request {@link AbtestContext}. Pure-create: construction
     * issues NO {@code GetExperimentResult} RPC. Every namespace is fetched
     * lazily and memoised on first dynamic {@link #getConfig} for that ns, so the
     * first {@code getConfig} for a namespace pays the RPC latency inline. To warm
     * a namespace ahead of {@code getConfig}, opt in via
     * {@link AbtestContext#prefetchConfigVersionFlatKvForNamespace(String)}
     * (non-blocking).
     *
     * <p>{@code attrs} is converted to {@code abtestv1.Value} entries on the
     * wire. Supported concrete types: {@code String}, {@code Boolean},
     * {@code Integer}/{@code Long}/{@code Short}/{@code Byte},
     * {@code Float}/{@code Double}. Unsupported values are skipped with a WARN.
     */
    public AbtestContext newAbtestContext(String userId, Map<String, Object> attrs) {
        return newAbtestContextInternal(userId, attrs, "");
    }

    /**
     * {@link #newAbtestContext(String, Map)} with an explicit per-request trace
     * id. Empty {@code traceId} &rArr; the SDK generates a fresh UUID; non-empty
     * &rArr; passed through verbatim. Every {@code GetExperimentResult} RPC
     * issued from this context carries this trace id.
     */
    public AbtestContext newAbtestContext(String userId, Map<String, Object> attrs, String traceId) {
        return newAbtestContextInternal(userId, attrs, traceId);
    }

    private AbtestContext newAbtestContextInternal(
            String userId, Map<String, Object> attrs, String traceId) {
        // trace_id: empty ⇒ generate locally so SDK-side and server-side log
        // lines for this request share the same id.
        String resolvedTraceId = (traceId == null || traceId.isEmpty())
                ? UUID.randomUUID().toString() : traceId;
        Map<String, CompletableFuture<AbtestComputeResult>> results = new HashMap<>(1);
        // Pure-create: no eager pre-request. The results map starts empty; the
        // first dynamic getConfig (or an explicit
        // AbtestContext.prefetchConfigVersionFlatKvForNamespace call) lazily
        // fetches and memoises each ns.
        return new AbtestContext(userId, attrs, this, resolvedTraceId, false, results);
    }

    /**
     * Returns an identity-less {@link AbtestContext} whose abtest results resolve
     * to the empty result for every namespace, never issuing a
     * {@code GetExperimentResult} RPC. Use it on paths with no user identity
     * (cron jobs, internal pipelines) so {@link #getConfig} still works. A fresh
     * trace id is generated for downstream-log consistency.
     */
    public AbtestContext emptyAbtestContext() {
        return new AbtestContext(
                "", null, this, UUID.randomUUID().toString(), true,
                new HashMap<>());
    }

    /**
     * Test helper: returns an {@link AbtestContext} with pre-resolved abtest
     * results. Each entry in {@code kvByNs} pre-resolves the abtest result for
     * that namespace (a completed future); namespaces not in the map resolve to
     * the empty result without an RPC ({@code empty=true}). A fresh trace id is
     * generated.
     */
    public AbtestContext mockAbtestContext(String userId, Map<String, Map<String, Long>> kvByNs) {
        Map<String, CompletableFuture<AbtestComputeResult>> results =
                new HashMap<>(kvByNs == null ? 0 : kvByNs.size());
        if (kvByNs != null) {
            for (Map.Entry<String, Map<String, Long>> e : kvByNs.entrySet()) {
                AbtestComputeResult r = new AbtestComputeResult(
                        e.getValue() == null ? Map.of() : new HashMap<>(e.getValue()));
                results.put(e.getKey(), CompletableFuture.completedFuture(r));
            }
        }
        return new AbtestContext(
                userId, null, this, UUID.randomUUID().toString(), true, results);
    }

    /**
     * Resolves the dynamic config {@code (ns, key)} for a specific user,
     * honouring abtest hits (whitelist &gt; experiment &gt; full release) per
     * design 05.
     *
     * <p>Resolution priority:
     * <ul>
     *   <li>{@link #closed()} &rArr; {@link SdkClosedException}.</li>
     *   <li>{@code abctx == null} &rArr; {@link AbtestContextMissingException}.</li>
     *   <li>{@code ns} is resolved (empty &rArr; default; still empty &rArr;
     *       {@link NamespaceRequiredException}; unsubscribed &rArr;
     *       {@link NamespaceNotSubscribedException}).</li>
     *   <li>The per-ns abtest result is memoised into {@code abctx} (at-most-once
     *       RPC per request link). When {@code key} is present in
     *       {@code config_flat_kv} with a non-zero version and the local cache
     *       holds that version, the value is returned (the empty string is a
     *       valid value). A cache miss on the ab version bumps the fallback
     *       metric + WARN and falls through to the full release.</li>
     *   <li>Full-release fallback: a key absent from the abtest map is the common
     *       "no experiment hit" case and resolves to the full-release version,
     *       NOT the default. The default is only returned when neither an abtest
     *       hit nor a full-release version exists.</li>
     * </ul>
     *
     * <p>A single-ns abtest failure degrades silently (full-release fallback +
     * fallback metric) and never throws a business exception to this caller.
     */
    public String getConfig(AbtestContext abctx, String ns, String key, String defaultValue) {
        if (closed()) {
            throw new SdkClosedException("tipsyabconfig: client closed");
        }
        if (abctx == null) {
            throw new AbtestContextMissingException("tipsyabconfig: abtest context missing");
        }
        String resolvedNs = resolveNamespace(ns);

        // Capture the snapshot ONCE so the fast-path decision and the per-key
        // value/full-release reads all observe one immutable view (no torn read
        // across a concurrent cache replace). An absent snapshot (null) preserves
        // the legacy behaviour: hdr is null, so the abtest wait still fires, and
        // resolveKeyFromSnapshot degrades to the default.
        io.github.lightspeedintelligence.abconfig.NamespaceSnapshot snap =
                cache.snapshot(resolvedNs).orElse(null);
        KeyState ks = (snap == null) ? null : snap.keys.get(key);

        // Fast-path (has_dynamic_resolution): if the server explicitly reported
        // this key as pure full-rollout (no gray-release / experiment), the abtest
        // result cannot possibly hit it, so skip resultFor (and its potential
        // GetExperimentResult RPC) entirely and resolve straight from the
        // full-release / default value. Gated on an EXPLICIT false: absent (null,
        // old server / no snapshot) or true keeps the existing always-wait path,
        // so a new SDK pointed at an old server never mis-skips and breaks
        // gray-release.
        Boolean hdr = (ks == null) ? null : ks.hasDynamicResolution;
        AbtestComputeResult abresult = AbtestComputeResult.EMPTY_RESULT;
        if (!Boolean.FALSE.equals(hdr)) {
            // Per-ns memoised abtest result (at-most-once RPC per request link). The
            // result is never exceptional (F5); a per-ns RPC failure already degraded
            // to the empty result inside resultFor.
            abresult = abctx.resultFor(resolvedNs);
        }

        KeyResolution r = resolveKeyFromSnapshot(snap, resolvedNs, key, abresult, abctx.traceId());
        switch (r.source) {
            case ABTEST:
                LOG.info("tipsyabconfig: get_config hit (abtest) "
                        + "(ns={}, key={}, version={}, uid={}, trace_id={})",
                        resolvedNs, key, r.version, abctx.userId(), abctx.traceId());
                return r.value;
            case FULL:
                LOG.info("tipsyabconfig: get_config hit (full) "
                        + "(ns={}, key={}, version={}, uid={}, trace_id={})",
                        resolvedNs, key, r.version, abctx.userId(), abctx.traceId());
                return r.value;
            default:
                return defaultValue;
        }
    }

    /**
     * The namespace-optional convenience form of {@link #getConfig}: resolves
     * the namespace from the project default namespace (i.e. {@code getConfig}
     * with an empty {@code ns}). Throws {@link NamespaceRequiredException} when
     * no default namespace is configured.
     */
    public String getConfigDefault(AbtestContext abctx, String key, String defaultValue) {
        return getConfig(abctx, "", key, defaultValue);
    }

    /**
     * Where a resolved key's value came from. {@link #NONE} means the key has
     * neither an abtest hit nor a full-release value in the captured snapshot.
     */
    private enum KeySource { ABTEST, FULL, NONE }

    /**
     * The outcome of resolving a single key against one captured snapshot: the
     * source, the resolved value (meaningless when {@code source == NONE}), and
     * the version id the value came from (0 when {@code source == NONE}).
     */
    private static final class KeyResolution {
        final KeySource source;
        final String value;
        final long version;

        KeyResolution(KeySource source, String value, long version) {
            this.source = source;
            this.value = value;
            this.version = version;
        }

        static final KeyResolution NONE = new KeyResolution(KeySource.NONE, null, 0L);
    }

    /**
     * Resolves one key's value against a single captured {@code snap} — the
     * shared per-key resolution logic behind both {@link #getConfig} (single key,
     * passing its own captured snapshot) and {@link #getAllConfigs} (the whole-ns
     * loop, passing the same snapshot for every key). Priority mirrors
     * {@code getConfig}: an abtest hit (non-zero version present in the snapshot)
     * wins; a non-zero abtest version whose value is missing from the snapshot
     * bumps the fallback metric + WARN and falls through to full release; then the
     * full-release version; then {@link KeyResolution#NONE}.
     *
     * <p>Reads ONLY {@code snap} (never the {@link ConfigCache} per-key accessors,
     * which re-snapshot internally) so a whole-ns resolution never mixes values
     * from two snapshots across a concurrent cache replace. The empty string is a
     * valid value (gated on {@code isPresent()}, not on length). A {@code null}
     * {@code snap} yields {@link KeyResolution#NONE}.
     */
    private KeyResolution resolveKeyFromSnapshot(
            io.github.lightspeedintelligence.abconfig.NamespaceSnapshot snap,
            String resolvedNs,
            String key,
            AbtestComputeResult abresult,
            String traceId) {
        // ks may be null (no snapshot, or key absent from this snapshot). The
        // abtest branch below still runs so getConfig keeps its WARN + fallback
        // metric when a non-zero ab version cannot be resolved locally (matching
        // the pre-refactor behaviour). get-all never reaches here with a null ks
        // because it iterates snap.keys, so its intentional "silent omit" for a
        // flat_kv key absent from the snapshot is preserved.
        KeyState ks = (snap == null) ? null : snap.keys.get(key);

        // abtest hit path: key present in config_flat_kv with a non-zero version.
        if (abresult != null) {
            Long abVersion = abresult.keyVersions.get(key);
            if (abVersion != null && abVersion != 0L) {
                String v = (ks == null) ? null : ks.versions.get(abVersion);
                if (v != null) {
                    return new KeyResolution(KeySource.ABTEST, v, abVersion);
                }
                // ab→full fallback: local cache missing the ab version.
                metrics.abtestFallback.inc(resolvedNs);
                LOG.warn("tipsyabconfig: ab version missing in local cache; falling back to full "
                        + "(ns={}, key={}, ab_version={}, trace_id={})",
                        resolvedNs, key, abVersion, traceId);
            }
        }

        // Full-release fallback: full_release_version present (non-zero sentinel)
        // and its value present in the snapshot.
        if (ks != null && ks.fullReleaseVersion != 0L) {
            String v = ks.versions.get(ks.fullReleaseVersion);
            if (v != null) {
                return new KeyResolution(KeySource.FULL, v, ks.fullReleaseVersion);
            }
        }
        return KeyResolution.NONE;
    }

    /**
     * Resolves EVERY dynamic config in {@code ns} for a specific user, returning a
     * fresh mutable {@code key -> value} map. The per-key resolution is identical
     * to {@link #getConfig} (abtest whitelist / experiment hit &gt; full release),
     * just without a specific key argument — the SDK assembles the whole map
     * client-side from the local cache snapshot plus the at-most-once memoised
     * abtest result. No new RPC beyond the single per-ns
     * {@code GetExperimentResult} that {@code getConfig} would already issue (and
     * zero when it is skippable).
     *
     * <p>Error / resolution semantics mirror {@link #getConfig} exactly:
     * <ul>
     *   <li>{@link #closed()} &rArr; {@link SdkClosedException}.</li>
     *   <li>{@code abctx == null} &rArr; {@link AbtestContextMissingException}.</li>
     *   <li>{@code ns} resolution is identical (empty &rArr; default; still empty
     *       &rArr; {@link NamespaceRequiredException}; unsubscribed &rArr;
     *       {@link NamespaceNotSubscribedException}).</li>
     * </ul>
     *
     * <p>Algorithm (design §2): capture the namespace snapshot ONCE. An absent
     * snapshot (subscribed but not yet pulled) returns a fresh empty map with zero
     * RPC. Whole-ns fast-path: when every key is explicitly
     * {@code has_dynamic_resolution == false}, skip the abtest RPC; otherwise
     * obtain the per-ns memoised abtest result exactly as {@code getConfig} does
     * (at-most-once). Each key is then resolved against that one captured snapshot
     * via {@link #resolveKeyFromSnapshot}; a key with neither an abtest hit nor a
     * full-release value is OMITTED from the map (there is no per-key default in
     * the get-all form). The empty string is a valid value and is preserved.
     *
     * <p>Two intentional, value-preserving differences from a key-by-key
     * {@code getConfig} sweep (RPC / metric behaviour only): (1) an absent
     * snapshot returns an empty map with zero RPC, whereas {@code getConfig} would
     * still fire the abtest RPC then return the default; (2) a key present in
     * {@code config_flat_kv} but absent from the snapshot is silently omitted (no
     * WARN, no fallback metric), since it has no resolvable value anyway.
     *
     * <p>The returned {@code HashMap} is freshly allocated and mutable; callers
     * may modify it freely without affecting the cache or later calls.
     */
    public Map<String, String> getAllConfigs(AbtestContext abctx, String ns) {
        if (closed()) {
            throw new SdkClosedException("tipsyabconfig: client closed");
        }
        if (abctx == null) {
            throw new AbtestContextMissingException("tipsyabconfig: abtest context missing");
        }
        String resolvedNs = resolveNamespace(ns);

        // Capture the snapshot ONCE: every per-key read below observes this single
        // immutable view (snapshot-consistency invariant — never call the cache
        // per-key accessors, which re-snapshot internally).
        io.github.lightspeedintelligence.abconfig.NamespaceSnapshot snap =
                cache.snapshot(resolvedNs).orElse(null);
        if (snap == null) {
            // Subscribed but no snapshot pulled yet: nothing to resolve, zero RPC.
            return new HashMap<>();
        }

        // Whole-ns fast-path: if EVERY key is explicitly pure full-rollout
        // (has_dynamic_resolution == false), abtest cannot hit any key, so skip the
        // GetExperimentResult RPC. Any key that is null (old server / absent field)
        // or true keeps the abtest wait, matching the single-key fast-path gate.
        boolean allFullRollout = true;
        for (KeyState ks : snap.keys.values()) {
            if (!Boolean.FALSE.equals(ks.hasDynamicResolution)) {
                allFullRollout = false;
                break;
            }
        }
        AbtestComputeResult abresult = allFullRollout
                ? AbtestComputeResult.EMPTY_RESULT
                : abctx.resultFor(resolvedNs);

        Map<String, String> out = new HashMap<>(snap.keys.size() * 2);
        int abHits = 0;
        int fullHits = 0;
        int omitted = 0;
        for (String key : snap.keys.keySet()) {
            KeyResolution r = resolveKeyFromSnapshot(snap, resolvedNs, key, abresult, abctx.traceId());
            switch (r.source) {
                case ABTEST:
                    out.put(key, r.value);
                    abHits++;
                    break;
                case FULL:
                    out.put(key, r.value);
                    fullHits++;
                    break;
                default:
                    omitted++;
                    break;
            }
        }
        LOG.debug("tipsyabconfig: get_all_configs "
                + "(ns={}, keys={}, ab_hits={}, full_hits={}, omitted={}, uid={}, trace_id={})",
                resolvedNs, snap.keys.size(), abHits, fullHits, omitted,
                abctx.userId(), abctx.traceId());
        return out;
    }

    /**
     * The namespace-optional convenience form of {@link #getAllConfigs}: resolves
     * the namespace from the project default namespace (i.e. {@code getAllConfigs}
     * with an empty {@code ns}). Throws {@link NamespaceRequiredException} when no
     * default namespace is configured.
     */
    public Map<String, String> getAllConfigsDefault(AbtestContext abctx) {
        return getAllConfigs(abctx, "");
    }

    // ------------------------------------------------------------------
    // Typed accessors.
    //
    // The platform stores every config value as a canonical string end-to-end
    // (DB / proto / snapshot / SDK cache / wire are all string; value_type lives
    // only on config_key as a console-side write contract and is NOT pushed to
    // the SDK). These helpers wrap the string-returning getConfigStatic /
    // getConfig and parse the value at the very edge, so callers get a typed
    // value directly (mirrors Apollo's getIntProperty / getBooleanProperty and
    // the Go SDK's GetConfig*Bool/Int64/Float64/JSON surface).
    //
    // API shape (Java idiom): both the static and dynamic forms take a typed
    // default and return the primitive/typed value. A miss OR a parse failure
    // yields the default; these accessors never throw for a bad value. (This is
    // intentionally different from the Go static (T, ok) shape — Java callers
    // get one consistent default-based surface. The underlying getConfig may
    // still throw its own exceptions — SdkClosedException,
    // AbtestContextMissingException, NamespaceRequiredException,
    // NamespaceNotSubscribedException — and those propagate unchanged.)
    //
    // Read/write asymmetry (by design): the console writes bool via a true/false
    // selector so stored bool values are always canonical "true"/"false", but
    // the SDK parses bool leniently to tolerate any source (e.g. a hand-written
    // SQL "TRUE"/"1"). Bool parsing therefore never fails.
    //
    // int64 precision: long values stay strings the whole way and are parsed
    // once here with Long.parseLong(trimmed) straight to a long — no JSON/double
    // round-trip — so values beyond 2^53 are lossless.
    // ------------------------------------------------------------------

    /**
     * Lenient bool rule shared by the static and dynamic bool accessors. After
     * trimming surrounding whitespace, {@code "true"} (case-insensitive) or
     * {@code "1"} is {@code true}; everything else — including {@code "false"},
     * {@code "0"}, the empty string and arbitrary garbage — is {@code false}. It
     * never throws.
     */
    private static boolean parseBoolLenient(String raw) {
        String s = raw.strip();
        return s.equalsIgnoreCase("true") || s.equals("1");
    }

    // ---- Static typed accessors (wrap getConfigStatic; no abtest RPC) -------
    //
    // getConfigStatic returns Optional<String>: a hit is Optional.of(value) (the
    // empty string is a valid value), a miss is Optional.empty(). These use that
    // presence to distinguish a hit from a miss — NOT a raw=="" heuristic.

    /**
     * Returns the full-release value for {@code (ns, key)} parsed as a bool. A
     * cache miss returns {@code def}; a hit returns the leniently-parsed value
     * (see {@link #parseBoolLenient}), which never fails.
     *
     * @param ns  the namespace (no resolution applied, like {@link #getConfigStatic})
     * @param key the config key
     * @param def the value returned on a cache miss
     * @return the parsed bool, or {@code def} on a miss
     */
    public boolean getConfigStaticBool(String ns, String key, boolean def) {
        Optional<String> raw = getConfigStatic(ns, key);
        if (raw.isEmpty()) {
            return def;
        }
        return parseBoolLenient(raw.get());
    }

    /**
     * Returns the full-release value for {@code (ns, key)} parsed as a long via
     * {@link Long#parseLong(String)} on the trimmed value (base 10, lossless — no
     * double round-trip, so values beyond 2^53 survive). A cache miss OR a parse
     * failure returns {@code def}.
     *
     * @param ns  the namespace (no resolution applied)
     * @param key the config key
     * @param def the value returned on a miss or parse failure
     * @return the parsed long, or {@code def}
     */
    public long getConfigStaticLong(String ns, String key, long def) {
        Optional<String> raw = getConfigStatic(ns, key);
        if (raw.isEmpty()) {
            return def;
        }
        try {
            return Long.parseLong(raw.get().strip());
        } catch (NumberFormatException e) {
            return def;
        }
    }

    /**
     * Returns the full-release value for {@code (ns, key)} parsed as a double via
     * {@link Double#parseDouble(String)} on the trimmed value. A cache miss OR a
     * parse failure returns {@code def}.
     *
     * @param ns  the namespace (no resolution applied)
     * @param key the config key
     * @param def the value returned on a miss or parse failure
     * @return the parsed double, or {@code def}
     */
    public double getConfigStaticDouble(String ns, String key, double def) {
        Optional<String> raw = getConfigStatic(ns, key);
        if (raw.isEmpty()) {
            return def;
        }
        try {
            return Double.parseDouble(raw.get().strip());
        } catch (NumberFormatException e) {
            return def;
        }
    }

    /**
     * The symmetry-named counterpart of {@link #getConfigStatic} that takes a
     * default: returns the raw full-release value for {@code (ns, key)}, or
     * {@code def} on a cache miss. The empty string is a valid hit (returned as
     * {@code ""}, not {@code def}).
     *
     * @param ns  the namespace (no resolution applied)
     * @param key the config key
     * @param def the value returned on a miss
     * @return the raw value, or {@code def} on a miss
     */
    public String getConfigStaticString(String ns, String key, String def) {
        return getConfigStatic(ns, key).orElse(def);
    }

    /**
     * Deserializes the full-release value for {@code (ns, key)} into {@code type}
     * with Gson. A cache miss OR a deserialize failure returns {@code def}; a hit
     * that parses cleanly returns the deserialized object.
     *
     * @param ns   the namespace (no resolution applied)
     * @param key  the config key
     * @param type the target class to deserialize into
     * @param def  the value returned on a miss or deserialize failure
     * @param <T>  the deserialized type
     * @return the deserialized object, or {@code def}
     */
    public <T> T getConfigStaticJson(String ns, String key, Class<T> type, T def) {
        Optional<String> raw = getConfigStatic(ns, key);
        if (raw.isEmpty()) {
            return def;
        }
        try {
            T v = GSON.fromJson(raw.get(), type);
            return v == null ? def : v;
        } catch (JsonSyntaxException e) {
            return def;
        }
    }

    /**
     * Generic/collection form of {@link #getConfigStaticJson(String, String,
     * Class, Object)}: deserializes the full-release value into an arbitrary
     * {@link Type} (typically obtained from a Gson {@code TypeToken}, e.g.
     * {@code new TypeToken<List<Foo>>(){}.getType()}). A cache miss OR a
     * deserialize failure returns {@code def}.
     *
     * @param ns   the namespace (no resolution applied)
     * @param key  the config key
     * @param type the target {@link Type} to deserialize into
     * @param def  the value returned on a miss or deserialize failure
     * @param <T>  the deserialized type
     * @return the deserialized object, or {@code def}
     */
    public <T> T getConfigStaticJson(String ns, String key, Type type, T def) {
        Optional<String> raw = getConfigStatic(ns, key);
        if (raw.isEmpty()) {
            return def;
        }
        try {
            T v = GSON.fromJson(raw.get(), type);
            return v == null ? def : v;
        } catch (JsonSyntaxException e) {
            return def;
        }
    }

    // ---- Dynamic typed accessors (wrap getConfig; honor abtest resolution) --
    //
    // These wrap getConfig(abctx, ns, key, "") — the underlying resolver returns
    // the "" default only when neither an abtest hit nor a full-release version
    // exists. A resolved-but-empty value ("") is therefore treated as a miss and
    // yields def, because non-string types never publish an empty value (this
    // mirrors the Go dynamic accessors' raw=="" ⇒ miss rule). Underlying
    // exceptions from getConfig (closed / null abctx / ns errors) propagate.

    /**
     * Resolves the dynamic config {@code (ns, key)} for the user and parses the
     * value as a bool. A miss (no hit / no full release, or a resolved-but-empty
     * value) returns {@code def}; otherwise the value is parsed leniently (see
     * {@link #parseBoolLenient}), which never fails.
     *
     * @param abctx the per-request abtest context
     * @param ns    the namespace (resolved like {@link #getConfig})
     * @param key   the config key
     * @param def   the value returned on a miss
     * @return the parsed bool, or {@code def}
     */
    public boolean getConfigBool(AbtestContext abctx, String ns, String key, boolean def) {
        String raw = getConfig(abctx, ns, key, "");
        if (raw.isEmpty()) {
            return def;
        }
        return parseBoolLenient(raw);
    }

    /**
     * Resolves the dynamic config {@code (ns, key)} for the user and parses the
     * value as a long via {@link Long#parseLong(String)} on the trimmed value
     * (base 10, lossless). A miss (including a resolved-but-empty value) OR a
     * parse failure returns {@code def}.
     *
     * @param abctx the per-request abtest context
     * @param ns    the namespace (resolved like {@link #getConfig})
     * @param key   the config key
     * @param def   the value returned on a miss or parse failure
     * @return the parsed long, or {@code def}
     */
    public long getConfigLong(AbtestContext abctx, String ns, String key, long def) {
        String raw = getConfig(abctx, ns, key, "");
        if (raw.isEmpty()) {
            return def;
        }
        try {
            return Long.parseLong(raw.strip());
        } catch (NumberFormatException e) {
            return def;
        }
    }

    /**
     * Resolves the dynamic config {@code (ns, key)} for the user and parses the
     * value as a double via {@link Double#parseDouble(String)} on the trimmed
     * value. A miss (including a resolved-but-empty value) OR a parse failure
     * returns {@code def}.
     *
     * @param abctx the per-request abtest context
     * @param ns    the namespace (resolved like {@link #getConfig})
     * @param key   the config key
     * @param def   the value returned on a miss or parse failure
     * @return the parsed double, or {@code def}
     */
    public double getConfigDouble(AbtestContext abctx, String ns, String key, double def) {
        String raw = getConfig(abctx, ns, key, "");
        if (raw.isEmpty()) {
            return def;
        }
        try {
            return Double.parseDouble(raw.strip());
        } catch (NumberFormatException e) {
            return def;
        }
    }

    /**
     * The symmetry-named counterpart of {@link #getConfig} that treats an empty
     * resolution as a miss: resolves {@code (ns, key)} for the user and returns
     * the raw value, or {@code def} when the resolved value is {@code ""} (no hit
     * / no full release, or a genuinely-empty value). Callers that must
     * distinguish a genuinely-empty string value from a miss should use
     * {@link #getConfigStaticString} instead.
     *
     * @param abctx the per-request abtest context
     * @param ns    the namespace (resolved like {@link #getConfig})
     * @param key   the config key
     * @param def   the value returned on an empty resolution
     * @return the raw value, or {@code def}
     */
    public String getConfigString(AbtestContext abctx, String ns, String key, String def) {
        String raw = getConfig(abctx, ns, key, "");
        return raw.isEmpty() ? def : raw;
    }

    /**
     * Resolves the dynamic config {@code (ns, key)} for the user and deserializes
     * the value into {@code type} with Gson. A miss (including a
     * resolved-but-empty value) OR a deserialize failure returns {@code def}.
     *
     * @param abctx the per-request abtest context
     * @param ns    the namespace (resolved like {@link #getConfig})
     * @param key   the config key
     * @param type  the target class to deserialize into
     * @param def   the value returned on a miss or deserialize failure
     * @param <T>   the deserialized type
     * @return the deserialized object, or {@code def}
     */
    public <T> T getConfigJson(AbtestContext abctx, String ns, String key, Class<T> type, T def) {
        String raw = getConfig(abctx, ns, key, "");
        if (raw.isEmpty()) {
            return def;
        }
        try {
            T v = GSON.fromJson(raw, type);
            return v == null ? def : v;
        } catch (JsonSyntaxException e) {
            return def;
        }
    }

    /**
     * Generic/collection form of {@link #getConfigJson(AbtestContext, String,
     * String, Class, Object)}: deserializes the resolved value into an arbitrary
     * {@link Type} (typically from a Gson {@code TypeToken}). A miss (including a
     * resolved-but-empty value) OR a deserialize failure returns {@code def}.
     *
     * @param abctx the per-request abtest context
     * @param ns    the namespace (resolved like {@link #getConfig})
     * @param key   the config key
     * @param type  the target {@link Type} to deserialize into
     * @param def   the value returned on a miss or deserialize failure
     * @param <T>   the deserialized type
     * @return the deserialized object, or {@code def}
     */
    public <T> T getConfigJson(AbtestContext abctx, String ns, String key, Type type, T def) {
        String raw = getConfig(abctx, ns, key, "");
        if (raw.isEmpty()) {
            return def;
        }
        try {
            T v = GSON.fromJson(raw, type);
            return v == null ? def : v;
        } catch (JsonSyntaxException e) {
            return def;
        }
    }

    /**
     * Thin wrapper over {@code AbtestService.GetExperimentResult} (design 05).
     * Unlike {@link #getConfig} it does NOT memoise into an {@link AbtestContext}
     * and does NOT touch the local config cache — it returns the raw proto
     * response so business code can read {@code config_flat_kv} /
     * {@code custom_flat_kv} / {@code groups} / {@code gray_hits} directly.
     *
     * <p>{@code gray_hits} is grouped per hit gray release: each
     * {@code GrayReleaseHit} carries a {@code release_id} plus a
     * {@code key_versions} map (config_key.key name &rarr; versionId), i.e. one
     * entry per hit {@code release_id} rather than the old flat
     * one-entry-per-(release, key) shape. Read a single key's target via
     * {@code grayHits.get(i).getKeyVersionsMap().get(keyName)}.
     *
     * <p>IMPORTANT: every int64 "version" value on this wire —
     * {@code gray_hits[].key_versions} values, {@code config_flat_kv} values,
     * {@code groups[].params_versions} values — is the config_version PRIMARY
     * KEY id (versionId, globally unique), NOT the per-key semantic version_no
     * (the n-th version of that config_key). version_no never appears on the
     * SDK/business wire; it lives only in upstream telemetry.
     *
     * <p>Namespace resolution mirrors {@link #getConfig}. The call is bounded by
     * {@code abtestTimeout}. An empty {@code traceId} is replaced with a fresh
     * UUID. When the abtest service was not configured at create time, this
     * throws.
     *
     * @throws SdkClosedException              if the client is closed
     * @throws NamespaceRequiredException      if no ns and no default ns
     * @throws NamespaceNotSubscribedException if the resolved ns is not subscribed
     * @throws TipsyConfigException            if the abtest service is not configured,
     *                                         or wrapping any transport / RPC failure
     */
    public GetExperimentResultResponse getExperimentResult(ExperimentResultRequest req) {
        if (closed()) {
            throw new SdkClosedException("tipsyabconfig: client closed");
        }
        String ns = resolveNamespace(req.namespace());
        if (abtestTr == null) {
            throw new TipsyConfigException("tipsyabconfig: abtest service not configured");
        }
        UserInfo userInfo = req.userInfo();
        String traceId = req.traceId();
        if (traceId == null || traceId.isEmpty()) {
            traceId = UUID.randomUUID().toString();
        }
        GetExperimentResultRequest pbReq = GetExperimentResultRequest.newBuilder()
                .setNamespace(ns)
                .setUserId(userInfo.uid())
                .putAllUserAttrs(AbtestContext.encodeUserAttrs(userInfo.attrs(), LOG))
                .addAllLayerIds(req.layerIds())
                .setExperimentType(req.type().toProto())
                .setDisplayType(req.displayType().toProto())
                .setTraceId(traceId)
                .build();
        long __start = System.nanoTime();
        try {
            GetExperimentResultResponse resp = abtestTr.getExperimentResult(pbReq, abtestTimeout);
            double durMs = (System.nanoTime() - __start) / 1_000_000.0;
            LOG.debug("tipsyabconfig: GetExperimentResult rpc (ns={}, trace_id={}, duration_ms={})", ns, traceId, durMs);
            return resp;
        } catch (Exception e) {
            double durMs = (System.nanoTime() - __start) / 1_000_000.0;
            LOG.debug("tipsyabconfig: GetExperimentResult rpc failed (ns={}, trace_id={}, duration_ms={})", ns, traceId, durMs, e);
            throw new TipsyConfigException(
                    "tipsyabconfig: AbtestService.GetExperimentResult failed (ns=" + ns + ")", e);
        }
    }

    /**
     * Stops the background loops, closes the gRPC channels, and releases any
     * SDK-owned HTTP client idle connections. Idempotent (a {@code closeOnce}
     * guard); safe to call multiple times.
     */
    @Override
    public void close() {
        if (!closeStarted.compareAndSet(false, true)) {
            return;
        }
        // Signal the loops to stop, then unblock the Subscribe iterator and the
        // pull loop's sleep by interrupting / shutting the channels down.
        closed.set(true);
        for (Thread t : backgroundThreads) {
            t.interrupt();
        }
        // Shutting the config channel down here (before join) cancels an
        // in-flight blocking Subscribe iterator so runSubscribe can exit. The
        // closed flag distinguishes this expected cancellation from a real error.
        shutdownConns();
        for (Thread t : backgroundThreads) {
            try {
                t.join(TimeUnit.SECONDS.toMillis(5));
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
            }
        }
        abtestExecutor.shutdownNow();
        closed.set(true);
    }

    private void shutdownConns() {
        if (configChannel != null) {
            configChannel.shutdownNow();
        }
        if (abtestChannel != null) {
            abtestChannel.shutdownNow();
        }
        // HTTP mode: java.net.http.HttpClient implements AutoCloseable on JDK 21
        // (close()/shutdown()/shutdownNow()). Release the client's executor and
        // connection pool ONLY when the SDK built it; an injected
        // Config.httpClient is the caller's to manage (mirrors Go's
        // ownsHTTPClient gating). We use shutdownNow() rather than close(): close()
        // blocks until in-flight requests finish, which is wrong on the teardown
        // path, whereas shutdownNow() cancels outstanding requests and returns.
        if (ownsHttpClient && httpClient != null) {
            httpClient.shutdownNow();
        }
    }

    // ------------------------------------------------------------------
    // Startup PullAll
    // ------------------------------------------------------------------

    /**
     * Performs the synchronous, ns-serial PullAll sweep. Throws
     * {@link StartupPullFailedException} (carrying the first per-ns failure as
     * its cause) if any namespace failed all retries.
     */
    private void startupPullAll() {
        Throwable firstErr = null;
        List<String> failed = new ArrayList<>();
        for (String ns : subscribedNamespaces) {
            try {
                pullOnceWithRetries(ns);
            } catch (InterruptedException ie) {
                Thread.currentThread().interrupt();
                throw new StartupPullFailedException(
                        "tipsyabconfig: startup PullAll interrupted for " + ns, ie);
            } catch (Exception e) {
                metrics.pullFailure.inc(ns);
                LOG.error("tipsyabconfig: startup PullAll failed for namespace (ns={})", ns, e);
                failed.add(ns);
                if (firstErr == null) {
                    firstErr = e;
                }
            }
        }
        if (!failed.isEmpty()) {
            throw new StartupPullFailedException(
                    "tipsyabconfig: startup PullAll failed for " + failed, firstErr);
        }
    }

    /**
     * Pulls one namespace with exponential backoff (200ms → ×2 → capped at 5s)
     * for up to {@code pullRetries} attempts. Returns on the first success;
     * throws the last error after exhausting the retries.
     */
    private void pullOnceWithRetries(String ns) throws Exception {
        Duration backoff = Duration.ofMillis(200);
        Exception lastErr = null;
        // Mirror Go applyDefaults: a non-positive PullRetries clamps to 3, so the
        // loop always runs at least once and lastErr is never null on failure.
        int retries = cfg.pullRetries() > 0 ? cfg.pullRetries() : 3;
        for (int attempt = 0; attempt < retries; attempt++) {
            if (attempt > 0) {
                Thread.sleep(backoff.toMillis());
                backoff = backoff.multipliedBy(2);
                if (backoff.compareTo(Duration.ofSeconds(5)) > 0) {
                    backoff = Duration.ofSeconds(5);
                }
            }
            try {
                pullOnce(ns);
                return;
            } catch (InterruptedException ie) {
                throw ie;
            } catch (Exception e) {
                lastErr = e;
            }
        }
        throw lastErr;
    }

    /** Sends a single PullAll for {@code ns} and applies the response to the cache. */
    private void pullOnce(String ns) throws Exception {
        String traceId = UUID.randomUUID().toString();
        LOG.debug("tipsyabconfig: PullAll (ns={}, trace_id={})", ns, traceId);
        PullAllRequest req = PullAllRequest.newBuilder()
                .addNamespaces(ns)
                .setTraceId(traceId)
                .build();
        PullAllResponse resp = configTr.pullAll(req, effectivePullTimeout());
        applySnapshots(resp.getSnapshotsList());
    }

    /** The per-call PullAll deadline, clamped to 5s when non-positive (Go parity). */
    private Duration effectivePullTimeout() {
        return isPositive(cfg.pullTimeout()) ? cfg.pullTimeout() : Duration.ofSeconds(5);
    }

    private static boolean isPositive(Duration d) {
        return d != null && !d.isZero() && !d.isNegative();
    }

    private void applySnapshots(List<NamespaceSnapshot> snaps) {
        for (NamespaceSnapshot s : snaps) {
            if (s == null) {
                continue;
            }
            ConfigCache.ApplyResult r = cache.applyProto(s);
            if (r.replaced) {
                LOG.debug("tipsyabconfig: cache replaced (ns={}, business_seq={}, experiment_seq={})",
                        s.getNamespace(), s.getBusinessSnapshotSeq(), s.getExperimentSnapshotSeq());
            }
        }
    }

    // ------------------------------------------------------------------
    // Background loops
    // ------------------------------------------------------------------

    private void startBackgroundLoops() {
        if (subscribeEnabled) {
            Thread sub = new Thread(this::runSubscribe, "tipsyabconfig-subscribe");
            sub.setDaemon(true);
            backgroundThreads.add(sub);
        }
        Thread pull = new Thread(this::runPullLoop, "tipsyabconfig-pull");
        pull.setDaemon(true);
        backgroundThreads.add(pull);
        for (Thread t : backgroundThreads) {
            t.start();
        }
    }

    /**
     * The {@code pullInterval} safety-net PullAll loop. Exits when the client is
     * closed.
     */
    private void runPullLoop() {
        long intervalMs = isPositive(cfg.pullInterval())
                ? cfg.pullInterval().toMillis() : Duration.ofSeconds(10).toMillis();
        while (!closed.get()) {
            try {
                Thread.sleep(intervalMs);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                return;
            }
            if (closed.get()) {
                return;
            }
            for (String ns : subscribedNamespaces) {
                if (closed.get()) {
                    return;
                }
                try {
                    pullOnce(ns);
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                    return;
                } catch (Exception e) {
                    if (closed.get()) {
                        return;
                    }
                    metrics.pullFailure.inc(ns);
                    LOG.error("tipsyabconfig: periodic PullAll failed (ns={})", ns, e);
                    fireBackgroundError(new BackgroundErrorEvent(
                            "periodic_pull", ns, e, Instant.now()));
                }
            }
        }
    }

    /**
     * Maintains a long-lived Subscribe stream with exponential backoff
     * (1s → ×2 → capped at 30s). A clean stream end resets the backoff and
     * reconnects immediately; a cancellation (from {@link #close()}) exits; a
     * real error bumps the disconnect metric, fires a {@code subscribe} event,
     * and backs off before reconnecting.
     */
    private void runSubscribe() {
        long backoffMs = 1000L;
        final long maxBackoffMs = 30_000L;
        while (!closed.get()) {
            boolean cleanEnd;
            long startNanos = System.nanoTime();
            try {
                subscribeOnce();
                cleanEnd = true; // server closed the stream cleanly (iterator drained).
            } catch (Exception e) {
                if (closed.get()) {
                    return; // cancellation from close() — do not reconnect / count.
                }
                cleanEnd = false;
                for (String ns : subscribedNamespaces) {
                    metrics.subscribeDisc.inc(ns);
                }
                // Reset backoff BEFORE logging/sleeping so the logged wait matches
                // the actual sleep: a connection that stayed alive long enough is
                // treated as healthy and reconnects from the initial backoff.
                long uptimeMs = (System.nanoTime() - startNanos) / 1_000_000L;
                backoffMs = resetBackoffIfStable(backoffMs, uptimeMs, stableResetThresholdMs);
                LOG.error("tipsyabconfig: Subscribe stream error; reconnecting (backoff={}ms)",
                        backoffMs, e);
                // recordSubscribeErr (inside fireBackgroundError) flips
                // subscribeConnected back to false.
                fireBackgroundError(new BackgroundErrorEvent(
                        "subscribe", "", e, Instant.now()));
            }
            if (closed.get()) {
                return;
            }
            if (cleanEnd) {
                backoffMs = 1000L; // reset + reconnect immediately.
                continue;
            }
            try {
                Thread.sleep(backoffMs);
            } catch (InterruptedException ie) {
                Thread.currentThread().interrupt();
                return;
            }
            backoffMs = Math.min(backoffMs * 2, maxBackoffMs);
        }
    }

    /**
     * Returns the reset backoff (1s) when the just-dropped connection stayed
     * alive for at least {@code thresholdMs}; otherwise returns {@code backoffMs}
     * unchanged. A non-positive {@code thresholdMs} never treats a connection as
     * stable (defensive, mirrors the Go SDK). Pure/static for deterministic unit
     * tests.
     */
    static long resetBackoffIfStable(long backoffMs, long uptimeMs, long thresholdMs) {
        return (thresholdMs > 0 && uptimeMs >= thresholdMs) ? 1000L : backoffMs;
    }

    /**
     * Opens one Subscribe stream and pumps events into the cache. Returns
     * normally on a clean end-of-stream (the iterator drained without error).
     */
    private void subscribeOnce() {
        String traceId = UUID.randomUUID().toString();
        SubscribeRequest req = SubscribeRequest.newBuilder()
                .addAllNamespaces(subscribedNamespaces)
                .putAllKnownSeqs(cache.knownSeqs(subscribedNamespaces))
                .setTraceId(traceId)
                .build();
        LOG.debug("tipsyabconfig: Subscribe (namespaces={}, trace_id={})", subscribedNamespaces, traceId);
        Iterator<ConfigUpdateEvent> stream = subscribeStub.subscribe(req);
        // The blocking server-streaming call connects lazily; the first hasNext()
        // drives the connection. Mark connected once we begin draining.
        health.setSubscribeConnected(true);
        while (stream.hasNext()) {
            handleEvent(stream.next());
        }
    }

    /**
     * Applies a {@link ConfigUpdateEvent} to the cache. The {@code SNAPSHOT}
     * branch increments the subscribe-event counter and applies the snapshot;
     * {@code HEARTBEAT} events are a liveness no-op; unknown oneof branches are
     * silently skipped (forward-compat).
     */
    private void handleEvent(ConfigUpdateEvent ev) {
        if (ev == null) {
            return; // guard before getPayloadCase() to avoid NPE.
        }
        switch (ev.getPayloadCase()) {
            case SNAPSHOT -> {
                NamespaceSnapshot s = ev.getSnapshot();
                metrics.subscribeEvent.inc(s.getNamespace());
                ConfigCache.ApplyResult r = cache.applyProto(s);
                if (r.replaced) {
                    LOG.debug("tipsyabconfig: subscribe applied snapshot "
                            + "(ns={}, business_seq={}, experiment_seq={})",
                            s.getNamespace(), s.getBusinessSnapshotSeq(), s.getExperimentSnapshotSeq());
                }
            }
            case HEARTBEAT -> {
                // Liveness ping — no cache/metric mutation (forward-compat no-op).
            }
            default -> {
                // Unknown / unset oneof branch — silently skipped (forward-compat).
            }
        }
    }

    // ------------------------------------------------------------------
    // Observability
    // ------------------------------------------------------------------

    /**
     * Records the failure into health state and invokes the user's
     * {@code onBackgroundError} callback synchronously, wrapping the callback in
     * a try-catch so a misbehaving callback cannot kill the background thread.
     */
    void fireBackgroundError(BackgroundErrorEvent ev) {
        switch (ev.phase()) {
            case "startup_pull" -> health.setStartupCacheEmpty();
            case "periodic_pull" -> health.recordPullErr(ev.error(), ev.time());
            case "subscribe" -> health.recordSubscribeErr(ev.error(), ev.time());
            default -> {
                // Unknown phase — no health mutation; still deliver to the callback.
            }
        }
        var cb = cfg.onBackgroundError();
        if (cb == null) {
            return;
        }
        try {
            cb.accept(ev);
        } catch (RuntimeException e) {
            // Swallow callback failures so the background thread survives
            // (mirrors Go's recover() boundary around the external callback).
            LOG.warn("tipsyabconfig: onBackgroundError callback threw; ignoring", e);
        }
    }

    // ------------------------------------------------------------------
    // Internal accessors for the abtest layer (ST4). All package-private.
    // ------------------------------------------------------------------

    /** The shared config cache (ST4 reads full-release / version values). */
    ConfigCache cache() {
        return cache;
    }

    /** The abtest transport, or {@code null} in degraded mode (no abtest address). */
    AbtestTransport abtestTransport() {
        return abtestTr;
    }

    /** The metrics handle (same instance as {@link #metrics()}); for ST4's fallback counters. */
    Metrics metricsInternal() {
        return metrics;
    }

    /** The per-compute {@code GetExperimentResult} deadline. */
    Duration abtestTimeout() {
        return abtestTimeout;
    }


    /** Whether {@code ns} is in the subscribed set (linear scan over the small sorted list). */
    boolean isSubscribed(String ns) {
        return subscribedNamespaces.contains(ns);
    }

    /**
     * Applies the namespace-resolution rules: explicit {@code ns} >
     * {@code defaultNamespace} > {@link NamespaceRequiredException}; a resolved
     * but unsubscribed namespace yields {@link NamespaceNotSubscribedException}.
     */
    String resolveNamespace(String ns) {
        String resolved = (ns == null || ns.isEmpty()) ? defaultNamespace : ns;
        if (resolved.isEmpty()) {
            throw new NamespaceRequiredException(
                    "tipsyabconfig: no namespace given and no default namespace configured");
        }
        if (!isSubscribed(resolved)) {
            throw new NamespaceNotSubscribedException(
                    "tipsyabconfig: namespace not subscribed: \"" + resolved + "\"");
        }
        return resolved;
    }

    /** Whether {@link #close()} has been initiated. */
    boolean closed() {
        return closed.get() || closeStarted.get();
    }

    /** The daemon executor for ST4's lazy abtest fan-out (lazy getConfig fetch + explicit prefetch). */
    ExecutorService abtestExecutor() {
        return abtestExecutor;
    }

    /** The shared SLF4J logger (ST4 can reuse it for parity-aligned log lines). */
    Logger logger() {
        return LOG;
    }

    // ------------------------------------------------------------------
    // Construction helper
    // ------------------------------------------------------------------

    /**
     * Internal assembly holder used by {@link #create(Config)} to gather the
     * fully-wired components before invoking the private constructor. Not part
     * of any public API.
     */
    private static final class Builder {
        private Config cfg;
        private Metrics metrics;
        private ConfigCache cache;
        private HealthState health;
        private List<String> subscribedNamespaces;
        private String defaultNamespace;
        private ConfigTransport configTr;
        private AbtestTransport abtestTr;
        private ManagedChannel configChannel;
        private ManagedChannel abtestChannel;
        private ConfigServiceGrpc.ConfigServiceBlockingStub subscribeStub;
        private HttpClient httpClient;
        private boolean ownsHttpClient;
        private boolean subscribeEnabled;
    }
}
