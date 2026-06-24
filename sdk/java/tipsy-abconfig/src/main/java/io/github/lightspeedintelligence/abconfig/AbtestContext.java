package io.github.lightspeedintelligence.abconfig;

import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultRequest;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.GetExperimentResultResponse;
import io.github.lightspeedintelligence.abconfig.proto.abtest.v1.Value;
import java.time.Duration;
import java.util.HashMap;
import java.util.Map;
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
     * {@link AbtestComputeResult#EMPTY_RESULT} future for an empty/owner-null or
     * unsubscribed ns — no RPC — otherwise an executor-backed future running
     * {@link #fetchConfigVersionFlatKvForNamespace(String)}), while every racing
     * caller finds and reuses the existing future. Net effect: AT MOST ONE
     * {@code GetExperimentResult} RPC per ns per request link. The returned
     * future never completes exceptionally (F5).
     */
    CompletableFuture<AbtestComputeResult> ensureFetch(String ns) {
        synchronized (this) {
            CompletableFuture<AbtestComputeResult> future = results.get(ns);
            if (future == null) {
                if (empty || owner == null) {
                    // Identity-less / mock ctx: resolve to empty without an RPC.
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
     * Explicit, opt-in prefetch (warm-up) of the {@code config_version flat_kv}
     * result for {@code ns} within this request link. Non-blocking: it triggers
     * the at-most-once fetch via {@link #ensureFetch} and returns immediately
     * without awaiting the result, so a subsequent {@link
     * TipsyAbConfigClient#getConfig} for the same ns reuses the warmed future
     * instead of paying the RPC latency inline.
     *
     * <p>Idempotent and at-most-once: calling this more than once for the same
     * ns (or prefetching then {@code getConfig}-ing) issues AT MOST ONE
     * {@code GetExperimentResult} RPC. An empty / mock context or an unsubscribed
     * ns short-circuits inside {@code ensureFetch} and issues NO RPC.
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
                    owner.metricsInternal().abtestFallback.inc(ns);
                    owner.logger().warn(
                            "tipsyabconfig: abtest compute task threw; falling back to full release"
                                    + " (ns={}, trace_id={})",
                            ns, traceId, t);
                }
                f.complete(AbtestComputeResult.EMPTY_RESULT);
            }
        });
        return f;
    }

    /**
     * Wraps {@code AbtestService.GetExperimentResult} with the per-call timeout
     * for the {@code config_version flat_kv} shape the dynamic {@code getConfig}
     * fast path consumes (the experiment type and display type are hardwired to
     * {@code CONFIG_VERSION} / {@code FLAT_KV}; this is NOT the general-purpose
     * {@link TipsyAbConfigClient#getExperimentResult} API). On ANY error
     * (including a missing abtest connection) it returns
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
                .setDisplayType(io.github.lightspeedintelligence.abconfig.proto.abtest.v1.ResultDisplayType.RESULT_DISPLAY_TYPE_FLAT_KV)
                .setTraceId(traceId)
                .build();
        try {
            GetExperimentResultResponse resp =
                    transport.getExperimentResult(req, owner.abtestTimeout());
            Map<String, Long> kv = new HashMap<>(resp.getConfigFlatKvMap());
            return new AbtestComputeResult(kv);
        } catch (Exception e) {
            owner.metricsInternal().abtestFallback.inc(ns);
            owner.logger().warn(
                    "tipsyabconfig: AbtestService.GetExperimentResult failed; falling back to full release"
                            + " (ns={}, trace_id={})",
                    ns, traceId, e);
            return AbtestComputeResult.EMPTY_RESULT;
        }
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
}
