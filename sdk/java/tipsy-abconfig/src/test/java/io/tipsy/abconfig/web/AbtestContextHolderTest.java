package io.tipsy.abconfig.web;

import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.tipsy.abconfig.AbtestContext;
import java.util.Map;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.atomic.AtomicReference;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * Unit tests for {@link AbtestContextHolder} (ST6 / design 06 "Web 上下文集成",
 * test plan §"Web(ST6)": "{@code AbtestContextHolder} set/get/clear/runWith").
 *
 * <p>The holder is a thin {@link ThreadLocal} wrapper; these tests pin its exact
 * contract:
 * <ul>
 *   <li>{@code set}/{@code get}/{@code clear} bind/read/unbind on the CURRENT
 *       thread; {@code set(null)} unbinds (so {@code get()} returns {@code null});</li>
 *   <li>{@code runWith} binds for the duration of the body and restores the
 *       PRIOR binding afterwards — including when the body throws, and including
 *       nested calls (the inner restores to the outer value, NOT {@code null});</li>
 *   <li>{@code runWith(ctx, null)} throws {@link NullPointerException};</li>
 *   <li>the fan-out warning is real: a bound value is NOT visible on a child
 *       (virtual) thread — the decisive symptom the holder's Javadoc warns about
 *       and the reason the SDK's first-class contract is explicit passing.</li>
 * </ul>
 *
 * <p>Real {@link AbtestContext} instances are needed (the holder stores the
 * reference). {@code AbtestContext}'s constructor is package-private to
 * {@code io.tipsy.abconfig}, so they are obtained from the public client
 * factories via {@link WebTestClient} (no RPC: identity-less / mock contexts
 * resolve locally).
 */
final class AbtestContextHolderTest {

    private WebTestClient web;
    private AbtestContext ctxA;
    private AbtestContext ctxB;

    @BeforeEach
    void setUp() {
        web = WebTestClient.create();
        // Two DISTINCT, non-null contexts. emptyAbtestContext + a mock context
        // both resolve locally (no GetExperimentResult RPC) and are never the
        // same instance, which is all the holder's reference semantics need.
        ctxA = web.client.emptyAbtestContext();
        ctxB = web.client.mockAbtestContext("u-2", Map.of());
        // Defensive: ensure a clean ThreadLocal before each test (the runner may
        // reuse the thread across tests).
        AbtestContextHolder.clear();
    }

    @AfterEach
    void tearDown() {
        // Never leak a binding onto a pooled / reused test thread.
        AbtestContextHolder.clear();
        if (web != null) {
            web.close();
        }
    }

    // ------------------------------------------------------------------
    // set / get / clear
    // ------------------------------------------------------------------

    @Test
    @DisplayName("set then get returns the same instance; clear unbinds (get == null)")
    void setGetClear() {
        assertNull(AbtestContextHolder.get(), "no binding initially");

        AbtestContextHolder.set(ctxA);
        assertSame(ctxA, AbtestContextHolder.get(), "get returns the exact instance that was set");

        AbtestContextHolder.clear();
        assertNull(AbtestContextHolder.get(), "clear unbinds the current thread");
    }

    @Test
    @DisplayName("set(null) unbinds the current thread (get == null)")
    void setNullUnbinds() {
        AbtestContextHolder.set(ctxA);
        assertSame(ctxA, AbtestContextHolder.get());

        AbtestContextHolder.set(null);
        assertNull(AbtestContextHolder.get(), "set(null) is an explicit unbind");
    }

    @Test
    @DisplayName("set replaces a prior binding")
    void setReplacesPriorBinding() {
        AbtestContextHolder.set(ctxA);
        AbtestContextHolder.set(ctxB);
        assertSame(ctxB, AbtestContextHolder.get(), "the second set wins");
    }

    // ------------------------------------------------------------------
    // runWith
    // ------------------------------------------------------------------

    @Test
    @DisplayName("runWith binds for the body and restores the prior binding (none -> null) after")
    void runWithBindsThenRestoresNull() {
        assertNull(AbtestContextHolder.get());

        AtomicReference<AbtestContext> seen = new AtomicReference<>();
        AbtestContextHolder.runWith(ctxA, () -> seen.set(AbtestContextHolder.get()));

        assertSame(ctxA, seen.get(), "the body sees the bound context");
        assertNull(AbtestContextHolder.get(), "no prior binding -> restored to null after runWith");
    }

