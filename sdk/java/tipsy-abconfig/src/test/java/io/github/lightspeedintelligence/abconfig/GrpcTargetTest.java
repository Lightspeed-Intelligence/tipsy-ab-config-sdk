package io.github.lightspeedintelligence.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Nested;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.Arguments;
import org.junit.jupiter.params.provider.CsvSource;
import org.junit.jupiter.params.provider.MethodSource;
import org.junit.jupiter.params.provider.ValueSource;

import java.util.stream.Stream;

/**
 * ST5 — {@link GrpcTarget#parseGrpcTarget(String)} 方案 Y 地址解析矩阵。
 *
 * <p>语义基准是 Go {@code grpc_target.go} / {@code grpc_target_test.go}：被测的契约
 * 是 <em>分类</em>（dialTarget / useTls / authority / insecureSkipVerify），不是 dial。
 *
 * <p>最关键的回归 backstop（F1/F9）：原生 grpc resolver 目标
 * （{@code passthrough:///}、{@code dns:///}、{@code unix:}、{@code xds:///}）与 bare
 * {@code host:port} 必须被判为「透传 + 明文」，使既有 bufconn / 内部 DNS dialing 不受影响。
 * 所有错误为参数错误（{@link ConfigValidationException}，消息前缀 {@code tipsyabconfig:}），
 * 在建 channel 前抛出。
 */
class GrpcTargetTest {

    // ------------------------------------------------------------------
    // 成功分类矩阵（每行：addr -> dialTarget/useTls/authority/insecureSkipVerify）
    // ------------------------------------------------------------------

    static Stream<Arguments> successCases() {
        return Stream.of(
                // ---- bare host:port -> 透传 + 明文（向后兼容） ----
                Arguments.of("bare hostname:port",
                        "ab-config-grpc:50051", "ab-config-grpc:50051", false, "", false),
                // F9 核心：127.0.0.1:443 不能被误读为 scheme；明文透传。
                Arguments.of("bare ipv4:port (F9 — 非 scheme)",
                        "127.0.0.1:443", "127.0.0.1:443", false, "", false),

                // ---- 原生 grpc resolver 目标 -> 透传 + 明文（F1） ----
                // F1 核心：含 "://" 但必须明文透传（bufnet bufconn backstop）。
                Arguments.of("passthrough:/// (F1 — bufconn backstop)",
                        "passthrough:///bufnet", "passthrough:///bufnet", false, "", false),
                Arguments.of("dns:/// 原生目标",
                        "dns:///svc.ns:443", "dns:///svc.ns:443", false, "", false),
                Arguments.of("unix: 原生目标",
                        "unix:/tmp/x", "unix:/tmp/x", false, "", false),
                Arguments.of("xds:/// 原生目标",
                        "xds:///foo", "xds:///foo", false, "", false),

                // ---- grpc:// 显式明文 ----
                Arguments.of("grpc:// 剥 scheme, 明文",
                        "grpc://host:50051", "host:50051", false, "", false),

                // ---- grpcs:// (TLS) ----
                Arguments.of("grpcs:// 无 query -> TLS, 无 authority",
                        "grpcs://host:443", "host:443", true, "", false),
                // 设计 Dev 接入串：authority + insecure=true。
                Arguments.of("grpcs:// authority + insecure=true (Dev串)",
                        "grpcs://host:443?authority=dom&insecure=true",
                        "host:443", true, "dom", true),
                Arguments.of("grpcs:// authority only (无 insecure)",
                        "grpcs://10.0.0.5:443?authority=ab-config-grpc.internal",
                        "10.0.0.5:443", true, "ab-config-grpc.internal", false),
                // insecure 取值 true/1/false/0 各对。
                Arguments.of("grpcs:// insecure=true",
                        "grpcs://host:443?insecure=true", "host:443", true, "", true),
                Arguments.of("grpcs:// insecure=1 truthy",
                        "grpcs://host:443?insecure=1", "host:443", true, "", true),
                Arguments.of("grpcs:// insecure=false explicit-verify",
                        "grpcs://host:443?insecure=false", "host:443", true, "", false),
                Arguments.of("grpcs:// insecure=0 falsy",
                        "grpcs://host:443?insecure=0", "host:443", true, "", false),

                // ---- IPv6 字面量 ----
                Arguments.of("grpcs:// IPv6 字面量 + port -> TLS, host 解析正确",
                        "grpcs://[::1]:443", "[::1]:443", true, "", false),
                Arguments.of("grpcs:// IPv6 字面量 + authority",
                        "grpcs://[2001:db8::1]:443?authority=ab-config-grpc.internal",
                        "[2001:db8::1]:443", true, "ab-config-grpc.internal", false));
    }

