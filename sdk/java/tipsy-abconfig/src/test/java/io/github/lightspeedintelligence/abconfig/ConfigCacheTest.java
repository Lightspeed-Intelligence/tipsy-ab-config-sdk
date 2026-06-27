package io.github.lightspeedintelligence.abconfig;

import static java.nio.charset.StandardCharsets.UTF_8;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.github.lightspeedintelligence.abconfig.proto.config.v1.KeyState;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSeqs;
import io.github.lightspeedintelligence.abconfig.proto.config.v1.NamespaceSnapshot;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.OptionalLong;
import org.junit.jupiter.api.Test;

/**
 * Unit tests for {@link ConfigCache} replacement rules and the two
 * existence-typed accessors that enforce global invariants 2 (0 is a sentinel)
 * and 3 (the empty string is a valid value).
 *
 * <p>Design refs: 04-transport-and-cache.md §"缓存（ConfigCache …）/ 替换规则" and
 * 00-index.md invariants 2, 3, 5.
 */
final class ConfigCacheTest {

    private static final String NS = "checkout";

    /** Builds a protobuf {@link NamespaceSnapshot} with the given seqs and key states. */
    private static NamespaceSnapshot.Builder pbSnapshot(String ns, long biz, long exp) {
        return NamespaceSnapshot.newBuilder()
                .setNamespace(ns)
                .setBusinessSnapshotSeq(biz)
                .setExperimentSnapshotSeq(exp);
    }

    private static KeyState.Builder pbKey(String key) {
        return KeyState.newBuilder().setKey(key);
    }

    // ---- replacement rules ------------------------------------------------

    @Test
    void firstSnapshotIsWritten_evenAtSeqZeroToOne() {
        ConfigCache cache = new ConfigCache(new Metrics());

        // 0 -> 1 on business; first snapshot for the ns is always written.
        ConfigCache.ApplyResult r = cache.applyProto(
                pbSnapshot(NS, 1, 0)
                        .addKeys(pbKey("color").putVersions(7L, "blue"))
                        .build());

        assertTrue(r.replaced, "first snapshot must be written");
        assertTrue(r.businessMoved, "business 0->1 moved");
        assertFalse(r.experimentMoved, "experiment stayed at 0");

        assertTrue(cache.snapshot(NS).isPresent(), "snapshot now cached");
        assertEquals(1L, cache.snapshot(NS).get().businessSnapshotSeq);
        assertEquals(0L, cache.snapshot(NS).get().experimentSnapshotSeq);
        assertEquals("blue", cache.valueOf(NS, "color", 7L).orElseThrow());
    }

    @Test
    void businessSeqAdvanceReplaces() {
        ConfigCache cache = new ConfigCache(new Metrics());
        cache.applyProto(pbSnapshot(NS, 1, 1)
                .addKeys(pbKey("color").putVersions(7L, "blue")).build());

        ConfigCache.ApplyResult r = cache.applyProto(pbSnapshot(NS, 2, 1)
                .addKeys(pbKey("color").putVersions(8L, "green")).build());

        assertTrue(r.replaced);
        assertTrue(r.businessMoved);
        assertFalse(r.experimentMoved);
        assertEquals("green", cache.valueOf(NS, "color", 8L).orElseThrow());
        // old version_id is gone after replace.
        assertTrue(cache.valueOf(NS, "color", 7L).isEmpty());
    }

    @Test
    void experimentSeqAdvanceReplaces() {
        ConfigCache cache = new ConfigCache(new Metrics());
        cache.applyProto(pbSnapshot(NS, 5, 1)
                .addKeys(pbKey("color").putVersions(7L, "blue")).build());

        ConfigCache.ApplyResult r = cache.applyProto(pbSnapshot(NS, 5, 2)
                .addKeys(pbKey("color").putVersions(9L, "red")).build());

        assertTrue(r.replaced);
        assertFalse(r.businessMoved);
        assertTrue(r.experimentMoved);
        assertEquals("red", cache.valueOf(NS, "color", 9L).orElseThrow());
    }

    @Test
    void neitherSeqAdvances_noReplace() {
        ConfigCache cache = new ConfigCache(new Metrics());
        cache.applyProto(pbSnapshot(NS, 3, 3)
                .addKeys(pbKey("color").putVersions(7L, "blue")).build());

        // Equal seqs (not strictly greater) on both axes -> stale, no replace.
        ConfigCache.ApplyResult r = cache.applyProto(pbSnapshot(NS, 3, 3)
                .addKeys(pbKey("color").putVersions(99L, "should-not-apply")).build());

        assertFalse(r.replaced, "stale snapshot must not replace");
        assertFalse(r.businessMoved);
        assertFalse(r.experimentMoved);
        // Original content untouched.
        assertEquals("blue", cache.valueOf(NS, "color", 7L).orElseThrow());
        assertTrue(cache.valueOf(NS, "color", 99L).isEmpty());
    }

