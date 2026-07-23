package io.github.lightspeedintelligence.abconfig;

import static java.nio.charset.StandardCharsets.UTF_8;

import java.util.HashMap;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.OptionalLong;
import java.util.concurrent.ConcurrentHashMap;

/**
 * In-process, per-namespace cache of {@link NamespaceSnapshot}.
 *
 * <p>Mirrors Go {@code configCache}. Concurrency model: many readers, one
 * writer per namespace at a time. Snapshots are immutable once published, so a
 * {@link ConcurrentHashMap} slot write is the only synchronisation needed —
 * readers fetch the current snapshot reference and read its fields lock-free.
 *
 * <p>The two existence-typed accessors ({@link #fullReleaseVersion} returning
 * {@link OptionalLong}, {@link #valueOf} returning {@link Optional}) enforce the
 * global invariants: {@code 0} is a sentinel (never "no full release") and the
 * empty string is a valid config value (never "missing").
 */
final class ConfigCache {

    private final ConcurrentHashMap<String, NamespaceSnapshot> byNs = new ConcurrentHashMap<>();
    private final Metrics metrics;

    ConfigCache(Metrics metrics) {
        this.metrics = metrics;
    }

    /** Result of {@link #applyProto}: whether the snapshot was replaced and which seqs advanced. */
    static final class ApplyResult {
        final boolean replaced;
        final boolean businessMoved;
        final boolean experimentMoved;

        ApplyResult(boolean replaced, boolean businessMoved, boolean experimentMoved) {
            this.replaced = replaced;
            this.businessMoved = businessMoved;
            this.experimentMoved = experimentMoved;
        }
    }

    /** Returns the current snapshot for {@code ns}, or empty if none cached. */
    Optional<NamespaceSnapshot> snapshot(String ns) {
        return Optional.ofNullable(byNs.get(ns));
    }

    /**
     * Returns the active full-release {@code version_id} for {@code (ns, key)}
     * when present and non-zero, otherwise empty. Callers MUST NOT treat
     * {@code 0} as "no full release" — an empty result is the only "no full
     * release" signal.
     */
    OptionalLong fullReleaseVersion(String ns, String key) {
        NamespaceSnapshot s = byNs.get(ns);
        if (s == null) {
            return OptionalLong.empty();
        }
        KeyState ks = s.keys.get(key);
        if (ks == null || ks.fullReleaseVersion == 0L) {
            return OptionalLong.empty();
        }
        return OptionalLong.of(ks.fullReleaseVersion);
    }

    /**
     * Returns the presence-aware {@code has_dynamic_resolution} flag for
     * {@code (ns, key)}:
     * <ul>
     *   <li>{@code null} &rArr; no snapshot, no such key, or the field was absent
     *       on the wire (old server) — the caller MUST treat this as "unknown"
     *       and keep the always-wait abtest path.</li>
     *   <li>{@link Boolean#FALSE} &rArr; the server explicitly reported this key
     *       as pure full-rollout (no gray-release / experiment); the
     *       {@code getConfig} fast-path may skip the abtest wait.</li>
     *   <li>{@link Boolean#TRUE} &rArr; the key needs abtest resolution.</li>
     * </ul>
     * Mirrors the proto3 {@code optional} tri-state: {@code null} = absent,
     * which is deliberately distinguished from an explicit {@code FALSE} so a new
     * SDK talking to an old server never mis-skips the abtest wait.
     */
    Boolean hasDynamicResolution(String ns, String key) {
        NamespaceSnapshot s = byNs.get(ns);
        if (s == null) {
            return null;
        }
        KeyState ks = s.keys.get(key);
        if (ks == null) {
            return null;
        }
        return ks.hasDynamicResolution;
    }

    /**
     * Returns the cached value for {@code (ns, key, versionId)}, or empty on a
     * cache miss. The empty string is a valid value, so callers MUST gate on
     * {@link Optional#isPresent()} rather than on the string contents.
     */
    Optional<String> valueOf(String ns, String key, long versionId) {
        NamespaceSnapshot s = byNs.get(ns);
        if (s == null) {
            return Optional.empty();
        }
        KeyState ks = s.keys.get(key);
        if (ks == null) {
            return Optional.empty();
        }
        return Optional.ofNullable(ks.versions.get(versionId));
    }

