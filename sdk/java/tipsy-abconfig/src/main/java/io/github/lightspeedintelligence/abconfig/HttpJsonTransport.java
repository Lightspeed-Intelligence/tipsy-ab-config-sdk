package io.github.lightspeedintelligence.abconfig;

import static java.nio.charset.StandardCharsets.UTF_8;

import com.google.protobuf.Message;
import com.google.protobuf.MessageOrBuilder;
import com.google.protobuf.util.JsonFormat;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.time.Duration;
import java.util.function.Supplier;

/**
 * Shared HTTP plumbing for {@link HttpConfigTransport} and
 * {@link HttpAbtestTransport}.
 *
 * <p>Mirrors Go {@code httpTransport}. Owns the JDK {@link HttpClient}, the
 * bearer-token supplier, and the response-size cap. Requests are protojson
 * (mirroring the server's publicread codec); 2xx bodies are decoded with
 * {@code ignoringUnknownFields()}; non-2xx bodies are surfaced as a transport
 * error carrying the HTTP status code and a parsed {@code {"error": msg}}
 * message (falling back to the raw body text).
 *
 * <p>Auth is decoupled via a {@code Supplier<String>} that returns the full
 * {@code Authorization} header value (e.g. {@code "Bearer xxx"}). The supplier
 * may throw a {@link RuntimeException} to signal a token-acquisition failure,
 * which is caught and wrapped as a transport error — the transport never
 * depends on any concrete token type.
 */
abstract class HttpJsonTransport {

    private static final String CONTENT_TYPE_HEADER = "Content-Type";
    private static final String AUTHORIZATION_HEADER = "Authorization";
    private static final String CONTENT_TYPE_JSON = "application/json";

    private final HttpClient client;
    private final Supplier<String> authHeaderValue;
    private final int maxRecvBytes;

    HttpJsonTransport(HttpClient client, Supplier<String> authHeaderValue, int maxRecvBytes) {
        this.client = client;
        this.authHeaderValue = authHeaderValue;
        this.maxRecvBytes = maxRecvBytes;
    }

    /**
     * Marshals {@code req} to protojson, POSTs it to {@code url} with a fresh
     * bearer token and the per-call {@code timeout}, and merges a 2xx response
     * body into {@code outBuilder}.
     *
     * @throws Exception on token acquisition, transport, oversized-response,
     *                   non-2xx, or decode failure.
     */
    protected void doProtoJson(String url, MessageOrBuilder req, Message.Builder outBuilder, Duration timeout)
            throws Exception {
        String body = JsonFormat.printer().print(req);

        // Acquire the bearer header per request, matching the gRPC per-RPC
        // credential timing. A supplier failure fails this request.
        String authz;
        try {
            authz = authHeaderValue.get();
        } catch (RuntimeException e) {
            throw new TransportException("tipsyabconfig: acquire token: " + e.getMessage(), e);
        }

        HttpRequest.Builder reqBuilder = HttpRequest.newBuilder()
                .uri(URI.create(url))
                .timeout(timeout)
                .header(CONTENT_TYPE_HEADER, CONTENT_TYPE_JSON)
                .POST(HttpRequest.BodyPublishers.ofString(body, UTF_8));
        if (authz != null && !authz.isEmpty()) {
            reqBuilder.header(AUTHORIZATION_HEADER, authz);
        }

        HttpResponse<InputStream> resp = client.send(reqBuilder.build(), HttpResponse.BodyHandlers.ofInputStream());

        byte[] respBody = readCapped(resp.body());
        int status = resp.statusCode();
        if (status < 200 || status >= 300) {
            throw statusError(status, respBody);
        }

        JsonFormat.parser().ignoringUnknownFields().merge(new String(respBody, UTF_8), outBuilder);
    }

