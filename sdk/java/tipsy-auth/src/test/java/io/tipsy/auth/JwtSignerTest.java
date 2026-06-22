package io.tipsy.auth;

import static java.nio.charset.StandardCharsets.UTF_8;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertThrows;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.time.Duration;
import java.time.Instant;
import java.util.Base64;
import java.util.List;

import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

/**
 * Unit tests for {@link JwtSigner} (ST7, tipsy-auth HS256 signing).
 *
 * <p>These tests deliberately decode the raw base64url JWT segments and assert
 * directly on the decoded JSON <em>text</em> (rather than relying solely on a
 * jjwt re-parse), because a structural re-parse can mask the difference between
 * an <em>omitted</em> claim and a present-but-empty array ({@code "roles":[]}).
 * The on-wire claim shape is the byte-level compatibility contract with the
 * server (see Go {@code sdk/go/tipsyauth/signer.go}), so it is asserted on the
 * actual JSON bytes.
 *
 * <p>The signature (#3, #6) is independently recomputed with a plain JCA
 * {@code Mac}/{@code SecretKeySpec} over the {@code header.payload} input. This
 * is the regression guard that the implementation does NOT fall back to jjwt's
 * {@code Keys.hmacShaKeyFor(...)} / built-in {@code Jwts.SIG.HS256} (which would
 * reject short secrets with a {@code WeakKeyException}): a short secret must
 * still sign, and the produced signature must match a vanilla HMAC-SHA256 over
 * the same raw secret bytes — exactly what golang-jwt and the server compute.
 */
final class JwtSignerTest {

    /** >= 32 bytes so the long-key path may use jjwt verifyWith without a weak-key error. */
    private static final String LONG_SECRET = "a-32-byte-or-longer-secret-string!!";

    /** 12 bytes, well under the JWA 256-bit (32-byte) HS256 minimum — the regression case. */
    private static final String SHORT_SECRET = "short-secret";

    /** Fixed issue instant with a sub-second component, to prove second-granularity truncation. */
    private static final Instant FIXED_IAT = Instant.ofEpochSecond(1_781_254_817L, 123_456_789L);

    private static final Base64.Decoder URL_DECODER = Base64.getUrlDecoder();
    private static final Base64.Encoder URL_ENCODER_NOPAD = Base64.getUrlEncoder().withoutPadding();

    // ---------------------------------------------------------------------
    // helpers
    // ---------------------------------------------------------------------

    /** Returns the three raw (still base64url-encoded) JWT segments. */
    private static String[] segments(String token) {
        String[] parts = token.split("\\.");
        assertEquals(3, parts.length,
                "a compact HS256 JWT must have exactly 3 dot-separated segments, got: " + token);
        return parts;
    }

    /** Decodes one base64url segment to its raw UTF-8 JSON text. */
    private static String decodeJson(String segment) {
        return new String(URL_DECODER.decode(segment), UTF_8);
    }

