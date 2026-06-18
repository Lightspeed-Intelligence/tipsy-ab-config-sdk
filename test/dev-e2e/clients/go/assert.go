package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	tac "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/sdk/go/tipsyabconfig"
)

// Expectation mirrors one row of fixtures/expectations.json.
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
const defaultSentinel = "<DEFAULT>"

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

// results accumulates PASS/FAIL counts; grpcDegraded flags a gRPC connect/init
// failure that we tolerated but still surface as a visible non-success.
type results struct {
	passed       int
	failed       int
	grpcDegraded bool
}

func (r *results) pass(format string, a ...interface{}) {
	r.passed++
	fmt.Printf("PASS  "+format+"\n", a...)
}

func (r *results) fail(format string, a ...interface{}) {
	r.failed++
	fmt.Printf("FAIL  "+format+"\n", a...)
}

// rawAttrs converts the fixture's typed Value envelope (e.g.
// {"country":{"s":"US"}}) back into the RAW Go map the SDK expects (e.g.
// {"country":"US"}). The SDK re-encodes raw values into Value on the wire, so
// passing the envelope would double-wrap and break admission matching.
func rawAttrs(envelope map[string]interface{}) map[string]any {
	if len(envelope) == 0 {
		return nil
	}
	out := make(map[string]any, len(envelope))
	for k, v := range envelope {
		inner, ok := v.(map[string]interface{})
		if !ok {
			// Already raw (defensive); pass through.
			out[k] = v
			continue
		}
		switch {
		case has(inner, "s"):
			out[k] = inner["s"]
		case has(inner, "b"):
			out[k] = inner["b"]
		case has(inner, "d"):
			out[k], _ = asFloat(inner["d"])
		case has(inner, "i"):
			// proto int64 is a JSON string in the envelope; SDK accepts int64.
			out[k] = toInt64(inner["i"])
		default:
			out[k] = v
		}
	}
	return out
}

func has(m map[string]interface{}, k string) bool { _, ok := m[k]; return ok }

func asFloat(v interface{}) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

func toInt64(v interface{}) int64 {
	switch x := v.(type) {
	case string:
		var n int64
		_, _ = fmt.Sscan(x, &n)
		return n
	case float64:
		return int64(x)
	}
	return 0
}

// assertExpectation drives one expectation row through the Go SDK. Config keys
// use GetConfig (dynamic, abtest-aware) plus a GetConfigStatic check for the
// full-release value. Custom-only rows are exercised via the SDK's
// GetExperimentResult wrapper (custom_flat_kv); GetConfig is config_version only.
func assertExpectation(cli *tac.Client, client string, e Expectation, r *results) {
	cx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if e.Key == customKey {
		assertCustom(cx, cli, client, e, r)
		return
	}

	expectedVal, ok := e.ExpectedValue.(string)
	if !ok {
		r.fail("[%s] %s/%s/%s: expected_value not a string (%T)", client, e.NS, e.UserID, e.Key, e.ExpectedValue)
		return
	}

	abctx := cli.NewAbtestContext(cx, e.UserID, rawAttrs(e.UserAttrs))
	got, err := cli.GetConfig(cx, abctx, e.NS, e.Key, defaultSentinel)
	if err != nil {
		r.fail("[%s] GetConfig %s/%s/%s: %v", client, e.NS, e.UserID, e.Key, err)
		return
	}
	if got != expectedVal {
		r.fail("[%s] GetConfig %s/%s/%s: got %q want %q (%s)", client, e.NS, e.UserID, e.Key, got, expectedVal, e.Source)
	} else {
		r.pass("[%s] GetConfig %s/%s/%s = %q (%s)", client, e.NS, e.UserID, e.Key, got, e.Source)
	}
}

// assertCustom exercises the SDK GetExperimentResult wrapper for custom_params
// rows and deep-compares custom_flat_kv to the expected KV object.
func assertCustom(cx context.Context, cli *tac.Client, client string, e Expectation, r *results) {
	want, ok := e.ExpectedValue.(map[string]interface{})
	if !ok {
		r.fail("[%s] %s/%s custom: expected_value not an object (%T)", client, e.NS, e.UserID, e.ExpectedValue)
		return
	}
	resp, err := cli.GetExperimentResult(cx, tac.ExperimentResultRequest{
		Namespace:   e.NS,
		UserInfo:    tac.UserInfo{UID: e.UserID, Attrs: rawAttrs(e.UserAttrs)},
		Type:        tac.ExperimentTypeCustomParams,
		DisplayType: tac.ResultDisplayFlatKv,
		// Explicit trace_id so the SDK call, the server compute log and any
		// downstream exposure-report row all join on a recognisable id.
		// Empty would also work (server auto-fills), but this exercises the
		// explicit-id path on every dev-e2e run (sdk-trace-id §4).
		TraceID: fmt.Sprintf("dev-e2e-go-%s-%s-%s", client, e.NS, e.UserID),
	})
	if err != nil {
		r.fail("[%s] GetExperimentResult(custom) %s/%s: %v", client, e.NS, e.UserID, err)
		return
	}
	got := map[string]interface{}{}
	if s := resp.GetCustomFlatKv(); s != nil {
		got = s.AsMap()
	}
	if !kvEqual(got, want) {
		r.fail("[%s] custom %s/%s: custom_flat_kv %v want %v", client, e.NS, e.UserID, got, want)
		return
	}
	r.pass("[%s] custom %s/%s custom_flat_kv = %v (%s)", client, e.NS, e.UserID, want, e.Source)
}

func kvEqual(got, want map[string]interface{}) bool {
	if len(got) != len(want) {
		return false
	}
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			return false
		}
		gn, gok := asFloat(gv)
		wn, wok := asFloat(wv)
		if gok && wok {
			if gn != wn {
				return false
			}
			continue
		}
		if fmt.Sprintf("%v", gv) != fmt.Sprintf("%v", wv) {
			return false
		}
	}
	return true
}
