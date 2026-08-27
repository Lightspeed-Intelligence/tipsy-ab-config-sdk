package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.github.lightspeedintelligence.abconfig.AbtestComputeResult.Attribution;
import io.github.lightspeedintelligence.abconfig.AbtestTestSupport.NsCache;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ExperimentGroupResult;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ExperimentType;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GrayReleaseHit;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ResultDisplayType;
import java.util.Map;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * SDK-local merge tests for {@code AbtestContext.mergeEachExperimentGroupResponse}
 * (design "actual-enrollment-log" §3, Testing Plan 1): the per-group response
 * shape ({@code groups[]} + {@code gray_hits[]}) merged into the flat
 * key&rarr;versionId map plus per-key attribution.
 *
 * <p><b>Test convention (design Testing Plan, r6)</b>: any case observing
 * "second and later call" behaviour (RPC counts, degrade deltas) uses a FRESH
 * {@code AbtestContext} — the ctx memoises per (request, ns)
 * ({@code AbtestContext.java} ensureFetch), so a reused ctx observes the cached
 * value from the second call on and the assertion passes vacuously.
 *
 * <p><b>What these tests do NOT prove</b> (do not read the green checks as
 * proof of these): they exercise the SDK merge against fixtures WE wrote; they
 * cannot detect a real server whose per-group output diverges from its flat
 * output (that risk is carried by the platform-source review (Goal 6), the
 * platform's own compute suite (Testing Plan 3b(ii)) and the pre-release live
 * test (AC10) — see {@code FlatPerGroupAlignmentTest} for the same boundary on
 * the alignment layer). Same-source conflict winner identity is deliberately
 * NOT asserted (user decision 2025-08-26: 正常情况下不会发生同类型 key 冲突，
 * 同类型 key 冲突是异常情况，此时平台 + SDK 只需保障至少返回可选值中的一个就算
 * 符合承诺).
 */
final class AbtestMergeTest {

    private static final String NS = "checkout";

    // ------------------------------------------------------------------
    // Response-building helpers (per-group shape).
    // ------------------------------------------------------------------

    private static ExperimentGroupResult group(String expId, String grpId, Map<String, Long> pv) {
        return ExperimentGroupResult.newBuilder()
                .setExperimentId(expId)
                .setGroupId(grpId)
                .setExperimentType(ExperimentType.EXPERIMENT_TYPE_CONFIG_VERSION)
                .putAllParamsVersions(pv)
                .build();
    }

    private static GrayReleaseHit gray(long releaseId, Map<String, Long> kv) {
        return GrayReleaseHit.newBuilder()
                .setReleaseId(releaseId)
                .putAllKeyVersions(kv)
                .build();
    }

    /** Fetches the merged per-ns result through a fresh context (one RPC). */
    private static AbtestComputeResult mergedResult(AbtestTestSupport h) {
        AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
        return ctx.resultFor(NS);
    }

