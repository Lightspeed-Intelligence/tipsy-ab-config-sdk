package io.github.lightspeedintelligence.abconfig.web;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpHandler;
import io.github.lightspeedintelligence.abconfig.AbtestContext;
import io.github.lightspeedintelligence.abconfig.TipsyAbConfigClient;
import io.github.lightspeedintelligence.abconfig.UserInfo;
import java.util.UUID;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * OPTIONAL, zero-extra-dependency helpers for wiring the SDK into the JDK
 * built-in {@code com.sun.net.httpserver.HttpServer}. This is the HTTP server the
 * surveyed consumer (pine-java) runs underneath, so a thin adapter against it
 * avoids pulling in any servlet / Spring / Netty / gRPC-server dependency and
 * keeps a shaded host jar clean (design 06 "Web 上下文集成").
 *
 * <p><b>{@code com.sun.*} caveat.</b> {@link com.sun.net.httpserver.HttpExchange}
 * lives in a JDK-internal {@code com.sun.*} package. It is stable and shipped
 * with every JDK, but it is not part of the {@code java.*} public API. These
 * helpers are an OPTIONAL convenience only: the first-class integration contract
 * is the explicitly-passed {@link AbtestContext} (see the package docs). If you
 * do not run on {@code com.sun.net.httpserver}, ignore this class entirely and
 * construct the context yourself via
 * {@link TipsyAbConfigClient#newAbtestContext(String, java.util.Map, String)}.
 *
 * <p><b>FAN-OUT WARNING.</b> {@link #wrap} exposes the per-request context via
 * the {@link AbtestContextHolder} {@code ThreadLocal}, which does NOT propagate
 * across virtual-thread / executor fan-out (see {@link AbtestContextHolder}).
 * Use it only when the handler does its config reads on the same thread the
 * {@code HttpServer} dispatched. For fan-out shapes, read the context off the
 * holder ONCE at the top of the handler and pass it explicitly into every
 * downstream call.
 *
 * <p>All members are static; this class is not instantiable.
 */
public final class HttpServerSupport {

    private static final Logger LOG = LoggerFactory.getLogger(HttpServerSupport.class);

    private static final String HEADER_TRACE_ID = "X-Trace-Id";
    private static final String HEADER_REQUEST_ID = "X-Request-Id";

    private HttpServerSupport() {
        // no instances
    }

    /**
     * Extracts the inbound trace id from request headers, mirroring the Go SDK's
     * {@code extractTraceFromRequest}: prefer {@code X-Trace-Id}, then
     * {@code X-Request-Id}, otherwise generate a fresh {@code UUID}. The returned
     * value is always non-empty and whitespace-trimmed; a header that is present
     * but blank (after trim) is treated as absent.
     *
     * @param ex the inbound exchange (may be {@code null} → a fresh UUID)
     * @return a non-empty, trimmed trace id
     */
    public static String extractTraceId(HttpExchange ex) {
        if (ex != null && ex.getRequestHeaders() != null) {
            String trace = firstNonBlankHeader(ex, HEADER_TRACE_ID);
            if (trace != null) {
                return trace;
            }
            String request = firstNonBlankHeader(ex, HEADER_REQUEST_ID);
            if (request != null) {
                return request;
            }
        }
        return UUID.randomUUID().toString();
    }

    /** Returns the first value of {@code name}, trimmed, or {@code null} if absent/blank. */
    private static String firstNonBlankHeader(HttpExchange ex, String name) {
        String v = ex.getRequestHeaders().getFirst(name);
        if (v == null) {
            return null;
        }
        String trimmed = v.trim();
        return trimmed.isEmpty() ? null : trimmed;
    }

    /**
     * Extracts the user identity ({@link UserInfo}: uid + attrs) for an inbound
     * exchange. The surveyed consumer carries {@code user_id} in the JSON request
     * body's {@code common} map, so this provider receives the raw
     * {@link HttpExchange} and is free to read headers and/or buffer the body as
     * needed.
     *
     * <p>A thrown exception is non-fatal: {@link #wrap} logs a WARN and builds an
     * {@link TipsyAbConfigClient#emptyAbtestContext() empty context} for that
     * request, so the request still succeeds on the full-release path.
     */
    @FunctionalInterface
    public interface AbtestUserProvider {

        /**
         * Returns the user identity for {@code ex}. Returning {@code null} is
         * treated like an empty user identity (an empty context).
         *
         * @param ex the inbound exchange
         * @return the user identity, or {@code null}
         * @throws Exception any failure; {@link #wrap} degrades to an empty context
         */
        UserInfo provide(HttpExchange ex) throws Exception;
    }

    /**
     * Wraps {@code next} so that, at the HTTP edge, the SDK:
     *
     * <ol>
     *   <li>extracts the trace id ({@link #extractTraceId});</li>
     *   <li>invokes {@code provider} to obtain the {@link UserInfo} (a
     *       {@code null} provider, a thrown exception, or a {@code null} return
     *       degrades to {@link TipsyAbConfigClient#emptyAbtestContext()} + a
     *       WARN; a present {@link UserInfo} — even one with an empty uid — is
     *       passed through as-is);</li>
     *   <li>builds an {@link AbtestContext} via
     *       {@link TipsyAbConfigClient#newAbtestContext(String, java.util.Map, String)};</li>
     *   <li>binds it on {@link AbtestContextHolder} for the duration of
     *       {@code next.handle}, clearing it in a {@code finally} block.</li>
     * </ol>
     *
     * <p><b>FAN-OUT WARNING.</b> The context is bound on a {@link ThreadLocal}
     * and is only visible on the dispatching thread. It does NOT propagate across
     * virtual-thread / executor fan-out. If your handler fans the request out,
     * read the context off the holder once at the top of {@code next} and pass it
     * explicitly to every downstream {@code getConfig} call.
     *
     * @param client   the SDK client used to build the context
     * @param provider extracts the user identity (may be {@code null} → always empty context)
     * @param next     the wrapped handler
     * @return a handler that binds a per-request {@link AbtestContext}
     * @throws NullPointerException if {@code client} or {@code next} is {@code null}
     */
    public static HttpHandler wrap(
            TipsyAbConfigClient client, AbtestUserProvider provider, HttpHandler next) {
        if (client == null) {
            throw new NullPointerException("client");
        }
        if (next == null) {
            throw new NullPointerException("next");
        }
        return exchange -> {
            String traceId = extractTraceId(exchange);
            AbtestContext abctx = buildContext(client, provider, exchange, traceId);
            AbtestContextHolder.set(abctx);
            try {
                next.handle(exchange);
            } finally {
                AbtestContextHolder.clear();
            }
        };
    }

    /**
     * Builds the per-request context, degrading to an empty context (with a WARN)
     * when {@code provider} is absent, returns {@code null}, or throws.
     */
    private static AbtestContext buildContext(
            TipsyAbConfigClient client,
            AbtestUserProvider provider,
            HttpExchange exchange,
            String traceId) {
        if (provider == null) {
            return client.emptyAbtestContext();
        }
        UserInfo user;
        try {
            user = provider.provide(exchange);
        } catch (Exception e) {
            LOG.warn("tipsyabconfig: user provider failed; using empty abtest context "
                    + "(trace_id={})", traceId, e);
            return client.emptyAbtestContext();
        }
        if (user == null) {
            return client.emptyAbtestContext();
        }
        return client.newAbtestContext(user.uid(), user.attrs(), traceId);
    }
}
