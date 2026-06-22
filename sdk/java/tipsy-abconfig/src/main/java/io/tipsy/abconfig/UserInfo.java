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
