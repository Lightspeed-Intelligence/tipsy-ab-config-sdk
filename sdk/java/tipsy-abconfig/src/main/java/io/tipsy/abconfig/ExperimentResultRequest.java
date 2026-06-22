package io.tipsy.abconfig;

import java.util.Collections;
import java.util.List;
import java.util.Map;

/**
 * SDK-stable request for {@link TipsyAbConfigClient#getExperimentResult} (design
 * 03 / 05). It exposes every wire parameter so business code can fetch
 * {@code custom_params} results (or per-group results) directly, without going
 * through the {@code config_version} fast path baked into {@code getConfig}.
 *
 * <p>Immutable; construct via {@link #builder()}. Mirrors the Go SDK's
 * {@code ExperimentResultRequest}.
 */
public final class ExperimentResultRequest {

    private final String namespace;
    private final UserInfo userInfo;
    private final List<String> layerIds;
    private final ExperimentType type;
    private final ResultDisplayType displayType;
    private final String traceId;

    private ExperimentResultRequest(Builder b) {
        this.namespace = b.namespace == null ? "" : b.namespace;
        this.userInfo = b.userInfo != null ? b.userInfo : new UserInfo("", null);
        this.layerIds = b.layerIds == null
                ? Collections.emptyList()
                : List.copyOf(b.layerIds);
        this.type = b.type != null ? b.type : ExperimentType.UNSPECIFIED;
        this.displayType = b.displayType != null ? b.displayType : ResultDisplayType.UNSPECIFIED;
        this.traceId = b.traceId == null ? "" : b.traceId;
    }

    /** Creates a new builder. */
    public static Builder builder() {
        return new Builder();
    }

    /** The target namespace. Empty resolves to the project default namespace. */
    public String namespace() {
        return namespace;
    }

    /** The user identity (uid + attrs) sent on the wire (never {@code null}). */
    public UserInfo userInfo() {
        return userInfo;
    }

    /**
     * Optional layer restriction (never {@code null}; empty means all layers).
     */
    public List<String> layerIds() {
        return layerIds;
    }

    /** The experiment type(s) to evaluate (default {@link ExperimentType#UNSPECIFIED}). */
    public ExperimentType type() {
        return type;
    }

    /** The result shape (default {@link ResultDisplayType#UNSPECIFIED}). */
    public ResultDisplayType displayType() {
        return displayType;
    }

    /** The optional per-request trace id (empty &rArr; the SDK generates a UUID). */
    public String traceId() {
        return traceId;
    }

    /** Mutable builder for {@link ExperimentResultRequest}. */
    public static final class Builder {
        private String namespace;
        private UserInfo userInfo;
        private List<String> layerIds;
        private ExperimentType type;
        private ResultDisplayType displayType;
        private String traceId;

        private Builder() {
        }

        /** Sets the target namespace (empty &rArr; project default namespace). */
        public Builder namespace(String namespace) {
            this.namespace = namespace;
            return this;
        }

        /** Sets the user identity (uid + attrs) sent on the wire. */
        public Builder userInfo(UserInfo userInfo) {
            this.userInfo = userInfo;
            return this;
        }

        /** Convenience: builds the {@link UserInfo} from a uid + attrs map. */
        public Builder userInfo(String uid, Map<String, Object> attrs) {
            this.userInfo = new UserInfo(uid, attrs);
            return this;
        }

        /** Restricts the computation to specific layers (empty &rArr; all layers). */
        public Builder layerIds(List<String> layerIds) {
            this.layerIds = layerIds;
            return this;
        }

        /** Selects the experiment type(s) to evaluate. */
        public Builder type(ExperimentType type) {
            this.type = type;
            return this;
        }

        /** Selects the result shape. */
        public Builder displayType(ResultDisplayType displayType) {
            this.displayType = displayType;
            return this;
        }

        /** Sets the per-request trace id (empty &rArr; SDK-generated UUID). */
        public Builder traceId(String traceId) {
            this.traceId = traceId;
            return this;
        }

        /** Builds the immutable request. */
        public ExperimentResultRequest build() {
            return new ExperimentResultRequest(this);
        }
    }
}
