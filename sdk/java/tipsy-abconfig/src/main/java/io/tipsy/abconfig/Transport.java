package io.tipsy.abconfig;

/**
 * Selects the wire transport used by the SDK.
 *
 * <p>Mirrors the Go SDK's {@code TransportGRPC} / {@code TransportHTTP} string
 * constants. The {@linkplain #wireValue() wire value} ({@code "grpc"} /
 * {@code "http"}) is the canonical lower-case form used when parsing the
 * {@code TIPSY_SDK_TRANSPORT} environment variable and when constructing error
 * messages.
 */
public enum Transport {

    /**
     * gRPC transport (the default). Addresses follow the {@code 方案 Y} gRPC
     * target grammar; the Subscribe stream and periodic PullAll polling are both
     * active.
     */
    GRPC("grpc"),

    /**
     * HTTP transport. Addresses are interpreted as {@code http(s)://} base URLs;
     * the Subscribe stream is not used, so config-change propagation latency is
     * bounded by the PullAll interval.
     */
    HTTP("http");

    private final String wireValue;

    Transport(String wireValue) {
        this.wireValue = wireValue;
    }

    /**
     * Returns the canonical lower-case wire value ({@code "grpc"} or
     * {@code "http"}).
     *
     * @return the wire value string
     */
    public String wireValue() {
        return wireValue;
    }
}
