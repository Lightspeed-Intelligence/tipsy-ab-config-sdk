package io.tipsy.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assertions.fail;

import io.tipsy.abconfig.proto.config.v1.KeyState;
import io.tipsy.abconfig.proto.config.v1.NamespaceSnapshot;
import java.util.ArrayList;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicReference;
import org.junit.jupiter.api.Test;

/**
 * Concurrency tests for {@link ConfigCache#applyProto} (review finding S1, paired
 * with the F1 atomic check-and-replace fix).
 *
 * <p>Background: in ST3 the periodic pull loop and the subscribe stream loop both
 * call {@code applyProto} for the same namespace concurrently. The Go SDK guards
 * this with a {@code -race} reader/writer test; Java has no {@code -race} switch,
 * so this suite leans on deterministic, assertable invariants instead:
 *
 * <ul>
 *   <li><b>writer-vs-writer</b>: regardless of interleaving the surviving
 *       snapshot must carry the highest submitted seq pair (strictly-advancing
 *       replacement), and the seq-change counters stay within
 *       {@code [1, totalApplyCalls]}.</li>
 *   <li><b>reader-vs-writer</b>: concurrent readers never throw and always see an
 *       internally self-consistent immutable snapshot (no torn reads, no NPE / no
 *       {@link java.util.ConcurrentModificationException}).</li>
 * </ul>
 *
 * <p>All structure is fixed (known seq set, fixed key / version / value encoding)
 * and iteration counts are bounded — no wall-clock sleeps — to avoid flakiness.
 */
final class ConfigCacheConcurrencyTest {

    private static final String NS = "checkout";
    private static final String KEY = "color";

    /**
     * Builds a protobuf {@link NamespaceSnapshot} for {@code (biz, exp)} with a
     * single key whose only version_id is {@code biz} mapping to the encoded
     * value {@code "v" + biz}. The (version_id == biz, value == "v"+biz) coupling
     * lets a reader verify it observed a coherent, untorn immutable snapshot.
     */
    private static NamespaceSnapshot snapshotPb(long biz, long exp) {
        return NamespaceSnapshot.newBuilder()
                .setNamespace(NS)
                .setBusinessSnapshotSeq(biz)
                .setExperimentSnapshotSeq(exp)
                .addKeys(KeyState.newBuilder()
                        .setKey(KEY)
                        .setFullReleaseVersion(biz)
                        .putVersions(biz, "v" + biz))
                .build();
    }

    // ---- writer-vs-writer: highest seq must win --------------------------

