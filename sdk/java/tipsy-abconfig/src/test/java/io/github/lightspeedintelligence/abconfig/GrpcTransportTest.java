package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.grpc.ManagedChannel;
import io.grpc.Server;
import io.grpc.StatusRuntimeException;
import io.grpc.inprocess.InProcessChannelBuilder;
import io.grpc.inprocess.InProcessServerBuilder;
import io.grpc.stub.StreamObserver;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.AbtestServiceGrpc;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.ConfigServiceGrpc;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.KeyState;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSnapshot;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllRequest;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllResponse;
import java.io.IOException;
import java.time.Duration;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

/**
 * Unit tests for {@link GrpcConfigTransport} / {@link GrpcAbtestTransport} using
 * an in-process gRPC server + channel (no sockets, no external network), with
 * fake {@code ConfigService} / {@code AbtestService} implementations.
 *
 * <p>Design refs: 04-transport-and-cache.md §"传输接口" (gRPC thin wrappers) and the
 * 06 test plan ST2 gRPC round-trip + deadline propagation.
 *
 * <p>The server runs on a real thread pool (not {@code directExecutor}) so the
 * deadline test can have the handler block without stalling the client thread;
 * the gRPC deadline machinery cancels the call independently.
 */
final class GrpcTransportTest {

    private Server server;
    private ManagedChannel channel;
    private final ExecutorService serverExecutor = Executors.newCachedThreadPool();

    @AfterEach
    void tearDown() {
        if (channel != null) {
            channel.shutdownNow();
        }
        if (server != null) {
            server.shutdownNow();
        }
        serverExecutor.shutdownNow();
    }

    /** Starts an in-process server hosting the given services and opens a channel to it. */
    private void start(io.grpc.BindableService... services) throws IOException {
        String name = InProcessServerBuilder.generateName();
        InProcessServerBuilder sb = InProcessServerBuilder.forName(name).executor(serverExecutor);
        for (io.grpc.BindableService svc : services) {
            sb.addService(svc);
        }
        server = sb.build().start();
        channel = InProcessChannelBuilder.forName(name).build();
    }

    // ---- ConfigService.pullAll round-trip --------------------------------

    @Test
    void pullAll_roundTripsThroughInProcessChannel() throws Exception {
        PullAllResponse canned = PullAllResponse.newBuilder()
                .addSnapshots(NamespaceSnapshot.newBuilder()
                        .setNamespace("checkout")
                        .setBusinessSnapshotSeq(2)
                        .setExperimentSnapshotSeq(1)
                        .addKeys(KeyState.newBuilder()
                                .setKey("color")
                                .setFullReleaseVersion(11)
                                .putVersions(11L, "blue")))
                .build();

        java.util.concurrent.atomic.AtomicReference<PullAllRequest> seen =
                new java.util.concurrent.atomic.AtomicReference<>();
        start(new ConfigServiceGrpc.ConfigServiceImplBase() {
            @Override
            public void pullAll(PullAllRequest request,
                                StreamObserver<PullAllResponse> responseObserver) {
                seen.set(request);
                responseObserver.onNext(canned);
                responseObserver.onCompleted();
            }
        });

        GrpcConfigTransport transport = new GrpcConfigTransport(
                ConfigServiceGrpc.newBlockingStub(channel));

        PullAllResponse resp = transport.pullAll(
                PullAllRequest.newBuilder().addNamespaces("checkout").setTraceId("t-1").build(),
                Duration.ofSeconds(5));

        // Server observed the request the client sent (asserted on the test thread).
        assertEquals("checkout", seen.get().getNamespaces(0));
        assertEquals("t-1", seen.get().getTraceId());
        assertEquals(1, resp.getSnapshotsCount());
        assertEquals("checkout", resp.getSnapshots(0).getNamespace());
        assertEquals(2, resp.getSnapshots(0).getBusinessSnapshotSeq());
        assertEquals("blue", resp.getSnapshots(0).getKeys(0).getVersionsMap().get(11L));
    }