    /**
     * Reads at most {@code maxRecvBytes + 1} bytes from {@code in}; if the body
     * exceeds {@code maxRecvBytes} a {@link TransportException} is thrown rather
     * than silently truncating (mirrors Go's {@code io.LimitReader} cap).
     */
    private byte[] readCapped(InputStream in) throws IOException, TransportException {
        long limit = (long) maxRecvBytes + 1L;
        ByteArrayOutputStream buf = new ByteArrayOutputStream();
        byte[] chunk = new byte[8192];
        long total = 0L;
        try (InputStream stream = in) {
            int n;
            while ((n = stream.read(chunk)) != -1) {
                long remaining = limit - total;
                if (remaining <= 0) {
                    total += n;
                    break;
                }
                int toWrite = (int) Math.min(n, remaining);
                buf.write(chunk, 0, toWrite);
                total += n;
                if (total > maxRecvBytes) {
                    break;
                }
            }
        }
        if (total > maxRecvBytes) {
            throw new TransportException(
                    "tipsyabconfig: response exceeds MaxRecvMessageSize (" + maxRecvBytes + " bytes)");
        }
        return buf.toByteArray();
    }

    /**
     * Builds the error for a non-2xx response. Attempts to extract a
     * {@code {"error": msg}} body (the publicread error shape); falls back to
     * the trimmed raw body when that shape is absent.
     */
    private static TransportException statusError(int status, byte[] body) {
        String text = new String(body, UTF_8);
        String msg = extractErrorField(text);
        if (msg == null) {
            msg = text.trim();
        }
        if (msg.isEmpty()) {
            return new TransportException("tipsyabconfig: HTTP " + status);
        }
        return new TransportException("tipsyabconfig: HTTP " + status + ": " + msg);
    }

    /**
     * Minimal extraction of the {@code "error"} string field from a flat JSON
     * object, avoiding a new JSON dependency. Returns {@code null} when the
     * field is absent or the body is not the expected {@code {"error": "..."}}
     * shape; the caller then falls back to the raw body text.
     *
     * <p>Handles standard JSON string escapes ({@code \" \\ \/ \b \f \n \r \t}
     * and {@code \\uXXXX}). This is deliberately small: the only structured
     * error body the server emits is {@code {"error": msg}}.
     */
    private static String extractErrorField(String json) {
        int key = json.indexOf("\"error\"");
        if (key < 0) {
            return null;
        }
        int i = key + "\"error\"".length();
        // skip whitespace then ':'
        while (i < json.length() && Character.isWhitespace(json.charAt(i))) {
            i++;
        }
        if (i >= json.length() || json.charAt(i) != ':') {
            return null;
        }
        i++;
        while (i < json.length() && Character.isWhitespace(json.charAt(i))) {
            i++;
        }
        if (i >= json.length() || json.charAt(i) != '"') {
            return null;
        }
        i++; // opening quote of value
        StringBuilder sb = new StringBuilder();
        while (i < json.length()) {
            char c = json.charAt(i);
            if (c == '"') {
                return sb.toString();
            }
            if (c == '\\') {
                i++;
                if (i >= json.length()) {
                    return null;
                }
                char esc = json.charAt(i);
                switch (esc) {
                    case '"' -> sb.append('"');
                    case '\\' -> sb.append('\\');
                    case '/' -> sb.append('/');
                    case 'b' -> sb.append('\b');
                    case 'f' -> sb.append('\f');
                    case 'n' -> sb.append('\n');
                    case 'r' -> sb.append('\r');
                    case 't' -> sb.append('\t');
                    case 'u' -> {
                        if (i + 4 >= json.length()) {
                            return null;
                        }
                        try {
                            sb.append((char) Integer.parseInt(json.substring(i + 1, i + 5), 16));
                        } catch (NumberFormatException e) {
                            return null;
                        }
                        i += 4;
                    }
                    default -> {
                        return null;
                    }
                }
            } else {
                sb.append(c);
            }
            i++;
        }
        return null; // unterminated string
    }
}
