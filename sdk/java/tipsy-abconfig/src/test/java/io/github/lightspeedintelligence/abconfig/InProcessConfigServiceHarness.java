package io.github.lightspeedintelligence.abconfig;

import io.grpc.Server;
import io.grpc.inprocess.InProcessChannelBuilder;
import io.grpc.inprocess.InProcessServerBuilder;
import io.grpc.stub.StreamObserver;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.ConfigServiceGrpc;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.ConfigUpdateEvent;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.KeyState;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSnapshot;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllRequest;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllResponse;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.SubscribeRequest;
import java.io.IOException;
import java.util.Map;
import java.util.concurrent.ConcurrentLinkedQueue;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;

/**
 * Reusable in-process gRPC harness for the lifecycle tests: a configurable fake
 * {@code ConfigService} (PullAll + Subscribe) hosted on an in-process server
 * with a unique name, plus the {@code channelConfigurator} the SDK uses to dial
 * it.
 *
 * <p>No sockets, no external network. Each instance allocates a fresh in-process
 * name so tests can run in parallel without colliding. The SDK is pointed at it
 * with {@code configServiceAddr("passthrough:///x")} (a bare/passthrough target
 * resolves to plaintext, so the dial never fails before the configurator swaps
 * in the in-process channel) plus
 * {@code channelConfigurator(h.channelConfigurator())}.
 *
 * <p>Design refs: 06 test plan ST3 (lifecycle / loops via the channel-configurator
 * injection seam) and 04-transport-and-cache.md §"dial" (the injection seam runs
 * after the SDK defaults).
 */
final class InProcessConfigServiceHarness implements AutoCloseable {

    /** Pluggable behaviour for one PullAll request; default returns the canned response. */
    interface PullHandler {
        PullAllResponse handle(PullAllRequest req) throws Exception;
    }

    /** Pluggable behaviour for a Subscribe stream; default emits nothing then completes. */
    interface SubscribeHandler {
        void handle(SubscribeRequest req, StreamObserver<ConfigUpdateEvent> obs);
    }

    private final String name;
    private final Server server;

    private volatile PullHandler pullHandler = req -> PullAllResponse.getDefaultInstance();
    private volatile SubscribeHandler subscribeHandler = (req, obs) -> obs.onCompleted();

    /** Observability for assertions. */
    final AtomicInteger pullCalls = new AtomicInteger();
    final AtomicInteger subscribeCalls = new AtomicInteger();
    final ConcurrentLinkedQueue<PullAllRequest> pullRequests = new ConcurrentLinkedQueue<>();
    final ConcurrentLinkedQueue<SubscribeRequest> subscribeRequests = new ConcurrentLinkedQueue<>();
    final CopyOnWriteArrayList<StreamObserver<ConfigUpdateEvent>> openStreams = new CopyOnWriteArrayList<>();

    InProcessConfigServiceHarness() throws IOException {
        this.name = InProcessServerBuilder.generateName();
        this.server = InProcessServerBuilder.forName(name)
                // directExecutor: handlers run on the caller's RPC thread, which
                // is fine for the simple canned responses here and keeps the
                // wiring minimal (matches the ST3 task hint).
                .directExecutor()
                .addService(new FakeConfigService())
                .build()
                .start();
    }

    /** Typed as the SDK's {@code channelConfigurator} parameter. */
    java.util.function.UnaryOperator<io.grpc.ManagedChannelBuilder<?>> channelConfigurator() {
        return b -> InProcessChannelBuilder.forName(name).directExecutor();
    }

    void setPullHandler(PullHandler h) {
        this.pullHandler = h;
    }

    void setSubscribeHandler(SubscribeHandler h) {
        this.subscribeHandler = h;
    }

    @Override
    public void close() {
        server.shutdownNow();
    }

    // ---- canned-response builders ----------------------------------------

    /**
     * A single-namespace snapshot with one key carrying a full-release version
     * and that version's value. The empty string is a legal value.
     */
    static NamespaceSnapshot snapshot(String ns, long bizSeq, long expSeq,
                                      String key, long fullReleaseVersion, String value) {
        return NamespaceSnapshot.newBuilder()
                .setNamespace(ns)
                .setBusinessSnapshotSeq(bizSeq)
                .setExperimentSnapshotSeq(expSeq)
                .addKeys(KeyState.newBuilder()
                        .setKey(key)
                        .setFullReleaseVersion(fullReleaseVersion)
                        .putVersions(fullReleaseVersion, value))
                .build();
    }

