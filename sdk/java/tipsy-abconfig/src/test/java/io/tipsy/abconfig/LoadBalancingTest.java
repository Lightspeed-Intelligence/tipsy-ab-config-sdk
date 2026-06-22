package io.tipsy.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNull;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.ValueSource;

/**
 * ST5 — {@link GrpcTarget#loadBalancingPolicyFor(String)} 的 Headless +
 * round_robin opt-in 映射。
 *
 * <p>语义基准 Go {@code serviceConfigFor}（{@code sdk.go}） /
 * {@code sdk_service_config_test.go}：仅 {@code dns:///} 前缀的 dialTarget 选
 * {@code round_robin}，其它一切（bare host:port / grpc:// / grpcs://[?...] /
 * passthrough:/// / unix: / xds:///）保持 grpc-java 默认 {@code pick_first}
 * （此处映射为返回 {@code null}）。这是 AC #10 的回归约束：只有 dns:/// 才注入。
 */
class LoadBalancingTest {

    @ParameterizedTest(name = "dns:/// -> round_robin: {0}")
    @ValueSource(strings = {
            "dns:///x",
            "dns:///foo",
            "dns:///foo:50051",
            "dns:///ab-config-grpc.svc.cluster.local:50051"
    })
    @DisplayName("dns:/// 目标 -> round_robin")
    void dnsPrefixSelectsRoundRobin(String target) {
        assertEquals("round_robin", GrpcTarget.loadBalancingPolicyFor(target),
                "dns:/// 目标必须选 round_robin: " + target);
    }

    @Test
    @DisplayName("dns:/// 空 host 边界仍走 round_robin（前缀匹配）")
    void dnsEmptyHostStillRoundRobin() {
        // 边界：startsWith("dns:///", "dns:///")=true，空 host 仍命中 round_robin
        // 分支（DNS resolve 失败延后到首 RPC，而非 SDK init）。与 Go 行为对齐。
        assertEquals("round_robin", GrpcTarget.loadBalancingPolicyFor("dns:///"));
    }

    @ParameterizedTest(name = "非 dns:/// -> null（默认 pick_first）: {0}")
    @ValueSource(strings = {
            "",
            "foo:50051",
            "foo.bar:50051",
            "ab-config-grpc:50051",
            "grpc://foo:50051",
            "grpcs://foo:443",
            "grpcs://foo:443?authority=x.y&insecure=true",
            "passthrough:///foo:50051",
            "passthrough:///bufnet",
            "unix:/tmp/abconfig.sock",
            "xds:///foo"
    })
    @DisplayName("非 dns:/// 前缀 -> null（保持默认 pick_first，AC #10）")
    void otherPrefixesReturnNull(String target) {
        assertNull(GrpcTarget.loadBalancingPolicyFor(target),
                "AC #10: 只有 dns:/// 目标才注入 LB policy，其它必须为 null: " + target);
    }
}
