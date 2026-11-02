package io.github.lightspeedintelligence.abconfig;

import java.util.LinkedHashMap;
import java.util.Map;

/**
 * Parsing helpers for the Java {@code getConfig} hit lines. Java emits one FIXED
 * kv text template per line: {@code "tipsyabconfig: get_config hit (<variant>) "}
 * followed by comma-separated {@code key=value} pairs, {@code reason} first.
 *
 * <p>The three variants are {@code abtest}, {@code full} and — since
 * real-enrollment part-c §C1 — {@code default}. Matching on {@link #HIT_PREFIX}
 * (with the opening parenthesis) deliberately covers all three, so "exactly one
 * hit line for this key" also fails when a value hit additionally emitted a
 * default line. Keys are matched on the PARSED {@code key} field, never by
 * substring, because the default template's field order is not pinned.
 */
final class HitLogLines {

    static final String HIT_PREFIX = "tipsyabconfig: get_config hit (";
    static final String DEFAULT_PREFIX = "tipsyabconfig: get_config hit (default) ";

    private HitLogLines() {
    }

    /** The variant inside the parentheses, or {@code null} if not a hit line. */
    static String variant(String formatted) {
        if (!formatted.startsWith(HIT_PREFIX)) {
            return null;
        }
        int close = formatted.indexOf(')', HIT_PREFIX.length());
        return close < 0 ? null : formatted.substring(HIT_PREFIX.length(), close);
    }

    /**
     * The template's {@code key=value} pairs in emission order. Fails (rather
     * than skipping) on any segment that is not a well-formed pair or on a
     * duplicated key, so a malformed template cannot pass a field-set check.
     */
    static Map<String, String> fields(String formatted) {
        int close = formatted.indexOf(") ", HIT_PREFIX.length());
        if (!formatted.startsWith(HIT_PREFIX) || close < 0) {
            throw new AssertionError("not a get_config hit line: " + formatted);
        }
        Map<String, String> out = new LinkedHashMap<>();
        for (String part : formatted.substring(close + 2).split(", ", -1)) {
            int eq = part.indexOf('=');
            if (eq <= 0) {
                throw new AssertionError("segment '" + part + "' is not key=value in: " + formatted);
            }
            if (out.put(part.substring(0, eq), part.substring(eq + 1)) != null) {
                throw new AssertionError("duplicate key '" + part.substring(0, eq) + "' in: " + formatted);
            }
        }
        return out;
    }
}