    @Test
    void writerVsWriter_highestSeqWins_andCountersAreBounded() throws Exception {
        final int threads = 8;
        final int repeats = 200;

        // The cache does WHOLE-snapshot replacement keyed on "either axis
        // strictly advances" (design 04 §整体替换, aligned with Go cache.go): the
        // survivor is always some submitted pair verbatim, never a per-axis max,
        // and one axis may legitimately regress when the other advances. So a
        // mixed-axis seq set has no single deterministic winner.
        //
        // To get a deterministic "highest pair wins" invariant that still
        // detects the F1 regression (a non-atomic get-then-put letting a LOW
        // pair overwrite a HIGH pair), use a JOINTLY-MONOTONIC (seq, seq) set:
        // {(1,1), (2,2), ..., (N,N)}. Submit them out of order / repeated /
        // equal across threads; under a correct atomic compute the surviving
        // pair must be exactly (N, N).
        final long n = 16; // the highest pair is (n, n)
        // A deterministic, shuffled-yet-known permutation of 1..n (includes a
        // repeat of the max and an equal-pair revisit to exercise no-advance).
        final long[] seqs = {1, 3, 2, 5, 4, 7, 6, 9, 8, 11, 10, 13, 12, 16, 15, 14};

        Metrics metrics = new Metrics();
        ConfigCache cache = new ConfigCache(metrics);

        ExecutorService pool = Executors.newFixedThreadPool(threads);
        CountDownLatch ready = new CountDownLatch(threads);
        CountDownLatch go = new CountDownLatch(1);
        AtomicReference<Throwable> failure = new AtomicReference<>();

        try {
            for (int t = 0; t < threads; t++) {
                final int seed = t;
                pool.submit(() -> {
                    ready.countDown();
                    try {
                        go.await();
                        for (int i = 0; i < repeats; i++) {
                            // Deterministic per-thread permutation: each thread
                            // walks the seq list from a different offset, so the
                            // max pair (n, n) is submitted many times interleaved
                            // with lower pairs.
                            int idx = (i + seed) % seqs.length;
                            long s = seqs[idx];
                            cache.applyProto(snapshotPb(s, s)); // jointly-monotonic (s, s)
                        }
                    } catch (Throwable th) {
                        failure.compareAndSet(null, th);
                    }
                });
            }

            assertTrue(ready.await(10, TimeUnit.SECONDS), "workers must reach the barrier");
            go.countDown(); // release all writers simultaneously
            pool.shutdown();
            assertTrue(pool.awaitTermination(30, TimeUnit.SECONDS), "writers must finish");
        } finally {
            pool.shutdownNow();
        }

        if (failure.get() != null) {
            fail("a writer thread threw", failure.get());
        }

        // Highest pair wins: with jointly-monotonic (s, s) pairs the surviving
        // snapshot must be exactly (n, n). A non-atomic get-then-put would let a
        // lower pair overwrite the highest one and fail this — exactly the F1
        // regression this test guards.
        // Fully-qualified SDK type: the bare name NamespaceSnapshot resolves to
        // the proto import used by snapshotPb().
        io.tipsy.abconfig.NamespaceSnapshot survivor = cache.snapshot(NS).orElseThrow();
        assertEquals(n, survivor.businessSnapshotSeq,
                "highest (n,n) pair must win the business axis regardless of interleaving");
        assertEquals(n, survivor.experimentSnapshotSeq,
                "highest (n,n) pair must win the experiment axis regardless of interleaving");
        assertEquals(survivor.businessSnapshotSeq, survivor.experimentSnapshotSeq,
                "survivor must be a submitted pair verbatim (both axes equal under (s,s) writes)");

        // The surviving snapshot's content matches its own seq (no torn state).
        assertEquals("v" + n, cache.valueOf(NS, KEY, n).orElseThrow(),
                "surviving snapshot content must be coherent with its seq");

        // Seq-change counters: at least one strict advance happened (0->1 on the
        // first write), and never more than the total number of apply calls.
        long totalApplyCalls = (long) threads * repeats;
        long bizMoves = metrics.businessSeqChangeTotal(NS);
        long expMoves = metrics.experimentSeqChangeTotal(NS);

        assertTrue(bizMoves >= 1, "at least one business strict-advance must be counted");
        assertTrue(expMoves >= 1, "at least one experiment strict-advance must be counted");
        assertTrue(bizMoves <= totalApplyCalls,
                "business seq-change count must not exceed total apply calls ("
                        + bizMoves + " > " + totalApplyCalls + ")");
        assertTrue(expMoves <= totalApplyCalls,
                "experiment seq-change count must not exceed total apply calls ("
                        + expMoves + " > " + totalApplyCalls + ")");

        // localCacheBytes gauge reflects the surviving snapshot's single key.
        long expectedBytes = KEY.getBytes(java.nio.charset.StandardCharsets.UTF_8).length
                + ("v" + n).getBytes(java.nio.charset.StandardCharsets.UTF_8).length;
        assertEquals(expectedBytes, metrics.localCacheBytes(NS),
                "localCacheBytes must reflect the surviving (n,n) snapshot");
    }

    // ---- reader-vs-writer: reads never throw, snapshots self-consistent ---

