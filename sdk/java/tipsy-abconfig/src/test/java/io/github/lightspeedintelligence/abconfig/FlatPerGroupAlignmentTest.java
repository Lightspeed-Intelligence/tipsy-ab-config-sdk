package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.github.lightspeedintelligence.abconfig.AbtestTestSupport.NsCache;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ExperimentGroupResult;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ExperimentType;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GrayReleaseHit;
import java.util.Map;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * Alignment tests (design "actual-enrollment-log" Testing Plan 3a, category
 * "alignment"): for one logical scenario, the flat_kv fixture (what an OLD SDK
 * consumed verbatim as its key&rarr;versionId map) and the per-group fixture
 * (what the NEW SDK receives and merges locally) must yield the SAME
 * key&rarr;versionId map.
 *
 * <p>The flat-side expectation in every strict case is HAND-COMPUTED from the
 * platform's flat assembly rules (platform repo
 * {@code internal/abtest/compute/engine.go:329-385}, pinned at HEAD
 * {@code dd3cf76}; design §3 rule table) — NOT from an intuited
 * "whitelist &gt; experiment" slogan and NOT by running the SDK merge on the
 * flat data (that would be a tautology). Rules used: gray keys are written
 * first ({@code engine.go:334-337}); an experiment entry whose key is already
 * gray-owned is skipped ({@code engine.go:355-356} — gray wins
 * unconditionally, the strictly guaranteed cross-source semantic); between
 * experiment groups the platform is last-write-wins ({@code engine.go:380}),
 * which the SDK replicates but does NOT guarantee (user relaxation — the weak
 * case below asserts membership only).
 *
 * <p><b>Capability boundary (design r3-F4 — do not read the green checks as
 * proof beyond it)</b>: BOTH fixtures here are written by us and the flat
 * expectation is our hand computation of the platform rules. These tests
 * verify "SDK merge == our understanding of the platform"; by construction
 * they CANNOT detect a real server whose per-group output diverges from its
 * flat output. That capability lives in Testing Plan 3b (platform-side
 * evidence: source review + running the platform's own
 * {@code internal/abtest/compute} suite) and in the pre-release live test
 * (AC10).
 *
 * <p><b>Test convention</b>: any observation of second-call behaviour needs a
 * fresh {@code AbtestContext} (per-ns memoisation); each case here uses its
 * own harness + one fresh ctx anyway.
 */
final class FlatPerGroupAlignmentTest {

    private static final String NS = "checkout";

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

    /**
     * Serves {@code perGroup} to a fresh context's internal fetch and returns
     * the SDK-local merge output. A minimal cache snapshot is enough — the
     * merge itself never consults the config cache.
     */
    private static Map<String, Long> mergePerGroup(GetExperimentResultResponse perGroup) {
        try (AbtestTestSupport h = AbtestTestSupport.newBuilder()
                .namespaces(NS)
                .snapshot(NS, new NsCache(1, 1).key("anchor", 1L, Map.of(1L, "v")))
                .build()) {
            h.abtest.setFullResponse(NS, perGroup);
            AbtestContext ctx = h.client.newAbtestContext("u-align", Map.of());
            return ctx.resultFor(NS).keyVersions;
        }
    }

    // ------------------------------------------------------------------
    // STRICT equality cases (gate-level).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("alignment/strict: pure experiment — merge == flat fixture map")
    void pureExperimentAligns() {
        // Old-SDK flat fixture for "one experiment group hits a,b":
        // flat assembly walks the same hits and writes params one by one
        // (engine.go:340-385, no gray keys involved) => {a:101, b:102}.
        Map<String, Long> flatFixture = Map.of("a", 101L, "b", 102L);

        GetExperimentResultResponse perGroup = GetExperimentResultResponse.newBuilder()
                .addGroups(group("exp-1", "grp-1", Map.of("a", 101L, "b", 102L)))
                .build();

        assertEquals(flatFixture, mergePerGroup(perGroup),
                "per-group merge must equal the flat_kv the old SDK consumed");
    }

