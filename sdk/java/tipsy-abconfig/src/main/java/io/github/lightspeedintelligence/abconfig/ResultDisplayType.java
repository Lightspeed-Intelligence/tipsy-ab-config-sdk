package io.github.lightspeedintelligence.abconfig;

import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;

/**
 * SDK-stable mirror of {@code abtestv1.ResultDisplayType} for the
 * {@link TipsyAbConfigClient#getExperimentResult(ExperimentResultRequest)}
 * client surface (design 03 / 05). It selects the shape of the experiment
 * result without importing the generated proto package directly.
 *
 * <p>Each constant carries the proto wire value via {@link #wireValue()}; the
 * SDK builds the proto enum from it when constructing the
 * {@link GetExperimentResultRequest}.
 */
public enum ResultDisplayType {

    /** Treated server-side as the default ({@code flat_kv}). */
    UNSPECIFIED(0),
    /** Requests the flattened key&rarr;version / key&rarr;value view. */
    FLAT_KV(1),
    /** Requests the per-experiment-group result list. */
    EACH_EXPERIMENT_GROUP(2);

    private final int wireValue;

    ResultDisplayType(int wireValue) {
        this.wireValue = wireValue;
    }

    /** The proto wire value (mirrors {@code abtestv1.ResultDisplayType}'s numbers). */
    public int wireValue() {
        return wireValue;
    }

    /**
     * Maps this SDK enum to the generated proto enum. An unrecognised wire value
     * (forward-compat) maps to the proto {@code UNRECOGNIZED} constant, but every
     * constant defined here has a known proto counterpart.
     */
    io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ResultDisplayType toProto() {
        return io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ResultDisplayType.forNumber(wireValue);
    }
}
