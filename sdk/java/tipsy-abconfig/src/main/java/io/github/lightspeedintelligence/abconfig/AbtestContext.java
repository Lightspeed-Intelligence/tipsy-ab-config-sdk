package io.github.lightspeedintelligence.abconfig;

import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ExperimentGroupResult;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GrayReleaseHit;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.Value;
import java.time.Duration;
import java.util.HashMap;
import java.util.HashSet;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.ExecutionException;
import org.slf4j.Logger;

/**
 * The per-request handle the SDK uses to memoise abtest
 * {@code GetExperimentResult} results per namespace across one request link.
 * Construct one per inbound HTTP / RPC request via the
 * {@code TipsyAbConfigClient.newAbtestContext*} factories (or
 * {@link TipsyAbConfigClient#emptyAbtestContext()} for "no user context" paths),
 * pass it through to every {@link TipsyAbConfigClient#getConfig} call within the
 * request, then let it go out of scope at request end.
 *
 * <p>Mirrors the Go SDK's {@code AbtestContext}. Construction is pure-create: it
 * issues NO {@code GetExperimentResult} RPC. Every namespace is fetched lazily
 * on first dynamic {@code getConfig} for that ns and memoised into
 * {@code results} so the whole request link issues AT MOST ONE
 * {@code GetExperimentResult} RPC per namespace. Callers that want to warm a
 * namespace ahead of {@code getConfig} can opt in via
 * {@link #prefetchConfigVersionFlatKvForNamespace(String)} (non-blocking).
 *
 * <p>Safe for concurrent use by all threads participating in the same request:
 * the per-ns lazy fetch deduplicates concurrent first-access via a shared
 * {@link CompletableFuture} (exactly one RPC, the rest wait on the same future).
 * Per the F5 invariant (design 05) the lazy {@code resultFor} path and the
 * explicit prefetch API SHARE {@link #ensureFetch(String)} (and through it
 * {@link #fetchConfigVersionFlatKvForNamespace(String)}), which swallows every
 * RPC error into {@link AbtestComputeResult#EMPTY_RESULT}; therefore every
 * future in {@code results} only ever completes normally with some result (a
 * successful value or {@code EMPTY_RESULT}) and never completes exceptionally.
 */
public final class AbtestContext {

    private final String userId;
    private final Map<String, Object> attrs;

    /**
     * The {@link TipsyAbConfigClient} that issued this context. Bound to one
     * client because the cache lookup in {@code getConfig} must use the same
     * per-process cache that issued the {@code GetExperimentResult} call. May be
     * {@code null} only in degenerate test constructions; treated like an empty
     * context.
     */
    private final TipsyAbConfigClient owner;

    /**
     * The per-request trace id propagated to every {@code GetExperimentResult}
     * RPC issued from this context (lazy {@code resultFor} + explicit prefetch).
     * Always non-empty post-construction.
     */
    private final String traceId;

    /**
     * Marks an identity-less / mock context: {@link #resultFor} short-circuits
     * every not-yet-resolved ns to {@link AbtestComputeResult#EMPTY_RESULT}
     * without issuing any RPC.
     */
    private final boolean empty;

    /**
     * Per-ns memoised compute futures. Guarded by {@code synchronized(this)} on
     * every read/insert (NOT {@code computeIfAbsent}) to match the Go {@code mu}
     * critical section exactly and to avoid submitting executor tasks inside a
     * mapping function. Each future, once present, completes normally with some
     * {@link AbtestComputeResult} (F5).
     */
    private final Map<String, CompletableFuture<AbtestComputeResult>> results;

    AbtestContext(
            String userId,
            Map<String, Object> attrs,
            TipsyAbConfigClient owner,
            String traceId,
            boolean empty,
            Map<String, CompletableFuture<AbtestComputeResult>> results) {
        this.userId = userId == null ? "" : userId;
        this.attrs = attrs;
        this.owner = owner;
        this.traceId = traceId;
        this.empty = empty;
        this.results = results;
    }

    // ------------------------------------------------------------------
    // Public read accessors (mirror Go UserID()/UserInfo()/TraceID()).
    // ------------------------------------------------------------------

    /** The user id this context was constructed with (never {@code null}). */
    public String userId() {
        return userId;
    }

    /**
     * The full user identity (uid + attrs) this context was constructed with.
     * The returned {@link UserInfo} exposes a read-only view of the attrs map.
     */
    public UserInfo userInfo() {
        return new UserInfo(userId, attrs);
    }

