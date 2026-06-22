package io.github.lightspeedintelligence.abconfig;

import static java.util.concurrent.TimeUnit.NANOSECONDS;

import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.AbtestServiceGrpc;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import java.time.Duration;

/**
 * gRPC {@link AbtestTransport}: a thin wrapper over the generated
 * {@code AbtestService} blocking stub.
 *
 * <p>The stub is supplied already configured by the client (CallCredentials and
 * {@code maxOutboundMessageSize} are set when the stub is built). This wrapper
 * only attaches the per-call deadline derived from {@code timeout} before each
 * RPC, mirroring Go {@code grpcAbtestTransport}.
 */
final class GrpcAbtestTransport implements AbtestTransport {

    private final AbtestServiceGrpc.AbtestServiceBlockingStub stub;

    GrpcAbtestTransport(AbtestServiceGrpc.AbtestServiceBlockingStub stub) {
        this.stub = stub;
    }

    @Override
    public GetExperimentResultResponse getExperimentResult(GetExperimentResultRequest req, Duration timeout) {
        return stub.withDeadlineAfter(timeout.toNanos(), NANOSECONDS).getExperimentResult(req);
    }
}
