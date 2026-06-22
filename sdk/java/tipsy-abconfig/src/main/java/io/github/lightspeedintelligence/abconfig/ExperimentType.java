package io.github.lightspeedintelligence.abconfig;

import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;

/**
 * SDK-stable mirror of {@code abtestv1.ExperimentType} for the
 * {@link TipsyAbConfigClient#getExperimentResult(ExperimentResultRequest)}
 * client surface (design 03 / 05). It lets business code select
 * {@code config_version} vs {@code custom_params} experiments without importing
 * the generated proto package directly.
 *
 * <p>Each constant carries the proto wire value via {@link #wireValue()}; the
 * SDK builds the proto enum from it when constructing the
 * {@link GetExperimentResultRequest}.
 */
public enum ExperimentType {

    /** Treated server-side as the default ({@code custom_params}). */
    UNSPECIFIED(0),
    /** Selects {@code config_version} experiments. */
    CONFIG_VERSION(1),
    /** Selects {@code custom_params} experiments. */
    CUSTOM_PARAMS(2),
    /** Selects both experiment types. */
    ALL(3);

    private final int wireValue;

    ExperimentType(int wireValue) {
        this.wireValue = wireValue;
    }

    /** The proto wire value (mirrors {@code abtestv1.ExperimentType}'s numbers). */
    public int wireValue() {
        return wireValue;
    }

    /**
     * Maps this SDK enum to the generated proto enum. An unrecognised wire value
     * (forward-compat) maps to the proto {@code UNRECOGNIZED} constant, but every
     * constant defined here has a known proto counterpart.
     */
    io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ExperimentType toProto() {
        return io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ExperimentType.forNumber(wireValue);
    }
}