    /**
     * The per-request trace id propagated to every {@code GetExperimentResult}
     * RPC issued from this context. Always non-empty post-construction.
     */
    public String traceId() {
        return traceId;
    }

    // ------------------------------------------------------------------
    // Memoised per-ns result (lazy + concurrency dedup).
    // ------------------------------------------------------------------

    /**
     * Ensures {@code ns} is being fetched (or has been resolved) exactly once
     * within this request link and returns the memoised future. Idempotent: a
     * second call for the same ns returns the existing future without spawning a
     * new fetch. This is the single primitive SHARED by the lazy
     * {@link #resultFor(String)} wait path and the explicit
     * {@link #prefetchConfigVersionFlatKvForNamespace(String)} API; centralising
     * the slot-creation critical section here keeps the at-most-once /
     * concurrency-dedup invariant in one place.
     *
     * <p>Owns the {@code synchronized(this)} critical section: under the lock the
     * per-ns future is double-checked; the first caller creates it (a completed
     * {@link AbtestComputeResult#EMPTY_RESULT} future for an empty/owner-null, a
     * no-user uid ({@code ""} / {@code "0"}), or an unsubscribed ns — no RPC —
     * otherwise an executor-backed future running
     * {@link #fetchConfigVersionFlatKvForNamespace(String)}), while every racing
     * caller finds and reuses the existing future. Net effect: AT MOST ONE
     * {@code GetExperimentResult} RPC per ns per request link. The returned
     * future never completes exceptionally (F5).
     *
     * <p>The pre-seeded results lookup precedes the short-circuit conditions, so a
     * {@code mockAbtestContext(uid="", …)} pre-resolved namespace still wins over
     * the no-user-uid short-circuit.
     */
    CompletableFuture<AbtestComputeResult> ensureFetch(String ns) {
        synchronized (this) {
            CompletableFuture<AbtestComputeResult> future = results.get(ns);
            if (future == null) {
                if (empty || owner == null) {
                    // Identity-less / mock ctx: resolve to empty without an RPC.
                    future = CompletableFuture.completedFuture(AbtestComputeResult.EMPTY_RESULT);
                } else if (isNoUserUid(userId)) {
                    // No real user identity (uid "" or "0"): the abtest bucketing /
                    // whitelist logic is meaningless, so skip the RPC and resolve to
                    // empty (caller falls through to full-release / default). Split
                    // from the empty/owner-null branch so the DEBUG log below never
                    // dereferences a null owner.
                    owner.logger().debug(
                            "tipsyabconfig: skip abtest: no-user uid (ns={}, uid={}, trace_id={})",
                            ns, userId, traceId);
                    future = CompletableFuture.completedFuture(AbtestComputeResult.EMPTY_RESULT);
                } else if (!owner.isSubscribed(ns)) {
                    // Unsubscribed ns: no local cache to resolve against, so
                    // degrade to empty without an RPC. Dynamic getConfig rejects
                    // unsubscribed ns earlier via resolveNamespace; this guards
                    // the low-level path.
                    future = CompletableFuture.completedFuture(AbtestComputeResult.EMPTY_RESULT);
                } else {
                    // Lazy fetch on the client's abtest executor via
                    // fetchConfigVersionFlatKvForNamespace so the future never
                    // completes exceptionally (F5).
                    future = submitFetch(ns);
                }
                results.put(ns, future);
            }
            return future;
        }
    }

