package io.github.lightspeedintelligence.abconfig;

import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import java.time.Duration;

/**
 * SDK-internal abstraction over the {@code AbtestService} call the SDK actually
 * issues. Satisfied by both the gRPC wrapper ({@link GrpcAbtestTransport}) and
 * the HTTP implementation ({@link HttpAbtestTransport}).
 */
interface AbtestTransport {

    /**
     * Issues a {@code GetExperimentResult} with a per-call {@code timeout}.
     *
     * @throws Exception transport / RPC / decode failure (propagated to the
     *                   caller's degrade path).
     */
    GetExperimentResultResponse getExperimentResult(GetExperimentResultRequest req, Duration timeout) throws Exception;
}
