package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertTrue;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.CsvSource;
import org.junit.jupiter.params.provider.EnumSource;
import org.junit.jupiter.params.provider.NullSource;
import org.junit.jupiter.params.provider.ValueSource;

/**
 * ST5 — {@link TransportResolver}：transport-mode 解析与 HTTP base-URL 校验。
 *
 * <p>语义基准 Go {@code resolveTransport} / {@code validateHTTPBaseURL}
 * （{@code sdk.go}）。优先级：显式 Config &gt; env {@code TIPSY_SDK_TRANSPORT} &gt;
 * 默认 grpc；非法值是参数错误（{@link ConfigValidationException}）。
 *
 * <p>约束：env 相关只用纯函数 {@link TransportResolver#resolveTransportFromEnv(String)}
 * 测，<strong>从不修改进程 env</strong>。{@link TransportResolver#resolveTransport(Transport)}
 * 仅在 configured 非 null 时测试（短路返回），避免读取/依赖真实 env。
 */
class TransportResolverTest {

    // ------------------------------------------------------------------
    // resolveTransport(Transport): 显式非 null 短路返回（不碰 env）。
    // ------------------------------------------------------------------

    @ParameterizedTest
    @EnumSource(Transport.class)
    @DisplayName("resolveTransport(非null) 短路返回同值（不读 env）")
    void resolveTransportExplicitShortCircuits(Transport configured) {
        assertSame(configured, TransportResolver.resolveTransport(configured),
                "显式非 null transport 必须原样短路返回");
    }

    // ------------------------------------------------------------------
    // resolveTransportFromEnv(String): null / 空 / grpc / http / 大小写混合 /
    // 带空白 / 非法。纯函数，不动进程 env。
    // ------------------------------------------------------------------

    @Test
    @DisplayName("resolveTransportFromEnv(null) -> GRPC（默认）")
    void envNullDefaultsGrpc() {
        assertSame(Transport.GRPC, TransportResolver.resolveTransportFromEnv(null));
    }

    @ParameterizedTest(name = "env=[{0}] -> GRPC")
    @ValueSource(strings = {"", "   ", "\t", "\n"})
    @DisplayName("resolveTransportFromEnv(空/空白) -> GRPC（默认）")
    void envBlankDefaultsGrpc(String raw) {
        assertSame(Transport.GRPC, TransportResolver.resolveTransportFromEnv(raw),
                "空或纯空白 env 必须默认 GRPC: [" + raw + "]");
    }

    @ParameterizedTest(name = "env=[{0}] -> {1}")
    @CsvSource({
            // grpc 大小写混合 + 带空白（前后空格 trim）
            "grpc,       GRPC",
            "GRPC,       GRPC",
            "Grpc,       GRPC",
            "'  grpc  ', GRPC",
            // http 大小写混合 + 带空白
            "http,       HTTP",
            "HTTP,       HTTP",
            "Http,       HTTP",
            "'  http  ', HTTP",
            "' HtTp ',   HTTP"
    })
    @DisplayName("resolveTransportFromEnv: grpc/http 大小写混合 + 带空白 -> 对应枚举")
    void envValidValues(String raw, Transport expected) {
        assertSame(expected, TransportResolver.resolveTransportFromEnv(raw),
                "env [" + raw + "] 应解析为 " + expected);
    }

    @Test
    @DisplayName("resolveTransportFromEnv: 制表/换行包裹 grpc 也 trim -> GRPC")
    void envTabWrappedValueTrimmed() {
        assertSame(Transport.GRPC, TransportResolver.resolveTransportFromEnv("\tgrpc\n"),
                "前后制表/换行应被 trim 后识别为 grpc");
        assertSame(Transport.HTTP, TransportResolver.resolveTransportFromEnv(" \thttp\r\n "),
                "前后空白（含制表/CR/LF）应被 trim 后识别为 http");
    }

