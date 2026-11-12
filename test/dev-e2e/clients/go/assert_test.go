package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	tac "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig"
)

// A transport can return mutually equal but incorrect results. Exercise the
// probe itself so empty/wrong-user data and an incorrect error code fail.
func TestRawResultProbeRequiresGoldenResultAndStatus(t *testing.T) {
	for _, tc := range []struct {
		name           string
		want           map[string]string
		result         map[string]string
		customOnConfig bool
		rejection      int
		fail           bool
	}{
		{name: "hit", want: map[string]string{"feature": "123"}, result: map[string]string{"feature": "123"}, rejection: 400},
		{name: "all_empty", want: map[string]string{"feature": "123"}, result: map[string]string{}, rejection: 400, fail: true},
		{name: "wrong_user", want: map[string]string{"feature": "123"}, result: map[string]string{"feature": "124"}, rejection: 400, fail: true},
		{name: "wrong_error_code", want: map[string]string{"feature": "123"}, result: map[string]string{"feature": "123"}, rejection: 500, fail: true},
		{name: "config_has_custom", want: map[string]string{"feature": "123"}, result: map[string]string{"feature": "123"}, rejection: 400, customOnConfig: true, fail: true},
		{name: "no_hit", want: map[string]string{}, result: map[string]string{}, rejection: 400},
		{name: "missing_golden", result: map[string]string{}, rejection: 400, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/api/v1/abtest/experiment_result" {
					_, _ = w.Write([]byte(`{}`))
					return
				}
				var req map[string]any
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					return
				}
				typ := req["experiment_type"]
				if typ == nil {
					typ = req["experimentType"]
				}
				if typ == "EXPERIMENT_TYPE_CUSTOM_PARAMS" || typ == float64(2) {
					w.WriteHeader(tc.rejection)
					_ = json.NewEncoder(w).Encode(map[string]string{"error": "custom_params is retired"})
					return
				}
				body := map[string]any{"config_flat_kv": tc.result}
				if tc.customOnConfig && (typ == "EXPERIMENT_TYPE_CONFIG_VERSION" || typ == float64(1)) {
					body["custom_flat_kv"] = map[string]any{"secret": "retired"}
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			cli, err := tac.Init(context.Background(), tac.Config{Namespaces: []string{"probe"}, ConfigServiceAddr: server.URL, AbtestServiceAddr: server.URL, Token: "test", Transport: "http", StartupFailOpen: true})
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()
			result := &results{}
			assertRetiredCustom(context.Background(), cli, "go_sdk_http", Expectation{NS: "probe", UserID: "u", ExpectedConfigFlatKV: tc.want}, result)
			if tc.fail {
				if result.failed != 1 || result.passed != 0 {
					t.Fatalf("bad response passed: %+v", result)
				}
			} else if result.failed != 0 || result.passed != 1 {
				t.Fatalf("valid response failed: %+v", result)
			}
		})
	}
}
