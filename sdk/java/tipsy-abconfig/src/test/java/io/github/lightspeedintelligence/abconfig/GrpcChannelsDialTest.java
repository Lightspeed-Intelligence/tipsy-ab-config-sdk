package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertDoesNotThrow;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.grpc.ManagedChannel;
import io.grpc.ManagedChannelBuilder;
import io.grpc.inprocess.InProcessChannelBuilder;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.Arguments;
import org.junit.jupiter.params.provider.MethodSource;

import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicReference;
import java.util.function.UnaryOperator;
import java.util.stream.Stream;

/**
 * ST5（集成）— {@link GrpcChannels#dial(GrpcTarget, int, UnaryOperator)}。
 *
 * <p>语义基准 Go {@code (*Client).dial} + {@code sdk_service_config_test.go}：
 * grpc-java 不暴露已解析的 service config / keepalive，且设计「关键约束」禁止
 * reflect 黑魔法，故强契约在纯函数层证明（见 {@link LoadBalancingTest} /
 * {@link GrpcTargetTest}）；dial 层只做行为冒烟：
 * <ul>
 *   <li>注入缝 {@code channelConfigurator} 被回调<strong>恰一次</strong>，且接收的是
 *       一个 {@code ManagedChannelBuilder}（SDK 默认项已设置完毕之后）；</li>
 *   <li>用注入缝把 builder 换成 {@link InProcessChannelBuilder}，dial 不抛、能 build
 *       出 channel（明文 bare host:port 路径走通，不真正连）；</li>
 *   <li>TLS / dns:/// round_robin 分支：无端点难以强断言 -&gt; 至少断言 dial 不抛
 *       （regression-watch），证明 SslContext 构建 + 选项组合不崩。</li>
 * </ul>
 */
class GrpcChannelsDialTest {

    private static final int MAX_RECV = 4 * 1024 * 1024; // 4MB，任意正值

    /** 注入缝：计调用次数 + 捕获收到的 builder，并替换为 in-process builder。 */
    private static final class SwapToInProcess
            implements UnaryOperator<ManagedChannelBuilder<?>> {
        final AtomicInteger calls = new AtomicInteger();
        final AtomicReference<ManagedChannelBuilder<?>> received = new AtomicReference<>();
        private final String inProcessName;

        SwapToInProcess(String inProcessName) {
            this.inProcessName = inProcessName;
        }

        @Override
        public ManagedChannelBuilder<?> apply(ManagedChannelBuilder<?> builder) {
            calls.incrementAndGet();
            received.set(builder);
            // 换成 in-process（无需真实网络），保证 build() 不触发任何连接。
            return InProcessChannelBuilder.forName(inProcessName);
        }
    }

    private static void shutdownQuietly(ManagedChannel ch) {
        if (ch == null) {
            return;
        }
        ch.shutdownNow();
        try {
            ch.awaitTermination(2, TimeUnit.SECONDS);
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
        }
    }

    // ------------------------------------------------------------------
    // 明文 bare host:port：注入缝走通、被调用恰一次、能 build。
    // ------------------------------------------------------------------

    @Test
    @DisplayName("明文 bare host:port：注入缝被调用恰一次，dial 能 build 出 channel")
    void plaintextBareHostPortDialsViaSeam() {
        GrpcTarget tgt = GrpcTarget.parseGrpcTarget("ab-config-grpc:50051");
        SwapToInProcess seam = new SwapToInProcess("plaintext-bare");

        ManagedChannel ch = null;
        try {
            ch = GrpcChannels.dial(tgt, MAX_RECV, seam);
            assertNotNull(ch, "dial 应返回 channel");
            assertEquals(1, seam.calls.get(), "注入缝必须被调用恰一次");
            assertNotNull(seam.received.get(), "注入缝应收到非 null builder");
        } finally {
            shutdownQuietly(ch);
        }
    }

    @Test
    @DisplayName("注入缝是最后一手：dial build() 的是 configurator 返回的 builder（对齐 Go DialOptions 末尾追加）")
    void seamIsLastWordAndReceivesBuilder() {
        GrpcTarget tgt = GrpcTarget.parseGrpcTarget("host:1234");
        // 注入缝换成命名 in-process builder；若 dial build 的是 SDK 自己的 builder
        // 而非 configurator 的返回值，得到的 channel 不会是 in-process 形态。
        SwapToInProcess seam = new SwapToInProcess("seam-last-word");

        ManagedChannel ch = null;
        try {
            ch = GrpcChannels.dial(tgt, MAX_RECV, seam);
            // 收到的 builder 是 SDK 在默认项设置完成后传入的（非 null）。
            assertNotNull(seam.received.get(), "注入缝应收到 SDK 构造的 builder");
            // build() 必须基于 configurator 的返回值：原 forTarget("host:1234") builder
            // 的 channel authority 会是 "host:1234"；被替换成 InProcessChannelBuilder 后
            // authority 不再是它（in-process 默认 "localhost"）。该负向断言对版本漂移稳健。
            assertTrue(!"host:1234".equals(ch.authority()),
                    "dial 必须 build configurator 返回的 builder，而非原 SDK builder "
                            + "（注入缝是最后一手，对齐 Go DialOptions 末尾追加），got authority="
                            + ch.authority());
        } finally {
            shutdownQuietly(ch);
        }
    }