    @Test
    void readerVsWriter_readsNeverThrow_andSeeCoherentSnapshots() throws Exception {
        final int writerThreads = 4;
        final int readerThreads = 4;
        final int writerIterations = 500;
        final int readerIterations = 2000;

        Metrics metrics = new Metrics();
        ConfigCache cache = new ConfigCache(metrics);

        // Seed so readers always have a snapshot to read from the start.
        cache.applyProto(snapshotPb(1, 1));

        ExecutorService pool = Executors.newFixedThreadPool(writerThreads + readerThreads);
        CountDownLatch ready = new CountDownLatch(writerThreads + readerThreads);
        CountDownLatch go = new CountDownLatch(1);
        List<Throwable> failures = Collections.synchronizedList(new ArrayList<>());

        try {
            // Writers: monotonically increasing distinct seqs so the cache keeps
            // replacing; each snapshot stays internally coherent by construction.
            for (int w = 0; w < writerThreads; w++) {
                final int base = w;
                pool.submit(() -> {
                    ready.countDown();
                    try {
                        go.await();
                        for (int i = 1; i <= writerIterations; i++) {
                            long seq = (long) i * writerThreads + base; // distinct, growing
                            cache.applyProto(snapshotPb(seq, seq));
                        }
                    } catch (Throwable th) {
                        failures.add(th);
                    }
                });
            }

            // Readers: continuously read; verify every observed snapshot is
            // self-consistent (value at version == business seq is "v"+seq) and
            // that no read throws.
            for (int r = 0; r < readerThreads; r++) {
                pool.submit(() -> {
                    ready.countDown();
                    try {
                        go.await();
                        for (int i = 0; i < readerIterations; i++) {
                            Optional<io.tipsy.abconfig.NamespaceSnapshot> opt = cache.snapshot(NS);
                            if (opt.isPresent()) {
                                io.tipsy.abconfig.NamespaceSnapshot s = opt.get();
                                long biz = s.businessSnapshotSeq;
                                // The single key's only version_id is the biz seq,
                                // and its value is "v"+biz. A torn read would break
                                // this coupling.
                                Map<Long, String> versions = s.keys.get(KEY).versions;
                                String v = versions.get(biz);
                                if (v == null || !v.equals("v" + biz)) {
                                    throw new AssertionError(
                                            "incoherent snapshot: biz=" + biz + " value=" + v);
                                }
                                // Iterate the immutable maps to surface any
                                // ConcurrentModificationException if the impl ever
                                // exposed a mutable map.
                                int count = 0;
                                for (Map.Entry<Long, String> e : versions.entrySet()) {
                                    if (e.getValue() != null) {
                                        count++;
                                    }
                                }
                                if (count == 0) {
                                    throw new AssertionError("snapshot had no versions");
                                }
                            }
                            // Existence-typed accessors must also never throw and
                            // must agree with each other on a present snapshot.
                            cache.valueOf(NS, KEY, 1L);
                            cache.fullReleaseVersion(NS, KEY);
                        }
                    } catch (Throwable th) {
                        failures.add(th);
                    }
                });
            }

            assertTrue(ready.await(10, TimeUnit.SECONDS), "all workers must reach the barrier");
            go.countDown();
            pool.shutdown();
            assertTrue(pool.awaitTermination(60, TimeUnit.SECONDS),
                    "readers and writers must finish");
        } finally {
            pool.shutdownNow();
        }

        if (!failures.isEmpty()) {
            AssertionError agg = new AssertionError(
                    "reader/writer concurrency violation(s): " + failures.size());
            for (Throwable th : failures) {
                agg.addSuppressed(th);
            }
            throw agg;
        }

        // Sanity: after the run a snapshot still exists and is coherent.
        io.tipsy.abconfig.NamespaceSnapshot finalSnap = cache.snapshot(NS).orElseThrow();
        long biz = finalSnap.businessSnapshotSeq;
        assertEquals("v" + biz, finalSnap.keys.get(KEY).versions.get(biz),
                "final snapshot must remain internally coherent");
    }
}
