package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Expectation mirrors one row of fixtures/expectations.json (the golden
// expected result emitted by tools/bucketfind).
//
// ExpectedValue is a string for config keys and an object (map) for custom rows
// (key == "__custom__"). UserAttrs uses the typed Value envelope, e.g.
// {"country":{"s":"US"}}.
type Expectation struct {
	NS                string                 `json:"ns"`
	UserID            string                 `json:"user_id"`
	UserAttrs         map[string]interface{} `json:"user_attrs"`
	Key               string                 `json:"key"`
	ExpectedVersionID string                 `json:"expected_version_id"`
	ExpectedValue     interface{}            `json:"expected_value"`
	Source            string                 `json:"source"`
	Note              string                 `json:"note"`
	AppliesTo         []string               `json:"applies_to"`
}

const customKey = "__custom__"

func (e Expectation) appliesTo(client string) bool {
	for _, c := range e.AppliesTo {
		if c == client {
			return true
		}
	}
	return false
}

func loadExpectations(path string) ([]Expectation, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Expectation
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, nil
}

// resolveFixturesPath returns the absolute fixtures path. An explicit flag wins;
// otherwise it walks up from the working dir looking for the fixtures file
// itself at the known relative path, so the driver works whether you run it
// from the repo root or from this driver's own module directory.
func resolveFixturesPath(flagVal string) (string, error) {
	if flagVal != "" {
		return filepath.Abs(flagVal)
	}
	const rel = "test/dev-e2e/fixtures/expectations.json"
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, rel)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Abs(rel)
		}
		dir = parent
	}
}

// httpClient wraps the dev base URL + bearer token for the three read endpoints.
type httpClient struct {
	base  string
	token string
	cli   *http.Client
}

func newHTTPClient(base, token string) *httpClient {
	return &httpClient{
		base:  base,
		token: token,
		cli:   &http.Client{Timeout: 15 * time.Second},
	}
}

func (h *httpClient) post(ctx context.Context, path string, reqBody interface{}, out interface{}) error {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		snippet := string(rb)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return fmt.Errorf("%s → HTTP %d: %s", path, resp.StatusCode, snippet)
	}
	if out != nil {
		if err := json.Unmarshal(rb, out); err != nil {
			return fmt.Errorf("decode %s response: %w (body=%s)", path, err, string(rb))
		}
	}
	return nil
}

// ---- request / response shapes (camelCase requests, snake_case responses) ----

type configRequest struct {
	Namespace string                 `json:"namespace"`
	UserID    string                 `json:"userId,omitempty"`
	UserAttrs map[string]interface{} `json:"userAttrs,omitempty"`
	Keys      []string               `json:"keys"`
}

type configResponse struct {
	Values map[string]string `json:"values"`
}

type experimentResultRequest struct {
	Namespace      string                 `json:"namespace"`
	UserID         string                 `json:"userId,omitempty"`
	UserAttrs      map[string]interface{} `json:"userAttrs,omitempty"`
	ExperimentType string                 `json:"experimentType"`
	DisplayType    string                 `json:"displayType"`
	LayerIDs       []string               `json:"layerIds,omitempty"`
}

// experimentResultResponse captures the fields the driver asserts on. The
// server returns int64 version ids as JSON strings, so config_flat_kv is a
// map[string]string here. custom_flat_kv is the raw object (protojson Struct).
type experimentResultResponse struct {
	ConfigFlatKV map[string]string      `json:"config_flat_kv"`
	CustomFlatKV map[string]interface{} `json:"custom_flat_kv"`
	ComputedAt   interface{}            `json:"computed_at"`
}

func (h *httpClient) getDynamic(ctx context.Context, ns, userID string, attrs map[string]interface{}, keys []string) (map[string]string, error) {
	var out configResponse
	err := h.post(ctx, "/api/v1/config/dynamic", configRequest{
		Namespace: ns, UserID: userID, UserAttrs: attrs, Keys: keys,
	}, &out)
	return out.Values, err
}

func (h *httpClient) getStatic(ctx context.Context, ns string, keys []string) (map[string]string, error) {
	var out configResponse
	err := h.post(ctx, "/api/v1/config/static", configRequest{
		Namespace: ns, Keys: keys,
	}, &out)
	return out.Values, err
}

func (h *httpClient) getExperimentResult(ctx context.Context, ns, userID string, attrs map[string]interface{}, expType, displayType string) (*experimentResultResponse, error) {
	var out experimentResultResponse
	err := h.post(ctx, "/api/v1/abtest/experiment_result", experimentResultRequest{
		Namespace:      ns,
		UserID:         userID,
		UserAttrs:      attrs,
		ExperimentType: expType,
		DisplayType:    displayType,
	}, &out)
	return &out, err
}