    @Test
    void lowerSeqDoesNotReplace() {
        ConfigCache cache = new ConfigCache(new Metrics());
        cache.applyProto(pbSnapshot(NS, 10, 10)
                .addKeys(pbKey("color").putVersions(7L, "blue")).build());

        ConfigCache.ApplyResult r = cache.applyProto(pbSnapshot(NS, 9, 9)
                .addKeys(pbKey("color").putVersions(8L, "stale")).build());

        assertFalse(r.replaced, "regressing seqs must not replace");
        assertEquals("blue", cache.valueOf(NS, "color", 7L).orElseThrow());
    }

    @Test
    void emptyNamespaceIsNeverReplaced() {
        ConfigCache cache = new ConfigCache(new Metrics());

        ConfigCache.ApplyResult r = cache.applyProto(pbSnapshot("", 5, 5)
                .addKeys(pbKey("color").putVersions(1L, "x")).build());

        assertFalse(r.replaced);
        assertFalse(r.businessMoved);
        assertFalse(r.experimentMoved);
        assertTrue(cache.snapshot("").isEmpty());
    }

    @Test
    void nullProtoIsNeverReplaced() {
        ConfigCache cache = new ConfigCache(new Metrics());
        ConfigCache.ApplyResult r = cache.applyProto(null);
        assertFalse(r.replaced);
        assertFalse(r.businessMoved);
        assertFalse(r.experimentMoved);
    }