    @Test
    @DisplayName("alignment/strict: pure gray — merge == flat fixture map")
    void pureGrayAligns() {
        // Old-SDK flat fixture for "two gray releases hit disjoint keys":
        // flat assembly writes every gray key from computeGray's folded map
        // (engine.go:334-337) => {a:7, b:8}.
        Map<String, Long> flatFixture = Map.of("a", 7L, "b", 8L);

        GetExperimentResultResponse perGroup = GetExperimentResultResponse.newBuilder()
                .addGrayHits(gray(41L, Map.of("a", 7L)))
                .addGrayHits(gray(42L, Map.of("b", 8L)))
                .build();

        assertEquals(flatFixture, mergePerGroup(perGroup),
                "per-group merge must equal the flat_kv the old SDK consumed");
    }

    @Test
    @DisplayName("alignment/strict: gray + experiment overlap — gray wins, exactly as the flat path folds it")
    void grayExperimentOverlapAligns() {
        // Hand computation per engine.go for "gray(41) covers a=7; experiment
        // (exp-1) declares a=9 and b=102":
        //   1. gray keys first: flat[a]=7            (engine.go:334-337)
        //   2. experiment walk: key a is grayOwned -> SKIP (engine.go:355-356);
        //      key b not owned -> flat[b]=102        (engine.go:380)
        // => {a:7, b:102}. Candidate versionIds on the conflicting key DIFFER
        // (7 vs 9) per the platform's own fixture discipline
        // (conflictkey_test.go:79-80) so an inverted-priority mutation cannot
        // pass by coincidence.
        Map<String, Long> flatFixture = Map.of("a", 7L, "b", 102L);

        GetExperimentResultResponse perGroup = GetExperimentResultResponse.newBuilder()
                .addGrayHits(gray(41L, Map.of("a", 7L)))
                .addGroups(group("exp-1", "grp-1", Map.of("a", 9L, "b", 102L)))
                .build();

        assertEquals(flatFixture, mergePerGroup(perGroup),
                "cross-source overlap: the merge must reproduce the flat path's gray-wins fold");
    }

    @Test
    @DisplayName("alignment/strict: empty — merge == empty flat map (zero discriminating power, documented)")
    void emptyAligns() {
        // NOTE: this case has ZERO discriminating power for the merge (an
        // entirely broken merge also yields an empty map; design no-op form 1).
        // It documents the empty-wire alignment contract only and must not be
        // counted as merge coverage.
        assertEquals(Map.of(), mergePerGroup(GetExperimentResultResponse.getDefaultInstance()));
    }

    // ------------------------------------------------------------------
    // WEAK case (same-source conflict — user relaxation, NOT gate-level).
    // ------------------------------------------------------------------

    @Test
    @DisplayName("alignment/weak: multi-experiment conflict — winner is one of the candidates, key never lost")
    void multiExperimentConflictWeak() {
        // The platform's flat path resolves this last-write-wins to 202
        // (engine.go:380; conflictkey_test.go:81-82 pins 202 platform-side).
        // The SDK replicates that rule but per the user decision (2025-08-26)
        // provides NO cross-repo winner guarantee: 正常情况下不会发生同类型 key
        // 冲突，同类型 key 冲突是异常情况，此时平台 + SDK 只需保障至少返回可选值
        // 中的一个就算符合承诺。 So: membership + no key loss ONLY. Do NOT
        // tighten this to assertEquals(202L, ...) — that would pin a
        // deliberately unguaranteed behaviour (and contradict the mutation
        // manifest's "must survive" entries).
        GetExperimentResultResponse perGroup = GetExperimentResultResponse.newBuilder()
                .addGroups(group("exp-A", "grp-A", Map.of("k", 101L)))
                .addGroups(group("exp-B", "grp-B", Map.of("k", 202L)))
                .build();

        Map<String, Long> merged = mergePerGroup(perGroup);
        Long winner = merged.get("k");
        assertNotNull(winner, "the conflicting key must never be dropped (value loss is out of contract)");
        assertTrue(winner == 101L || winner == 202L,
                "winner must be one of the candidate versionIds, got " + winner);
        assertEquals(1, merged.size(), "no other keys may appear");
    }

    // NOT present by design: a multi-gray same-key alignment case. That wire
    // shape is structurally unreachable (platform topology.go:433-437 folds
    // multi-gray conflicts BEFORE assembleGrayHits buckets keys per release),
    // so there is no flat-side expectation to compute — the defensive guard
    // for the SDK loop lives in AbtestMergeTest.multiGrayConflictDefensiveSynthetic.
}
