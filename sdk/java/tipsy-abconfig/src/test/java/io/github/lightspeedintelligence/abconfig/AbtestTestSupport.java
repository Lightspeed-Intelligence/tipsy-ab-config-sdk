package io.github.lightspeedintelligence.abconfig;

import io.grpc.ManagedChannelBuilder;
import io.grpc.Server;
import io.grpc.inprocess.InProcessChannelBuilder;
import io.grpc.inprocess.InProcessServerBuilder;
import io.grpc.stub.StreamObserver;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.AbtestServiceGrpc;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.ConfigServiceGrpc;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.ConfigUpdateEvent;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.KeyState;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSnapshot;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllRequest;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllResponse;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.SubscribeRequest;
import java.io.IOException;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ConcurrentLinkedQueue;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.function.Supplier;

/**
 * Shared in-process test harness for the ST4 abtest layer
 * ({@code AbtestContext} memoisation, {@code getConfig} resolution,
 * {@code getExperimentResult}, encode/enums/UserInfo).
 *
 * <p>Wires a single in-process gRPC server hosting BOTH a fake
 * {@link FakeConfigService} (so startup PullAll populates the cache for the
 * full-release fallback path) and a fake {@link FakeAbtestService} (a
 * <strong>call-counting</strong>, request-capturing, per-ns-controllable
 * {@code GetExperimentResult} stub). A {@link TipsyAbConfigClient} is then built
 * through {@link TipsyAbConfigClient#create(Config)} with a
 * {@code channelConfigurator} that swaps the SDK's would-be channel for an
 * in-process channel to that server. Because {@code create} dials the config and
 * abtest channels separately but both seam invocations return a channel to the
 * same in-process server, both services are reachable.
 *
 * <p>Closeable: {@link #close()} closes the client, the channel(s) it owns, and
 * shuts the server + executor down. Use one instance per test (or per logical
 * scenario) and close it in {@code @AfterEach}.
 */
final class AbtestTestSupport implements AutoCloseable {

    /** A canned config snapshot for one namespace: key -> (versionId -> value). */
    static final class NsCache {
        final long businessSeq;
        final long experimentSeq;
        /** key -> full_release_version (0 means none). */
        final Map<String, Long> fullRelease = new java.util.LinkedHashMap<>();
        /** key -> (versionId -> value). */
        final Map<String, Map<Long, String>> versions = new java.util.LinkedHashMap<>();

        NsCache(long businessSeq, long experimentSeq) {
            this.businessSeq = businessSeq;
            this.experimentSeq = experimentSeq;
        }

        NsCache key(String key, long fullReleaseVersion, Map<Long, String> versionMap) {
            fullRelease.put(key, fullReleaseVersion);
            versions.put(key, new java.util.LinkedHashMap<>(versionMap));
            return this;
        }

        /** Adds a version without marking it as the active full release (full_release_version=0). */
        NsCache versionOnly(String key, long versionId, String value) {
            versions.computeIfAbsent(key, k -> new java.util.LinkedHashMap<>()).put(versionId, value);
            fullRelease.putIfAbsent(key, 0L);
            return this;
        }
    }

    /**
     * Call-counting, request-capturing fake {@code AbtestService}. The unary
     * {@code getExperimentResult} handler increments a per-ns counter, records
     * the full request, then either throws (per-ns error) or returns the per-ns
     * canned {@code config_flat_kv} response.
     */
    static final class FakeAbtestService extends AbtestServiceGrpc.AbtestServiceImplBase {
        /** ns -> number of GetExperimentResult RPCs observed for that ns. */
        final ConcurrentHashMap<String, AtomicInteger> callsByNs = new ConcurrentHashMap<>();
        /** Total GetExperimentResult RPCs observed (any ns). */
        final AtomicInteger totalCalls = new AtomicInteger();
        /** Every request the server received, in arrival order. */
        final ConcurrentLinkedQueue<GetExperimentResultRequest> requests = new ConcurrentLinkedQueue<>();
        /** ns -> config_flat_kv map to return on success. */
        final ConcurrentHashMap<String, Map<String, Long>> configFlatKvByNs = new ConcurrentHashMap<>();
        /**
         * ns -> a full canned response to return verbatim (overrides
         * {@link #configFlatKvByNs}). Used by the getExperimentResult passthrough
         * tests to return richer shapes (groups / custom_flat_kv).
         */
        final ConcurrentHashMap<String, GetExperimentResultResponse> fullResponseByNs = new ConcurrentHashMap<>();
        /** ns -> if true, the handler fails the call with INTERNAL. */
        final ConcurrentHashMap<String, Boolean> failNs = new ConcurrentHashMap<>();
        /**
         * Optional barrier: when set, the handler blocks on this latch before
         * responding, letting a test force concurrent first-accessors to pile up
         * on the same in-flight RPC.
         */
        volatile CountDownLatch releaseGate;

