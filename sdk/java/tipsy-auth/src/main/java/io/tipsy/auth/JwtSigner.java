package io.tipsy.auth;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.security.InvalidKeyException;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.Date;
import java.util.List;

import javax.crypto.Mac;
import javax.crypto.SecretKey;
import javax.crypto.spec.SecretKeySpec;

import io.jsonwebtoken.Jwts;
import io.jsonwebtoken.security.MacAlgorithm;
import io.jsonwebtoken.security.SecretKeyBuilder;
import io.jsonwebtoken.security.SecureRequest;
import io.jsonwebtoken.security.SecurityException;
import io.jsonwebtoken.security.VerifySecureDigestRequest;

/**
 * Issues HS256 JWTs accepted by the ab-config gRPC / HTTP services.
 *
 * <p>This is the PUBLIC counterpart of the server's internal auth boundary: it
 * owns only token signing and the on-wire claims shape, so external modules can
 * mint their own service tokens without depending on server internals.
 * Verification (signature + {@code exp}/{@code iat} + namespace authorization)
 * stays inside the server and is intentionally NOT implemented here.
 *
 * <p>Counterpart of Go {@code tipsyauth.Signer}.
 *
 * <p><b>Compatibility contract.</b> The signing algorithm (HS256) and the
 * claims set ({@code roles}, {@code namespaces}, {@code sub}, {@code iat},
 * {@code exp}) match the server's internal signer, so tokens minted here are
 * accepted by the server's verifier when both sides share the same secret
 * ({@code TIPSY_SERVICE_SECRET}). {@code iat}/{@code exp} are emitted as unix
 * seconds (NumericDate). There is no {@code nbf}/{@code iss}/{@code aud}.
 *
 * <p>Instances are immutable and thread-safe; a single signer may be shared.
 */
public final class JwtSigner {

    private static final String ERR_PREFIX = "tipsyauth: ";

    /**
     * Shared, stateless HS256 algorithm that performs the same HMAC-SHA256 as
     * jjwt's {@code Jwts.SIG.HS256} but WITHOUT the JWA minimum-key-length
     * (256-bit) validation. See {@link #create(String)} for the rationale.
     */
    private static final MacAlgorithm HS256_NO_STRENGTH_CHECK = new Hs256NoStrengthCheck();

    private final SecretKey key;

    private JwtSigner(SecretKey key) {
        this.key = key;
    }

    /**
     * Creates a signer from a shared HMAC secret. An empty or {@code null}
     * secret is rejected with {@link IllegalArgumentException}.
     *
     * <p><b>Key length compatibility (why a custom MAC algorithm).</b> jjwt
     * 0.12.x enforces JWA's minimum HS256 key length (256 bits / 32 bytes) and
     * throws a {@code WeakKeyException} at sign time for shorter secrets. That
     * check is performed by jjwt's built-in {@code Jwts.SIG.HS256} on every
     * {@code signWith}, and it cannot be sidestepped by key trickery: making the
     * key report a non-{@code "RAW"} format dodges the length check but then the
     * JCA {@code Mac} provider rejects the key ("No installed provider supports
     * this key"). Go's golang-jwt imposes no minimum, so the server and the Go
     * SDK accept HMAC secrets of any length.
     *
     * <p>To stay compatible with an arbitrary-length shared
     * {@code TIPSY_SERVICE_SECRET}, this signer supplies its own
     * {@link io.jsonwebtoken.security.MacAlgorithm} ({@code Hs256NoStrengthCheck})
     * that advertises {@code id="HS256"} and computes the identical HMAC-SHA256
     * via the JCA {@code Mac} over a standard {@link SecretKeySpec}, but performs
     * NO key-length validation. The resulting signature over the same secret is
     * byte-for-byte identical to what Go produces, so cross-language verification
     * holds, while secrets shorter than 32 bytes are accepted just like Go.
     *
     * @param secret shared HMAC secret; must be non-empty
     * @return a ready-to-use signer
     * @throws IllegalArgumentException if {@code secret} is {@code null} or empty
     */
    public static JwtSigner create(String secret) {
        if (secret == null || secret.isEmpty()) {
            throw new IllegalArgumentException(ERR_PREFIX + "HMAC secret must be non-empty");
        }
        // Standard RAW-format HmacSHA256 key (works with the JCA Mac). The
        // weak-key check is avoided via the custom algorithm, not via the key.
        SecretKey key = new SecretKeySpec(secret.getBytes(StandardCharsets.UTF_8), "HmacSHA256");
        return new JwtSigner(key);
    }