    /**
     * Returns the memoised abtest result for {@code ns} within this request
     * link, fetching it asynchronously exactly once on first access (design
     * 05). Delegates the at-most-once slot creation to {@link #ensureFetch} and
     * then blocks on the returned future for the resolved value.
     *
     * <p>The future never completes exceptionally (F5): a per-ns RPC failure is
     * degraded inside {@code fetchConfigVersionFlatKvForNamespace} to
     * {@link AbtestComputeResult#EMPTY_RESULT}. The only exception this method
     * can surface is a thread interrupt while waiting; per design the
     * {@code getConfig} path must not throw a business exception for abtest, so
     * the interrupt restores the interrupt flag and degrades to
     * {@code EMPTY_RESULT}.
     */
    AbtestComputeResult resultFor(String ns) {
        CompletableFuture<AbtestComputeResult> future = ensureFetch(ns);
        try {
            AbtestComputeResult r = future.get();
            return r == null ? AbtestComputeResult.EMPTY_RESULT : r;
        } catch (InterruptedException ie) {
            // Restore the interrupt flag and degrade to empty: getConfig must
            // not throw a business exception because of an abtest wait.
            Thread.currentThread().interrupt();
            return AbtestComputeResult.EMPTY_RESULT;
        } catch (ExecutionException ee) {
            // Defensive: fetchConfigVersionFlatKvForNamespace swallows all RPC
            // errors, so the future should never complete exceptionally (F5).
            // Treat any unexpected exceptional completion as a silent degrade
            // rather than propagating it to the getConfig caller.
            if (owner != null) {
                owner.metricsInternal().abtestFallback.inc(ns);
                owner.logger().warn(
                        "tipsyabconfig: unexpected abtest compute failure; falling back to full release"
                                + " (ns={}, trace_id={})",
                        ns, traceId, ee.getCause());
            }
            return AbtestComputeResult.EMPTY_RESULT;
        }
    }

    /**
     * Explicit, opt-in prefetch (warm-up) of the per-namespace abtest result
     * for {@code ns} within this request link. The method name keeps its
     * historical "FlatKv" wording (API compatibility): the OUTPUT consumed by
     * {@code getConfig} is still the flat key&rarr;versionId map, but the SDK
     * internally requests the {@code EACH_EXPERIMENT_GROUP} display type and
     * merges the per-group response locally (preserving per-key attribution
     * for the hit log). Non-blocking: it triggers the at-most-once fetch via
     * {@link #ensureFetch} and returns immediately without awaiting the
     * result, so a subsequent {@link TipsyAbConfigClient#getConfig} for the
     * same ns reuses the warmed future instead of paying the RPC latency
     * inline.
     *
     * <p>Idempotent and at-most-once: calling this more than once for the same
     * ns (or prefetching then {@code getConfig}-ing) issues AT MOST ONE
     * {@code GetExperimentResult} RPC. An empty / mock context, a no-user uid
     * ({@code ""} / {@code "0"}), or an unsubscribed ns short-circuits inside
     * {@code ensureFetch} and issues NO RPC.
     *
     * <p>Construction itself never prefetches; this is the only way to warm a
     * namespace ahead of first use.
     */
    public void prefetchConfigVersionFlatKvForNamespace(String ns) {
        // Trigger the shared at-most-once primitive and discard the future:
        // prefetch never blocks on the result.
        ensureFetch(ns);
    }

    /**
     * Submits an async fetch of {@code ns} onto the owner's abtest executor.
     * Called by {@link #ensureFetch} for the live-fetch branch (shared by both
     * the lazy {@link #resultFor} wait path and the explicit prefetch API),
     * guaranteeing they share one future mechanism and the never-exceptional
     * contract.
     */
    CompletableFuture<AbtestComputeResult> submitFetch(String ns) {
        CompletableFuture<AbtestComputeResult> f = new CompletableFuture<>();
        owner.abtestExecutor().submit(() -> {
            // fetchConfigVersionFlatKvForNamespace never throws (F5): it degrades
            // to EMPTY_RESULT internally. complete(...) is therefore always with
            // a non-null result. The try/catch is a last-resort guard so a stray
            // RuntimeException can never leave the future exceptional.
            try {
                f.complete(fetchConfigVersionFlatKvForNamespace(ns));
            } catch (Throwable t) {
                if (owner != null) {
                    recordFetchFailure(ns, t, "abtest compute task threw");
                }
                f.complete(AbtestComputeResult.EMPTY_RESULT);
            }
        });
        return f;
    }

