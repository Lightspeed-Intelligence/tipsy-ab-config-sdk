package io.github.lightspeedintelligence.abconfig;

import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllRequest;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllResponse;
import java.net.http.HttpClient;
import java.time.Duration;
import java.util.function.Supplier;

/**
 * HTTP {@link ConfigTransport}: POSTs protojson to the configured
 * {@code ConfigService} base URL.
 *
 * <p>Mirrors Go {@code httpConfigTransport}. The endpoint is
 * {@code baseUrl + "/api/v1/config/pull_all"}; {@code baseUrl} must already be a
 * validated, trailing-slash-trimmed {@code http(s)} base URL.
 */
final class HttpConfigTransport extends HttpJsonTransport implements ConfigTransport {

    static final String PATH_PULL_ALL = "/api/v1/config/pull_all";

    private final String pullAllUrl;

    HttpConfigTransport(HttpClient client,
                        Supplier<String> authHeaderValue,
                        int maxRecvBytes,
                        String baseUrl) {
        super(client, authHeaderValue, maxRecvBytes);
        this.pullAllUrl = baseUrl + PATH_PULL_ALL;
    }

    @Override
    public PullAllResponse pullAll(PullAllRequest req, Duration timeout) throws Exception {
        PullAllResponse.Builder out = PullAllResponse.newBuilder();
        doProtoJson(pullAllUrl, req, out, timeout);
        return out.build();
    }
}
