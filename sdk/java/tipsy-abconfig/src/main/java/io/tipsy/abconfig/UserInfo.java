package io.tipsy.abconfig;

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

    private final String uid;
    private final Map<String, Object> attrs;

    UserInfo(String uid, Map<String, Object> attrs) {
        this.uid = uid == null ? "" : uid;
        this.attrs = attrs == null
                ? Collections.emptyMap()
                : Collections.unmodifiableMap(attrs);
    }

    /**
     * Builds a {@link UserInfo} from a user id and attribute map. This is the
     * public factory web-integration callers use to return a user identity from
     * a {@code io.tipsy.abconfig.web.HttpServerSupport.AbtestUserProvider} (the
     * package-private constructor is reserved for {@link AbtestContext}).
     *
     * <p>A {@code null} {@code uid} normalises to the empty string; a
     * {@code null} {@code attrs} normalises to an empty map. The supplied
     * {@code attrs} map is aliased (wrapped unmodifiable), not copied, so callers
     * must not mutate it after handing it over.
     *
     * @param uid   the user id (may be {@code null} → "")
     * @param attrs the user attributes (may be {@code null} → empty)
     * @return an immutable {@link UserInfo}
     */
    public static UserInfo of(String uid, Map<String, Object> attrs) {
        return new UserInfo(uid, attrs);
    }

    /** The user id this context was constructed with (never {@code null}). */
    public String uid() {
        return uid;
    }

    /**
     * A read-only view of the user attributes (never {@code null}; may be an
     * empty map). Aliases the constructor map; treat as read-only.
     */
    public Map<String, Object> attrs() {
        return attrs;
    }
}
