package io.github.lightspeedintelligence.abconfig;

import java.io.UnsupportedEncodingException;
import java.net.URLDecoder;
import java.nio.charset.StandardCharsets;
import java.util.LinkedHashMap;
import java.util.Map;

/**
 * Parsed form of a gRPC-mode address string (design {@code 方案 Y}:
 * "gRPC 地址 Scheme 解析 + TLS 接入"). {@link #parseGrpcTarget(String)} produces
 * it; the dialer ({@link GrpcChannels}) consumes it.
 *
 * <p>Immutable port of the Go {@code grpcTarget} struct / {@code parseGRPCTarget}
 * function ({@code grpc_target.go}). The judgement that drives scheme-based
 * parsing is a <strong>literal prefix match</strong> on the four managed scheme
 * strings — not a generic "does the string contain {@code ://}" test. This is
 * load bearing: native grpc resolver targets such as
 * {@code passthrough:///bufnet-config} also contain {@code ://} but MUST pass
 * through as plaintext (rule 2).
 */
final class GrpcTarget {

    /** The closed set of scheme prefixes that drive scheme-based parsing. */
    private static final String SCHEME_GRPC = "grpc://";
    private static final String SCHEME_GRPCS = "grpcs://";
    private static final String SCHEME_HTTP = "http://";
    private static final String SCHEME_HTTPS = "https://";

    /** The {@code dns:///} resolver prefix that opts a target into round_robin. */
    private static final String DNS_PREFIX = "dns:///";

    /**
     * Address handed to the channel builder: scheme + query are stripped,
     * leaving a bare {@code host:port} (for the managed grpc/grpcs schemes) or
     * the original string verbatim (for bare host:port and native grpc resolver
     * targets such as {@code passthrough:///}, {@code dns:///}, {@code unix:},
     * {@code xds:///}).
     */
    private final String dialTarget;

    /**
     * {@code true} only for {@code grpcs://}; {@code false} for everything else
     * (bare host:port, {@code grpc://}, native resolver targets) → plaintext h2c.
     */
    private final boolean useTls;

    /**
     * Overrides the HTTP/2 {@code :authority} and, under TLS, the SNI /
     * ServerName (certificate-name target). Empty string means "do not override".
     */
    private final String authority;

    /**
     * Disables TLS certificate verification ({@code grpcs://} with
     * {@code ?insecure=true}). Dev / Origin-Cert-direct-IP only; never in
     * production.
     */
    private final boolean insecureSkipVerify;

    GrpcTarget(String dialTarget, boolean useTls, String authority, boolean insecureSkipVerify) {
        this.dialTarget = dialTarget;
        this.useTls = useTls;
        this.authority = authority;
        this.insecureSkipVerify = insecureSkipVerify;
    }

    String dialTarget() {
        return dialTarget;
    }

    boolean useTls() {
        return useTls;
    }

    String authority() {
        return authority;
    }

    boolean insecureSkipVerify() {
        return insecureSkipVerify;
    }