    @Test
    @DisplayName("runWith restores the prior binding even when the body throws")
    void runWithRestoresOnBodyThrow() {
        assertNull(AbtestContextHolder.get());

        RuntimeException boom = new RuntimeException("boom");
        RuntimeException thrown = assertThrows(RuntimeException.class, () ->
                AbtestContextHolder.runWith(ctxA, () -> {
                    // Confirm the binding is live mid-body before throwing.
                    assertSame(ctxA, AbtestContextHolder.get());
                    throw boom;
                }));

        assertSame(boom, thrown, "runWith propagates the body's exception unchanged");
        assertNull(AbtestContextHolder.get(),
                "the finally-block restore runs even when the body throws");
    }

    @Test
    @DisplayName("nested runWith: inner binds ctxB, and on inner exit restores to OUTER ctxA (not null)")
    void nestedRunWithRestoresToOuter() {
        assertNull(AbtestContextHolder.get());

        AtomicReference<AbtestContext> innerSaw = new AtomicReference<>();
        AtomicReference<AbtestContext> afterInner = new AtomicReference<>();

        AbtestContextHolder.runWith(ctxA, () -> {
            assertSame(ctxA, AbtestContextHolder.get(), "outer body sees ctxA");
            AbtestContextHolder.runWith(ctxB, () ->
                    innerSaw.set(AbtestContextHolder.get()));
            // After the inner runWith returns, the binding must be the OUTER ctxA,
            // NOT null — this is the composition guarantee.
            afterInner.set(AbtestContextHolder.get());
        });

        assertSame(ctxB, innerSaw.get(), "inner body sees ctxB");
        assertSame(ctxA, afterInner.get(), "inner exit restores the OUTER binding (ctxA), not null");
        assertNull(AbtestContextHolder.get(), "outer exit restores to the original (null)");
    }

    @Test
    @DisplayName("runWith over an existing binding restores THAT binding (not null) on exit")
    void runWithRestoresExistingBinding() {
        AbtestContextHolder.set(ctxA); // pre-existing binding, not via runWith
        try {
            AbtestContextHolder.runWith(ctxB, () ->
                    assertSame(ctxB, AbtestContextHolder.get(), "body sees ctxB"));
            assertSame(ctxA, AbtestContextHolder.get(),
                    "runWith restores the pre-existing ctxA binding, not null");
        } finally {
            AbtestContextHolder.clear();
        }
    }

    @Test
    @DisplayName("runWith(ctx, null) throws NullPointerException")
    void runWithNullBodyThrowsNpe() {
        assertThrows(NullPointerException.class, () -> AbtestContextHolder.runWith(ctxA, null));
        // A null body must not leave a stray binding behind.
        assertNull(AbtestContextHolder.get(), "a rejected runWith leaves no binding");
    }

    @Test
    @DisplayName("runWith(null, body) binds null (an empty/unbound holder) for the body, then restores")
    void runWithNullCtxIsAllowed() {
        AbtestContextHolder.set(ctxA);
        try {
            AtomicReference<AbtestContext> seen = new AtomicReference<>();
            // A null abctx is a legal "run with no binding" — set(null) unbinds.
            AbtestContextHolder.runWith(null, () -> seen.set(AbtestContextHolder.get()));
            assertNull(seen.get(), "a null abctx means the body sees no binding");
            assertSame(ctxA, AbtestContextHolder.get(), "the prior ctxA is restored after");
        } finally {
            AbtestContextHolder.clear();
        }
    }

    // ------------------------------------------------------------------
    // FAN-OUT: the holder does NOT propagate across a (virtual) thread.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("fan-out: a bound context is NOT visible on a child virtual thread (get() == null)")
    void fanOutDoesNotPropagateToChildThread() throws Exception {
        AbtestContextHolder.set(ctxA);
        assertSame(ctxA, AbtestContextHolder.get(), "bound on the parent thread");

        // The decisive design assertion: a ThreadLocal binding does NOT cross a
        // fan-out to a virtual thread. This is exactly why the SDK's first-class
        // contract is to pass the AbtestContext EXPLICITLY (the holder's Javadoc
        // warns about this). Determinism: we BLOCK on Future.get(), never sleep.
        try (ExecutorService vexec = Executors.newVirtualThreadPerTaskExecutor()) {
            Future<AbtestContext> childSaw = vexec.submit(AbtestContextHolder::get);
            assertNull(childSaw.get(),
                    "a child virtual thread sees NO binding — fan-out does not propagate the holder");
        }

        // The parent's binding is unaffected by the child read.
        assertSame(ctxA, AbtestContextHolder.get(), "the parent binding is untouched by the fan-out");
        AbtestContextHolder.clear();
        assertNull(AbtestContextHolder.get());
    }

    @Test
    @DisplayName("sanity: the test contexts are distinct, non-null instances")
    void testContextsAreDistinct() {
        assertTrue(ctxA != null && ctxB != null, "both contexts are non-null");
        assertTrue(ctxA != ctxB, "ctxA and ctxB are different instances");
    }
}
