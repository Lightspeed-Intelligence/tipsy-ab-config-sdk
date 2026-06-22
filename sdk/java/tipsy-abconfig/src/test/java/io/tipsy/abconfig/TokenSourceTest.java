package io.tipsy.abconfig;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertSame;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import io.grpc.CallCredentials;
import io.grpc.Metadata;
import io.grpc.Status;
import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import java.util.concurrent.Executor;

/**
 * ST5 — {@link TokenSource}：gRPC {@link CallCredentials} + HTTP header 双形态。
 *
 * <p>语义基准 Go {@code tokenSource}（{@code sdk.go}）：
 * <ul>
 *   <li>provider 优先于静态 token；</li>
 *   <li>metadata key 小写 {@code authorization}，value {@code "Bearer <token>"}；</li>
 *   <li>provider 抛异常 -&gt; {@code applier.fail(Status.UNAUTHENTICATED)}；</li>
 *   <li>HTTP header 同一逻辑产出 {@code "Bearer <token>"}；supplier 在 provider
 *       抛异常时 rethrow（unchecked）。</li>
 * </ul>
 */
class TokenSourceTest {

    private static final String STATIC_TOKEN = "static-tok-123";
    private static final Metadata.Key<String> AUTHORIZATION =
            Metadata.Key.of("authorization", Metadata.ASCII_STRING_MARSHALLER);

    // ------------------------------------------------------------------
    // 假的 MetadataApplier：捕获 apply(Metadata) 或 fail(Status)，二选一。
    // ------------------------------------------------------------------

    private static final class CapturingApplier extends CallCredentials.MetadataApplier {
        Metadata appliedHeaders;
        Status failStatus;
        int applyCalls;
        int failCalls;

        @Override
        public void apply(Metadata headers) {
            this.appliedHeaders = headers;
            this.applyCalls++;
        }

        @Override
        public void fail(Status status) {
            this.failStatus = status;
            this.failCalls++;
        }
    }

    /** 同步执行的 Executor：直接在当前线程跑（避免异步 flakiness）。 */
    private static final Executor DIRECT_EXECUTOR = Runnable::run;

    /**
     * 驱动 {@code applyRequestMetadata} 一次。requestInfo 实现从不读取，传 null 安全
     * （被测实现仅调用 {@code source.token()}）。
     */
    private static CapturingApplier driveCallCredentials(TokenSource ts) {
        CapturingApplier applier = new CapturingApplier();
        CallCredentials cc = ts.toCallCredentials();
        cc.applyRequestMetadata(null, DIRECT_EXECUTOR, applier);
        return applier;
    }

    // ------------------------------------------------------------------
    // 静态 token: token() / authHeaderValue() / httpAuthHeaderSupplier()。
    // ------------------------------------------------------------------

    @Test
    @DisplayName("静态 token: token() 返回原值")
    void staticTokenValue() throws Exception {
        TokenSource ts = TokenSource.of(STATIC_TOKEN, null);
        assertEquals(STATIC_TOKEN, ts.token());
    }

    @Test
    @DisplayName("静态 token: authHeaderValue() == \"Bearer <token>\"")
    void staticAuthHeaderValue() throws Exception {
        TokenSource ts = TokenSource.of(STATIC_TOKEN, null);
        assertEquals("Bearer " + STATIC_TOKEN, ts.authHeaderValue());
    }

    @Test
    @DisplayName("静态 token: httpAuthHeaderSupplier().get() 同 authHeaderValue()")
    void staticHttpSupplierMatchesHeader() throws Exception {
        TokenSource ts = TokenSource.of(STATIC_TOKEN, null);
        assertEquals(ts.authHeaderValue(), ts.httpAuthHeaderSupplier().get());
        assertEquals("Bearer " + STATIC_TOKEN, ts.httpAuthHeaderSupplier().get());
    }

    // ------------------------------------------------------------------
    // provider 优先于静态：两者都设时用 provider 值。
    // ------------------------------------------------------------------

    @Test
    @DisplayName("provider 优先于静态 token（两者都设时用 provider 值）")
    void providerTakesPrecedenceOverStatic() throws Exception {
        TokenProvider provider = () -> "dynamic-tok-xyz";
        TokenSource ts = TokenSource.of(STATIC_TOKEN, provider);
        assertEquals("dynamic-tok-xyz", ts.token());
        assertEquals("Bearer dynamic-tok-xyz", ts.authHeaderValue());
        assertEquals("Bearer dynamic-tok-xyz", ts.httpAuthHeaderSupplier().get());
    }

    // ------------------------------------------------------------------
    // 静态 token 经 CallCredentials -> metadata 含小写 authorization: Bearer <token>。
    // ------------------------------------------------------------------