    /**
     * Replaces the cached snapshot for the protobuf namespace iff either
     * {@code snapshot_seq} strictly advanced (including {@code 0 -> 1}); the
     * first snapshot for a namespace is always written.
     *
     * <ul>
     *   <li>{@code pb == null} or empty namespace → no replace.</li>
     *   <li>{@code cur != null} and neither seq advanced → no replace.</li>
     *   <li>otherwise build a fresh immutable snapshot and swap it in.</li>
     * </ul>
     *
     * On a replace, updates metrics: {@code localCacheBytes.set(ns, byteSize)};
     * {@code businessSeqMoved.inc(ns)} / {@code experimentSeqMov.inc(ns)} when
     * the respective seq advanced. Keys with an empty key string are skipped;
     * {@code full_release_version} uses proto3 {@code hasFullReleaseVersion()}
     * (absent → 0). {@code byteSize} sums UTF-8 byte lengths of each key plus
     * each value (matching Go's {@code len()}).
     *
     * <p>Concurrency: the read-decide-write (compare current seqs, then
     * conditionally swap) runs atomically inside {@link ConcurrentHashMap#compute}
     * so concurrent writers to the same namespace (the periodic pull loop and the
     * subscribe loop in particular) cannot clobber each other — a lower-seq
     * snapshot can never overwrite a higher-seq one, preserving the "seq strictly
     * advances" invariant. Reads ({@link #snapshot}, {@link #valueOf},
     * {@link #fullReleaseVersion}, {@link #knownSeqs}) stay lock-free. The
     * candidate snapshot and its {@code byteSize} are built before {@code compute}
     * (pure work, no side effects) so the mapping function stays light; the
     * metrics side effects, being cheap, are applied inside {@code compute} on the
     * replace path so the gauge {@code set} and the snapshot swap are one atomic
     * step under the same bin lock (mirroring Go's {@code set} under
     * {@code c.mu.Lock()}). Doing them after {@code compute} let a stale writer's
     * delayed {@code set(ns, byteSize)} overwrite the survivor's, pinning the gauge
     * to the wrong byte count.
     */
    ApplyResult applyProto(io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSnapshot pb) {
        if (pb == null || pb.getNamespace().isEmpty()) {
            return new ApplyResult(false, false, false);
        }
        String ns = pb.getNamespace();

        long newBiz = pb.getBusinessSnapshotSeq();
        long newExp = pb.getExperimentSnapshotSeq();

        // Build the candidate snapshot and its byteSize up front: this work is
        // pure (depends only on pb) and is discarded if the seq check rejects it.
        Map<String, KeyState> keys = new HashMap<>(pb.getKeysCount() * 2);
        long byteSizeAccum = 0L;
        for (io.github.lightspeedintelligence.abconfig.proto.config.v1.KeyState k : pb.getKeysList()) {
            if (k == null || k.getKey().isEmpty()) {
                continue;
            }
            Map<Long, String> versions = new HashMap<>(k.getVersionsCount() * 2);
            for (Map.Entry<Long, String> e : k.getVersionsMap().entrySet()) {
                String val = e.getValue();
                versions.put(e.getKey(), val);
                byteSizeAccum += val.getBytes(UTF_8).length;
            }
            byteSizeAccum += k.getKey().getBytes(UTF_8).length;
            long frv = k.hasFullReleaseVersion() ? k.getFullReleaseVersion() : 0L;
            // Presence-aware: null when the field is absent (old server), else the
            // explicit boolean. Only an explicit FALSE enables the getConfig
            // fast-path; null/TRUE keep the always-wait abtest path.
            Boolean hdr = k.hasHasDynamicResolution() ? k.getHasDynamicResolution() : null;
            keys.put(k.getKey(), new KeyState(frv, Map.copyOf(versions), hdr));
        }
        NamespaceSnapshot candidate = new NamespaceSnapshot(ns, newBiz, newExp, Map.copyOf(keys));
        long byteSize = byteSizeAccum;

        // The mapping function records its decision here for the ApplyResult. The
        // metrics side effects run INSIDE compute on the replace path so the gauge
        // set and the snapshot swap are one atomic step under the CHM bin lock,
        // mirroring Go's "set under c.mu.Lock()" (cache.go). Applying them after
        // compute let a stale (lower-seq) writer's delayed set(ns, byteSize)
        // overwrite the survivor's, leaving localCacheBytes stuck on the wrong
        // byte count. The candidate build/byteSize accumulation stays outside
        // compute (pure heavy work); only the light set/inc move in.
        final Metrics m = metrics; // hoist null check out of the mapping function
        boolean[] decision = {false, false, false}; // {replaced, businessMoved, experimentMoved}
        byNs.compute(ns, (k, cur) -> {
            long curBiz = cur == null ? 0L : cur.businessSnapshotSeq;
            long curExp = cur == null ? 0L : cur.experimentSnapshotSeq;
            boolean businessMoved = newBiz > curBiz;
            boolean experimentMoved = newExp > curExp;
            if (cur != null && !businessMoved && !experimentMoved) {
                return cur; // no advance → keep current snapshot
            }
            decision[0] = true;
            decision[1] = businessMoved;
            decision[2] = experimentMoved;
            if (m != null) {
                m.localCacheBytes.set(ns, byteSize);
                if (businessMoved) {
                    m.businessSeqMoved.inc(ns);
                }
                if (experimentMoved) {
                    m.experimentSeqMov.inc(ns);
                }
            }
            return candidate;
        });

        if (!decision[0]) {
            return new ApplyResult(false, false, false);
        }
        return new ApplyResult(true, decision[1], decision[2]);
    }

    /**
     * Returns the {@code NamespaceSeqs} the cache currently holds for each of
     * {@code namespaces}. A namespace with no snapshot is still listed with the
     * zero pair {@code {0, 0}}, which the server treats as "send me
     * everything". Iteration order follows {@code namespaces}.
     */
    Map<String, io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSeqs> knownSeqs(List<String> namespaces) {
        Map<String, io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSeqs> out =
                new LinkedHashMap<>(namespaces.size() * 2);
        for (String ns : namespaces) {
            NamespaceSnapshot s = byNs.get(ns);
            io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSeqs.Builder b =
                    io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSeqs.newBuilder();
            if (s != null) {
                b.setBusinessSnapshotSeq(s.businessSnapshotSeq)
                 .setExperimentSnapshotSeq(s.experimentSnapshotSeq);
            }
            out.put(ns, b.build());
        }
        return out;
    }
}
