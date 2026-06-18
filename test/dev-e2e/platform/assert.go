package main

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"
)

const (
	expTypeConfigVersion = "EXPERIMENT_TYPE_CONFIG_VERSION"
	expTypeCustomParams  = "EXPERIMENT_TYPE_CUSTOM_PARAMS"
	displayFlatKV        = "RESULT_DISPLAY_TYPE_FLAT_KV"
)

// results accumulates PASS/FAIL counts across all checks.
type results struct {
	passed int
	failed int
}

func (r *results) pass(format string, a ...interface{}) {
	r.passed++
	fmt.Printf("PASS  "+format+"\n", a...)
}

func (r *results) fail(format string, a ...interface{}) {
	r.failed++
	fmt.Printf("FAIL  "+format+"\n", a...)
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 20*time.Second)
}

// runStickyStability exercises Testing Plan #8: demo-test E_cfg has
// sticky_enabled=true, so once a user is bucketed into a group the server
// persists a sticky assignment (experiment_sticky_assignment) on the compute
// path and every subsequent experiment_result for that user must return the
// SAME group/version. We assert stability across repeated calls on the SERVER
// path (experiment_result), which is where sticky writes happen — the SDK's
// local compute does not write sticky. We use a fresh, unique UID per run so
// the first call establishes the assignment and the rest must match it.
func runStickyStability(c *httpClient, r *results) {
	const ns = "demo-test"
	const key = "welcome_text"
	uid := fmt.Sprintf("sticky-probe-%d", time.Now().UnixNano())

	first := ""
	const rounds = 4
	stable := true
	for i := 0; i < rounds; i++ {
		cx, cancel := ctx()
		res, err := c.getExperimentResult(cx, ns, uid, nil, expTypeConfigVersion, displayFlatKV)
		cancel()
		if err != nil {
			r.fail("[sticky] %s/%s round %d: %v", ns, uid, i, err)
			return
		}
		got, present := res.ConfigFlatKV[key]
		if !present {
			r.fail("[sticky] %s/%s round %d: %s absent from config_flat_kv (expected a stable group hit)", ns, uid, i, key)
			return
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			stable = false
			r.fail("[sticky] %s/%s round %d: version %s != first %s (sticky drift)", ns, uid, i, got, first)
		}
	}
	if stable {
		r.pass("[sticky] %s/%s stable across %d rounds (version=%s)", ns, uid, rounds, first)
	}
}

// client. For config keys it checks BOTH the resolved value (config/dynamic)
// AND the version id (experiment_result config_flat_kv). For custom rows it
// checks custom_flat_kv deep-equals the expected object.
func runExpectations(c *httpClient, exps []Expectation, r *results) {
	for _, e := range exps {
		if !e.appliesTo("http") {
			continue
		}
		if e.Key == customKey {
			assertCustom(c, e, r)
			continue
		}
		assertConfigKey(c, e, r)
	}
}

// assertConfigKey verifies a config_version expectation via both the dynamic
// resolved value and the experiment_result version id.
func assertConfigKey(c *httpClient, e Expectation, r *results) {
	expectedVal, ok := e.ExpectedValue.(string)
	if !ok {
		r.fail("[dynamic] %s/%s/%s: expected_value is not a string (%T)", e.NS, e.UserID, e.Key, e.ExpectedValue)
		return
	}

	// 1) config/dynamic resolved value.
	cx, cancel := ctx()
	vals, err := c.getDynamic(cx, e.NS, e.UserID, e.UserAttrs, []string{e.Key})
	cancel()
	if err != nil {
		r.fail("[dynamic] %s/%s/%s: %v", e.NS, e.UserID, e.Key, err)
	} else if got := vals[e.Key]; got != expectedVal {
		r.fail("[dynamic] %s/%s/%s: got %q want %q (%s)", e.NS, e.UserID, e.Key, got, expectedVal, e.Source)
	} else {
		r.pass("[dynamic] %s/%s/%s = %q (%s)", e.NS, e.UserID, e.Key, got, e.Source)
	}

	// 2) experiment_result config_flat_kv version id.
	//    Both experiment hits (source=experiment_group) AND gray-release hits
	//    (source=whitelist) surface in config_flat_kv — the compute engine
	//    assembles gray FIRST then experiments (internal/abtest/compute/
	//    engine.go:187-219). Only a full-release FALLBACK (source=full, i.e. no
	//    experiment hit and no gray) is ABSENT from config_flat_kv: dynamic
	//    resolves it via the release_full table, not via the abtest result.
	cx2, cancel2 := ctx()
	res, err := c.getExperimentResult(cx2, e.NS, e.UserID, e.UserAttrs, expTypeConfigVersion, displayFlatKV)
	cancel2()
	if err != nil {
		r.fail("[exp_result] %s/%s/%s: %v", e.NS, e.UserID, e.Key, err)
		return
	}
	gotVer, present := res.ConfigFlatKV[e.Key]
	switch e.Source {
	case "experiment_group", "whitelist":
		if !present {
			r.fail("[exp_result] %s/%s/%s: version absent from config_flat_kv, want %s (%s)", e.NS, e.UserID, e.Key, e.ExpectedVersionID, e.Source)
		} else if gotVer != e.ExpectedVersionID {
			r.fail("[exp_result] %s/%s/%s: version %s want %s (%s)", e.NS, e.UserID, e.Key, gotVer, e.ExpectedVersionID, e.Source)
		} else {
			r.pass("[exp_result] %s/%s/%s version=%s (%s)", e.NS, e.UserID, e.Key, gotVer, e.Source)
		}
	case "full":
		// Full-release fallback does not surface in config_flat_kv (the abtest
		// result only carries experiment + gray hits). Absence is correct.
		if present {
			r.fail("[exp_result] %s/%s/%s: unexpected config_flat_kv version %s for source=full (expected absent)", e.NS, e.UserID, e.Key, gotVer)
		} else {
			r.pass("[exp_result] %s/%s/%s absent from config_flat_kv as expected (full fallback)", e.NS, e.UserID, e.Key)
		}
	default:
		r.fail("[exp_result] %s/%s/%s: unknown source %q", e.NS, e.UserID, e.Key, e.Source)
	}
}