    /**
     * Wraps {@code AbtestService.GetExperimentResult} with the per-call timeout
     * for the shape the dynamic {@code getConfig} fast path consumes: the
     * experiment type and display type are hardwired to {@code CONFIG_VERSION}
     * / {@code EACH_EXPERIMENT_GROUP}, and the per-group response
     * ({@code groups[]} + {@code gray_hits[]}) is merged SDK-locally via
     * {@link #mergeEachExperimentGroupResponse} into the flat key&rarr;version
     * map (plus per-key attribution for the hit log). The method name keeps its
     * historical "FlatKv" wording because the OUTPUT is still the flat
     * key&rarr;versionId map; only the wire shape changed. This is NOT the
     * general-purpose {@link TipsyAbConfigClient#getExperimentResult} API. On
     * ANY error (including a missing abtest connection) it returns
     * {@link AbtestComputeResult#EMPTY_RESULT} and bumps the per-ns fallback
     * counter so the caller can monitor degraded mode. NEVER throws (F5): the
     * lazy fetch and explicit prefetch paths both rely on this.
     */
    AbtestComputeResult fetchConfigVersionFlatKvForNamespace(String ns) {
        AbtestTransport transport = owner.abtestTransport();
        if (transport == null) {
            owner.metricsInternal().abtestFallback.inc(ns);
            return AbtestComputeResult.EMPTY_RESULT;
        }
        GetExperimentResultRequest req = GetExperimentResultRequest.newBuilder()
                .setNamespace(ns)
                .setUserId(userId)
                .putAllUserAttrs(encodeUserAttrs(attrs, owner.logger()))
                .setExperimentType(io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ExperimentType.EXPERIMENT_TYPE_CONFIG_VERSION)
                .setDisplayType(io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ResultDisplayType.RESULT_DISPLAY_TYPE_EACH_EXPERIMENT_GROUP)
                .setTraceId(traceId)
                .build();
        long __start = System.nanoTime();
        try {
            GetExperimentResultResponse resp =
                    transport.getExperimentResult(req, owner.abtestTimeout());
            double durMs = (System.nanoTime() - __start) / 1_000_000.0;
            owner.logger().debug("tipsyabconfig: GetExperimentResult rpc (ns={}, trace_id={}, duration_ms={})", ns, traceId, durMs);
            return mergeEachExperimentGroupResponse(resp, ns);
        } catch (Exception e) {
            recordFetchFailure(ns, e, "AbtestService.GetExperimentResult failed");
            double durMs = (System.nanoTime() - __start) / 1_000_000.0;
            owner.logger().debug("tipsyabconfig: GetExperimentResult rpc failed (ns={}, trace_id={}, duration_ms={})", ns, traceId, durMs, e);
            return AbtestComputeResult.EMPTY_RESULT;
        }
    }

    /**
     * Records one failed per-ns abtest fetch on the owner's observability
     * surface (#15). A caller-context cancellation (gRPC {@code CANCELLED} —
     * SDK {@code close()}'s executor {@code shutdownNow()}, upstream client
     * disconnect / deadline propagation, handler already returned) is expected
     * termination, NOT a fault: it logs at INFO and bumps the dedicated
     * {@code abtestCanceled} counter. Anything else keeps the historical WARN +
     * {@code abtestFallback} accounting. Either way the caller degrades to
     * {@link AbtestComputeResult#EMPTY_RESULT} (getConfig still resolves to the
     * full release / default value) — only the observability differs.
     *
     * <p>Callers must ensure {@code owner != null}.
     *
     * @param failureClause the leading clause of the non-cancel WARN line
     *        (e.g. {@code "AbtestService.GetExperimentResult failed"}), kept as
     *        a parameter so both catch sites preserve their historical wording.
     */
    private void recordFetchFailure(String ns, Throwable t, String failureClause) {
        if (TipsyAbConfigClient.isCancelled(t)) {
            owner.metricsInternal().abtestCanceled.inc(ns);
            owner.logger().info(
                    "tipsyabconfig: abtest fetch canceled by caller context; falling back to full release"
                            + " (ns={}, trace_id={}, err={})",
                    ns, traceId, String.valueOf(t));
        } else {
            owner.metricsInternal().abtestFallback.inc(ns);
            owner.logger().warn(
                    "tipsyabconfig: {}; falling back to full release (ns={}, trace_id={})",
                    failureClause, ns, traceId, t);
        }
    }

