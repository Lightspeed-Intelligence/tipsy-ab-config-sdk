package io.tipsy.auth;

import static java.nio.charset.StandardCharsets.UTF_8;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.security.MessageDigest;
import java.time.Duration;
import java.time.Instant;
import java.util.Base64;
import java.util.List;

import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;

import org.junit.jupiter.api.DisplayName;
import org.junit.jupiter.api.Test;

import io.jsonwebtoken.Claims;
import io.jsonwebtoken.Jws;
import io.jsonwebtoken.Jwts;

/**
 * Round-trip ("server can verify") proof tests for {@link JwtSigner} (ST7 #6).
 *
 * <p>This stands in for the server-side verifier: a token minted here must be
 * verifiable using the same shared secret. We split the proof in two:
 *
 * <ul>
 *   <li><b>Long secret (>= 32 bytes):</b> verified with jjwt's
 *       {@code Jwts.parser().verifyWith(...)} end-to-end, exercising the full
 *       parse-and-verify path that mirrors a JWA-conformant verifier.</li>
 *   <li><b>Short secret (&lt; 32 bytes):</b> jjwt's parser ALSO enforces the
 *       JWA 256-bit minimum on the <em>verification</em> side
 *       ({@code SignatureException}/{@code WeakKeyException}), so
 *       {@code verifyWith} cannot be used for the very secrets this module is
 *       designed to support. We therefore verify the short-secret token with an
 *       independent JCA {@code Mac} (the same algorithm golang-jwt and the
 *       server use), which is the authoritative cross-language round-trip.</li>
 * </ul>
 */
final class JwtSignerRoundTripTest {

    private static final String LONG_SECRET = "a-32-byte-or-longer-secret-string!!";
    private static final String SHORT_SECRET = "short-secret";
    private static final Instant FIXED_IAT = Instant.ofEpochSecond(1_781_254_817L);

    private static final Base64.Decoder URL_DECODER = Base64.getUrlDecoder();
    private static final Base64.Encoder URL_ENCODER_NOPAD = Base64.getUrlEncoder().withoutPadding();

    private static IssueOptions sampleOpts() {
        return IssueOptions.builder()
                .subject("svc")
                .roles(List.of("business_sdk"))
                .namespaces(List.of("*"))
                .ttl(Duration.ofMinutes(5))
                .issuedAt(FIXED_IAT)
                .build();
    }

    @Test
    @DisplayName("#6 round-trip (long secret): jjwt verifyWith parses the token and reads back claims")
    void roundTrip_longSecret_jjwtVerifyWith() {
        JwtSigner signer = JwtSigner.create(LONG_SECRET);
        String token = signer.issue(sampleOpts());

        SecretKeySpec key = new SecretKeySpec(LONG_SECRET.getBytes(UTF_8), "HmacSHA256");
        // parseSignedClaims() validates exp against the parser's clock. The token
        // is minted with the fixed FIXED_IAT (well in the past relative to wall
        // time), so a real-time clock would throw ExpiredJwtException. Pin the
        // parser's clock just inside the token's [iat, exp] window so verification
        // exercises the full signature + exp/iat path deterministically, without a
        // time-bomb. FIXED_IAT is kept for the iat/exp contract assertions below.
        Jws<Claims> jws = Jwts.parser()
                .verifyWith(key)
                .clock(() -> java.util.Date.from(FIXED_IAT.plusSeconds(60)))
                .build()
                .parseSignedClaims(token); // throws if the signature does not verify

        Claims c = jws.getPayload();
        assertEquals("svc", c.getSubject(), "verified sub");
        assertEquals(List.of("business_sdk"), c.get("roles"), "verified roles");
        assertEquals(List.of("*"), c.get("namespaces"), "verified namespaces");
        assertNotNull(c.getIssuedAt(), "verified iat present");
        assertNotNull(c.getExpiration(), "verified exp present");
        assertEquals(FIXED_IAT.getEpochSecond(), c.getIssuedAt().toInstant().getEpochSecond(),
                "verified iat equals the fixed issuedAt (unix seconds)");
        assertEquals(FIXED_IAT.plus(Duration.ofMinutes(5)).getEpochSecond(),
                c.getExpiration().toInstant().getEpochSecond(),
                "verified exp equals iat + ttl");
        assertEquals("HS256", jws.getHeader().getAlgorithm(), "verified alg HS256");
    }

    @Test
    @DisplayName("#6 round-trip (short secret): independent Mac verification (jjwt verifyWith rejects sub-32B keys)")
    void roundTrip_shortSecret_independentMac() {
        // NOTE: jjwt's Jwts.parser().verifyWith(SecretKeySpec) enforces the JWA
        // HS256 minimum key length (256 bits) on the verification path too, and
        // throws for a 12-byte secret. Since this module's whole purpose is to
        // sign/verify with arbitrary-length secrets (matching golang-jwt and the
        // server), the faithful round-trip proof for short secrets is a plain
        // HMAC-SHA256 recomputation over header.payload — the exact check the
        // server performs.
        JwtSigner signer = JwtSigner.create(SHORT_SECRET);
        String token = signer.issue(sampleOpts());

        String[] seg = token.split("\\.");
        assertEquals(3, seg.length, "expected a 3-segment compact JWT");
        String signingInput = seg[0] + "." + seg[1];

        byte[] expectedSig = hmacSha256(SHORT_SECRET, signingInput);
        byte[] actualSig = URL_DECODER.decode(seg[2]);
        assertTrue(MessageDigest.isEqual(expectedSig, actualSig),
                "independent HMAC-SHA256 over header.payload must match the JWT signature segment, "
                        + "proving a server sharing this short secret would accept the token");

        // Sanity: re-encoding the recomputed signature reproduces the raw segment.
        assertEquals(seg[2], URL_ENCODER_NOPAD.encodeToString(expectedSig),
                "base64url(no padding) of the recomputed HMAC must equal the signature segment");
    }

    private static byte[] hmacSha256(String secret, String signingInput) {
        try {
            Mac mac = Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(secret.getBytes(UTF_8), "HmacSHA256"));
            return mac.doFinal(signingInput.getBytes(UTF_8));
        } catch (Exception e) {
            throw new AssertionError("independent HMAC computation failed", e);
        }
    }
}
