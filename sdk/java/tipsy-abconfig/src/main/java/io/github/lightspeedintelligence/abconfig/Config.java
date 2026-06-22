package io.github.lightspeedintelligence.abconfig;

import io.grpc.ManagedChannelBuilder;
import java.net.http.HttpClient;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.Objects;
import java.util.function.Consumer;
import java.util.function.UnaryOperator;

/**
 * Immutable startup parameters for {@link TipsyAbConfigClient#create(Config)}.
 *
 * <p>Mirrors the Go SDK's {@code Config}. Every field except {@code namespaces}
 * and {@code configServiceAddr} is optional; the {@link Builder} fills in safe
 * defaults that match the Go SDK ({@code pullInterval=10s}, {@code pullTimeout=5s},
 * {@code pullRetries=3}, {@code abtestTimeout=1500ms}, {@code maxRecvMessageSize}
 * / {@code maxSendMessageSize=512MB}).
 *
 * <p>Two knobs are resolved at {@link TipsyAbConfigClient#create(Config) create}
 * time rather than at {@code build()} time, so a {@code Config} stays a pure
 * value snapshot of what the host set explicitly (this mirrors the Go SDK
 * reading the environment <em>once</em> at {@code Init}):
 * <ul>
 *   <li>{@link #transport()} {@code == null} → the {@code TIPSY_SDK_TRANSPORT}
 *       environment variable, defaulting to {@link Transport#GRPC}.</li>
 *   <li>{@link #defaultNamespace()} empty → the {@code PROJECT_DEFAULT_NAMESPACE}
 *       environment variable.</li>
 * </ul>
 *
 * <p>The structured logger is intentionally NOT a {@code Config} field: Java
 * mirrors the Go {@code Config.Logger} with an SLF4J facade obtained per class
 * via {@code LoggerFactory.getLogger}.
 */
public final class Config {

    private final List<String> namespaces;
    private final String configServiceAddr;
    private final String abtestServiceAddr;
    private final Duration pullInterval;
    private final Duration pullTimeout;
    private final int pullRetries;
    private final Duration abtestTimeout;
    private final boolean startupFailOpen;
    private final String token;
    private final TokenProvider tokenProvider;
    private final int maxRecvMessageSize;
    private final int maxSendMessageSize;
    private final Consumer<BackgroundErrorEvent> onBackgroundError;
    private final String defaultNamespace;
    private final Transport transport;
    private final UnaryOperator<ManagedChannelBuilder<?>> channelConfigurator;
    private final HttpClient httpClient;

    private Config(Builder b) {
        this.namespaces = List.copyOf(b.namespaces);
        this.configServiceAddr = b.configServiceAddr;
        this.abtestServiceAddr = b.abtestServiceAddr;
        this.pullInterval = b.pullInterval;
        this.pullTimeout = b.pullTimeout;
        this.pullRetries = b.pullRetries;
        this.abtestTimeout = b.abtestTimeout;
        this.startupFailOpen = b.startupFailOpen;
        this.token = b.token;
        this.tokenProvider = b.tokenProvider;
        this.maxRecvMessageSize = b.maxRecvMessageSize;
        this.maxSendMessageSize = b.maxSendMessageSize;
        this.onBackgroundError = b.onBackgroundError;
        this.defaultNamespace = b.defaultNamespace;
        this.transport = b.transport;
        this.channelConfigurator = b.channelConfigurator;
        this.httpClient = b.httpClient;
    }

    /** Returns a new, empty {@link Builder}. */
    public static Builder builder() {
        return new Builder();
    }

    /** The business namespaces this client subscribes to (immutable copy). */
    public List<String> namespaces() {
        return namespaces;
    }

    /** The {@code ConfigService} address (gRPC target or HTTP base URL). */
    public String configServiceAddr() {
        return configServiceAddr;
    }

    /** The {@code AbtestService} address, or {@code null}/empty for degraded mode. */
    public String abtestServiceAddr() {
        return abtestServiceAddr;
    }

    /** The fallback PullAll period (default 10s). */
    public Duration pullInterval() {
        return pullInterval;
    }

    /** The per-namespace PullAll deadline (default 5s). */
    public Duration pullTimeout() {
        return pullTimeout;
    }

    /** The number of exponential-backoff retries used per namespace at startup (default 3). */
    public int pullRetries() {
        return pullRetries;
    }

    /** The per-compute {@code GetExperimentResult} deadline (default 1500ms). */
    public Duration abtestTimeout() {
        return abtestTimeout;
    }

    /** Whether a failed startup PullAll is absorbed (empty cache) instead of aborting. */
    public boolean startupFailOpen() {
        return startupFailOpen;
    }

    /** The static bearer token (required unless {@link #tokenProvider()} is set). */
    public String token() {
        return token;
    }

    /** The dynamic token provider (takes precedence over {@link #token()}); may be {@code null}. */
    public TokenProvider tokenProvider() {
        return tokenProvider;
    }

    /** The gRPC channel-level max inbound message size (default 512MB). */
    public int maxRecvMessageSize() {
        return maxRecvMessageSize;
    }

    /** The per-stub max outbound message size (default 512MB). */
    public int maxSendMessageSize() {
        return maxSendMessageSize;
    }

    /** The background-error callback, or {@code null}. Invoked synchronously, recover-wrapped. */
    public Consumer<BackgroundErrorEvent> onBackgroundError() {
        return onBackgroundError;
    }

    /**
     * The configured default namespace; empty means "consult the
     * {@code PROJECT_DEFAULT_NAMESPACE} environment variable at create time".
     */
    public String defaultNamespace() {
        return defaultNamespace;
    }

    /**
     * The configured transport, or {@code null} meaning "consult the
     * {@code TIPSY_SDK_TRANSPORT} environment variable, defaulting to gRPC".
     */
    public Transport transport() {
        return transport;
    }