        void setConfigFlatKv(String ns, Map<String, Long> kv) {
            configFlatKvByNs.put(ns, kv);
        }

        void setFullResponse(String ns, GetExperimentResultResponse resp) {
            fullResponseByNs.put(ns, resp);
        }

        void failFor(String ns) {
            failNs.put(ns, Boolean.TRUE);
        }

        int callsFor(String ns) {
            AtomicInteger c = callsByNs.get(ns);
            return c == null ? 0 : c.get();
        }

        @Override
        public void getExperimentResult(
                GetExperimentResultRequest request,
                StreamObserver<GetExperimentResultResponse> responseObserver) {
            String ns = request.getNamespace();
            callsByNs.computeIfAbsent(ns, k -> new AtomicInteger()).incrementAndGet();
            totalCalls.incrementAndGet();
            requests.add(request);
            CountDownLatch gate = releaseGate;
            if (gate != null) {
                try {
                    gate.await(10, TimeUnit.SECONDS);
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                }
            }
            if (Boolean.TRUE.equals(failNs.get(ns))) {
                responseObserver.onError(io.grpc.Status.INTERNAL
                        .withDescription("fake abtest failure for ns=" + ns).asRuntimeException());
                return;
            }
            GetExperimentResultResponse full = fullResponseByNs.get(ns);
            if (full != null) {
                responseObserver.onNext(full);
                responseObserver.onCompleted();
                return;
            }
            GetExperimentResultResponse.Builder resp = GetExperimentResultResponse.newBuilder();
            Map<String, Long> kv = configFlatKvByNs.get(ns);
            if (kv != null) {
                resp.putAllConfigFlatKv(kv);
            }
            responseObserver.onNext(resp.build());
            responseObserver.onCompleted();
        }
    }

    /**
     * Fake {@code ConfigService}: {@code pullAll} returns the canned snapshot(s)
     * for the requested namespaces so the SDK's startup PullAll fills the cache;
     * {@code subscribe} parks (a clean idle stream) until the call is cancelled
     * by {@link TipsyAbConfigClient#close()}, so the background Subscribe loop
     * never errors during the test.
     */
    static final class FakeConfigService extends ConfigServiceGrpc.ConfigServiceImplBase {
        private final Map<String, NsCache> snapshotsByNs;
        final AtomicInteger pullAllCalls = new AtomicInteger();

        FakeConfigService(Map<String, NsCache> snapshotsByNs) {
            this.snapshotsByNs = snapshotsByNs;
        }

        @Override
        public void pullAll(PullAllRequest request, StreamObserver<PullAllResponse> responseObserver) {
            pullAllCalls.incrementAndGet();
            PullAllResponse.Builder resp = PullAllResponse.newBuilder();
            for (String ns : request.getNamespacesList()) {
                NsCache nc = snapshotsByNs.get(ns);
                if (nc != null) {
                    resp.addSnapshots(toProto(ns, nc));
                }
            }
            responseObserver.onNext(resp.build());
            responseObserver.onCompleted();
        }

        @Override
        public void subscribe(SubscribeRequest request, StreamObserver<ConfigUpdateEvent> responseObserver) {
            // Park: a clean, idle server-streaming call. Never push, never error.
            // The client's close() cancels it; we simply never complete here.
        }
    }

    static NamespaceSnapshot toProto(String ns, NsCache nc) {
        NamespaceSnapshot.Builder b = NamespaceSnapshot.newBuilder()
                .setNamespace(ns)
                .setBusinessSnapshotSeq(nc.businessSeq)
                .setExperimentSnapshotSeq(nc.experimentSeq);
        for (Map.Entry<String, Map<Long, String>> e : nc.versions.entrySet()) {
            String key = e.getKey();
            KeyState.Builder ks = KeyState.newBuilder().setKey(key);
            long frv = nc.fullRelease.getOrDefault(key, 0L);
            if (frv != 0L) {
                ks.setFullReleaseVersion(frv);
            }
            for (Map.Entry<Long, String> v : e.getValue().entrySet()) {
                ks.putVersions(v.getKey(), v.getValue());
            }
            b.addKeys(ks);
        }
        return b.build();
    }

    // ------------------------------------------------------------------
    // Harness state
    // ------------------------------------------------------------------

    final FakeAbtestService abtest;
    final FakeConfigService config;
    final TipsyAbConfigClient client;

    private final Server server;
    private final ExecutorService serverExecutor;

