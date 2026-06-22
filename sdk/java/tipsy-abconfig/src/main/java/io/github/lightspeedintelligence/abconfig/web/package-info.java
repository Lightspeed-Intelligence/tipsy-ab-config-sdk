/**
 * OPTIONAL, framework-agnostic web-integration helpers for the Tipsy AB-config
 * Java SDK. Pure JDK, zero extra dependencies (no servlet / Spring / Netty /
 * gRPC-server / Jackson).
 *
 * <p><b>The first-class contract is the explicitly-passed
 * {@link io.github.lightspeedintelligence.abconfig.AbtestContext}.</b> Build one per inbound request via
 * {@code TipsyAbConfigClient.newAbtestContext*} and pass it as an argument to
 * every {@code getConfig} call within that request. This is the only approach
 * that is always correct, including under the surveyed consumer's
 * single-request-across-virtual-threads fan-out.
 *
 * <p>Two OPTIONAL conveniences live here for {@code thread-per-request} edges:
 * <ul>
 *   <li>{@link io.github.lightspeedintelligence.abconfig.web.AbtestContextHolder} — a {@code ThreadLocal}
 *       holder. <b>Does NOT propagate across virtual-thread / executor
 *       fan-out</b>; see its Javadoc.</li>
 *   <li>{@link io.github.lightspeedintelligence.abconfig.web.HttpServerSupport} — thin helpers for the JDK
 *       built-in {@code com.sun.net.httpserver.HttpServer} (trace-id extraction +
 *       a context-binding handler wrapper). Same fan-out caveat.</li>
 * </ul>
 *
 * <p>Deliberately NOT provided (design 01 / 06): servlet {@code Filter}, Spring
 * auto-configuration, gRPC {@code ServerInterceptor}, and the Go SDK's
 * {@code context.Context}-based {@code WithAbtestContext} /
 * {@code AbtestContextFromContext} (Java has no request-scoped context, and an
 * implicit ThreadLocal carry is unsafe under the consumer's fan-out).
 */
package io.github.lightspeedintelligence.abconfig.web;