    @Test
    void emptyKeyIsSkipped() {
        ConfigCache cache = new ConfigCache(new Metrics());

        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey("").putVersions(1L, "ghost")) // empty key -> skipped
                .addKeys(pbKey("real").putVersions(2L, "kept"))
                .build());

        assertTrue(cache.snapshot(NS).isPresent());
        assertFalse(cache.snapshot(NS).get().keys.containsKey(""),
                "empty key must not be cached");
        assertTrue(cache.snapshot(NS).get().keys.containsKey("real"));
        assertEquals("kept", cache.valueOf(NS, "real", 2L).orElseThrow());
    }

    // ---- invariant 2: 0 is a sentinel (full_release_version) --------------

    @Test
    void missingFullReleaseVersion_yieldsEmptyAndNeverTreatsZeroAsHit() {
        ConfigCache cache = new ConfigCache(new Metrics());

        // KeyState WITHOUT setFullReleaseVersion -> proto3 optional absent -> 0.
        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey("color").putVersions(7L, "blue")) // no full_release_version set
                .build());

        OptionalLong frv = cache.fullReleaseVersion(NS, "color");
        assertTrue(frv.isEmpty(),
                "absent full_release_version must surface as empty, never 0-as-hit");
    }

    @Test
    void explicitZeroFullReleaseVersion_isStillEmpty() {
        ConfigCache cache = new ConfigCache(new Metrics());

        // Even an explicitly-set 0 is the sentinel, not a real version id.
        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey("color").setFullReleaseVersion(0L).putVersions(7L, "blue"))
                .build());

        assertTrue(cache.fullReleaseVersion(NS, "color").isEmpty(),
                "full_release_version == 0 is the sentinel, must be empty");
    }

    @Test
    void presentFullReleaseVersion_isReturned() {
        ConfigCache cache = new ConfigCache(new Metrics());
        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey("color").setFullReleaseVersion(42L).putVersions(42L, "blue"))
                .build());

        OptionalLong frv = cache.fullReleaseVersion(NS, "color");
        assertTrue(frv.isPresent());
        assertEquals(42L, frv.getAsLong());
    }

    @Test
    void fullReleaseVersion_unknownNsOrKey_isEmpty() {
        ConfigCache cache = new ConfigCache(new Metrics());
        assertTrue(cache.fullReleaseVersion("nope", "color").isEmpty());

        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey("color").setFullReleaseVersion(42L).putVersions(42L, "blue"))
                .build());
        assertTrue(cache.fullReleaseVersion(NS, "missing-key").isEmpty());
    }

    // ---- invariant 3: empty string is a valid value (valueOf) -------------

    @Test
    void emptyStringValueIsCacheHit() {
        ConfigCache cache = new ConfigCache(new Metrics());

        // The stored value is the empty string — a legal config value.
        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey("flag").putVersions(3L, ""))
                .build());

        Optional<String> v = cache.valueOf(NS, "flag", 3L);
        assertTrue(v.isPresent(), "empty-string value must be a cache HIT");
        assertEquals("", v.get(), "the cached value is the empty string");
    }

    @Test
    void valueOf_missingVersionIsCacheMiss() {
        ConfigCache cache = new ConfigCache(new Metrics());
        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey("flag").putVersions(3L, "on"))
                .build());

        assertTrue(cache.valueOf(NS, "flag", 999L).isEmpty(), "missing version -> empty");
        assertTrue(cache.valueOf(NS, "missing", 3L).isEmpty(), "missing key -> empty");
        assertTrue(cache.valueOf("missing-ns", "flag", 3L).isEmpty(), "missing ns -> empty");
    }

    // ---- has_dynamic_resolution tri-state (ST5) ---------------------------
    //
    // proto3 `optional bool has_dynamic_resolution`: the cache must surface the
    // presence-aware tri-state so getConfig only fast-paths on an EXPLICIT false.
    //   - field set true   -> Boolean.TRUE
    //   - field set false  -> Boolean.FALSE
    //   - field unset      -> null (absent / old server)
    //   - missing key / ns -> null
    // The null-vs-FALSE distinction is load-bearing (mis-skip guard), so these
    // assertions use assertNull / assertSame on the boxed Boolean rather than a
    // truthiness check.

    @Test
    void hasDynamicResolution_explicitTrue_isBooleanTrue() {
        ConfigCache cache = new ConfigCache(new Metrics());
        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey("color").putVersions(7L, "blue").setHasDynamicResolution(true))
                .build());

        assertSame(Boolean.TRUE, cache.hasDynamicResolution(NS, "color"),
                "field set true must surface as Boolean.TRUE");
    }

    @Test
    void hasDynamicResolution_explicitFalse_isBooleanFalse() {
        ConfigCache cache = new ConfigCache(new Metrics());
        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey("color").putVersions(7L, "blue").setHasDynamicResolution(false))
                .build());

        assertSame(Boolean.FALSE, cache.hasDynamicResolution(NS, "color"),
                "field set false must surface as Boolean.FALSE (enables fast-path)");
    }

    @Test
    void hasDynamicResolution_fieldAbsent_isNull() {
        ConfigCache cache = new ConfigCache(new Metrics());
        // pbKey never calls setHasDynamicResolution -> proto3 optional absent.
        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey("color").putVersions(7L, "blue"))
                .build());

        assertNull(cache.hasDynamicResolution(NS, "color"),
                "absent field (old server) must surface as null, NEVER as FALSE");
    }

    @Test
    void hasDynamicResolution_missingKeyOrNs_isNull() {
        ConfigCache cache = new ConfigCache(new Metrics());
        // Empty cache: unknown ns -> null.
        assertNull(cache.hasDynamicResolution("nope", "color"),
                "unknown namespace must surface as null");

        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey("color").putVersions(7L, "blue").setHasDynamicResolution(true))
                .build());
        // Known ns, unknown key -> null.
        assertNull(cache.hasDynamicResolution(NS, "missing-key"),
                "known ns but unknown key must surface as null");
    }

    @Test
    void hasDynamicResolution_isReplacedOnNewerSnapshot() {
        ConfigCache cache = new ConfigCache(new Metrics());
        // First snapshot reports the key as dynamic (true).
        cache.applyProto(pbSnapshot(NS, 1, 1)
                .addKeys(pbKey("color").putVersions(7L, "blue").setHasDynamicResolution(true))
                .build());
        assertSame(Boolean.TRUE, cache.hasDynamicResolution(NS, "color"));

        // A newer snapshot flips it to pure full-rollout (false): the cache must
        // adopt the new flag, not retain the stale TRUE.
        cache.applyProto(pbSnapshot(NS, 2, 1)
                .addKeys(pbKey("color").putVersions(7L, "blue").setHasDynamicResolution(false))
                .build());
        assertSame(Boolean.FALSE, cache.hasDynamicResolution(NS, "color"),
                "a newer snapshot must replace the flag (true -> false)");
    }

    // ---- knownSeqs --------------------------------------------------------

    @Test
    void knownSeqs_missingNsReturnsZeroPair_andPreservesInputOrder() {
        ConfigCache cache = new ConfigCache(new Metrics());
        cache.applyProto(pbSnapshot("b", 4, 5)
                .addKeys(pbKey("k").putVersions(1L, "v")).build());

        // Order: a (absent), b (present), c (absent).
        List<String> input = List.of("a", "b", "c");
        Map<String, NamespaceSeqs> seqs = cache.knownSeqs(input);

        // Order preserved (LinkedHashMap).
        assertEquals(List.of("a", "b", "c"), List.copyOf(seqs.keySet()),
                "knownSeqs iteration order must follow the input list");

        // Missing ns -> {0, 0}.
        assertEquals(0L, seqs.get("a").getBusinessSnapshotSeq());
        assertEquals(0L, seqs.get("a").getExperimentSnapshotSeq());
        assertEquals(0L, seqs.get("c").getBusinessSnapshotSeq());
        assertEquals(0L, seqs.get("c").getExperimentSnapshotSeq());

        // Present ns reflects its cached seqs.
        assertEquals(4L, seqs.get("b").getBusinessSnapshotSeq());
        assertEquals(5L, seqs.get("b").getExperimentSnapshotSeq());
    }

    @Test
    void knownSeqs_emptyInputYieldsEmptyMap() {
        ConfigCache cache = new ConfigCache(new Metrics());
        assertTrue(cache.knownSeqs(List.of()).isEmpty());
    }

    // ---- metrics side-effects of applyProto -------------------------------

    @Test
    void replace_setsLocalCacheBytesToUtf8ByteCount_notCharCount() {
        Metrics metrics = new Metrics();
        ConfigCache cache = new ConfigCache(metrics);

        // Multi-byte UTF-8 content: key "café" + value "naïve—€" so byteSize
        // MUST diverge from char count.
        String key = "café";      // 'é' is 2 bytes in UTF-8
        String val = "naïve—€";   // 'ï' 2 bytes, '—' 3 bytes, '€' 3 bytes
        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey(key).putVersions(1L, val))
                .build());

        long expected = key.getBytes(UTF_8).length + val.getBytes(UTF_8).length;
        // Sanity: UTF-8 byte count strictly exceeds the char (code unit) count
        // for this multi-byte content, so a char-based impl would fail here.
        long charCount = key.length() + val.length();
        assertTrue(expected > charCount,
                "test fixture must contain multi-byte chars (bytes=" + expected
                        + " > chars=" + charCount + ")");

        assertEquals(expected, metrics.localCacheBytes(NS),
                "localCacheBytes must equal the UTF-8 byte count, not the char count");
    }

    @Test
    void replace_incrementsSeqChangeCountersOnlyForMovedAxis() {
        Metrics metrics = new Metrics();
        ConfigCache cache = new ConfigCache(metrics);

        // First write: business 0->1 only.
        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey("k").putVersions(1L, "v")).build());
        assertEquals(1L, metrics.businessSeqChangeTotal(NS));
        assertEquals(0L, metrics.experimentSeqChangeTotal(NS));

        // Second write: experiment 0->1 only.
        cache.applyProto(pbSnapshot(NS, 1, 1)
                .addKeys(pbKey("k").putVersions(2L, "v2")).build());
        assertEquals(1L, metrics.businessSeqChangeTotal(NS), "business unchanged");
        assertEquals(1L, metrics.experimentSeqChangeTotal(NS), "experiment now moved once");

        // Third write: both advance.
        cache.applyProto(pbSnapshot(NS, 2, 2)
                .addKeys(pbKey("k").putVersions(3L, "v3")).build());
        assertEquals(2L, metrics.businessSeqChangeTotal(NS));
        assertEquals(2L, metrics.experimentSeqChangeTotal(NS));

        // Stale write: no counter movement.
        cache.applyProto(pbSnapshot(NS, 2, 2)
                .addKeys(pbKey("k").putVersions(4L, "v4")).build());
        assertEquals(2L, metrics.businessSeqChangeTotal(NS));
        assertEquals(2L, metrics.experimentSeqChangeTotal(NS));
    }

    @Test
    void snapshotReferenceIsStableUntilReplaced() {
        ConfigCache cache = new ConfigCache(new Metrics());
        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey("k").putVersions(1L, "v")).build());

        // Fully-qualified: the bare name NamespaceSnapshot resolves to the proto
        // import (needed by pbSnapshot); cache.snapshot() returns the SDK type.
        io.github.lightspeedintelligence.abconfig.NamespaceSnapshot snap1 = cache.snapshot(NS).orElseThrow();
        // A stale apply does not swap the slot; reference identity is preserved.
        cache.applyProto(pbSnapshot(NS, 1, 0)
                .addKeys(pbKey("k").putVersions(2L, "v2")).build());
        assertSame(snap1, cache.snapshot(NS).orElseThrow(),
                "stale apply must not swap the cached snapshot reference");
    }
}