    /**
     * Mints an HS256 JWT for the given options.
     *
     * <p>{@code opts.ttl()} must be {@code > 0}; otherwise an
     * {@link IllegalArgumentException} is thrown. When {@code opts.issuedAt()}
     * is {@code null}, {@code Instant.now()} is used. The {@code roles} and
     * {@code namespaces} claims are always emitted as JSON arrays (empty
     * {@code []} when no values), matching the Go signer.
     *
     * @param opts token description; must be non-{@code null} with a positive ttl
     * @return the compact, signed JWT string
     * @throws IllegalArgumentException if {@code opts} is {@code null} or
     *         {@code opts.ttl()} is {@code null} or {@code <= 0}
     */
    public String issue(IssueOptions opts) {
        if (opts == null) {
            throw new IllegalArgumentException(ERR_PREFIX + "options must be non-null");
        }
        Duration ttl = opts.ttl();
        if (ttl == null || ttl.isZero() || ttl.isNegative()) {
            throw new IllegalArgumentException(ERR_PREFIX + "TTL must be > 0");
        }
        Instant iat = opts.issuedAt();
        if (iat == null) {
            iat = Instant.now();
        }
        Instant exp = iat.plus(ttl);

        // Defensive copies into fresh ArrayLists so jjwt serializes them as JSON
        // arrays. opts.roles()/opts.namespaces() are already non-null (empty when
        // unset), which guarantees "roles":[] / "namespaces":[] appear in the
        // payload, mirroring Go's append([]string{}, ...).
        List<String> roles = new ArrayList<>(opts.roles());
        List<String> namespaces = new ArrayList<>(opts.namespaces());

        return Jwts.builder()
                .header().add("typ", "JWT").and()
                .claim("roles", roles)
                .claim("namespaces", namespaces)
                .subject(opts.subject())
                // NumericDate: Date.from(Instant) truncates to seconds on the wire,
                // emitting iat/exp as unix seconds, matching golang-jwt NumericDate.
                .issuedAt(Date.from(iat))
                .expiration(Date.from(exp))
                .signWith(key, HS256_NO_STRENGTH_CHECK)
                .compact();
    }

    /**
     * HMAC-SHA256 ("HS256") {@link MacAlgorithm} implementation that produces the
     * exact same signature as jjwt's built-in {@code Jwts.SIG.HS256}, but does
     * NOT validate the key length. This is the supported jjwt extension point
     * ({@link io.jsonwebtoken.security.SecureDigestAlgorithm}) and lets the signer
     * accept arbitrary-length secrets to match Go/golang-jwt and the server.
     *
     * <p>Only {@link #getId()} (for the {@code alg} header) and {@link #digest}
     * (to compute the signature) are exercised on the signing path. {@link #verify}
     * is implemented for completeness (constant-time compare). The key-generation
     * helpers ({@link #key()}, {@link #getKeyBitLength()}) are not used by signing.
     */
    private static final class Hs256NoStrengthCheck implements MacAlgorithm {

        private static final String JCA_NAME = "HmacSHA256";
        private static final int HS256_BIT_LENGTH = 256;

        @Override
        public String getId() {
            return "HS256";
        }

        @Override
        public byte[] digest(SecureRequest<InputStream, SecretKey> request) throws SecurityException {
            SecretKey key = request.getKey();
            InputStream payload = request.getPayload();
            try {
                Mac mac = Mac.getInstance(JCA_NAME);
                mac.init(key);
                byte[] buf = readAll(payload);
                return mac.doFinal(buf);
            } catch (NoSuchAlgorithmException e) {
                throw new SecurityException(ERR_PREFIX + "HmacSHA256 not available", e);
            } catch (InvalidKeyException e) {
                throw new SecurityException(ERR_PREFIX + "invalid HMAC key", e);
            } catch (IOException e) {
                throw new SecurityException(ERR_PREFIX + "failed to read JWT signing input", e);
            }
        }

        @Override
        public boolean verify(VerifySecureDigestRequest<SecretKey> request) throws SecurityException {
            byte[] expected = request.getDigest();
            // Recompute over the same payload/key and constant-time compare.
            byte[] computed = digest(request);
            return MessageDigest.isEqual(expected, computed);
        }

        @Override
        public int getKeyBitLength() {
            return HS256_BIT_LENGTH;
        }

        @Override
        public SecretKeyBuilder key() {
            // Key generation is not part of the signing path used by this module.
            // Delegate to jjwt's standard HS256 generator if ever invoked.
            return Jwts.SIG.HS256.key();
        }

        private static byte[] readAll(InputStream in) throws IOException {
            ByteArrayOutputStream out = new ByteArrayOutputStream(256);
            byte[] chunk = new byte[1024];
            int n;
            while ((n = in.read(chunk)) != -1) {
                out.write(chunk, 0, n);
            }
            return out.toByteArray();
        }
    }
}