    private AbtestTestSupport(
            FakeAbtestService abtest,
            FakeConfigService config,
            TipsyAbConfigClient client,
            Server server,
            ExecutorService serverExecutor) {
        this.abtest = abtest;
        this.config = config;
        this.client = client;
        this.server = server;
        this.serverExecutor = serverExecutor;
    }

    @Override
    public void close() {
        if (client != null) {
            client.close();
        }
        if (server != null) {
            server.shutdownNow();
        }
        if (serverExecutor != null) {
            serverExecutor.shutdownNow();
        }
    }

    // ------------------------------------------------------------------
    // Builder
    // ------------------------------------------------------------------

    static Builder newBuilder() {
        return new Builder();
    }

    static final class Builder {
        private final List<String> namespaces = new ArrayList<>();
        private String defaultNamespace = "";
        private boolean withAbtest = true;
        private final Map<String, NsCache> snapshots = new java.util.LinkedHashMap<>();
        private final Map<String, Map<String, Long>> abtestKv = new java.util.LinkedHashMap<>();
        private Duration abtestTimeout = Duration.ofSeconds(2);

        Builder namespaces(String... ns) {
            for (String n : ns) {
                namespaces.add(n);
            }
            return this;
        }

        Builder defaultNamespace(String ns) {
            this.defaultNamespace = ns;
            return this;
        }

        /** Disables the abtest address (degraded mode: no AbtestService configured). */
        Builder withoutAbtest() {
            this.withAbtest = false;
            return this;
        }

        Builder abtestTimeout(Duration d) {
            this.abtestTimeout = d;
            return this;
        }

        /** Registers a canned config snapshot for a namespace (drives the full-release cache). */
        Builder snapshot(String ns, NsCache nc) {
            snapshots.put(ns, nc);
            return this;
        }

        /** Pre-seeds the fake AbtestService's config_flat_kv for a namespace. */
        Builder abtestConfigFlatKv(String ns, Map<String, Long> kv) {
            abtestKv.put(ns, kv);
            return this;
        }

        AbtestTestSupport build() {
            FakeAbtestService abtest = new FakeAbtestService();
            for (Map.Entry<String, Map<String, Long>> e : abtestKv.entrySet()) {
                abtest.setConfigFlatKv(e.getKey(), e.getValue());
            }
            FakeConfigService config = new FakeConfigService(snapshots);

            ExecutorService serverExecutor = Executors.newCachedThreadPool();
            String name = InProcessServerBuilder.generateName();
            Server server;
            try {
                server = InProcessServerBuilder.forName(name)
                        .executor(serverExecutor)
                        .addService(config)
                        .addService(abtest)
                        .build()
                        .start();
            } catch (IOException e) {
                serverExecutor.shutdownNow();
                throw new RuntimeException("failed to start in-process server", e);
            }

            // Seam: every dial (config + abtest) is swapped to an in-process
            // channel to the single shared server. Each invocation must return a
            // FRESH builder (the SDK opens two distinct channels).
            Supplier<ManagedChannelBuilder<?>> seam = () -> InProcessChannelBuilder.forName(name);

            Config.Builder cfg = Config.builder()
                    .namespaces(namespaces)
                    .configServiceAddr("passthrough:///fake-config")
                    .token("test-token")
                    .defaultNamespace(defaultNamespace)
                    .transport(Transport.GRPC)
                    .abtestTimeout(abtestTimeout)
                    .pullTimeout(Duration.ofSeconds(2))
                    .channelConfigurator(builder -> seam.get());
            if (withAbtest) {
                cfg.abtestServiceAddr("passthrough:///fake-abtest");
            }

            TipsyAbConfigClient client;
            try {
                client = TipsyAbConfigClient.create(cfg.build());
            } catch (RuntimeException e) {
                server.shutdownNow();
                serverExecutor.shutdownNow();
                throw e;
            }
            return new AbtestTestSupport(abtest, config, client, server, serverExecutor);
        }
    }

    // ------------------------------------------------------------------
    // Small polling helper (bounded; no fixed sleeps for the assertion).
    // ------------------------------------------------------------------

    /**
     * Polls {@code condition} until it returns true or the timeout elapses,
     * sleeping briefly between attempts. Returns whether the condition held. Used
     * to await eager-prefetch / async future completion without a fixed sleep.
     */
    static boolean awaitTrue(Supplier<Boolean> condition, Duration timeout) {
        long deadline = System.nanoTime() + timeout.toNanos();
        while (System.nanoTime() < deadline) {
            if (Boolean.TRUE.equals(condition.get())) {
                return true;
            }
            try {
                Thread.sleep(5);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                return false;
            }
        }
        return Boolean.TRUE.equals(condition.get());
    }
}