    /**
     * Resolves a gRPC-mode address string into a {@link GrpcTarget} per the
     * scheme-whitelist rules (design rules 1-5). All errors are parameter errors
     * (prefix {@code tipsyabconfig:}) raised at parse time, before dialing; they
     * are never absorbed by {@code startupFailOpen}.
     *
     * <p>Rules:
     * <ol>
     *   <li>Only addresses literally prefixed with {@code grpc://} /
     *       {@code grpcs://} / {@code http://} / {@code https://} are parsed by
     *       scheme.</li>
     *   <li>Everything else (bare {@code host:port}; native resolver targets like
     *       {@code passthrough:///...}, {@code dns:///host}, {@code unix:path},
     *       {@code xds:///}) is passed through verbatim as plaintext.</li>
     *   <li>{@code grpc://host:port} → plaintext; any query is a parameter
     *       error.</li>
     *   <li>{@code grpcs://host:port[?authority=&insecure=]} → TLS; an explicit
     *       numeric port is required; the query allows only {@code authority} /
     *       {@code insecure}.</li>
     *   <li>{@code http://} / {@code https://} in gRPC mode is a parameter error
     *       (use {@code Transport=http}).</li>
     * </ol>
     *
     * @param addr the configured address string
     * @return the parsed target
     * @throws ConfigValidationException on any parameter error
     */
    static GrpcTarget parseGrpcTarget(String addr) {
        if (addr.startsWith(SCHEME_GRPCS)) {
            return parseGrpcsScheme(addr);
        }
        if (addr.startsWith(SCHEME_GRPC)) {
            return parseGrpcScheme(addr);
        }
        if (addr.startsWith(SCHEME_HTTP) || addr.startsWith(SCHEME_HTTPS)) {
            // HTTP base URLs belong to the HTTP transport mode (rule 5).
            throw new ConfigValidationException(String.format(
                    "tipsyabconfig: %s is an HTTP base URL; gRPC mode expects a gRPC target "
                            + "(use Transport=http for http(s):// base URLs)",
                    quote(addr)));
        }
        // Rule 2: bare host:port and native grpc resolver targets pass through
        // verbatim as plaintext. This is the unchanged legacy path.
        return new GrpcTarget(addr, false, "", false);
    }

    /**
     * Handles {@code grpc://} (plaintext, explicit form). Query parameters are
     * rejected because they only make sense under TLS.
     */
    private static GrpcTarget parseGrpcScheme(String addr) {
        String rest = addr.substring(SCHEME_GRPC.length());
        SplitQuery sq = splitQuery(rest);
        if (sq.hadQuery && !sq.query.isEmpty()) {
            throw new ConfigValidationException(String.format(
                    "tipsyabconfig: query parameters are only valid under grpcs:// "
                            + "(got %s on a plaintext grpc:// target); did you mean grpcs://?",
                    quote(addr)));
        }
        if (sq.head.isEmpty()) {
            throw new ConfigValidationException(String.format(
                    "tipsyabconfig: grpc:// target is missing host:port in %s", quote(addr)));
        }
        return new GrpcTarget(sq.head, false, "", false);
    }

    /**
     * Handles {@code grpcs://} (TLS). Requires an explicit numeric port and
     * parses the {@code authority} / {@code insecure} query parameters.
     */
    private static GrpcTarget parseGrpcsScheme(String addr) {
        String rest = addr.substring(SCHEME_GRPCS.length());
        SplitQuery sq = splitQuery(rest);
        String hostport = sq.head;
        if (hostport.isEmpty()) {
            throw new ConfigValidationException(String.format(
                    "tipsyabconfig: grpcs:// target is missing host:port in %s", quote(addr)));
        }
        // Require an explicit, numeric port. A missing port is one error; a
        // present-but-non-numeric port is a distinct error.
        validateHostPort(hostport, addr);

        String authority = "";
        boolean insecureSkipVerify = false;

        String rawQuery = sq.query;
        if (sq.hadQuery && !rawQuery.isEmpty()) {
            Map<String, String> values = parseQuery(rawQuery, addr);
            for (String key : values.keySet()) {
                if (!"authority".equals(key) && !"insecure".equals(key)) {
                    throw new ConfigValidationException(String.format(
                            "tipsyabconfig: unknown query parameter %s in grpcs:// target %s "
                                    + "(supported: authority, insecure)",
                            quote(key), quote(addr)));
                }
            }
            String a = values.get("authority");
            if (a != null) {
                authority = a;
            }
            String raw = values.get("insecure");
            if (raw != null && !raw.isEmpty()) {
                switch (raw.trim().toLowerCase()) {
                    case "true":
                    case "1":
                        insecureSkipVerify = true;
                        break;
                    case "false":
                    case "0":
                        insecureSkipVerify = false;
                        break;
                    default:
                        throw new ConfigValidationException(String.format(
                                "tipsyabconfig: invalid insecure value %s in grpcs:// target %s "
                                        + "(expected true/false)",
                                quote(raw), quote(addr)));
                }
            }
        }
        return new GrpcTarget(hostport, true, authority, insecureSkipVerify);
    }