    @ParameterizedTest(name = "[{index}] {0}")
    @MethodSource("successCases")
    @DisplayName("parseGrpcTarget: 合法地址 -> 正确分类")
    void parseSuccess(String name, String addr, String wantDial, boolean wantTls,
            String wantAuthority, boolean wantInsecure) {
        GrpcTarget got = GrpcTarget.parseGrpcTarget(addr);
        assertEquals(wantDial, got.dialTarget(), "dialTarget for " + addr);
        assertEquals(wantTls, got.useTls(), "useTls for " + addr);
        assertEquals(wantAuthority, got.authority(), "authority for " + addr);
        assertEquals(wantInsecure, got.insecureSkipVerify(), "insecureSkipVerify for " + addr);
    }

    // ------------------------------------------------------------------
    // 错误矩阵：所有路径抛 ConfigValidationException，前缀 tipsyabconfig:，
    // 且消息含可区分关键字（捕捉「重写时丢掉契约意图」）。
    // ------------------------------------------------------------------

    static Stream<Arguments> errorCases() {
        return Stream.of(
                // grpc:// 带 query（authority / insecure）-> 参数错误。
                Arguments.of("grpc:// 带 authority query 拒绝",
                        "grpc://host:50051?authority=x", "query parameters are only valid"),
                Arguments.of("grpc:// 带 insecure query 拒绝",
                        "grpc://host:50051?insecure=true", "query parameters are only valid"),

                // grpcs:// 缺端口 -> "explicit port"（与 invalid port 区分）。
                Arguments.of("grpcs:// 缺端口",
                        "grpcs://host", "explicit port"),
                Arguments.of("grpcs:// IPv6 缺端口",
                        "grpcs://[::1]", "explicit port"),

                // grpcs:// 非数字 / 越界端口 -> "invalid port"。
                Arguments.of("grpcs:// 非数字端口",
                        "grpcs://host:abc", "invalid port"),
                Arguments.of("grpcs:// IPv6 非数字端口",
                        "grpcs://[::1]:abc", "invalid port"),
                Arguments.of("grpcs:// 越界端口 99999",
                        "grpcs://host:99999", "invalid port"),

                // grpcs:// 缺 host（:443）-> 缺 host 报错。
                Arguments.of("grpcs:// 缺 host (:443)",
                        "grpcs://:443", "missing a host"),

                // grpcs:// 未知 query key / 非法 insecure 值。
                Arguments.of("grpcs:// 未知 query key 拒绝",
                        "grpcs://host:443?foo=bar", "unknown query parameter"),
                Arguments.of("grpcs:// 非法 insecure 值拒绝",
                        "grpcs://host:443?insecure=maybe", "invalid insecure value"),

                // http:// / https:// 在 gRPC 模式 -> 提示用 Transport=http。
                Arguments.of("http:// 在 gRPC 模式拒绝",
                        "http://lb.internal:8080", "HTTP base URL"),
                Arguments.of("https:// 在 gRPC 模式拒绝",
                        "https://lb.internal:8443", "HTTP base URL"));
    }

    @ParameterizedTest(name = "[{index}] {0}")
    @MethodSource("errorCases")
    @DisplayName("parseGrpcTarget: 非法地址 -> ConfigValidationException(tipsyabconfig: + 关键字)")
    void parseError(String name, String addr, String wantKeyword) {
        ConfigValidationException ex = assertThrows(ConfigValidationException.class,
                () -> GrpcTarget.parseGrpcTarget(addr), "expected error for " + addr);
        String msg = ex.getMessage();
        assertTrue(msg.startsWith("tipsyabconfig:"),
                "message must carry tipsyabconfig: prefix, got: " + msg);
        assertTrue(msg.contains(wantKeyword),
                "message must contain keyword \"" + wantKeyword + "\", got: " + msg);
    }

