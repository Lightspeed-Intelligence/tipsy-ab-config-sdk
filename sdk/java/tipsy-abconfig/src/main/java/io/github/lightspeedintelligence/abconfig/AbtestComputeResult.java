package io.github.lightspeedintelligence.abconfig;

import java.util.Collections;
import java.util.Map;

/**
 * SDK-local view of a {@code GetExperimentResult} response: the flat
 * key&rarr;version map consumed by the dynamic
 * {@link TipsyAbConfigClient#getConfig} fast path, assembled by the SDK-local
 * merge of the {@code EACH_EXPERIMENT_GROUP} response shape
 * ({@code groups[]} + {@code gray_hits[]}), plus the per-key attribution the
 * hit log reports (which experiment group / gray release produced the value).
 * The map key is the config-key name (not its id).
 *
 * <p>Mirrors the Go SDK's unexported {@code abtestComputeResult}. Package-private
 * and immutable; never exposed on the public API.
 */
final class AbtestComputeResult {

    /**
     * Why a key ended up in {@link #keyVersions}: which source produced the
     * winning versionId. Package-private; surfaces in the hit log only as the
     * structured {@code reason} field (experiment / gray_whitelist /
     * abtest_unattributed).
     */
    static final class Attribution {

        enum Source { EXPERIMENT, GRAY_WHITELIST, UNATTRIBUTED }

        /**
         * The shared "value valid, attribution unknown" sentinel: an
         * empty-string-id experiment group, a {@code release_id=0} gray hit, a
         * mock-seeded result, or a key missing from {@link #attribution}
         * entirely. Attribution loss NEVER changes the resolved value (F3) —
         * it only degrades the log {@code reason} to
         * {@code abtest_unattributed}.
         */
        static final Attribution UNATTRIBUTED =
                new Attribution(Source.UNATTRIBUTED, "", "", 0L);

        final Source source;
        /** Non-empty only when {@code source == EXPERIMENT}. */
        final String experimentId;
        /** Non-empty only when {@code source == EXPERIMENT}. */
        final String groupId;
        /** Non-zero only when {@code source == GRAY_WHITELIST}. */
        final long releaseId;

        private Attribution(Source source, String experimentId, String groupId, long releaseId) {
            this.source = source;
            this.experimentId = experimentId;
            this.groupId = groupId;
            this.releaseId = releaseId;
        }

        static Attribution experiment(String experimentId, String groupId) {
            return new Attribution(Source.EXPERIMENT, experimentId, groupId, 0L);
        }

        static Attribution grayWhitelist(long releaseId) {
            return new Attribution(Source.GRAY_WHITELIST, "", "", releaseId);
        }
    }

    /**
     * The shared sentinel "no abtest hits" result. It is reused freely because
     * it is immutable and holds an empty map; callers must construct fresh
     * {@link AbtestContext} instances per request rather than sharing this.
     */
    static final AbtestComputeResult EMPTY_RESULT =
            new AbtestComputeResult(Collections.emptyMap());

    /** Config-key name &rarr; version id (SDK-local merge output). Unmodifiable. */
    final Map<String, Long> keyVersions;

    /**
     * Config-key name &rarr; {@link Attribution}, parallel to
     * {@link #keyVersions}. For a merge-produced result every key present in
     * {@code keyVersions} has an entry here (possibly
     * {@link Attribution#UNATTRIBUTED}); mock-seeded results
     * ({@code mockAbtestContext}) carry an empty map and readers treat a
     * missing entry as {@link Attribution#UNATTRIBUTED}. Unmodifiable.
     */
    final Map<String, Attribution> attribution;

    AbtestComputeResult(Map<String, Long> keyVersions) {
        this(keyVersions, null);
    }

    AbtestComputeResult(Map<String, Long> keyVersions, Map<String, Attribution> attribution) {
        this.keyVersions = keyVersions == null
                ? Collections.emptyMap()
                : Collections.unmodifiableMap(keyVersions);
        this.attribution = attribution == null
                ? Collections.emptyMap()
                : Collections.unmodifiableMap(attribution);
    }
}