    /** Independent HMAC-SHA256 over the signing input, base64url(no padding) encoded. */
    private static String hmacSha256B64Url(String secret, String signingInput) {
        try {
            Mac mac = Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(secret.getBytes(UTF_8), "HmacSHA256"));
            byte[] sig = mac.doFinal(signingInput.getBytes(UTF_8));
            return URL_ENCODER_NOPAD.encodeToString(sig);
        } catch (Exception e) {
            throw new AssertionError("independent HMAC computation failed", e);
        }
    }

    /**
     * Extracts the integer value of a numeric JSON claim from raw payload JSON
     * text, asserting it is encoded as an integer (no decimal point, no quotes,
     * no exponent) — i.e. a JWT NumericDate in unix seconds.
     */
    private static long numericClaim(String payloadJson, String name) {
        // matches  "name": 1781254817   (optional surrounding whitespace)
        java.util.regex.Matcher m = java.util.regex.Pattern
                .compile("\"" + name + "\"\\s*:\\s*(-?\\d+)([,}])")
                .matcher(payloadJson);
        assertTrue(m.find(),
                "claim \"" + name + "\" not present as a bare integer in payload JSON: " + payloadJson);
        return Long.parseLong(m.group(1));
    }

    // ---------------------------------------------------------------------
    // #1 happy path: header / payload shape, iat/exp seconds, exp-iat==ttl
    // ---------------------------------------------------------------------

    @Test
    @DisplayName("#1 issue: header + payload claim shape, iat/exp as unix seconds, exp-iat == ttl")
    void issue_happyPath_claimShapeAndTimes() {
        JwtSigner signer = JwtSigner.create(LONG_SECRET);
        IssueOptions opts = IssueOptions.builder()
                .subject("svc")
                .roles(List.of("business_sdk"))
                .namespaces(List.of("*"))
                .ttl(Duration.ofMinutes(5))
                .issuedAt(FIXED_IAT)
                .build();

        String token = signer.issue(opts);
        String[] seg = segments(token);
        String headerJson = decodeJson(seg[0]);
        String payloadJson = decodeJson(seg[1]);

        // --- header: exactly alg=HS256 + typ=JWT, nothing else ---
        assertTrue(headerJson.contains("\"alg\":\"HS256\""),
                "header must declare alg HS256: " + headerJson);
        assertTrue(headerJson.contains("\"typ\":\"JWT\""),
                "header must declare typ JWT: " + headerJson);
        // No other header parameters (e.g. no "kid", "cty", "zip").
        assertEquals(2, countJsonKeys(headerJson),
                "header must carry exactly two parameters (alg, typ): " + headerJson);

        // --- payload: required claims present ---
        assertTrue(payloadJson.contains("\"roles\":[\"business_sdk\"]"),
                "roles claim must be a JSON array of the supplied role: " + payloadJson);
        assertTrue(payloadJson.contains("\"namespaces\":[\"*\"]"),
                "namespaces claim must be a JSON array of the supplied namespace: " + payloadJson);
        assertTrue(payloadJson.contains("\"sub\":\"svc\""),
                "sub claim must equal the subject: " + payloadJson);
        assertTrue(payloadJson.contains("\"iat\":"), "iat claim must be present: " + payloadJson);
        assertTrue(payloadJson.contains("\"exp\":"), "exp claim must be present: " + payloadJson);

        // --- payload: forbidden standard claims absent ---
        assertFalse(payloadJson.contains("\"nbf\""), "nbf must be absent: " + payloadJson);
        assertFalse(payloadJson.contains("\"iss\""), "iss must be absent: " + payloadJson);
        assertFalse(payloadJson.contains("\"aud\""), "aud must be absent: " + payloadJson);

        // --- iat/exp are integers (unix seconds); exp-iat == ttl seconds ---
        long iat = numericClaim(payloadJson, "iat");
        long exp = numericClaim(payloadJson, "exp");
        assertEquals(FIXED_IAT.getEpochSecond(), iat,
                "iat must be the fixed issuedAt truncated to unix seconds");
        assertEquals(300L, exp - iat, "exp - iat must equal the 5-minute ttl in seconds");
    }

    // ---------------------------------------------------------------------
    // #2 empty roles/namespaces serialize to present empty arrays, not omitted
    // ---------------------------------------------------------------------

    @Test
    @DisplayName("#2 issue: unset roles/namespaces emit present empty arrays \"roles\":[] / \"namespaces\":[]")
    void issue_emptyRolesNamespaces_emitEmptyArrays() {
        JwtSigner signer = JwtSigner.create(LONG_SECRET);
        // roles/namespaces left unset entirely.
        IssueOptions opts = IssueOptions.builder()
                .subject("svc")
                .ttl(Duration.ofMinutes(5))
                .issuedAt(FIXED_IAT)
                .build();

        String payloadJson = decodeJson(segments(signer.issue(opts))[1]);

        // Present AND empty — the key distinction from an omitted claim.
        assertTrue(payloadJson.matches(".*\"roles\"\\s*:\\s*\\[\\s*\\].*"),
                "roles must be present as an empty array []: " + payloadJson);
        assertTrue(payloadJson.matches(".*\"namespaces\"\\s*:\\s*\\[\\s*\\].*"),
                "namespaces must be present as an empty array []: " + payloadJson);
        // Guard against accidental null serialization.
        assertFalse(payloadJson.contains("\"roles\":null"),
                "roles must not serialize as null: " + payloadJson);
        assertFalse(payloadJson.contains("\"namespaces\":null"),
                "namespaces must not serialize as null: " + payloadJson);
    }

    @Test
    @DisplayName("#2b issue: explicitly-empty List for roles/namespaces also emits empty arrays")
    void issue_explicitEmptyLists_emitEmptyArrays() {
        JwtSigner signer = JwtSigner.create(LONG_SECRET);
        IssueOptions opts = IssueOptions.builder()
                .subject("svc")
                .roles(List.of())
                .namespaces(List.of())
                .ttl(Duration.ofMinutes(5))
                .issuedAt(FIXED_IAT)
                .build();

        String payloadJson = decodeJson(segments(signer.issue(opts))[1]);
        assertTrue(payloadJson.matches(".*\"roles\"\\s*:\\s*\\[\\s*\\].*"),
                "roles must be present as an empty array []: " + payloadJson);
        assertTrue(payloadJson.matches(".*\"namespaces\"\\s*:\\s*\\[\\s*\\].*"),
                "namespaces must be present as an empty array []: " + payloadJson);
    }

    // ---------------------------------------------------------------------
    // #3 short-secret compatibility + independent HMAC over header.payload
    // ---------------------------------------------------------------------

    @Test
    @DisplayName("#3 issue: short (<32B) secret does not throw WeakKeyException and signs a token")
    void issue_shortSecret_doesNotThrowAndSigns() {
        assertTrue(SHORT_SECRET.getBytes(UTF_8).length < 32,
                "precondition: SHORT_SECRET must be under the 32-byte HS256 minimum");

        JwtSigner signer = JwtSigner.create(SHORT_SECRET);
        IssueOptions opts = IssueOptions.builder()
                .subject("svc")
                .roles(List.of("business_sdk"))
                .namespaces(List.of("*"))
                .ttl(Duration.ofMinutes(5))
                .issuedAt(FIXED_IAT)
                .build();

        // Must NOT throw io.jsonwebtoken.security.WeakKeyException (or any other).
        String token = signer.issue(opts);
        assertNotNull(token);
        segments(token); // still a structurally valid 3-segment JWT
    }

    @Test
    @DisplayName("#3 signature: third segment equals independent HMAC-SHA256(SecretKeySpec) over header.payload (short secret)")
    void issue_shortSecret_signatureMatchesRawHmac() {
        JwtSigner signer = JwtSigner.create(SHORT_SECRET);
        IssueOptions opts = IssueOptions.builder()
                .subject("svc")
                .roles(List.of("business_sdk"))
                .namespaces(List.of("*"))
                .ttl(Duration.ofMinutes(5))
                .issuedAt(FIXED_IAT)
                .build();

        String token = signer.issue(opts);
        String[] seg = segments(token);
        String signingInput = seg[0] + "." + seg[1]; // raw base64url header + "." + raw base64url payload

        String expectedSig = hmacSha256B64Url(SHORT_SECRET, signingInput);
        assertEquals(expectedSig, seg[2],
                "JWT signature segment must equal a plain HMAC-SHA256 over header.payload using the "
                        + "raw secret bytes — proving no hmacShaKeyFor / weak-key fallback and "
                        + "byte-identical signing with golang-jwt / the server");
    }

    @Test
    @DisplayName("#3b signature: same independent-HMAC equality also holds for a long secret")
    void issue_longSecret_signatureMatchesRawHmac() {
        JwtSigner signer = JwtSigner.create(LONG_SECRET);
        IssueOptions opts = IssueOptions.builder()
                .subject("svc")
                .roles(List.of("business_sdk"))
                .namespaces(List.of("*"))
                .ttl(Duration.ofMinutes(5))
                .issuedAt(FIXED_IAT)
                .build();

        String[] seg = segments(signer.issue(opts));
        String signingInput = seg[0] + "." + seg[1];
        assertEquals(hmacSha256B64Url(LONG_SECRET, signingInput), seg[2],
                "long-secret signature must also equal a plain HMAC-SHA256 over header.payload");
    }

    // ---------------------------------------------------------------------
    // #4 issuedAt == null → iat ~ now, exp == iat + ttl
    // ---------------------------------------------------------------------

    @Test
    @DisplayName("#4 issue: null issuedAt uses ~now for iat and exp == iat + ttl (second granularity)")
    void issue_nullIssuedAt_usesNow() {
        JwtSigner signer = JwtSigner.create(LONG_SECRET);
        long ttlSeconds = 600L;

        long before = Instant.now().getEpochSecond();
        String token = signer.issue(IssueOptions.builder()
                .subject("svc")
                .ttl(Duration.ofSeconds(ttlSeconds))
                // issuedAt deliberately not set → null → Instant.now()
                .build());
        long after = Instant.now().getEpochSecond();

        String payloadJson = decodeJson(segments(token)[1]);
        long iat = numericClaim(payloadJson, "iat");
        long exp = numericClaim(payloadJson, "exp");

        // iat ~ now, allowing a small tolerance around the call window.
        assertTrue(iat >= before - 5 && iat <= after + 5,
                "iat (" + iat + ") must be within ~5s of now [" + before + ".." + after + "]");
        assertEquals(ttlSeconds, exp - iat, "exp must be iat + ttl at second granularity");
    }

    // ---------------------------------------------------------------------
    // #5 error cases
    // ---------------------------------------------------------------------

    @Test
    @DisplayName("#5 create: null secret rejected with 'HMAC secret must be non-empty'")
    void create_nullSecret_throws() {
        IllegalArgumentException ex =
                assertThrows(IllegalArgumentException.class, () -> JwtSigner.create(null));
        assertTrue(ex.getMessage().contains("HMAC secret must be non-empty"),
                "message was: " + ex.getMessage());
    }

    @Test
    @DisplayName("#5 create: empty secret rejected with 'HMAC secret must be non-empty'")
    void create_emptySecret_throws() {
        IllegalArgumentException ex =
                assertThrows(IllegalArgumentException.class, () -> JwtSigner.create(""));
        assertTrue(ex.getMessage().contains("HMAC secret must be non-empty"),
                "message was: " + ex.getMessage());
    }

    @Test
    @DisplayName("#5 issue: ttl == 0 rejected with 'TTL must be > 0'")
    void issue_zeroTtl_throws() {
        JwtSigner signer = JwtSigner.create(LONG_SECRET);
        IssueOptions opts = IssueOptions.builder()
                .subject("svc")
                .ttl(Duration.ZERO)
                .build();
        IllegalArgumentException ex =
                assertThrows(IllegalArgumentException.class, () -> signer.issue(opts));
        assertTrue(ex.getMessage().contains("TTL must be > 0"), "message was: " + ex.getMessage());
    }

    @Test
    @DisplayName("#5 issue: negative ttl rejected with 'TTL must be > 0'")
    void issue_negativeTtl_throws() {
        JwtSigner signer = JwtSigner.create(LONG_SECRET);
        IssueOptions opts = IssueOptions.builder()
                .subject("svc")
                .ttl(Duration.ofSeconds(-1))
                .build();
        IllegalArgumentException ex =
                assertThrows(IllegalArgumentException.class, () -> signer.issue(opts));
        assertTrue(ex.getMessage().contains("TTL must be > 0"), "message was: " + ex.getMessage());
    }

    @Test
    @DisplayName("#5 issue: null options rejected with 'options must be non-null'")
    void issue_nullOptions_throws() {
        JwtSigner signer = JwtSigner.create(LONG_SECRET);
        IllegalArgumentException ex =
                assertThrows(IllegalArgumentException.class, () -> signer.issue(null));
        assertTrue(ex.getMessage().contains("options must be non-null"),
                "message was: " + ex.getMessage());
    }

    // ---------------------------------------------------------------------
    // small JSON-key counter used by the header-shape assertion
    // ---------------------------------------------------------------------

    /** Counts top-level JSON object keys (sufficient for the flat JWT header). */
    private static int countJsonKeys(String json) {
        java.util.regex.Matcher m =
                java.util.regex.Pattern.compile("\"[^\"]+\"\\s*:").matcher(json);
        int n = 0;
        while (m.find()) {
            n++;
        }
        return n;
    }
}