    /**
     * Returns the gRPC service-config load-balancing policy name to inject for a
     * given dial target, or {@code null} to leave grpc-java on its default
     * {@code pick_first}.
     *
     * <p>Headless Service + round_robin opt-in: when the dial target starts with
     * {@code dns:///} (the gRPC name-resolver scheme that resolves to all backend
     * pod IPs) we select {@code round_robin}. Every other scheme — bare
     * host:port, {@code grpc://}, {@code grpcs://[?...]}, {@code passthrough:///},
     * {@code unix:} — falls through to the default, preserving backwards
     * compatibility (equivalent to the Go injection
     * {@code {"loadBalancingConfig":[{"round_robin":{}}]}}).
     *
     * @param dialTarget the resolved dial target string
     * @return {@code "round_robin"} for {@code dns:///} targets, otherwise
     *         {@code null}
     */
    static String loadBalancingPolicyFor(String dialTarget) {
        if (dialTarget.startsWith(DNS_PREFIX)) {
            return "round_robin";
        }
        return null;
    }

    /** Result of splitting {@code "host:port?query"} into head / query. */
    private static final class SplitQuery {
        final String head;
        final String query;
        final boolean hadQuery;

        SplitQuery(String head, String query, boolean hadQuery) {
            this.head = head;
            this.query = query;
            this.hadQuery = hadQuery;
        }
    }

    /** Splits {@code "host:port?query"} into ({@code host:port}, query, hadQuery). */
    private static SplitQuery splitQuery(String s) {
        int i = s.indexOf('?');
        if (i >= 0) {
            return new SplitQuery(s.substring(0, i), s.substring(i + 1), true);
        }
        return new SplitQuery(s, "", false);
    }

    /**
     * Validates that {@code hostport} carries an explicit, numeric port,
     * mirroring the Go {@code validateHostPort} (built on {@code net.SplitHostPort}
     * + {@code strconv.ParseUint}). Raises a {@link ConfigValidationException}
     * (prefix {@code tipsyabconfig:}) otherwise:
     * <ul>
     *   <li>missing port (no {@code :port} suffix, a trailing-colon-only form, or
     *       a bare IPv6 literal {@code [::1]}) → "must specify an explicit
     *       port".</li>
     *   <li>present-but-non-numeric / out-of-range port → "invalid port".</li>
     * </ul>
     */
    private static void validateHostPort(String hostport, String addr) {
        HostPort hp;
        try {
            hp = splitHostPort(hostport);
        } catch (HostPortException e) {
            // SplitHostPort errors on a missing port ("host", "[::1]") and a
            // trailing-colon-only form yields an empty port ("host:"). Both mean
            // "no explicit port".
            throw new ConfigValidationException(String.format(
                    "tipsyabconfig: grpcs:// target must specify an explicit port (e.g. :443) in %s",
                    quote(addr)));
        }
        if (hp.port.isEmpty()) {
            throw new ConfigValidationException(String.format(
                    "tipsyabconfig: grpcs:// target must specify an explicit port (e.g. :443) in %s",
                    quote(addr)));
        }
        if (hp.host.isEmpty()) {
            // e.g. ":443" — no host. Treat as a malformed target.
            throw new ConfigValidationException(String.format(
                    "tipsyabconfig: grpcs:// target is missing a host before the port in %s",
                    quote(addr)));
        }
        int port;
        try {
            port = Integer.parseInt(hp.port);
        } catch (NumberFormatException e) {
            throw new ConfigValidationException(String.format(
                    "tipsyabconfig: invalid port %s in grpcs:// target %s (expected a number 0-65535)",
                    quote(hp.port), quote(addr)));
        }
        if (port < 0 || port > 65535) {
            throw new ConfigValidationException(String.format(
                    "tipsyabconfig: invalid port %s in grpcs:// target %s (expected a number 0-65535)",
                    quote(hp.port), quote(addr)));
        }
    }

