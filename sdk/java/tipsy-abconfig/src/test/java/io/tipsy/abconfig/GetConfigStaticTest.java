package io.tipsy.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.Map;
import java.util.Optional;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/**
 * Pure-cache-read semantics of {@link TipsyAbConfigClient#getConfigStatic}.
 *
 * <p>Design ref: 03-core-client-api.md §"getConfigStatic" — it does NOT resolve
 * the namespace and NEVER throws a namespace error (unlike {@code getConfig}).
 * An unsubscribed / unknown namespace yields {@link Optional#empty()}; a hit
 * returns the value (the empty string is a valid value); an unknown key yields
 * {@link Optional#empty()}.
 *
 * <p>The cache is populated by a successful startup PullAll over the in-process
 * harness, then asserted purely on the read side.
 */
final class GetConfigStaticTest {

    private InProcessConfigServiceHarness harness;
    private TipsyAbConfigClient client;

    @BeforeEach
    void setUp() throws Exception {
        harness = new InProcessConfigServiceHarness();
    }

    @AfterEach
    void tearDown() {
        if (client != null) {
            client.close();
        }
        if (harness != null) {
            harness.close();
        }
    }

    /** Builds a client whose "checkout" cache holds two keys: color=blue, note="". */
    private TipsyAbConfigClient buildClient() {
        harness.setPullHandler(req -> InProcessConfigServiceHarness.pullResponse(
                InProcessConfigServiceHarness.snapshotWithKeys("checkout", 2, 1, Map.of(
                        "color", Map.entry(11L, "blue"),
                        "note", Map.entry(7L, "") // empty-string value: a valid hit
                ))));
        return TipsyAbConfigClient.create(grpcConfig("checkout"));
    }

    @Test
    void hit_returnsValue() {
        client = buildClient();
        Optional<String> v = client.getConfigStatic("checkout", "color");
        assertTrue(v.isPresent(), "a cached key must be a hit");
        assertEquals("blue", v.get());
    }

    @Test
    void hit_emptyStringValue_isStillPresent() {
        client = buildClient();
        Optional<String> v = client.getConfigStatic("checkout", "note");
        assertTrue(v.isPresent(), "the empty string is a valid value and must be a hit");
        assertEquals("", v.get());
    }

    @Test
    void unknownKey_inKnownNamespace_returnsEmpty() {
        client = buildClient();
        assertFalse(client.getConfigStatic("checkout", "no-such-key").isPresent());
    }

    @Test
    void unknownNamespace_returnsEmpty_doesNotThrow() {
        client = buildClient();
        // Distinct from getConfig: an unknown / unsubscribed ns must NOT throw a
        // namespace exception here — it is a pure cache miss.
        Optional<String> v = client.getConfigStatic("does-not-exist", "color");
        assertFalse(v.isPresent(), "an unknown namespace must be an empty cache read, not an exception");
    }

    @Test
    void subscribedButEmptyCacheNamespace_returnsEmpty() {
        // A namespace that is subscribed but whose cache was never populated:
        // still a pure miss, no exception.
        harness.setPullHandler(req -> {
            // Only return a snapshot for "checkout"; "orders" stays uncached.
            if (req.getNamespacesList().contains("orders")) {
                return InProcessConfigServiceHarness.pullResponse(); // empty
            }
            return InProcessConfigServiceHarness.pullResponse(
                    InProcessConfigServiceHarness.snapshot("checkout", 2, 1, "color", 11, "blue"));
        });
        client = TipsyAbConfigClient.create(grpcConfig("checkout", "orders"));
        assertFalse(client.getConfigStatic("orders", "color").isPresent(),
                "a subscribed-but-uncached namespace is still a pure miss");
    }

    private Config grpcConfig(String... namespaces) {
        return Config.builder()
                .namespaces(namespaces)
                .configServiceAddr("passthrough:///x")
                .token("tok")
                .transport(Transport.GRPC)
                .channelConfigurator(harness.channelConfigurator())
                // long interval so the periodic loop never fires during the test
                .pullInterval(java.time.Duration.ofMinutes(10))
                .pullTimeout(java.time.Duration.ofSeconds(2))
                .build();
    }
}