    // ------------------------------------------------------------------
    // 显式 backstop：缺端口 vs 非数字端口必须是两个不同错误（设计明确区分）。
    // ------------------------------------------------------------------

    @Test
    @DisplayName("缺端口与非数字端口报不同错误关键字（explicit port vs invalid port）")
    void missingPortVsInvalidPortAreDistinct() {
        ConfigValidationException missing = assertThrows(ConfigValidationException.class,
                () -> GrpcTarget.parseGrpcTarget("grpcs://host"));
        ConfigValidationException invalid = assertThrows(ConfigValidationException.class,
                () -> GrpcTarget.parseGrpcTarget("grpcs://host:abc"));

        assertTrue(missing.getMessage().contains("explicit port"),
                "缺端口应报 explicit port: " + missing.getMessage());
        assertFalse(missing.getMessage().contains("invalid port"),
                "缺端口不应误报 invalid port: " + missing.getMessage());

        assertTrue(invalid.getMessage().contains("invalid port"),
                "非数字端口应报 invalid port: " + invalid.getMessage());
        assertFalse(invalid.getMessage().contains("explicit port"),
                "非数字端口不应误报 explicit port: " + invalid.getMessage());
    }

    // ------------------------------------------------------------------
    // ConfigValidationException 是 TipsyConfigException 的子类（异常体系）。
    // ------------------------------------------------------------------

    @Test
    @DisplayName("解析错误为 ConfigValidationException 且是 TipsyConfigException 子类")
    void errorIsConfigValidationSubclassOfTipsyConfig() {
        ConfigValidationException ex = assertThrows(ConfigValidationException.class,
                () -> GrpcTarget.parseGrpcTarget("http://x"));
        assertTrue(ex instanceof TipsyConfigException,
                "ConfigValidationException 必须是 TipsyConfigException 子类");
    }

    // ------------------------------------------------------------------
    // 标准化的 F1 / F9 backstop（独立命名，便于断点定位回归）。
    // ------------------------------------------------------------------

    @Nested
    @DisplayName("回归 backstop（F1/F9）")
    class RegressionBackstops {

        @ParameterizedTest(name = "passthrough 透传明文: {0}")
        @ValueSource(strings = {"passthrough:///bufnet-config", "passthrough:///bufnet-abtest"})
        @DisplayName("passthrough:/// 必须透传明文（F1 — bufconn backstop）")
        void passthroughIsPlaintext(String addr) {
            GrpcTarget got = GrpcTarget.parseGrpcTarget(addr);
            assertEquals(addr, got.dialTarget(), "dialTarget 必须逐字透传");
            assertFalse(got.useTls(), "原生 resolver 目标必须保持明文");
            assertEquals("", got.authority(), "authority 必须为空（不覆盖）");
            assertFalse(got.insecureSkipVerify(), "insecureSkipVerify 必须为 false");
        }

        @Test
        @DisplayName("bare 127.0.0.1:443 明文透传（F9 — 不误判为 TLS）")
        void bareLoopbackIsPlaintext() {
            GrpcTarget got = GrpcTarget.parseGrpcTarget("127.0.0.1:443");
            assertFalse(got.useTls(), "bare 127.0.0.1:443 不应被误判为 TLS");
            assertEquals("127.0.0.1:443", got.dialTarget());
        }
    }

    // ------------------------------------------------------------------
    // insecure 取值的 CSV 矩阵（与 MethodSource 重叠，但用 CsvSource 保证
    // 真值表完整、断点定位单值）。
    // ------------------------------------------------------------------

    @ParameterizedTest(name = "insecure={0} -> {1}")
    @CsvSource({
            "true,  true",
            "1,     true",
            "false, false",
            "0,     false"
    })
    @DisplayName("grpcs:// insecure 真值表 true/1/false/0")
    void insecureTruthTable(String raw, boolean expected) {
        GrpcTarget got = GrpcTarget.parseGrpcTarget("grpcs://host:443?insecure=" + raw);
        assertTrue(got.useTls());
        assertEquals(expected, got.insecureSkipVerify(),
                "insecure=" + raw + " should map to " + expected);
    }
}
