/**
 * Tipsy AB-config Java SDK runnable example.
 *
 * <p>{@link io.github.lightspeedintelligence.abconfig.example.Main} runs a small, framework-agnostic
 * HTTP service on the JDK built-in {@code com.sun.net.httpserver.HttpServer},
 * demonstrating the SDK lifecycle ({@code create} → {@code newAbtestContext} →
 * {@code getConfig} → {@code close}), the Optional-ised {@code getConfigStatic},
 * the optional {@code io.github.lightspeedintelligence.abconfig.web} helpers, and minting a Dev token
 * with {@code tipsy-auth}. This module is not published (deploy is skipped).
 */
package io.github.lightspeedintelligence.abconfig.example;
