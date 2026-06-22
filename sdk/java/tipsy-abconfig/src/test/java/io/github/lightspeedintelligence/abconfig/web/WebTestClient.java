package io.github.lightspeedintelligence.abconfig.web;

import io.grpc.ManagedChannelBuilder;
import io.grpc.Server;
import io.grpc.inprocess.InProcessChannelBuilder;
import io.grpc.inprocess.InProcessServerBuilder;
import io.grpc.stub.StreamObserver;
import io.github.lightspeedintelligence.abconfig.Config;
import io.github.lightspeedintelligence.abconfig.TipsyAbConfigClient;
import io.github.lightspeedintelligence.abconfig.Transport;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.ConfigServiceGrpc;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.ConfigUpdateEvent;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllRequest;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.PullAllResponse;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.SubscribeRequest;
import java.io.IOException;
import java.time.Duration;

/**
 * A minimal in-process {@link TipsyAbConfigClient} factory for the
 * {@code io.github.lightspeedintelligence.abconfig.web} tests.
 *
 * <p>The ST6 web helpers operate on real {@link io.github.lightspeedintelligence.abconfig.AbtestContext}
 * instances, but that class's constructor is package-private to
 * {@code io.github.lightspeedintelligence.abconfig}, so a test in THIS package cannot {@code new} one. The
 * supported way to obtain a context from outside the SDK package is the public
 * client factories ({@code emptyAbtestContext} / {@code newAbtestContext} /
 * {@code mockAbtestContext}). Those only need a live client.
 *
 * <p>Rather than couple these web tests to the package-private
 * {@code AbtestTestSupport}/{@code InProcessConfigServiceHarness} (different
 * package, heavier), this helper stands up its own tiny in-process gRPC server
 * hosting a parking {@code ConfigService}:
 * <ul>
 *   <li>{@code pullAll} returns an empty response — startup PullAll "succeeds"
 *       with an empty cache (no namespaces are subscribed anyway);</li>
 *   <li>{@code subscribe} parks (never pushes, never errors) so the background
 *       loop stays quiet for the test's lifetime.</li>
 * </ul>
 *
 * <p>The SDK is dialed at a {@code passthrough:///} target (resolves to plaintext
 * so the dial never tries TLS) and the channel is swapped to the in-process
 * channel via {@code channelConfigurator}. {@code startupFailOpen(true)} is set
 * defensively so construction never aborts on the (already-empty) startup pull.
 * No sockets, no external network.
 *
 * <p>Close it (try-with-resources) to tear down both the client and the server.
 */
final class WebTestClient implements AutoCloseable {

    final TipsyAbConfigClient client;
    private final Server server;

    private WebTestClient(TipsyAbConfigClient client, Server server) {
        this.client = client;
        this.server = server;
    }

    /** Builds a client with no subscribed namespaces (sufficient for the holder/wrap tests). */
    static WebTestClient create() {
        String name = InProcessServerBuilder.generateName();
        Server server;
        try {
            server = InProcessServerBuilder.forName(name)
                    .directExecutor()
                    .addService(new ParkingConfigService())
                    .build()
                    .start();
        } catch (IOException e) {
            throw new RuntimeException("failed to start in-process config server", e);
        }

        // Fresh in-process channel per dial (the SDK may open more than one).
        java.util.function.UnaryOperator<ManagedChannelBuilder<?>> configurator =
                b -> InProcessChannelBuilder.forName(name).directExecutor();

        Config cfg = Config.builder()
                // create() requires a non-empty namespace set. getConfig is NOT
                // exercised by the web tests (only context construction), and no
                // default namespace is set, so newAbtestContext does no eager
                // prefetch and issues no GetExperimentResult RPC. The startup
                // PullAll returns an empty snapshot (a successful, empty cache).
                .namespaces("web-test-ns")
                .configServiceAddr("passthrough:///web-test")
                .token("web-test-token")
                .transport(Transport.GRPC)
                .startupFailOpen(true)
                // Long interval so the periodic pull loop never fires mid-test.
                .pullInterval(Duration.ofMinutes(10))
                .pullTimeout(Duration.ofSeconds(2))
                .channelConfigurator(configurator)
                .build();

        TipsyAbConfigClient client;
        try {
            client = TipsyAbConfigClient.create(cfg);
        } catch (RuntimeException e) {
            server.shutdownNow();
            throw e;
        }
        return new WebTestClient(client, server);
    }

    @Override
    public void close() {
        if (client != null) {
            client.close();
        }
        if (server != null) {
            server.shutdownNow();
        }
    }

    /** PullAll returns empty (success); Subscribe parks forever (never errors). */
    private static final class ParkingConfigService extends ConfigServiceGrpc.ConfigServiceImplBase {
        @Override
        public void pullAll(PullAllRequest request, StreamObserver<PullAllResponse> obs) {
            obs.onNext(PullAllResponse.getDefaultInstance());
            obs.onCompleted();
        }

        @Override
        public void subscribe(SubscribeRequest request, StreamObserver<ConfigUpdateEvent> obs) {
            // Park: a clean, idle server-streaming call. close() cancels it.
        }
    }
}