    @Test
    @DisplayName("channelConfigurator == null：dial 仍能 build（明文 bare，不真正连）")
    void nullConfiguratorStillBuilds() {
        // 注：无注入缝时走真实 ManagedChannelBuilder.forTarget(...).usePlaintext()。
        // grpc-java 懒连接，build() 不发起连接，故 bare host:port 不会因 DNS/连接失败抛。
        GrpcTarget tgt = GrpcTarget.parseGrpcTarget("ab-config-grpc:50051");
        ManagedChannel ch = null;
        try {
            ch = GrpcChannels.dial(tgt, MAX_RECV, null);
            assertNotNull(ch, "无注入缝时 dial 仍应返回 channel");
        } finally {
            shutdownQuietly(ch);
        }
    }

    // ------------------------------------------------------------------
    // regression-watch：各 target 形态 dial 不抛（注入缝换 in-process）。
    // TLS / dns:/// round_robin 在无端点下只能断言「不抛」。
    // ------------------------------------------------------------------

    static Stream<Arguments> dialNoThrowCases() {
        return Stream.of(
                Arguments.of("明文 bare host:port", "ab-config-grpc:50051"),
                Arguments.of("明文 grpc://", "grpc://host:50051"),
                Arguments.of("passthrough:/// 原生 resolver", "passthrough:///bufnet"),
                Arguments.of("dns:/// (round_robin 分支)", "dns:///svc.ns:443"),
                Arguments.of("TLS grpcs:// 普通", "grpcs://host:443"),
                Arguments.of("TLS grpcs:// + authority", "grpcs://host:443?authority=dom"),
                Arguments.of("TLS grpcs:// + insecure skip-verify", "grpcs://host:443?insecure=true"),
                Arguments.of("TLS grpcs:// + authority + insecure",
                        "grpcs://host:443?authority=dom&insecure=true"),
                Arguments.of("TLS grpcs:// IPv6", "grpcs://[::1]:443"),
                Arguments.of("dns:/// + TLS-less round_robin k8s fqdn",
                        "dns:///ab-config-grpc.svc.cluster.local:50051"));
    }

    @ParameterizedTest(name = "[{index}] dial 不抛 + 注入缝一次: {0}")
    @MethodSource("dialNoThrowCases")
    @DisplayName("dial 各 target 形态不抛，注入缝恰一次（regression-watch）")
    void dialDoesNotThrowAcrossTargets(String name, String addr) {
        GrpcTarget tgt = GrpcTarget.parseGrpcTarget(addr);
        SwapToInProcess seam = new SwapToInProcess("noThrow-" + addr.hashCode());

        AtomicReference<ManagedChannel> chRef = new AtomicReference<>();
        try {
            assertDoesNotThrow(() -> chRef.set(GrpcChannels.dial(tgt, MAX_RECV, seam)),
                    "dial 不应抛: " + addr);
            assertNotNull(chRef.get(), "dial 应返回 channel: " + addr);
            assertEquals(1, seam.calls.get(),
                    "注入缝必须被调用恰一次（SDK 默认项设置后回调一次）: " + addr);
        } finally {
            shutdownQuietly(chRef.get());
        }
    }

    // ------------------------------------------------------------------
    // TLS skip-verify 的 SslContext 构建路径单独冒烟（不经注入缝替换 builder 之前
    // 已构建 SslContext；用注入缝换 in-process 避免真实连接）。
    // ------------------------------------------------------------------

    @Test
    @DisplayName("grpcs://?insecure=true：SslContext(trust-all) 构建不抛，dial 走通")
    void tlsSkipVerifyBuildsSslContext() {
        GrpcTarget tgt = GrpcTarget.parseGrpcTarget("grpcs://host:443?insecure=true");
        // 前置断言：解析确实进入 TLS + skip-verify 分支。
        assertTrue(tgt.useTls(), "应为 TLS 分支");
        assertTrue(tgt.insecureSkipVerify(), "应为 skip-verify 分支");

        SwapToInProcess seam = new SwapToInProcess("tls-skipverify");
        ManagedChannel ch = null;
        try {
            ch = GrpcChannels.dial(tgt, MAX_RECV, seam);
            assertNotNull(ch);
            assertEquals(1, seam.calls.get());
        } finally {
            shutdownQuietly(ch);
        }
    }
}
