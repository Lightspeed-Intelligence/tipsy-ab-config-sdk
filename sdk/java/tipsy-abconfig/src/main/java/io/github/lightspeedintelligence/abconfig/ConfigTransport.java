package io.github.lightspeedintelligence.abconfig;

import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllRequest;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllResponse;
import java.time.Duration;

/**
 * SDK-internal abstraction over the {@code ConfigService} call the SDK actually
 * issues. Satisfied by both the gRPC wrapper ({@link GrpcConfigTransport}) and
 * the HTTP implementation ({@link HttpConfigTransport}).
 *
 * <p>Subscribe is intentionally NOT part of this interface: it is a gRPC-only
 * streaming path used directly via the generated stub.
 */
interface ConfigTransport {

    /**
     * Issues a {@code PullAll} with a per-call {@code timeout}.
     *
     * @throws Exception transport / RPC / decode failure (propagated to the
     *                   caller's retry / degrade path).
     */
    PullAllResponse pullAll(PullAllRequest req, Duration timeout) throws Exception;
}
