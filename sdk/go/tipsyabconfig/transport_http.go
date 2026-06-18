package tipsyabconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
)

// HTTP path suffixes appended to the configured base URLs in HTTP mode. They
// mirror the server-side publicread routes (see design §3).
const (
	httpPathPullAll          = "/api/v1/config/pull_all"
	httpPathExperimentResult = "/api/v1/abtest/experiment_result"
	httpContentTypeJSON      = "application/json"
	httpAuthorizationHeader  = "Authorization"
	httpContentTypeHeader    = "Content-Type"
)

// httpTransport is the shared HTTP plumbing used by both httpConfigTransport and
// httpAbtestTransport. It owns the *http.Client, the bearer-token source, and
// the response-size cap.
type httpTransport struct {
	client      *http.Client
	tokenSource tokenSource
	// maxRecvBytes is the response-body read cap (Config.MaxRecvMessageSize,
	// default 512MB). The transport reads at most maxRecvBytes+1 bytes and
	// returns a clear error when the limit is exceeded rather than silently
	// truncating.
	maxRecvBytes int
}

// httpConfigTransport implements configTransport over HTTP (POST protojson to
// the configured ConfigService base URL).
type httpConfigTransport struct {
	httpTransport
	// pullAllURL is the fully-resolved endpoint (baseURL + httpPathPullAll).
	pullAllURL string
}

// httpAbtestTransport implements abtestTransport over HTTP (POST protojson to
// the configured AbtestService base URL).
type httpAbtestTransport struct {
	httpTransport
	// experimentResultURL is the fully-resolved endpoint
	// (baseURL + httpPathExperimentResult).
	experimentResultURL string
}

// newHTTPConfigTransport builds an HTTP configTransport. configBaseURL must be a
// validated, trailing-slash-trimmed http(s) base URL (validated in Init).
func newHTTPConfigTransport(client *http.Client, ts tokenSource, maxRecvBytes int, configBaseURL string) configTransport {
	return &httpConfigTransport{
		httpTransport: httpTransport{client: client, tokenSource: ts, maxRecvBytes: maxRecvBytes},
		pullAllURL:    configBaseURL + httpPathPullAll,
	}
}

// newHTTPAbtestTransport builds an HTTP abtestTransport. abtestBaseURL must be a
// validated, trailing-slash-trimmed http(s) base URL (validated in Init).
func newHTTPAbtestTransport(client *http.Client, ts tokenSource, maxRecvBytes int, abtestBaseURL string) abtestTransport {
	return &httpAbtestTransport{
		httpTransport:       httpTransport{client: client, tokenSource: ts, maxRecvBytes: maxRecvBytes},
		experimentResultURL: abtestBaseURL + httpPathExperimentResult,
	}
}

func (t *httpConfigTransport) PullAll(ctx context.Context, req *configv1.PullAllRequest) (*configv1.PullAllResponse, error) {
	resp := &configv1.PullAllResponse{}
	if err := t.doProtoJSON(ctx, t.pullAllURL, req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func (t *httpAbtestTransport) GetExperimentResult(ctx context.Context, req *abtestv1.GetExperimentResultRequest) (*abtestv1.GetExperimentResultResponse, error) {
	resp := &abtestv1.GetExperimentResultResponse{}
	if err := t.doProtoJSON(ctx, t.experimentResultURL, req, resp); err != nil {
		return nil, err
	}
	return resp, nil
}

// doProtoJSON marshals req to protojson, POSTs it to url with a fresh bearer
// token, and unmarshals a 2xx response body into out. It mirrors the server
// publicread codec: protojson on the wire, DiscardUnknown on decode. The
// response body is read with an explicit cap (maxRecvBytes+1) so an oversized
// payload yields a clear error rather than a truncated decode.
//
// Non-2xx responses are read for a {"error": msg} body and surfaced as an error
// carrying the HTTP status code and message. ctx cancellation propagates via
// http.NewRequestWithContext; net/http wraps the cancellation so
// errors.Is(err, context.Canceled) holds for the caller's retry/degrade paths.
func (t *httpTransport) doProtoJSON(ctx context.Context, url string, req proto.Message, out proto.Message) error {
	body, err := protojson.Marshal(req)
	if err != nil {
		return fmt.Errorf("tipsyabconfig: marshal request: %w", err)
	}

	// Acquire the bearer token per request (static value or TokenProvider),
	// matching the gRPC PerRPCCredentials timing. A TokenProvider error fails
	// this request.
	md, err := t.tokenSource.GetRequestMetadata(ctx)
	if err != nil {
		return fmt.Errorf("tipsyabconfig: acquire token: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("tipsyabconfig: build request: %w", err)
	}
	httpReq.Header.Set(httpContentTypeHeader, httpContentTypeJSON)
	// tokenSource emits the lower-case "authorization" metadata key with a
	// "Bearer <token>" value; forward it verbatim as the HTTP header.
	if authz := md["authorization"]; authz != "" {
		httpReq.Header.Set(httpAuthorizationHeader, authz)
	}

	httpResp, err := t.client.Do(httpReq)
	if err != nil {
		// net/http already wraps context.Canceled / DeadlineExceeded so
		// errors.Is on the returned error resolves them.
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, httpResp.Body)
		_ = httpResp.Body.Close()
	}()

	// Read the body with an explicit cap. Reading maxRecvBytes+1 lets us detect
	// (rather than silently truncate) an oversized response.
	limit := int64(t.maxRecvBytes) + 1
	respBody, err := io.ReadAll(io.LimitReader(httpResp.Body, limit))
	if err != nil {
		return err
	}
	if int64(len(respBody)) > int64(t.maxRecvBytes) {
		return fmt.Errorf("tipsyabconfig: response exceeds MaxRecvMessageSize (%d bytes)", t.maxRecvBytes)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return httpStatusError(httpResp.StatusCode, respBody)
	}

	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("tipsyabconfig: decode response: %w", err)
	}
	return nil
}

// httpStatusError builds an error for a non-2xx HTTP response. It attempts to
// parse a {"error": msg} JSON body (the publicread error shape) and includes
// the HTTP status code. When the body is not the expected shape it falls back to
// the raw (trimmed) body text.
func httpStatusError(status int, body []byte) error {
	msg := ""
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Error != "" {
		msg = parsed.Error
	} else {
		msg = strings.TrimSpace(string(body))
	}
	if msg == "" {
		return fmt.Errorf("tipsyabconfig: HTTP %d", status)
	}
	return fmt.Errorf("tipsyabconfig: HTTP %d: %s", status, msg)
}
