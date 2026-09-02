package io.github.lightspeedintelligence.abconfig;

import java.util.Collections;
import java.util.Map;

/**
 * The SDK-stable view of the user identity carried by an {@link AbtestContext}
 * (design 03 / 05). Business code retrieves it via
 * {@link AbtestContext#userInfo()}. Mirrors the Go SDK's {@code UserInfo}.
 *
 * <p>Immutable: {@link #attrs()} is an unmodifiable view of the map the owning
 * {@link AbtestContext} was constructed with (may be an empty map, never
 * {@code null}). Callers MUST treat it as read-only.
 */
public final class UserInfo {

    private final String experimentHashId;
    private final Map<String, Object> attrs;

    UserInfo(String experimentHashId, Map<String, Object> attrs) {
        this.experimentHashId = experimentHashId == null ? "" : experimentHashId;
        this.attrs = attrs == null
                ? Collections.emptyMap()
                : Collections.unmodifiableMap(attrs);
    }

    /**
     * Builds a {@link UserInfo} from an experiment hash id and attribute map. This is the
     * public factory web-integration callers use to return a user identity from
     * a {@code io.github.lightspeedintelligence.abconfig.web.HttpServerSupport.AbtestUserProvider} (the
     * package-private constructor is reserved for {@link AbtestContext}).
     *
     * <p>A {@code null} {@code experimentHashId} normalises to the empty string; a
     * {@code null} {@code attrs} normalises to an empty map. The supplied
     * {@code attrs} map is aliased (wrapped unmodifiable), not copied, so callers
     * must not mutate it after handing it over.
     *
     * @param experimentHashId the identifier the abtest platform hashes for
     *                         bucketing (typically a uid; sent on the wire as
     *                         {@code user_id}; may be {@code null} → "")
     * @param attrs            the user attributes (may be {@code null} → empty)
     * @return an immutable {@link UserInfo}
     */
    public static UserInfo of(String experimentHashId, Map<String, Object> attrs) {
        return new UserInfo(experimentHashId, attrs);
    }

    /**
     * The experiment hash id this context was constructed with (never
     * {@code null}). This is the identifier the abtest platform hashes to bucket
     * the request into an experiment group (sent on the wire as {@code user_id}).
     */
    public String experimentHashId() {
        return experimentHashId;
    }

    /**
     * A read-only view of the user attributes (never {@code null}; may be an
     * empty map). Aliases the constructor map; treat as read-only.
     */
    public Map<String, Object> attrs() {
        return attrs;
    }
}