    @Test
    @DisplayName("静态 token 经 CallCredentials -> 小写 authorization: Bearer <token>")
    void staticTokenAppliesAuthorizationMetadata() {
        TokenSource ts = TokenSource.of(STATIC_TOKEN, null);
        CapturingApplier applier = driveCallCredentials(ts);

        assertEquals(1, applier.applyCalls, "applier.apply 应被调用一次");
        assertEquals(0, applier.failCalls, "成功路径不应 fail");
        assertNotNull(applier.appliedHeaders, "headers 应被传入");
        assertEquals("Bearer " + STATIC_TOKEN, applier.appliedHeaders.get(AUTHORIZATION),
                "authorization metadata 值必须为 Bearer <token>");
    }

    @Test
    @DisplayName("provider token 经 CallCredentials -> authorization: Bearer <providerToken>")
    void providerTokenAppliesAuthorizationMetadata() {
        TokenProvider provider = () -> "dyn-99";
        TokenSource ts = TokenSource.of(STATIC_TOKEN, provider);
        CapturingApplier applier = driveCallCredentials(ts);

        assertEquals(1, applier.applyCalls);
        assertEquals("Bearer dyn-99", applier.appliedHeaders.get(AUTHORIZATION),
                "provider 优先：metadata 必须用 provider 值");
    }

    // ------------------------------------------------------------------
    // provider 抛异常: applyRequestMetadata -> fail(Status.UNAUTHENTICATED)。
    // ------------------------------------------------------------------

    @Test
    @DisplayName("provider 抛异常 -> applier.fail(Status.UNAUTHENTICATED)，不 apply")
    void providerFailureFailsRpcWithUnauthenticated() {
        Exception boom = new IllegalStateException("token endpoint down");
        TokenProvider provider = () -> {
            throw boom;
        };
        TokenSource ts = TokenSource.of(null, provider);
        CapturingApplier applier = driveCallCredentials(ts);

        assertEquals(0, applier.applyCalls, "失败路径不应 apply headers");
        assertEquals(1, applier.failCalls, "失败路径应调用 fail 恰一次");
        assertNull(applier.appliedHeaders, "失败路径不应传 headers");
        assertNotNull(applier.failStatus, "fail status 不应为 null");
        assertEquals(Status.Code.UNAUTHENTICATED, applier.failStatus.getCode(),
                "失败 status code 必须为 UNAUTHENTICATED");
        assertSame(boom, applier.failStatus.getCause(),
                "fail status 应携带原始异常为 cause");
    }

    // ------------------------------------------------------------------
    // httpAuthHeaderSupplier 在 provider 抛异常时 rethrow（unchecked）。
    // ------------------------------------------------------------------

    @Test
    @DisplayName("httpAuthHeaderSupplier: checked provider 异常 -> 包成 unchecked RuntimeException 抛出")
    void httpSupplierWrapsCheckedProviderException() {
        Exception checked = new Exception("checked provider failure");
        TokenProvider provider = () -> {
            throw checked;
        };
        TokenSource ts = TokenSource.of(null, provider);

        RuntimeException ex = assertThrows(RuntimeException.class,
                () -> ts.httpAuthHeaderSupplier().get(),
                "supplier 契约 unchecked：checked 异常必须被包装重抛");
        assertSame(checked, ex.getCause(),
                "包装后的 RuntimeException 应以原 checked 异常为 cause");
    }

    @Test
    @DisplayName("httpAuthHeaderSupplier: unchecked provider 异常 -> 原样重抛（不双重包装）")
    void httpSupplierRethrowsUncheckedAsIs() {
        RuntimeException runtime = new IllegalStateException("unchecked provider failure");
        TokenProvider provider = () -> {
            throw runtime;
        };
        TokenSource ts = TokenSource.of(null, provider);

        RuntimeException ex = assertThrows(RuntimeException.class,
                () -> ts.httpAuthHeaderSupplier().get());
        assertSame(runtime, ex,
                "unchecked 异常应原样重抛，不应被再包一层 RuntimeException");
    }

    // ------------------------------------------------------------------
    // CallCredentials 类型形状（异常体系/契约锚定）。
    // ------------------------------------------------------------------

    @Test
    @DisplayName("toCallCredentials 返回 BearerCallCredentials（extends io.grpc.CallCredentials）")
    void toCallCredentialsTypeShape() {
        TokenSource ts = TokenSource.of(STATIC_TOKEN, null);
        CallCredentials cc = ts.toCallCredentials();
        assertNotNull(cc);
        assertTrue(cc instanceof TokenSource.BearerCallCredentials,
                "应为嵌套 BearerCallCredentials");
        assertTrue(cc instanceof CallCredentials,
                "必须是 io.grpc.CallCredentials 子类");
    }

    @Test
    @DisplayName("provider 优先：provider 成功时静态 token 完全不被使用（CallCredentials 路径）")
    void providerPathIgnoresStaticToken() {
        TokenProvider provider = () -> "only-provider";
        TokenSource ts = TokenSource.of("should-be-ignored", provider);
        CapturingApplier applier = driveCallCredentials(ts);
        assertEquals("Bearer only-provider", applier.appliedHeaders.get(AUTHORIZATION));
        assertFalse("Bearer should-be-ignored".equals(applier.appliedHeaders.get(AUTHORIZATION)),
                "静态 token 不应出现在 metadata 中");
    }
}
