package io.github.lightspeedintelligence.abconfig;

import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import java.net.http.HttpClient;
import java.time.Duration;
import java.util.function.Supplier;

/**
 * HTTP {@link AbtestTransport}: POSTs protojson to the configured
 * {@code AbtestService} base URL.
 *
 * <p>Mirrors Go {@code httpAbtestTransport}. The endpoint is
 * {@code baseUrl + "/api/v1/abtest/experiment_result"}; {@code baseUrl} must
 * already be a validated, trailing-slash-trimmed {@code http(s)} base URL.
 */
final class HttpAbtestTransport extends HttpJsonTransport implements AbtestTransport {

    static final String PATH_EXPERIMENT_RESULT = "/api/v1/abtest/experiment_result";

    private final String experimentResultUrl;

    HttpAbtestTransport(HttpClient client,
                        Supplier<String> authHeaderValue,
                        int maxRecvBytes,
                        String baseUrl) {
        super(client, authHeaderValue, maxRecvBytes);
        this.experimentResultUrl = baseUrl + PATH_EXPERIMENT_RESULT;
    }

    @Override
    public GetExperimentResultResponse getExperimentResult(GetExperimentResultRequest req, Duration timeout)
            throws Exception {
        GetExperimentResultResponse.Builder out = GetExperimentResultResponse.newBuilder();
        doProtoJson(experimentResultUrl, req, out, timeout);
        return out.build();
    }
}