    @ParameterizedTest(name = "非法 env=[{0}]")
    @ValueSource(strings = {"grpcs", "https", "tcp", "GRPCX", "rest", "h2", "grpc-web"})
    @DisplayName("resolveTransportFromEnv(非法值) -> ConfigValidationException(tipsyabconfig:)")
    void envInvalidValuesThrow(String raw) {
        ConfigValidationException ex = assertThrows(ConfigValidationException.class,
                () -> TransportResolver.resolveTransportFromEnv(raw),
                "非法 transport 值必须抛 ConfigValidationException: " + raw);
        assertTrue(ex.getMessage().startsWith("tipsyabconfig:"),
                "消息前缀必须为 tipsyabconfig:, got: " + ex.getMessage());
        assertTrue(ex.getMessage().contains("invalid transport"),
                "消息应含 invalid transport 关键字, got: " + ex.getMessage());
    }

    // ------------------------------------------------------------------
    // validateHttpBaseURL(field, addr): http/https 接受 + 去尾斜杠（多个尾斜杠）；
    // 非 http(s) 拒绝；null 拒绝。
    // ------------------------------------------------------------------

    @ParameterizedTest(name = "base=[{0}] -> [{1}]")
    @CsvSource({
            // 无尾斜杠原样
            "http://x,                       http://x",
            "https://x,                      https://x",
            "http://host:8080,               http://host:8080",
            "https://dev-ab-config.example,  https://dev-ab-config.example",
            // 单个尾斜杠去除
            "http://x/,                       http://x",
            "https://x/,                      https://x",
            // 多个尾斜杠全部去除
            "http://x///,                     http://x",
            "https://host:8080////,           https://host:8080",
            // 路径保留，仅去尾斜杠
            "https://host/base/,              https://host/base",
            "https://host/base//,             https://host/base"
    })
    @DisplayName("validateHttpBaseURL: http/https 接受 + 去尾斜杠（含多个）")
    void httpBaseUrlAcceptsAndTrimsTrailingSlash(String addr, String expected) {
        assertEquals(expected, TransportResolver.validateHttpBaseURL("configServiceAddr", addr),
                "base [" + addr + "] 应规范化为 [" + expected + "]");
    }

    @ParameterizedTest(name = "非 http(s) base=[{0}]")
    @ValueSource(strings = {
            "ftp://x",
            "grpc://x",
            "grpcs://x",
            "x:8080",
            "host:8080",
            "//host",
            "HTTP://x",   // 大小写敏感：必须小写 http://
            "HTTPS://x",
            ""
    })
    @DisplayName("validateHttpBaseURL(非 http(s)) -> ConfigValidationException(tipsyabconfig:)")
    void httpBaseUrlRejectsNonHttp(String addr) {
        ConfigValidationException ex = assertThrows(ConfigValidationException.class,
                () -> TransportResolver.validateHttpBaseURL("abtestServiceAddr", addr),
                "非 http(s) base 必须被拒绝: " + addr);
        assertTrue(ex.getMessage().startsWith("tipsyabconfig:"),
                "消息前缀必须为 tipsyabconfig:, got: " + ex.getMessage());
        assertTrue(ex.getMessage().contains("abtestServiceAddr"),
                "消息应携带 field 名以便定位, got: " + ex.getMessage());
        assertTrue(ex.getMessage().contains("http://") && ex.getMessage().contains("https://"),
                "消息应提示需 http:// 或 https://, got: " + ex.getMessage());
    }

    @ParameterizedTest
    @NullSource
    @DisplayName("validateHttpBaseURL(null) -> ConfigValidationException（不 NPE）")
    void httpBaseUrlRejectsNull(String addr) {
        ConfigValidationException ex = assertThrows(ConfigValidationException.class,
                () -> TransportResolver.validateHttpBaseURL("configServiceAddr", addr),
                "null base 必须被拒绝而非 NPE");
        assertTrue(ex.getMessage().startsWith("tipsyabconfig:"),
                "消息前缀必须为 tipsyabconfig:, got: " + ex.getMessage());
    }

    // ------------------------------------------------------------------
    // 常量名锚定（Go transportEnvVarName 对齐）。
    // ------------------------------------------------------------------

    @Test
    @DisplayName("TRANSPORT_ENV_VAR 常量值锚定 TIPSY_SDK_TRANSPORT")
    void envVarConstantPinned() {
        assertEquals("TIPSY_SDK_TRANSPORT", TransportResolver.TRANSPORT_ENV_VAR);
    }
}
