package io.tipsy.abconfig;

import static java.util.concurrent.TimeUnit.NANOSECONDS;

import io.tipsy.abconfig.proto.config.v1.ConfigServiceGrpc;
import io.tipsy.abconfig.proto.config.v1.PullAllRequest;
import io.tipsy.abconfig.proto.config.v1.PullAllResponse;
import java.time.Duration;

/**
 * gRPC {@link ConfigTransport}: a thin wrapper over the generated
 * {@code ConfigService} blocking stub.
 *
 * <p>The stub is supplied already configured by the client (CallCredentials and
 * {@code maxOutboundMessageSize} are set when the stub is built). This wrapper
 * only attaches the per-call deadline derived from {@code timeout} before each
 * RPC, mirroring Go {@code grpcConfigTransport}.
 */
final class GrpcConfigTransport implements ConfigTransport {

    private final ConfigServiceGrpc.ConfigServiceBlockingStub stub;

    GrpcConfigTransport(ConfigServiceGrpc.ConfigServiceBlockingStub stub) {
        this.stub = stub;
    }

    @Override
    public PullAllResponse pullAll(PullAllRequest req, Duration timeout) {
        return stub.withDeadlineAfter(timeout.toNanos(), NANOSECONDS).pullAll(req);
    }
}