    /**
     * SDK-local merge of an {@code EACH_EXPERIMENT_GROUP} response into the
     * flat key&rarr;versionId map plus per-key attribution. This is a
     * line-by-line replica of the platform's flat_kv assembly
     * ({@code internal/abtest/compute/engine.go:329-385} in the platform repo,
     * pinned at HEAD {@code dd3cf76}), because "the merged result equals what
     * the old FLAT_KV wire returned" is the sole equivalence criterion:
     * <ol>
     *   <li>{@code gray_hits} first, in wire order (the server emits them
     *       sorted by release_id ascending), first-writer-wins. A cross-entry
     *       duplicate key is structurally unreachable on the wire (the
     *       platform's computeGray already folds multi-gray conflicts), so the
     *       skip here is purely defensive.</li>
     *   <li>{@code groups} next, in wire order, {@code CONFIG_VERSION} entries
     *       only. A key already owned by a gray hit is SKIPPED — gray wins
     *       unconditionally over experiment (replicates
     *       {@code engine.go:355-356}; this cross-source rule is the strictly
     *       guaranteed product semantic). Between experiment groups the last
     *       write wins (replicates {@code engine.go:380}).</li>
     * </ol>
     *
     * <p><b>核心不变式（F3）</b>：keyVersions 的写入永不依赖归因字段是否有效。
     * 空 experiment_id/group_id 的组、release_id=0 的 gray hit 仍写入 versionId，
     * 仅将 attribution 记为 {@code UNATTRIBUTED}（日志 reason 降级，值不变）。
     * 唯一的刻意例外：{@code experiment_type != CONFIG_VERSION} 的组按 fail-closed
     * 过滤跳过——复刻平台 flat 路径的同一过滤（matchesType），且平台侧对该字段填充
     * 有可变红守护（探针 M10）。
     *
     * <p><b>同类型冲突契约（用户决策 2025-08-26，逐字）</b>：
     * 正常情况下不会发生同类型 key 冲突，同类型 key 冲突是异常情况，此时平台 + SDK
     * 只需保障至少返回可选值中的一个就算符合承诺。
     *
     * <p><b>实现约束（防 flapping）</b>：合并必须顺序遍历 {@code groups} /
     * {@code gray_hits} 两个 proto repeated 字段（有序数组），绝不可把候选中转进
     * map 再按 map 迭代序合并——顺序遍历使同一响应的重复合并结果恒定。（每个元素
     * 内部的 {@code key_versions} / {@code params_versions} map 键唯一，迭代序不
     * 影响结果。）跨仓备注（R3）：平台侧 wire {@code groups} 顺序等于其 flat 路径
     * 的遍历顺序（assembleGroups 不排序），但平台无测试钉住该性质；按上述契约放宽
     * 后 SDK 不依赖它——同类型冲突返回任一候选值皆符合承诺。
     */
    AbtestComputeResult mergeEachExperimentGroupResponse(
            GetExperimentResultResponse resp, String ns) {
        Map<String, Long> keyVersions = new HashMap<>();
        Map<String, AbtestComputeResult.Attribution> attribution = new HashMap<>();
        // Keys written by a gray hit: the experiment pass below never overwrites
        // these (gray wins unconditionally — engine.go:355-356, strict).
        Set<String> grayOwned = new HashSet<>();

        // Step 1: gray_hits in wire order (release_id ascending), first-writer-wins.
        for (GrayReleaseHit hit : resp.getGrayHitsList()) {
            for (Map.Entry<String, Long> e : hit.getKeyVersionsMap().entrySet()) {
                String key = e.getKey();
                if (grayOwned.contains(key)) {
                    // Same-source (gray vs gray) conflict: wire-unreachable,
                    // defensive first-writer-wins (topology.go:433-435). The
                    // platform treats same-source conflicts as deterministic
                    // normal behaviour, so DEBUG, not WARN.
                    if (owner != null) {
                        AbtestComputeResult.Attribution kept = attribution.get(key);
                        owner.logger().debug(
                                "tipsyabconfig: abtest merge conflict (gray vs gray), first writer wins "
                                        + "(ns={}, key={}, kept_version={}, kept_release_id={}, "
                                        + "skipped_version={}, skipped_release_id={}, trace_id={})",
                                ns, key, keyVersions.get(key),
                                kept == null ? 0L : kept.releaseId,
                                e.getValue(), hit.getReleaseId(), traceId);
                    }
                    continue;
                }
                grayOwned.add(key);
                keyVersions.put(key, e.getValue());
                // F3: the value is written above regardless; release_id=0 only
                // degrades the attribution to UNATTRIBUTED.
                attribution.put(key, hit.getReleaseId() != 0L
                        ? AbtestComputeResult.Attribution.grayWhitelist(hit.getReleaseId())
                        : AbtestComputeResult.Attribution.UNATTRIBUTED);
            }
        }

        // Step 2: groups in wire order, CONFIG_VERSION only.
        for (ExperimentGroupResult g : resp.getGroupsList()) {
            if (g.getExperimentType()
                    != io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ExperimentType.EXPERIMENT_TYPE_CONFIG_VERSION) {
                // Fail-closed type filter — deliberate exception to F3 (see
                // the method javadoc): replicates the platform flat path's
                // matchesType filter, guarded platform-side (probe M10).
                continue;
            }
            for (Map.Entry<String, Long> e : g.getParamsVersionsMap().entrySet()) {
                String key = e.getKey();
                if (grayOwned.contains(key)) {
                    // Cross-source: gray wins unconditionally (engine.go:355-356,
                    // strictly guaranteed product semantic) — skip, no overwrite.
                    continue;
                }
                Long prev = keyVersions.get(key);
                if (prev != null && owner != null) {
                    // Same-source (experiment vs experiment) conflict:
                    // last-write-wins (engine.go:380). Deterministic normal
                    // behaviour platform-side, so DEBUG, not WARN.
                    AbtestComputeResult.Attribution prevAttr = attribution.get(key);
                    owner.logger().debug(
                            "tipsyabconfig: abtest merge conflict (experiment vs experiment), last writer wins "
                                    + "(ns={}, key={}, prev_version={}, prev_experiment_id={}, "
                                    + "new_version={}, new_experiment_id={}, trace_id={})",
                            ns, key, prev,
                            prevAttr == null ? "" : prevAttr.experimentId,
                            e.getValue(), g.getExperimentId(), traceId);
                }
                keyVersions.put(key, e.getValue());
                // F3: the value is written above regardless; empty ids only
                // degrade the attribution to UNATTRIBUTED (never skip the key —
                // skipping would drop the versionId, a value-resolution change).
                boolean attributed = !g.getExperimentId().isEmpty() && !g.getGroupId().isEmpty();
                attribution.put(key, attributed
                        ? AbtestComputeResult.Attribution.experiment(g.getExperimentId(), g.getGroupId())
                        : AbtestComputeResult.Attribution.UNATTRIBUTED);
            }
        }
        return new AbtestComputeResult(keyVersions, attribution);
    }