// assertCustom verifies a custom_params expectation: custom_flat_kv deep-equals
// the expected KV object (after numeric normalization).
func assertCustom(c *httpClient, e Expectation, r *results) {
	want, ok := e.ExpectedValue.(map[string]interface{})
	if !ok {
		r.fail("[custom] %s/%s: expected_value is not an object (%T)", e.NS, e.UserID, e.ExpectedValue)
		return
	}
	cx, cancel := ctx()
	res, err := c.getExperimentResult(cx, e.NS, e.UserID, e.UserAttrs, expTypeCustomParams, displayFlatKV)
	cancel()
	if err != nil {
		r.fail("[custom] %s/%s: %v", e.NS, e.UserID, err)
		return
	}
	if !kvEqual(res.CustomFlatKV, want) {
		r.fail("[custom] %s/%s: custom_flat_kv %v want %v", e.NS, e.UserID, res.CustomFlatKV, want)
		return
	}
	r.pass("[custom] %s/%s custom_flat_kv = %v (%s)", e.NS, e.UserID, want, e.Source)
}

// kvEqual deep-compares two JSON-decoded KV maps, normalizing all numbers to
// float64 (JSON's universal number) so 10 == 10.0 and 1.5 == 1.5 regardless of
// how protojson rendered them.
func kvEqual(got, want map[string]interface{}) bool {
	if len(got) != len(want) {
		return false
	}
	for k, wv := range want {
		gv, ok := got[k]
		if !ok {
			return false
		}
		if !valueEqual(gv, wv) {
			return false
		}
	}
	return true
}

func valueEqual(a, b interface{}) bool {
	an, aok := asFloat(a)
	bn, bok := asFloat(b)
	if aok && bok {
		return an == bn
	}
	return reflect.DeepEqual(a, b)
}

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

// staticExpectation is a fixed full-release value asserted via config/static.
// These are the FULL (active full-release) values regardless of any experiment
// or gray overlay — static must always return them.
var staticExpectations = []struct {
	ns, key, want string
}{
	{"demo-test", "welcome_text", "welcome-FULL"},
	{"demo-test", "banner_color", "green"},
	{"demo-test", "gap_key", "gap-FULL"},
	{"demo-test", "admit_key", "admit-FULL"},
	{"for_dev_agent_test", "color", "c-FULL"},
	{"for_dev_agent_test", "greeting", "hi-FULL"},
}

// runStaticMatrix asserts config/static returns the full-release value for each
// distinct (ns,key), proving static is independent of experiments/gray.
func runStaticMatrix(c *httpClient, r *results) {
	// group keys by namespace so we issue one static call per ns.
	byNS := map[string][]string{}
	wantByNSKey := map[string]string{}
	for _, s := range staticExpectations {
		byNS[s.ns] = append(byNS[s.ns], s.key)
		wantByNSKey[s.ns+"\x00"+s.key] = s.want
	}
	nsList := make([]string, 0, len(byNS))
	for ns := range byNS {
		nsList = append(nsList, ns)
	}
	sort.Strings(nsList)

	for _, ns := range nsList {
		keys := byNS[ns]
		sort.Strings(keys)
		cx, cancel := ctx()
		vals, err := c.getStatic(cx, ns, keys)
		cancel()
		if err != nil {
			for _, k := range keys {
				r.fail("[static] %s/%s: %v", ns, k, err)
			}
			continue
		}
		for _, k := range keys {
			want := wantByNSKey[ns+"\x00"+k]
			if got := vals[k]; got != want {
				r.fail("[static] %s/%s: got %q want %q (full-release value)", ns, k, got, want)
			} else {
				r.pass("[static] %s/%s = %q (full-release)", ns, k, got)
			}
		}
	}
}