    /** Host + port pair, as produced by {@link #splitHostPort(String)}. */
    private static final class HostPort {
        final String host;
        final String port;

        HostPort(String host, String port) {
            this.host = host;
            this.port = port;
        }
    }

    /** Signals that an address has no {@code host:port} shape (no port colon). */
    private static final class HostPortException extends Exception {
        private static final long serialVersionUID = 1L;

        HostPortException(String message) {
            super(message);
        }
    }

    /**
     * Splits a {@code host:port} string into host and port, equivalent to Go's
     * {@code net.SplitHostPort}. Handles bracketed IPv6 literals
     * ({@code [::1]:443} → host {@code ::1}, port {@code 443}). The port may be
     * empty (trailing-colon form {@code host:}); the caller validates emptiness.
     *
     * @param hostport the {@code host:port} input
     * @return the split host / port
     * @throws HostPortException if there is no port colon (bare {@code host} or a
     *                           bare bracketed IPv6 literal {@code [::1]}), or the
     *                           brackets are malformed
     */
    private static HostPort splitHostPort(String hostport) throws HostPortException {
        if (hostport.startsWith("[")) {
            // Bracketed IPv6 literal: "[host]:port".
            int close = hostport.indexOf(']');
            if (close < 0) {
                throw new HostPortException("missing ']' in address");
            }
            String host = hostport.substring(1, close);
            String afterBracket = hostport.substring(close + 1);
            if (afterBracket.isEmpty()) {
                // "[::1]" with no port colon.
                throw new HostPortException("missing port in address");
            }
            if (afterBracket.charAt(0) != ':') {
                throw new HostPortException("missing port in address");
            }
            String port = afterBracket.substring(1);
            if (port.indexOf(':') >= 0) {
                throw new HostPortException("too many colons in address");
            }
            return new HostPort(host, port);
        }
        int lastColon = hostport.lastIndexOf(':');
        if (lastColon < 0) {
            // No port colon at all ("host").
            throw new HostPortException("missing port in address");
        }
        String host = hostport.substring(0, lastColon);
        String port = hostport.substring(lastColon + 1);
        if (host.indexOf(':') >= 0) {
            // Multiple colons in an unbracketed host → ambiguous IPv6, like Go's
            // "too many colons in address".
            throw new HostPortException("too many colons in address");
        }
        return new HostPort(host, port);
    }

    /**
     * Parses an {@code application/x-www-form-urlencoded} query string into a
     * key→value map (last value wins per key, matching the keys we care about),
     * mirroring Go's {@code url.ParseQuery} for the recognised keys. Raises a
     * {@link ConfigValidationException} on a malformed escape sequence.
     */
    private static Map<String, String> parseQuery(String rawQuery, String addr) {
        Map<String, String> out = new LinkedHashMap<>();
        for (String pair : rawQuery.split("&", -1)) {
            if (pair.isEmpty()) {
                continue;
            }
            int eq = pair.indexOf('=');
            String key;
            String val;
            if (eq >= 0) {
                key = pair.substring(0, eq);
                val = pair.substring(eq + 1);
            } else {
                key = pair;
                val = "";
            }
            out.put(decodeQueryComponent(key, addr), decodeQueryComponent(val, addr));
        }
        return out;
    }

    private static String decodeQueryComponent(String s, String addr) {
        try {
            return URLDecoder.decode(s, StandardCharsets.UTF_8.name());
        } catch (UnsupportedEncodingException | IllegalArgumentException e) {
            throw new ConfigValidationException(String.format(
                    "tipsyabconfig: invalid query in grpcs:// target %s: %s",
                    quote(addr), e.getMessage()), e);
        }
    }

    /** Renders a value with Go {@code %q}-style double quotes for error parity. */
    private static String quote(String s) {
        return "\"" + s + "\"";
    }
}
