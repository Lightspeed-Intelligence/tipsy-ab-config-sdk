package io.tipsy.abconfig.web;

import io.tipsy.abconfig.AbtestContext;

/**
 * A {@link ThreadLocal}-backed holder for the per-request {@link AbtestContext},
 * provided as an OPTIONAL convenience for {@code thread-per-request} HTTP edges
 * (e.g. the JDK {@code com.sun.net.httpserver.HttpServer}, plain servlet
 * containers). Pure JDK, zero extra dependencies.
 *
 * <p><b>FAN-OUT WARNING (read before using).</b> A {@link ThreadLocal} value is
 * bound to a single thread. It is NOT propagated across thread boundaries — in
 * particular it does NOT cross
 * {@link java.util.concurrent.Executors#newVirtualThreadPerTaskExecutor()} or any
 * other fan-out. The consumer surveyed for this SDK (pine-java) runs a
 * thread-per-request HTTP pool but fans a single request out across virtual
 * threads inside the engine; in that shape a child virtual thread calling
 * {@link #get()} would see {@code null}, not the request's context. Therefore:
 *
 * <ul>
 *   <li>The first-class, always-correct contract is to pass the
 *       {@link AbtestContext} EXPLICITLY as a method argument to every
 *       {@code getConfig} call (mirrors the Go SDK's explicit {@code abctx}
 *       parameter). This is the only fan-out-safe approach.</li>
 *   <li>Use this holder ONLY when the request never leaves its handler thread
 *       (no executor / virtual-thread fan-out). When in doubt, pass the context
 *       explicitly.</li>
 * </ul>
 *
 * <p>This holder intentionally does NOT replicate the Go SDK's
 * {@code WithAbtestContext} / {@code AbtestContextFromContext} ({@code context.Context})
 * API: Java has no request-scoped {@code Context}, and an implicit
 * {@code ThreadLocal} carry would be unsafe under the consumer's fan-out (design
 * 01 "有意差异" F3).
 *
 * <p>All methods are static; this class is not instantiable.
 */
public final class AbtestContextHolder {

    private static final ThreadLocal<AbtestContext> HOLDER = new ThreadLocal<>();

    private AbtestContextHolder() {
        // no instances
    }

    /**
     * Binds {@code abctx} to the current thread, replacing any previously bound
     * value. Pair every {@code set} with a {@link #clear()} in a {@code finally}
     * block to avoid leaking the context onto a pooled thread.
     *
     * @param abctx the per-request context to bind (may be {@code null} to unbind)
     */
    public static void set(AbtestContext abctx) {
        if (abctx == null) {
            HOLDER.remove();
        } else {
            HOLDER.set(abctx);
        }
    }

    /**
     * Returns the {@link AbtestContext} bound to the CURRENT thread, or
     * {@code null} if none is bound. A {@code null} return on a fanned-out child
     * thread is the expected symptom of the fan-out warning above — fall back to
     * an explicitly-passed context.
     *
     * @return the bound context, or {@code null}
     */
    public static AbtestContext get() {
        return HOLDER.get();
    }

    /**
     * Removes any {@link AbtestContext} bound to the current thread. Always call
     * this in a {@code finally} block after {@link #set(AbtestContext)} so a
     * recycled pool thread does not retain a stale context.
     */
    public static void clear() {
        HOLDER.remove();
    }

    /**
     * Runs {@code body} with {@code abctx} bound to the current thread, clearing
     * (restoring the prior binding) afterwards even if {@code body} throws. This
     * is the recommended structured way to use the holder on a single thread.
     *
     * <p>Same fan-out caveat as the class-level warning: any work {@code body}
     * dispatches to another thread (executor, virtual-thread fan-out) will NOT
     * see the bound context.
     *
     * @param abctx the context to bind for the duration of {@code body}
     * @param body  the work to run
     * @throws NullPointerException if {@code body} is {@code null}
     */
    public static void runWith(AbtestContext abctx, Runnable body) {
        if (body == null) {
            throw new NullPointerException("body");
        }
        AbtestContext previous = HOLDER.get();
        set(abctx);
        try {
            body.run();
        } finally {
            // Restore the prior binding (usually null) rather than blindly
            // clearing, so nested runWith calls compose correctly.
            set(previous);
        }
    }
}
