package io.github.lightspeedintelligence.abconfig;

import java.util.Collections;
import java.util.Map;

/**
 * SDK-local view of a {@code GetExperimentResult} response: the
 * {@code config_flat_kv} key&rarr;version map consumed by the dynamic
 * {@link TipsyAbConfigClient#getConfig} fast path. The map key is the config-key
 * name (not its id).
 *
 * <p>Mirrors the Go SDK's unexported {@code abtestComputeResult}. Package-private
 * and immutable; never exposed on the public API.
 */
final class AbtestComputeResult {

    /**
     * The shared sentinel "no abtest hits" result. It is reused freely because
     * it is immutable and holds an empty map; callers must construct fresh
     * {@link AbtestContext} instances per request rather than sharing this.
     */
    static final AbtestComputeResult EMPTY_RESULT =
            new AbtestComputeResult(Collections.emptyMap());

    /** {@code config_flat_kv}: config-key name &rarr; version id. Unmodifiable. */
    final Map<String, Long> keyVersions;

    AbtestComputeResult(Map<String, Long> keyVersions) {
        this.keyVersions = keyVersions == null
                ? Collections.emptyMap()
                : Collections.unmodifiableMap(keyVersions);
    }
}