    /** The gRPC channel-builder injection seam, or {@code null}. */
    public UnaryOperator<ManagedChannelBuilder<?>> channelConfigurator() {
        return channelConfigurator;
    }

    /** The injected HTTP-mode {@link HttpClient}, or {@code null} (SDK builds its own). */
    public HttpClient httpClient() {
        return httpClient;
    }

    /**
     * Fluent builder for {@link Config}. Defaults mirror the Go SDK's
     * {@code applyDefaults}.
     */
    public static final class Builder {

        private List<String> namespaces = new ArrayList<>();
        private String configServiceAddr;
        private String abtestServiceAddr;
        private Duration pullInterval = Duration.ofSeconds(10);
        private Duration pullTimeout = Duration.ofSeconds(5);
        private int pullRetries = 3;
        private Duration abtestTimeout = Duration.ofMillis(1500);
        private boolean startupFailOpen;
        private String token;
        private TokenProvider tokenProvider;
        private int maxRecvMessageSize = 512 * 1024 * 1024;
        private int maxSendMessageSize = 512 * 1024 * 1024;
        private Consumer<BackgroundErrorEvent> onBackgroundError;
        private String defaultNamespace = "";
        private Transport transport;
        private UnaryOperator<ManagedChannelBuilder<?>> channelConfigurator;
        private HttpClient httpClient;

        private Builder() {
        }

        /** Sets the subscribed namespaces from a list (copied). */
        public Builder namespaces(List<String> namespaces) {
            Objects.requireNonNull(namespaces, "namespaces");
            this.namespaces = new ArrayList<>(namespaces);
            return this;
        }

        /** Sets the subscribed namespaces from a varargs list. */
        public Builder namespaces(String... namespaces) {
            Objects.requireNonNull(namespaces, "namespaces");
            this.namespaces = new ArrayList<>(Arrays.asList(namespaces));
            return this;
        }

        /** Sets the required {@code ConfigService} address. */
        public Builder configServiceAddr(String configServiceAddr) {
            this.configServiceAddr = configServiceAddr;
            return this;
        }

        /** Sets the optional {@code AbtestService} address (empty/null → degraded mode). */
        public Builder abtestServiceAddr(String abtestServiceAddr) {
            this.abtestServiceAddr = abtestServiceAddr;
            return this;
        }

        /** Overrides the fallback PullAll period (default 10s). */
        public Builder pullInterval(Duration pullInterval) {
            this.pullInterval = Objects.requireNonNull(pullInterval, "pullInterval");
            return this;
        }

        /** Overrides the per-namespace PullAll deadline (default 5s). */
        public Builder pullTimeout(Duration pullTimeout) {
            this.pullTimeout = Objects.requireNonNull(pullTimeout, "pullTimeout");
            return this;
        }

        /** Overrides the startup retry count per namespace (default 3). */
        public Builder pullRetries(int pullRetries) {
            this.pullRetries = pullRetries;
            return this;
        }

        /** Overrides the per-compute {@code GetExperimentResult} deadline (default 1500ms). */
        public Builder abtestTimeout(Duration abtestTimeout) {
            this.abtestTimeout = Objects.requireNonNull(abtestTimeout, "abtestTimeout");
            return this;
        }

        /** Sets whether a failed startup PullAll is absorbed instead of aborting (default false). */
        public Builder startupFailOpen(boolean startupFailOpen) {
            this.startupFailOpen = startupFailOpen;
            return this;
        }

        /** Sets the static bearer token. */
        public Builder token(String token) {
            this.token = token;
            return this;
        }

        /** Sets the dynamic token provider (takes precedence over {@link #token(String)}). */
        public Builder tokenProvider(TokenProvider tokenProvider) {
            this.tokenProvider = tokenProvider;
            return this;
        }

        /** Overrides the gRPC channel-level max inbound message size (default 512MB). */
        public Builder maxRecvMessageSize(int maxRecvMessageSize) {
            this.maxRecvMessageSize = maxRecvMessageSize;
            return this;
        }

        /** Overrides the per-stub max outbound message size (default 512MB). */
        public Builder maxSendMessageSize(int maxSendMessageSize) {
            this.maxSendMessageSize = maxSendMessageSize;
            return this;
        }

        /** Sets the synchronous, recover-wrapped background-error callback. */
        public Builder onBackgroundError(Consumer<BackgroundErrorEvent> onBackgroundError) {
            this.onBackgroundError = onBackgroundError;
            return this;
        }

        /**
         * Overrides the default namespace. Empty (the default) defers to the
         * {@code PROJECT_DEFAULT_NAMESPACE} environment variable at create time.
         */
        public Builder defaultNamespace(String defaultNamespace) {
            this.defaultNamespace = defaultNamespace == null ? "" : defaultNamespace;
            return this;
        }

        /**
         * Selects the transport. {@code null} (the default) defers to the
         * {@code TIPSY_SDK_TRANSPORT} environment variable, then gRPC.
         */
        public Builder transport(Transport transport) {
            this.transport = transport;
            return this;
        }

        /** Sets the gRPC channel-builder injection seam (e.g. an in-process channel for tests). */
        public Builder channelConfigurator(UnaryOperator<ManagedChannelBuilder<?>> channelConfigurator) {
            this.channelConfigurator = channelConfigurator;
            return this;
        }

        /** Sets the injected HTTP-mode {@link HttpClient} (the SDK builds its own when null). */
        public Builder httpClient(HttpClient httpClient) {
            this.httpClient = httpClient;
            return this;
        }

        /** Builds an immutable {@link Config} from the current builder state. */
        public Config build() {
            return new Config(this);
        }
    }
}