    // ------------------------------------------------------------------
    // Cross-source conflict: gray beats experiment — STRICT (gate-level).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("cross-source conflict: gray wins unconditionally over experiment (strict)")
    void crossSourceConflictGrayBeatsExperiment() {
        // Fixture discipline (platform conflictkey_test.go:79-80): the two
        // candidates MUST carry different versionIds ("Exact winner, not one of
        // {101,202}") — with equal values the experiment branch would satisfy
        // the assertion with the same result and the case would be a no-op.
        GetExperimentResultResponse resp = GetExperimentResultResponse.newBuilder()
                .addGrayHits(gray(41L, Map.of("color", 7L)))
                .addGroups(group("exp-1", "grp-1", Map.of("color", 9L, "banner", 12L)))
                .build();
        NsCache cache = new NsCache(2, 2)
                .key("color", 3L, Map.of(3L, "full", 7L, "grayval", 9L, "expval"))
                .key("banner", 5L, Map.of(5L, "bfull", 12L, "bexp"));
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, cache)
                .build()) {
            h.abtest.setFullResponse(NS, resp);

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            AbtestComputeResult r = ctx.resultFor(NS);

            // Whole-map equality: gray owns "color" (7, NOT the experiment's 9);
            // the experiment still owns the non-conflicting "banner".
            assertEquals(Map.of("color", 7L, "banner", 12L), r.keyVersions,
                    "gray must win the conflicting key; experiment keeps the rest");

            Attribution color = r.attribution.get("color");
            assertNotNull(color, "every merged key must carry an attribution entry");
            assertEquals(Attribution.Source.GRAY_WHITELIST, color.source);
            assertEquals(41L, color.releaseId);

            Attribution banner = r.attribution.get("banner");
            assertNotNull(banner);
            assertEquals(Attribution.Source.EXPERIMENT, banner.source);
            assertEquals("exp-1", banner.experimentId);
            assertEquals("grp-1", banner.groupId);

            // End-to-end: the merged map actually feeds getConfig resolution.
            assertEquals("grayval", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "getConfig must resolve the gray-owned versionId's value");
            assertEquals("bexp", h.client.getConfig(ctx, NS, "banner", "DEF"));
        }
    }

    // ------------------------------------------------------------------
    // Same-source conflicts: WEAK assertions only (user relaxation).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("same-source experiment conflict: winner is one of the candidates, key never dropped (weak)")
    void sameSourceExperimentConflictWeak() {
        // User contract (verbatim, 2025-08-26): 正常情况下不会发生同类型 key 冲突，
        // 同类型 key 冲突是异常情况，此时平台 + SDK 只需保障至少返回可选值中的一个
        // 就算符合承诺。 We therefore assert ONLY membership + no key loss — NOT
        // which candidate wins and NOT cross-run stability (both deliberately
        // out of contract; do not "strengthen" this into pinning the platform's
        // last-write-wins).
        GetExperimentResultResponse resp = GetExperimentResultResponse.newBuilder()
                .addGroups(group("exp-A", "grp-A", Map.of("k", 101L)))
                .addGroups(group("exp-B", "grp-B", Map.of("k", 202L)))
                .build();
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("k", 3L, Map.of(3L, "full", 101L, "a", 202L, "b")))
                .build()) {
            h.abtest.setFullResponse(NS, resp);

            AbtestComputeResult r = mergedResult(h);

            Long winner = r.keyVersions.get("k");
            assertNotNull(winner, "a same-source conflict must NEVER drop the key (value loss)");
            assertTrue(winner == 101L || winner == 202L,
                    "winner must be one of the candidate versionIds, got " + winner);

            // Attribution must be consistent with whichever candidate won.
            Attribution a = r.attribution.get("k");
            assertNotNull(a, "the winning key must carry an attribution entry");
            assertEquals(Attribution.Source.EXPERIMENT, a.source);
            if (winner == 101L) {
                assertEquals("exp-A", a.experimentId);
                assertEquals("grp-A", a.groupId);
            } else {
                assertEquals("exp-B", a.experimentId);
                assertEquals("grp-B", a.groupId);
            }
        }
    }

    @Test
    @DisplayName("multi-gray same-key conflict (synthetic, wire-unreachable): no crash, key never dropped (defensive)")
    void multiGrayConflictDefensiveSynthetic() {
        // This response shape is structurally UNREACHABLE on the real wire: the
        // platform's computeGray already folds multi-gray conflicts before
        // assembleGrayHits buckets keys per release (platform topology.go:433-437,
        // :499-514) — the same key name never appears in two gray_hits entries.
        // The case is a purely defensive guard on the SDK's merge loop: it is
        // NOT wire-level equivalence evidence and has NO flat-side expected
        // value to compare against (design §3 / Testing Plan 3a excludes it).
        GetExperimentResultResponse resp = GetExperimentResultResponse.newBuilder()
                .addGrayHits(gray(1L, Map.of("k", 11L)))
                .addGrayHits(gray(2L, Map.of("k", 22L)))
                .build();
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("k", 3L, Map.of(3L, "full", 11L, "a", 22L, "b")))
                .build()) {
            h.abtest.setFullResponse(NS, resp);

            AbtestComputeResult r = mergedResult(h);

            Long winner = r.keyVersions.get("k");
            assertNotNull(winner, "a multi-gray conflict must never drop the key");
            assertTrue(winner == 11L || winner == 22L,
                    "winner must be one of the candidate versionIds, got " + winner);
            Attribution a = r.attribution.get("k");
            assertNotNull(a);
            assertEquals(Attribution.Source.GRAY_WHITELIST, a.source);
        }
    }

    // ------------------------------------------------------------------
    // Empty response (documented: zero discriminating power for the merge).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("empty per-group response: empty merge, getConfig falls through to full release")
    void emptyResponseYieldsEmptyMerge() {
        // NOTE (design Testing Plan, no-op form 1): this case has ZERO
        // discriminating power for the merge mechanism — an entirely broken
        // merge also produces an empty map here. It documents the empty-wire
        // contract (no flat fallback, §7) and must NOT be counted as merge
        // coverage in the delivery report.
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("color", 7L, Map.of(7L, "blue")))
                .build()) {
            h.abtest.setFullResponse(NS, GetExperimentResultResponse.getDefaultInstance());

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            AbtestComputeResult r = ctx.resultFor(NS);
            assertTrue(r.keyVersions.isEmpty(), "empty groups+gray_hits => empty merge");
            assertEquals("blue", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "no abtest hits => full release");
        }
    }

    // ------------------------------------------------------------------
    // F3: value/attribution decoupling — the core invariant.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("F3: empty-id group and release_id=0 gray hit still WRITE the versionId; only attribution degrades")
    void f3ValueAttributionDecoupling() {
        // Fixture constraint (design Testing Plan): the empty-id group is PAIRED
        // with an attributed group. Without the pairing, reason=abtest_unattributed
        // would hold vacuously in a merge that never writes attribution at all
        // (no-op form 1). The discriminating assertion is that the VALUES are
        // still written (a "skip empty-id groups" mutation loses flagA's
        // versionId and must turn this red).
        GetExperimentResultResponse resp = GetExperimentResultResponse.newBuilder()
                .addGrayHits(gray(0L, Map.of("flagC", 7L)))              // release_id=0
                .addGroups(group("", "", Map.of("flagA", 5L)))            // empty ids
                .addGroups(group("exp-1", "grp-1", Map.of("flagB", 6L))) // attributed pair
                .build();
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2)
                        .key("flagA", 1L, Map.of(1L, "afull", 5L, "aab"))
                        .key("flagB", 2L, Map.of(2L, "bfull", 6L, "bab"))
                        .key("flagC", 3L, Map.of(3L, "cfull", 7L, "cab")))
                .build()) {
            h.abtest.setFullResponse(NS, resp);

            AbtestComputeResult r = mergedResult(h);

            // Values written for ALL keys regardless of attribution validity.
            assertEquals(Map.of("flagA", 5L, "flagB", 6L, "flagC", 7L), r.keyVersions,
                    "attribution loss must NEVER change or drop the resolved versionId (F3)");

            assertEquals(Attribution.Source.UNATTRIBUTED, r.attribution.get("flagA").source,
                    "empty experiment_id/group_id => unattributed");
            assertEquals(Attribution.Source.UNATTRIBUTED, r.attribution.get("flagC").source,
                    "release_id=0 => unattributed");
            Attribution b = r.attribution.get("flagB");
            assertEquals(Attribution.Source.EXPERIMENT, b.source,
                    "the paired attributed group proves the attribution track is live");
            assertEquals("exp-1", b.experimentId);
            assertEquals("grp-1", b.groupId);
        }
    }

    // ------------------------------------------------------------------
    // Type filter: only CONFIG_VERSION groups feed the merge.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("non-CONFIG_VERSION groups are filtered out (fail-closed, deliberate F3 exception)")
    void nonConfigVersionGroupsFiltered() {
        // The type filter is the ONE deliberate exception to F3 (design §3):
        // it replicates the platform flat path's matchesType filter, and the
        // field's population is guarded platform-side (probe M10).
        GetExperimentResultResponse resp = GetExperimentResultResponse.newBuilder()
                .addGroups(ExperimentGroupResult.newBuilder()
                        .setExperimentId("exp-cp").setGroupId("grp-cp")
                        .setExperimentType(ExperimentType.EXPERIMENT_TYPE_CUSTOM_PARAMS)
                        .putParamsVersions("other", 9L))
                .addGroups(group("exp-1", "grp-1", Map.of("k", 5L)))
                .build();
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("k", 3L, Map.of(3L, "full", 5L, "ab")))
                .build()) {
            h.abtest.setFullResponse(NS, resp);

            AbtestComputeResult r = mergedResult(h);
            assertEquals(Map.of("k", 5L), r.keyVersions,
                    "a CUSTOM_PARAMS group must not contribute keys to the config merge");
        }
    }

    // ------------------------------------------------------------------
    // Internal fetch request shape (the core switch under test).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("internal per-ns fetch requests display_type=EACH_EXPERIMENT_GROUP + experiment_type=CONFIG_VERSION")
    void internalFetchRequestShape() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("color", 7L, Map.of(7L, "blue", 9L, "gold")))
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            AbtestContext ctx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("gold", h.client.getConfig(ctx, NS, "color", "DEF"),
                    "the per-group served steering must resolve (proves the dual-shape fake is live)");

            GetExperimentResultRequest seen = h.abtest.requests.peek();
            assertNotNull(seen, "the internal fetch must have reached the fake server");
            assertEquals(ResultDisplayType.RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP,
                    seen.getDisplayType(),
                    "internal fetch must request the per-group shape (design §1)");
            assertEquals(ExperimentType.EXPERIMENT_TYPE_CONFIG_VERSION,
                    seen.getExperimentType(),
                    "internal fetch keeps experiment_type=CONFIG_VERSION");
        }
    }

    // ------------------------------------------------------------------
    // Per-ns RPC failure degrade (F8) — DELTA assertion on the fallback counter.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("RPC failure degrades to full release; fallback counter delta is exactly +1 (F8)")
    void rpcFailureDegradeCounterDelta() {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(2, 2).key("color", 7L, Map.of(7L, "blue", 9L, "gold")))
                .abtestConfigFlatKv(NS, Map.of("color", 9L))
                .build()) {

            // Phase 1: a clean hit on its own ctx (no fallback contribution).
            AbtestContext okCtx = h.client.newAbtestContext("u-1", Map.of());
            assertEquals("gold", h.client.getConfig(okCtx, NS, "color", "DEF"));

            // Phase 2: fail the ns and observe with a FRESH ctx (the memoised
            // okCtx future would otherwise serve the cached success — see the
            // file-header convention). DELTA assertion, not an absolute total:
            // an absolute total can be "right" for the wrong reason when other
            // paths also bump the counter (design: no-op form 8).
            long before = h.client.metrics().abtestFallbackTotal(NS);
            h.abtest.failFor(NS);
            AbtestContext failCtx = h.client.newAbtestContext("u-2", Map.of());
            assertEquals("blue", h.client.getConfig(failCtx, NS, "color", "DEF"),
                    "RPC failure must degrade to the full-release value");
            long after = h.client.metrics().abtestFallbackTotal(NS);
            assertEquals(before + 1, after,
                    "exactly one fallback increment for the single failed per-ns fetch");
        }
    }
}