    /** A snapshot with arbitrary keys (each: key -> {fullReleaseVersion, value}). */
    static NamespaceSnapshot snapshotWithKeys(String ns, long bizSeq, long expSeq,
                                              Map<String, Map.Entry<Long, String>> keys) {
        NamespaceSnapshot.Builder b = NamespaceSnapshot.newBuilder()
                .setNamespace(ns)
                .setBusinessSnapshotSeq(bizSeq)
                .setExperimentSnapshotSeq(expSeq);
        for (Map.Entry<String, Map.Entry<Long, String>> e : keys.entrySet()) {
            long ver = e.getValue().getKey();
            b.addKeys(KeyState.newBuilder()
                    .setKey(e.getKey())
                    .setFullReleaseVersion(ver)
                    .putVersions(ver, e.getValue().getValue()));
        }
        return b.build();
    }

    static PullAllResponse pullResponse(NamespaceSnapshot... snapshots) {
        PullAllResponse.Builder b = PullAllResponse.newBuilder();
        for (NamespaceSnapshot s : snapshots) {
            b.addSnapshots(s);
        }
        return b.build();
    }

    static ConfigUpdateEvent snapshotEvent(NamespaceSnapshot s) {
        return ConfigUpdateEvent.newBuilder().setSnapshot(s).build();
    }

    // ---- the fake service -------------------------------------------------

    private final class FakeConfigService extends ConfigServiceGrpc.ConfigServiceImplBase {
        @Override
        public void pullAll(PullAllRequest request, StreamObserver<PullAllResponse> obs) {
            pullCalls.incrementAndGet();
            pullRequests.add(request);
            try {
                PullAllResponse resp = pullHandler.handle(request);
                obs.onNext(resp);
                obs.onCompleted();
            } catch (Exception e) {
                obs.onError(io.grpc.Status.INTERNAL
                        .withDescription(String.valueOf(e.getMessage()))
                        .withCause(e)
                        .asRuntimeException());
            }
        }

        @Override
        public void subscribe(SubscribeRequest request, StreamObserver<ConfigUpdateEvent> obs) {
            subscribeCalls.incrementAndGet();
            subscribeRequests.add(request);
            openStreams.add(obs);
            subscribeHandler.handle(request, obs);
        }
    }

    /**
     * Counting PullHandler that fails the first {@code failuresBeforeSuccess}
     * calls (per the underlying request) then returns {@code success}. Useful
     * for the startup-retry test.
     */
    static final class FailNTimesThenSucceed implements PullHandler {
        private final AtomicInteger calls = new AtomicInteger();
        private final int failuresBeforeSuccess;
        private final PullAllResponse success;

        FailNTimesThenSucceed(int failuresBeforeSuccess, PullAllResponse success) {
            this.failuresBeforeSuccess = failuresBeforeSuccess;
            this.success = success;
        }

        int callCount() {
            return calls.get();
        }

        @Override
        public PullAllResponse handle(PullAllRequest req) throws Exception {
            int n = calls.incrementAndGet();
            if (n <= failuresBeforeSuccess) {
                throw new RuntimeException("induced pull failure #" + n);
            }
            return success;
        }
    }

    /**
     * A pull handler whose behaviour can flip at runtime: it delegates to the
     * current {@link #delegate}. Used by the periodic-pull test (start healthy,
     * then break).
     */
    static final class SwitchablePullHandler implements PullHandler {
        final AtomicReference<PullHandler> delegate;

        SwitchablePullHandler(PullHandler initial) {
            this.delegate = new AtomicReference<>(initial);
        }

        void set(PullHandler h) {
            delegate.set(h);
        }

        @Override
        public PullAllResponse handle(PullAllRequest req) throws Exception {
            return delegate.get().handle(req);
        }
    }

    /** Small polling helper: waits up to {@code timeoutMs} for {@code cond} to hold. */
    static boolean awaitTrue(java.util.function.BooleanSupplier cond, long timeoutMs) {
        long deadline = System.nanoTime() + timeoutMs * 1_000_000L;
        while (System.nanoTime() < deadline) {
            if (cond.getAsBoolean()) {
                return true;
            }
            try {
                Thread.sleep(20);
            } catch (InterruptedException e) {
                Thread.currentThread().interrupt();
                return false;
            }
        }
        return cond.getAsBoolean();
    }
}
