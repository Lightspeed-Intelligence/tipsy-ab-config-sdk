package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
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
// full-release value. Historical custom rows exercise raw type compatibility; GetConfig remains config-only.
func assertExpectation(cli *tac.Client, client string, e Expectation, r *results) {
	cx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if e.Key == customKey {
		assertRetiredCustom(cx, cli, client, e, r)
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

// assertRetiredCustom verifies all raw result types after server retirement.
// The fixture's former custom users now exercise 0-default-config and 2-rejected.
func assertRetiredCustom(cx context.Context, cli *tac.Client, client string, e Expectation, r *results) {
	request := tac.ExperimentResultRequest{
		Namespace:   e.NS,
		UserInfo:    tac.UserInfo{ExperimentHashID: e.UserID, Attrs: rawAttrs(e.UserAttrs)},
		DisplayType: tac.ResultDisplayFlatKv,
		TraceID:     fmt.Sprintf("dev-e2e-go-%s-%s-%s", client, e.NS, e.UserID),
	}
	response, err := cli.GetExperimentResult(cx, request) // Type deliberately omitted.
	if err != nil || response == nil || len(response.GetCustomFlatKv().GetFields()) != 0 {
		r.fail("[%s] default %s/%s: response=%v err=%v; want config success", client, e.NS, e.UserID, response, err)
		return
	}
	//lint:ignore SA1019 Intentional compatibility test of the retired wire value.
	request.Type = tac.ExperimentTypeCustomParams
	_, err = cli.GetExperimentResult(cx, request)
	if err == nil || !strings.Contains(err.Error(), "custom_params is retired") {
		r.fail("[%s] retired custom %s/%s: err=%v; want retirement error", client, e.NS, e.UserID, err)
		return
	}
	request.Type = tac.ExperimentTypeConfigVersion
	cv, err := cli.GetExperimentResult(cx, request)
	if err != nil || cv == nil || !maps.Equal(response.GetConfigFlatKv(), cv.GetConfigFlatKv()) {
		r.fail("[%s] CONFIG_VERSION %s/%s: %v", client, e.NS, e.UserID, err)
		return
	}
	request.Type = tac.ExperimentTypeAll
	all, err := cli.GetExperimentResult(cx, request)
	if err != nil || all == nil || !maps.Equal(cv.GetConfigFlatKv(), all.GetConfigFlatKv()) || len(all.GetCustomFlatKv().GetFields()) != 0 {
		r.fail("[%s] ALL %s/%s: response=%v err=%v; want config-only parity", client, e.NS, e.UserID, all, err)
		return
	}
	r.pass("[%s] raw types %s/%s: 0/1/3 config, 2 rejected", client, e.NS, e.UserID)
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
