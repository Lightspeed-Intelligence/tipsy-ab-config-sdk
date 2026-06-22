package io.github.lightspeedintelligence.abconfig;

import java.util.Map;

/**
 * Per-namespace immutable cache view handed to readers.
 *
 * <p>SDK-local mirror of {@code config.v1.NamespaceSnapshot}. The dual
 * {@code snapshot_seq} pair (business + experiment) is the freshness vector:
 * either value advancing triggers a cache replace (see
 * {@link ConfigCache#applyProto}). Once published the instance is read-only and
 * shared lock-free across reader threads.
 */
final class NamespaceSnapshot {

    final String namespace;
    final long businessSnapshotSeq;
    final long experimentSnapshotSeq;

    /** Immutable {@code key -> KeyState} map. */
    final Map<String, KeyState> keys;

    NamespaceSnapshot(String namespace,
                      long businessSnapshotSeq,
                      long experimentSnapshotSeq,
                      Map<String, KeyState> keys) {
        this.namespace = namespace;
        this.businessSnapshotSeq = businessSnapshotSeq;
        this.experimentSnapshotSeq = experimentSnapshotSeq;
        this.keys = keys;
    }
}
