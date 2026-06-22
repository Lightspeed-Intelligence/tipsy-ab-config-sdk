package io.tipsy.abconfig.web;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;

import com.sun.net.httpserver.Headers;
import com.sun.net.httpserver.HttpContext;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpHandler;
import com.sun.net.httpserver.HttpPrincipal;
import io.tipsy.abconfig.AbtestContext;
import io.tipsy.abconfig.UserInfo;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.net.URI;
import java.util.Map;
import java.util.UUID;
import java.util.concurrent.atomic.AtomicReference;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * Unit tests for {@link HttpServerSupport} (ST6 / design 06 "Web 上下文集成",
 * test plan §"Web(ST6)": "{@code HttpServerSupport.extractTraceId}: X-Trace-Id >
 * X-Request-Id > UUID; trim; 空当缺失").
 *
 * <p>{@link HttpServerSupport#extractTraceId(HttpExchange)} mirrors the Go SDK's
 * {@code extractTraceFromRequest} (sdk/go/tipsyabconfig/middleware.go): prefer
 * {@code X-Trace-Id}, then {@code X-Request-Id}, otherwise a fresh UUID; values
 * are trimmed and a present-but-blank header is treated as absent.
 *
 * <p>The exchange is faked via {@link FakeHttpExchange}, an anonymous-style
 * subclass of the abstract {@link HttpExchange} that implements ONLY
 * {@code getRequestHeaders()} (the sole method {@code extractTraceId} touches);
 * every other abstract method throws / returns a stub so an accidental use is
 * loud.
 *
 * <p>{@link HttpServerSupport#wrap} is additionally covered with a lightweight
 * integration over a real in-process {@link WebTestClient}: it asserts the
 * per-request context is exposed on the {@link AbtestContextHolder} for the
 * duration of the wrapped handler, cleared afterwards, and degraded to an empty
 * context when the provider is absent / null-returning / throwing.
 */
final class HttpServerSupportTest {

    // ------------------------------------------------------------------
    // extractTraceId — header precedence + trim + blank-as-absent.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("X-Trace-Id only -> returned, trimmed")
    void traceIdOnly() {
        HttpExchange ex = FakeHttpExchange.withHeaders(h -> h.add("X-Trace-Id", "  trace-123  "));
        assertEquals("trace-123", HttpServerSupport.extractTraceId(ex),
                "X-Trace-Id is returned with surrounding whitespace trimmed");
    }

    @Test
    @DisplayName("X-Request-Id only -> returned, trimmed")
    void requestIdOnly() {
        HttpExchange ex = FakeHttpExchange.withHeaders(h -> h.add("X-Request-Id", " req-456 "));
        assertEquals("req-456", HttpServerSupport.extractTraceId(ex),
                "falls through to X-Request-Id when X-Trace-Id is absent");
    }

    @Test
    @DisplayName("both present -> X-Trace-Id wins")
    void bothPresentTraceWins() {
        HttpExchange ex = FakeHttpExchange.withHeaders(h -> {
            h.add("X-Trace-Id", "trace-AAA");
            h.add("X-Request-Id", "req-BBB");
        });
        assertEquals("trace-AAA", HttpServerSupport.extractTraceId(ex),
                "X-Trace-Id takes precedence over X-Request-Id");
    }

    @Test
    @DisplayName("neither present -> a fresh, well-formed UUID")
    void neitherPresentGeneratesUuid() {
        HttpExchange ex = FakeHttpExchange.withHeaders(h -> { /* no trace headers */ });
        String id = HttpServerSupport.extractTraceId(ex);
        assertNotNull(id);
        assertFalse(id.isBlank(), "generated trace id is non-empty");
        // A valid UUID round-trips through UUID.fromString without throwing.
        assertDoesNotThrow(() -> UUID.fromString(id),
                "generated trace id is a well-formed UUID: " + id);
    }

    @Test
    @DisplayName("X-Trace-Id present but blank (whitespace) -> treated as absent, falls to X-Request-Id")
    void blankTracePresentFallsToRequest() {
        HttpExchange ex = FakeHttpExchange.withHeaders(h -> {
            h.add("X-Trace-Id", "   "); // present but blank after trim -> absent
            h.add("X-Request-Id", "req-789");
        });
        assertEquals("req-789", HttpServerSupport.extractTraceId(ex),
                "a present-but-blank X-Trace-Id is ignored; X-Request-Id is used");
    }

    @Test
    @DisplayName("both present but blank -> treated as absent, falls to a fresh UUID")
    void bothBlankGeneratesUuid() {
        HttpExchange ex = FakeHttpExchange.withHeaders(h -> {
            h.add("X-Trace-Id", "  ");
            h.add("X-Request-Id", "\t");
        });
        String id = HttpServerSupport.extractTraceId(ex);
        assertDoesNotThrow(() -> UUID.fromString(id),
                "both blank -> a fresh UUID, not the blank header value: " + id);
    }

    @Test
    @DisplayName("X-Trace-Id with empty string -> treated as absent")
    void emptyTraceFallsToRequest() {
        HttpExchange ex = FakeHttpExchange.withHeaders(h -> {
            h.add("X-Trace-Id", "");
            h.add("X-Request-Id", "req-empty-trace");
        });
        assertEquals("req-empty-trace", HttpServerSupport.extractTraceId(ex),
                "an empty-string X-Trace-Id is treated as absent");
    }

    @Test
    @DisplayName("null exchange -> a fresh, well-formed UUID (no NPE)")
    void nullExchangeGeneratesUuid() {
        String id = HttpServerSupport.extractTraceId(null);
        assertNotNull(id);
        assertFalse(id.isBlank());
        assertDoesNotThrow(() -> UUID.fromString(id),
                "a null exchange degrades to a fresh UUID rather than throwing: " + id);
    }

    @Test
    @DisplayName("two generated trace ids (no headers) are distinct")
    void generatedIdsAreDistinct() {
        HttpExchange ex1 = FakeHttpExchange.withHeaders(h -> { });
        HttpExchange ex2 = FakeHttpExchange.withHeaders(h -> { });
        assertNotEquals(HttpServerSupport.extractTraceId(ex1), HttpServerSupport.extractTraceId(ex2),
                "each missing-header request gets its own fresh UUID");
    }

    // ------------------------------------------------------------------
    // wrap — lightweight integration over a real in-process client.
    // ------------------------------------------------------------------

    @Test
    @DisplayName("wrap: exposes the per-request context on the holder during the handler, clears after")
    void wrapBindsContextForHandlerThenClears() throws Exception {
        try (WebTestClient web = WebTestClient.create()) {
            AbtestContextHolder.clear();

            AtomicReference<AbtestContext> insideHandler = new AtomicReference<>();
            HttpHandler next = exchange -> insideHandler.set(AbtestContextHolder.get());

            HttpServerSupport.AbtestUserProvider provider =
                    ex -> UserInfo.of("user-42", Map.of("plan", "pro"));

            HttpHandler wrapped = HttpServerSupport.wrap(web.client, provider, next);

            HttpExchange ex = FakeHttpExchange.withHeaders(h -> h.add("X-Trace-Id", "trace-wrap-1"));
            wrapped.handle(ex);

            AbtestContext seen = insideHandler.get();
            assertNotNull(seen, "the handler sees a bound context");
            assertEquals("user-42", seen.userId(), "the context carries the provider's uid");
            assertEquals("trace-wrap-1", seen.traceId(),
                    "the inbound X-Trace-Id is propagated into the context");

            assertNull(AbtestContextHolder.get(),
                    "the binding is cleared in the finally block after the handler returns");
        } finally {
            AbtestContextHolder.clear();
        }
    }

    @Test
    @DisplayName("wrap: clears the holder even when the wrapped handler throws")
    void wrapClearsOnHandlerThrow() {
        try (WebTestClient web = WebTestClient.create()) {
            AbtestContextHolder.clear();

            java.io.IOException boom = new java.io.IOException("handler boom");
            HttpHandler next = exchange -> {
                assertNotNull(AbtestContextHolder.get(), "context is bound mid-handler");
                throw boom;
            };
            HttpHandler wrapped = HttpServerSupport.wrap(
                    web.client, ex -> UserInfo.of("u", Map.of()), next);

            HttpExchange ex = FakeHttpExchange.withHeaders(h -> { });
            java.io.IOException thrown = assertThrows(java.io.IOException.class, () -> wrapped.handle(ex));
            assertSame(boom, thrown, "the handler's exception propagates unchanged");
            assertNull(AbtestContextHolder.get(),
                    "the binding is cleared even when the handler throws");
        } finally {
            AbtestContextHolder.clear();
        }
    }

    @Test
    @DisplayName("wrap: a null provider degrades to an empty context (empty uid), still bound")
    void wrapNullProviderUsesEmptyContext() throws Exception {
        try (WebTestClient web = WebTestClient.create()) {
            AbtestContextHolder.clear();
            AtomicReference<AbtestContext> seen = new AtomicReference<>();
            HttpHandler wrapped = HttpServerSupport.wrap(
                    web.client, null, exchange -> seen.set(AbtestContextHolder.get()));

            wrapped.handle(FakeHttpExchange.withHeaders(h -> { }));

            assertNotNull(seen.get(), "even with no provider a context is bound");
            assertEquals("", seen.get().userId(),
                    "a null provider yields an identity-less (empty) context");
            assertNull(AbtestContextHolder.get(), "cleared after the handler");
        } finally {
            AbtestContextHolder.clear();
        }
    }

    @Test
    @DisplayName("wrap: a provider that returns null degrades to an empty context")
    void wrapNullReturningProviderUsesEmptyContext() throws Exception {
        try (WebTestClient web = WebTestClient.create()) {
            AbtestContextHolder.clear();
            AtomicReference<AbtestContext> seen = new AtomicReference<>();
            HttpHandler wrapped = HttpServerSupport.wrap(
                    web.client, ex -> null, exchange -> seen.set(AbtestContextHolder.get()));

            wrapped.handle(FakeHttpExchange.withHeaders(h -> { }));

            assertNotNull(seen.get());
            assertEquals("", seen.get().userId(),
                    "a null-returning provider degrades to an empty context");
        } finally {
            AbtestContextHolder.clear();
        }
    }

    @Test
    @DisplayName("wrap: a provider that throws degrades to an empty context (request still served)")
    void wrapThrowingProviderUsesEmptyContext() throws Exception {
        try (WebTestClient web = WebTestClient.create()) {
            AbtestContextHolder.clear();
            AtomicReference<AbtestContext> seen = new AtomicReference<>();
            HttpServerSupport.AbtestUserProvider provider = ex -> {
                throw new IllegalStateException("provider blew up");
            };
            HttpHandler wrapped = HttpServerSupport.wrap(
                    web.client, provider, exchange -> seen.set(AbtestContextHolder.get()));

            // A provider failure is non-fatal: the handler still runs.
            wrapped.handle(FakeHttpExchange.withHeaders(h -> { }));

            assertNotNull(seen.get(), "the request is still served with an empty context");
            assertEquals("", seen.get().userId(),
                    "a thrown provider exception degrades to an empty context, not a failed request");
        } finally {
            AbtestContextHolder.clear();
        }
    }

    @Test
    @DisplayName("wrap: null client or null next handler -> NullPointerException")
    void wrapRejectsNullClientOrNext() {
        try (WebTestClient web = WebTestClient.create()) {
            HttpHandler next = exchange -> { };
            HttpServerSupport.AbtestUserProvider provider = ex -> UserInfo.of("u", Map.of());

            assertThrows(NullPointerException.class,
                    () -> HttpServerSupport.wrap(null, provider, next),
                    "a null client is rejected");
            assertThrows(NullPointerException.class,
                    () -> HttpServerSupport.wrap(web.client, provider, null),
                    "a null next handler is rejected");
        }
    }

    @Test
    @DisplayName("AbtestUserProvider is a functional interface usable as a lambda")
    void providerIsFunctionalInterface() throws Exception {
        HttpServerSupport.AbtestUserProvider provider =
                ex -> UserInfo.of("x", Map.of("k", Integer.valueOf(1)));
        UserInfo u = provider.provide(FakeHttpExchange.withHeaders(h -> { }));
        assertEquals("x", u.uid());
        assertEquals(Integer.valueOf(1), u.attrs().get("k"));
    }

    // ------------------------------------------------------------------
    // Fake HttpExchange: implements ONLY getRequestHeaders(); everything
    // else is a loud stub (extractTraceId touches headers only).
    // ------------------------------------------------------------------

    /**
     * A minimal {@link HttpExchange} test double. {@link #getRequestHeaders()}
     * returns a caller-populated {@link Headers}; every other abstract method
     * throws {@link UnsupportedOperationException} (or returns {@code null}) so an
     * unexpected use surfaces immediately rather than passing silently.
     */
    static final class FakeHttpExchange extends HttpExchange {

        private final Headers requestHeaders = new Headers();
        private final Headers responseHeaders = new Headers();

        private FakeHttpExchange() { }

        /** Builds a fake exchange and lets the caller seed its request headers. */
        static FakeHttpExchange withHeaders(java.util.function.Consumer<Headers> seed) {
            FakeHttpExchange ex = new FakeHttpExchange();
            seed.accept(ex.requestHeaders);
            return ex;
        }

        @Override
        public Headers getRequestHeaders() {
            return requestHeaders;
        }

        @Override
        public Headers getResponseHeaders() {
            return responseHeaders;
        }

        @Override
        public URI getRequestURI() {
            throw new UnsupportedOperationException("getRequestURI");
        }

        @Override
        public String getRequestMethod() {
            throw new UnsupportedOperationException("getRequestMethod");
        }

        @Override
        public HttpContext getHttpContext() {
            throw new UnsupportedOperationException("getHttpContext");
        }

        @Override
        public void close() {
            // no-op
        }

        @Override
        public InputStream getRequestBody() {
            throw new UnsupportedOperationException("getRequestBody");
        }

        @Override
        public OutputStream getResponseBody() {
            throw new UnsupportedOperationException("getResponseBody");
        }

        @Override
        public void sendResponseHeaders(int rCode, long responseLength) {
            throw new UnsupportedOperationException("sendResponseHeaders");
        }

        @Override
        public InetSocketAddress getRemoteAddress() {
            throw new UnsupportedOperationException("getRemoteAddress");
        }

        @Override
        public int getResponseCode() {
            return -1;
        }

        @Override
        public InetSocketAddress getLocalAddress() {
            throw new UnsupportedOperationException("getLocalAddress");
        }

        @Override
        public String getProtocol() {
            throw new UnsupportedOperationException("getProtocol");
        }

        @Override
        public Object getAttribute(String name) {
            return null;
        }

        @Override
        public void setAttribute(String name, Object value) {
            // no-op
        }

        @Override
        public void setStreams(InputStream i, OutputStream o) {
            throw new UnsupportedOperationException("setStreams");
        }

        @Override
        public HttpPrincipal getPrincipal() {
            return null;
        }
    }

    /** Compile-time guard that the fake stays assignable to the helper's parameter type. */
    @Test
    @DisplayName("sanity: FakeHttpExchange is a usable HttpExchange double")
    void fakeExchangeShape() {
        HttpExchange ex = FakeHttpExchange.withHeaders(h -> h.add("X-Trace-Id", "t"));
        assertTrue(ex instanceof HttpExchange);
        assertEquals("t", ex.getRequestHeaders().getFirst("X-Trace-Id"));
    }
}