    // ---- AbtestService.getExperimentResult round-trip --------------------

    @Test
    void getExperimentResult_roundTripsThroughInProcessChannel() throws Exception {
        GetExperimentResultResponse canned = GetExperimentResultResponse.newBuilder()
                .putConfigFlatKv("layout", 5L)
                .build();

        java.util.concurrent.atomic.AtomicReference<GetExperimentResultRequest> seen =
                new java.util.concurrent.atomic.AtomicReference<>();
        start(new AbtestServiceGrpc.AbtestServiceImplBase() {
            @Override
            public void getExperimentResult(GetExperimentResultRequest request,
                                            StreamObserver<GetExperimentResultResponse> responseObserver) {
                seen.set(request);
                responseObserver.onNext(canned);
                responseObserver.onCompleted();
            }
        });

        GrpcAbtestTransport transport = new GrpcAbtestTransport(
                AbtestServiceGrpc.newBlockingStub(channel));

        GetExperimentResultResponse resp = transport.getExperimentResult(
                GetExperimentResultRequest.newBuilder()
                        .setNamespace("checkout").setUserId("u-1").build(),
                Duration.ofSeconds(5));

        assertEquals("checkout", seen.get().getNamespace());
        assertEquals("u-1", seen.get().getUserId());
        assertEquals(5L, resp.getConfigFlatKvMap().get("layout"));
    }

    // ---- deadline propagation --------------------------------------------

    @Test
    void pullAll_propagatesDeadline_serverSlowerThanTimeout() throws Exception {
        start(new ConfigServiceGrpc.ConfigServiceImplBase() {
            @Override
            public void pullAll(PullAllRequest request,
                                StreamObserver<PullAllResponse> responseObserver) {
                // Sleep well past the client's tiny deadline so the call is
                // cancelled with DEADLINE_EXCEEDED before we ever respond.
                try {
                    Thread.sleep(2_000);
                } catch (InterruptedException e) {
                    Thread.currentThread().interrupt();
                    return;
                }
                responseObserver.onNext(PullAllResponse.getDefaultInstance());
                responseObserver.onCompleted();
            }
        });

        GrpcConfigTransport transport = new GrpcConfigTransport(
                ConfigServiceGrpc.newBlockingStub(channel));

        StatusRuntimeException ex = assertThrows(StatusRuntimeException.class, () ->
                transport.pullAll(
                        PullAllRequest.newBuilder().addNamespaces("checkout").build(),
                        Duration.ofMillis(50)));

        assertEquals(io.grpc.Status.Code.DEADLINE_EXCEEDED, ex.getStatus().getCode(),
                "a per-call deadline shorter than the server delay must yield DEADLINE_EXCEEDED");
    }

    // ---- server-side error surfaces as StatusRuntimeException -------------

    @Test
    void pullAll_serverErrorSurfacesAsStatusRuntimeException() throws Exception {
        start(new ConfigServiceGrpc.ConfigServiceImplBase() {
            @Override
            public void pullAll(PullAllRequest request,
                                StreamObserver<PullAllResponse> responseObserver) {
                responseObserver.onError(io.grpc.Status.INTERNAL
                        .withDescription("backend exploded").asRuntimeException());
            }
        });

        GrpcConfigTransport transport = new GrpcConfigTransport(
                ConfigServiceGrpc.newBlockingStub(channel));

        StatusRuntimeException ex = assertThrows(StatusRuntimeException.class, () ->
                transport.pullAll(
                        PullAllRequest.newBuilder().addNamespaces("checkout").build(),
                        Duration.ofSeconds(5)));

        assertEquals(io.grpc.Status.Code.INTERNAL, ex.getStatus().getCode());
        assertTrue(ex.getStatus().getDescription() != null
                        && ex.getStatus().getDescription().contains("backend exploded"),
                "the server's status description must be propagated to the client");
    }
}
