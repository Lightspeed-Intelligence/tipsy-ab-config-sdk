package io.github.lightspeedintelligence.abconfig;

import java.util.Map;

/**
 * SDK-local immutable mirror of {@code config.v1.KeyState}.
 *
 * <p>Embedded by value in {@link NamespaceSnapshot} so cache reads need no
 * pointer indirection. Published instances are treated as read-only.
 */
final class KeyState {

    /**
     * The {@code version_id} of the active full release for this key, or
     * {@code 0} if none. {@code 0} is the sentinel: proto3
     * {@code optional int64 full_release_version} that is absent normalises to
     * {@code 0} here. Callers MUST gate on existence (see
     * {@link ConfigCache#fullReleaseVersion}), never compare against {@code 0}.
     */
    final long fullReleaseVersion;

    /**
     * Immutable {@code version_id -> value} map. Contains the full-release
     * version (if any) plus every version abtest reports as possibly applicable
     * for the namespace.
     */
    final Map<Long, String> versions;

    /**
     * Presence-aware mirror of proto3 {@code optional bool has_dynamic_resolution}:
     * whether this key has any gray-release / experiment attached (i.e. it needs
     * abtest resolution). {@code null} = field absent (old server that does not
     * emit it); {@link Boolean#TRUE}/{@link Boolean#FALSE} = explicitly set.
     *
     * <p>The {@code null}-vs-{@code FALSE} distinction is load-bearing: the
     * {@code getConfig} fast-path skips the abtest wait ONLY when this is
     * explicitly {@code FALSE}. Absent ({@code null}) or {@code TRUE} keeps the
     * existing always-wait path, so a new SDK pointed at an old server never
     * mis-skips and silently breaks gray-release.
     */
    final Boolean hasDynamicResolution;

    KeyState(long fullReleaseVersion, Map<Long, String> versions, Boolean hasDynamicResolution) {
        this.fullReleaseVersion = fullReleaseVersion;
        this.versions = versions;
        this.hasDynamicResolution = hasDynamicResolution;
    }
}