    // ------------------------------------------------------------------
    // Attr encoding (shared with TipsyAbConfigClient.getExperimentResult).
    // ------------------------------------------------------------------

    /**
     * Converts a {@code Map<String,Object>} of user attributes to a
     * {@code Map<String, Value>} for the proto request. Null / empty input
     * yields an empty map. Unsupported value types are dropped with a WARN.
     *
     * <p>Type mapping (design 05; Boolean is tested before Number to stay
     * unambiguous): {@code String}&rarr;{@code s}, {@code Boolean}&rarr;{@code b},
     * {@code Integer/Long/Short/Byte}&rarr;{@code i}, {@code Float/Double}&rarr;
     * {@code d}, everything else dropped.
     */
    static Map<String, Value> encodeUserAttrs(Map<String, Object> attrs, Logger logger) {
        if (attrs == null || attrs.isEmpty()) {
            return Map.of();
        }
        Map<String, Value> out = new HashMap<>(attrs.size());
        for (Map.Entry<String, Object> e : attrs.entrySet()) {
            Value v = encodeValue(e.getValue());
            if (v == null) {
                if (logger != null) {
                    logger.warn("tipsyabconfig: dropping unsupported user_attr value type (key={})",
                            e.getKey());
                }
                continue;
            }
            out.put(e.getKey(), v);
        }
        return out;
    }

    /**
     * Encodes a single attribute value to a proto {@link Value}, or {@code null}
     * if the concrete type is unsupported. Boolean is checked before Number so a
     * boolean never falls through to the integer branch.
     */
    static Value encodeValue(Object v) {
        if (v instanceof String s) {
            return Value.newBuilder().setS(s).build();
        }
        if (v instanceof Boolean b) {
            return Value.newBuilder().setB(b).build();
        }
        if (v instanceof Integer || v instanceof Long || v instanceof Short || v instanceof Byte) {
            return Value.newBuilder().setI(((Number) v).longValue()).build();
        }
        if (v instanceof Float || v instanceof Double) {
            return Value.newBuilder().setD(((Number) v).doubleValue()).build();
        }
        return null;
    }

    /**
     * Whether {@code uid} carries no real user identity. The empty string (the
     * constructor normalises {@code null} to {@code ""}) and the string zero
     * {@code "0"} both mean "no user", for which abtest bucketing / whitelisting
     * is meaningless — {@link #ensureFetch} short-circuits these to the empty
     * result without a {@code GetExperimentResult} RPC.
     */
    private static boolean isNoUserUid(String uid) {
        return uid.isEmpty() || "0".equals(uid);
    }
}
