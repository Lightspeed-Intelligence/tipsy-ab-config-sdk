package tipsyabconfig

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strconv"
	"testing"

	abtestv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/abtest/v1"
)

// Typed config accessor tests.
//
// All cases are pure in-process: we seed c.cache via the same fake-config
// snapshot harness the rest of the package uses (newHarness +
// cfgServer.SetPullSnapshot(makeSnapshot(...))), then call the typed wrappers.
// No live server is required.
//
// Cache-seeding cheatsheet (mirrors get_config_test.go / cache_test.go):
//   - Static hit for (ns, key) = "raw"     -> {full: V, versions: {V: "raw"}}
//   - Static miss                           -> key absent, or full: 0 (no full release)
//   - Dynamic full-release hit              -> EmptyAbtestContext() (no Compute RPC)
//   - Dynamic abtest hit on version W       -> MockAbtestContext(uid,{ns:{key:W}})
//                                              + {versions:{W:"raw", ...}}
//   - Dynamic underlying error              -> nil *AbtestContext (ErrAbtestContextMissing)

// typedKey is a small helper describing one cached key for makeSnapshot.
type typedKey = struct {
	full     int64
	versions map[int64]string
}

// seedStatic builds a Client whose cache holds the given full-release values,
// with NO abtest channel (static-only). keys maps key -> cached full value.
func seedStatic(t *testing.T, keys map[string]string) *Client {
	t.Helper()
	h := newHarness(t)
	snapKeys := map[string]typedKey{}
	var ver int64 = 100
	for k, v := range keys {
		ver++
		snapKeys[k] = typedKey{full: ver, versions: map[int64]string{ver: v}}
	}
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, snapKeys))
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// ---------------------------------------------------------------------------
// Static — Bool
// ---------------------------------------------------------------------------

func TestGetConfigStaticBool_HitMissAndLenientMatrix(t *testing.T) {
	// Miss: key not cached -> (def, false). def deliberately true so we can
	// tell it through.
	cli := seedStatic(t, map[string]string{"present": "true"})
	if v, ok := cli.GetConfigStaticBool("ns1", "absent", true); ok || v != true {
		t.Fatalf("miss: got (%v,%v), want (true,false)", v, ok)
	}
	// Hit always returns ok=true (bool parse never fails).
	if v, ok := cli.GetConfigStaticBool("ns1", "present", false); !ok || v != true {
		t.Fatalf("hit true: got (%v,%v), want (true,true)", v, ok)
	}

	// Full lenient matrix on a cache HIT: must return (parsed, true) for every
	// case and NEVER ok=false on a hit.
	truthy := []string{"true", "TRUE", "True", "1", " true ", "\ttrue\n", " 1 "}
	falsy := []string{"false", "FALSE", "False", "0", "yes", "no", "garbage", "t", "2", "-1", "1.0", "on", "  "}

	for _, raw := range truthy {
		c := seedStatic(t, map[string]string{"b": raw})
		v, ok := c.GetConfigStaticBool("ns1", "b", false)
		if !ok {
			t.Fatalf("truthy %q: static hit must always return ok=true", raw)
		}
		if v != true {
			t.Fatalf("truthy %q: got %v, want true", raw, v)
		}
	}
	for _, raw := range falsy {
		c := seedStatic(t, map[string]string{"b": raw})
		v, ok := c.GetConfigStaticBool("ns1", "b", true)
		if !ok {
			t.Fatalf("falsy %q: static hit must always return ok=true", raw)
		}
		if v != false {
			t.Fatalf("falsy %q: got %v, want false", raw, v)
		}
	}
}

func TestGetConfigStaticBool_EmptyCachedValueIsHitFalse(t *testing.T) {
	// A genuinely cached empty string is a HIT (ok=true) for the static path
	// (it gates on the underlying ok, not raw==""), and the lenient bool rule
	// maps "" -> false.
	cli := seedStaticEmpty(t, "b")
	if v, ok := cli.GetConfigStaticBool("ns1", "b", true); !ok || v != false {
		t.Fatalf("empty cached bool: got (%v,%v), want (false,true)", v, ok)
	}
}

// ---------------------------------------------------------------------------
// Static — Int64 (incl. precision)
// ---------------------------------------------------------------------------

func TestGetConfigStaticInt64_HitMissParseFail(t *testing.T) {
	cli := seedStatic(t, map[string]string{
		"n":       "42",
		"neg":     "-7",
		"spaced":  "  123  ",
		"bad":     "12.5",
		"garbage": "notanint",
	})
	// Miss.
	if v, ok := cli.GetConfigStaticInt64("ns1", "absent", 99); ok || v != 99 {
		t.Fatalf("miss: got (%d,%v), want (99,false)", v, ok)
	}
	// Hit.
	if v, ok := cli.GetConfigStaticInt64("ns1", "n", -1); !ok || v != 42 {
		t.Fatalf("hit: got (%d,%v), want (42,true)", v, ok)
	}
	if v, ok := cli.GetConfigStaticInt64("ns1", "neg", -1); !ok || v != -7 {
		t.Fatalf("neg: got (%d,%v), want (-7,true)", v, ok)
	}
	// Surrounding whitespace is trimmed before ParseInt.
	if v, ok := cli.GetConfigStaticInt64("ns1", "spaced", -1); !ok || v != 123 {
		t.Fatalf("spaced: got (%d,%v), want (123,true)", v, ok)
	}
	// Parse failure -> (def, false).
	if v, ok := cli.GetConfigStaticInt64("ns1", "bad", 7); ok || v != 7 {
		t.Fatalf("float string: got (%d,%v), want (7,false)", v, ok)
	}
	if v, ok := cli.GetConfigStaticInt64("ns1", "garbage", 7); ok || v != 7 {
		t.Fatalf("garbage: got (%d,%v), want (7,false)", v, ok)
	}
}

func TestGetConfigStaticInt64_PrecisionBeyond2Pow53(t *testing.T) {
	// The whole point of the long-as-string discipline: values beyond 2^53
	// must survive intact (a float64 round-trip would corrupt them).
	const big1 = int64(9007199254740993) // 2^53 + 1, NOT representable as float64
	const big2 = int64(20000000003)      // the experiment-preview-style big id
	cases := map[string]int64{
		"big1": big1,
		"big2": big2,
		"max":  math.MaxInt64,
		"min":  math.MinInt64,
	}
	seed := map[string]string{}
	for k, v := range cases {
		seed[k] = strconv.FormatInt(v, 10)
	}
	cli := seedStatic(t, seed)
	for k, want := range cases {
		v, ok := cli.GetConfigStaticInt64("ns1", k, 0)
		if !ok {
			t.Fatalf("%s: expected hit", k)
		}
		if v != want {
			t.Fatalf("%s: got %d, want EXACT %d (precision loss)", k, v, want)
		}
	}
	// Sanity: big1 really is the classic float-lossy value.
	if float64(big1) == float64(big1-1) {
		// expected: the two int64 collapse to the same float64
	} else {
		t.Fatal("precondition wrong: 2^53+1 should be float-indistinguishable from 2^53")
	}

	// Overflow beyond int64 must be a parse failure (def,false), not a wrap.
	over := seedStatic(t, map[string]string{"over": "9223372036854775808"}) // MaxInt64+1
	if v, ok := over.GetConfigStaticInt64("ns1", "over", -5); ok || v != -5 {
		t.Fatalf("overflow: got (%d,%v), want (-5,false)", v, ok)
	}
}

// ---------------------------------------------------------------------------
// Static — Float64
// ---------------------------------------------------------------------------

func TestGetConfigStaticFloat64_HitMissParseFail(t *testing.T) {
	cli := seedStatic(t, map[string]string{
		"f":       "0.5",
		"sci":     "1.5e3",
		"spaced":  "  2.25 ",
		"garbage": "notafloat",
	})
	if v, ok := cli.GetConfigStaticFloat64("ns1", "absent", 9.9); ok || v != 9.9 {
		t.Fatalf("miss: got (%v,%v), want (9.9,false)", v, ok)
	}
	if v, ok := cli.GetConfigStaticFloat64("ns1", "f", -1); !ok || v != 0.5 {
		t.Fatalf("hit: got (%v,%v), want (0.5,true)", v, ok)
	}
	if v, ok := cli.GetConfigStaticFloat64("ns1", "sci", -1); !ok || v != 1500 {
		t.Fatalf("sci: got (%v,%v), want (1500,true)", v, ok)
	}
	if v, ok := cli.GetConfigStaticFloat64("ns1", "spaced", -1); !ok || v != 2.25 {
		t.Fatalf("spaced: got (%v,%v), want (2.25,true)", v, ok)
	}
	if v, ok := cli.GetConfigStaticFloat64("ns1", "garbage", 3.3); ok || v != 3.3 {
		t.Fatalf("garbage: got (%v,%v), want (3.3,false)", v, ok)
	}
}

// ---------------------------------------------------------------------------
// Static — String (legacy equivalence)
// ---------------------------------------------------------------------------

func TestGetConfigStaticString_EquivalentToLegacy(t *testing.T) {
	cli := seedStatic(t, map[string]string{"s": "hello world"})
	// Hit: returns raw + true.
	if v, ok := cli.GetConfigStaticString("ns1", "s", "def"); !ok || v != "hello world" {
		t.Fatalf("hit: got (%q,%v), want (hello world,true)", v, ok)
	}
	// Equivalence with legacy GetConfigStatic on the same key.
	legacy, lok := cli.GetConfigStatic("ns1", "s", "def")
	typed, tok := cli.GetConfigStaticString("ns1", "s", "def")
	if legacy != typed || lok != tok {
		t.Fatalf("string accessor diverged from legacy: legacy=(%q,%v) typed=(%q,%v)", legacy, lok, typed, tok)
	}
	// Miss: returns def + false (same as legacy).
	if v, ok := cli.GetConfigStaticString("ns1", "absent", "def"); ok || v != "def" {
		t.Fatalf("miss: got (%q,%v), want (def,false)", v, ok)
	}
	lv, llok := cli.GetConfigStatic("ns1", "absent", "def")
	tv, ttok := cli.GetConfigStaticString("ns1", "absent", "def")
	if lv != tv || llok != ttok {
		t.Fatalf("string accessor miss diverged from legacy: legacy=(%q,%v) typed=(%q,%v)", lv, llok, tv, ttok)
	}
}

func TestGetConfigStaticString_EmptyCachedValueIsHit(t *testing.T) {
	// An empty cached string is a real hit for the static path (gates on ok).
	cli := seedStaticEmpty(t, "s")
	if v, ok := cli.GetConfigStaticString("ns1", "s", "def"); !ok || v != "" {
		t.Fatalf("empty cached string: got (%q,%v), want ('',true)", v, ok)
	}
}

// ---------------------------------------------------------------------------
// Static — JSON (struct + map + parse fail)
// ---------------------------------------------------------------------------

type jsonPayload struct {
	Name    string `json:"name"`
	Count   int    `json:"count"`
	Enabled bool   `json:"enabled"`
}

func TestGetConfigStaticJSON_StructAndMap(t *testing.T) {
	cli := seedStatic(t, map[string]string{
		"obj":  `{"name":"x","count":3,"enabled":true}`,
		"arr":  `[1,2,3]`,
		"bad":  `{not json`,
		"frag": `{"name":"y"`, // truncated -> unmarshal fails
	})

	// Unmarshal into a struct.
	var p jsonPayload
	if ok := cli.GetConfigStaticJSON("ns1", "obj", &p); !ok {
		t.Fatal("obj struct: expected ok=true")
	}
	if p.Name != "x" || p.Count != 3 || !p.Enabled {
		t.Fatalf("obj struct: got %+v", p)
	}

	// Unmarshal into a map.
	var m map[string]any
	if ok := cli.GetConfigStaticJSON("ns1", "obj", &m); !ok {
		t.Fatal("obj map: expected ok=true")
	}
	if m["name"] != "x" {
		t.Fatalf("obj map: got %+v", m)
	}

	// Unmarshal an array into a slice.
	var arr []int
	if ok := cli.GetConfigStaticJSON("ns1", "arr", &arr); !ok {
		t.Fatal("arr: expected ok=true")
	}
	if !reflect.DeepEqual(arr, []int{1, 2, 3}) {
		t.Fatalf("arr: got %v", arr)
	}

	// Parse failure -> ok=false.
	var bad map[string]any
	if ok := cli.GetConfigStaticJSON("ns1", "bad", &bad); ok {
		t.Fatal("bad json: expected ok=false")
	}
	var frag jsonPayload
	if ok := cli.GetConfigStaticJSON("ns1", "frag", &frag); ok {
		t.Fatal("truncated json: expected ok=false")
	}

	// Miss -> ok=false.
	var miss map[string]any
	if ok := cli.GetConfigStaticJSON("ns1", "absent", &miss); ok {
		t.Fatal("miss json: expected ok=false")
	}
}

// ---------------------------------------------------------------------------
// seedStaticEmpty seeds a key whose cached full-release value is the empty
// string (a genuine hit of ""), to exercise the hit-vs-empty distinction the
// static path makes via the underlying ok.
// ---------------------------------------------------------------------------

func seedStaticEmpty(t *testing.T, key string) *Client {
	t.Helper()
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		key: {full: 11, versions: map[int64]string{11: ""}},
	}))
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// ===========================================================================
// Dynamic accessors
//
// The dynamic methods wrap GetConfig (which has NO miss signal — a miss
// returns ("", nil)). Per design Q4 they keep (T, error) and treat raw=="" as
// a miss returning (def, nil). They surface the underlying ctx/ns error and,
// for int64/float64/json, wrap parse failures into a non-nil error. Bool never
// produces a parse error.
// ===========================================================================

// seedDynamicFull builds a Client with an abtest channel wired (baseConfig)
// and full-release values seeded. The dynamic full-release path is exercised
// with EmptyAbtestContext() so no Compute RPC fires; the value resolves from
// the full release.
func seedDynamicFull(t *testing.T, keys map[string]string) *Client {
	t.Helper()
	h := newHarness(t)
	snapKeys := map[string]typedKey{}
	var ver int64 = 100
	for k, v := range keys {
		ver++
		snapKeys[k] = typedKey{full: ver, versions: map[int64]string{ver: v}}
	}
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, snapKeys))
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// emptyCtx returns an EmptyAbtestContext bound to a fresh background context.
func emptyCtx(cli *Client) (context.Context, *AbtestContext) {
	return context.Background(), cli.EmptyAbtestContext()
}

// ---------------------------------------------------------------------------
// Dynamic — Bool (full-release + abtest path + lenient matrix + never-errors)
// ---------------------------------------------------------------------------

func TestGetConfigBool_FullReleaseHitAndMiss(t *testing.T) {
	cli := seedDynamicFull(t, map[string]string{"flag": "true"})
	ctx, abctx := emptyCtx(cli)

	// Hit on full release.
	if v, err := cli.GetConfigBool(ctx, abctx, "ns1", "flag", false); err != nil || v != true {
		t.Fatalf("hit: got (%v,%v), want (true,nil)", v, err)
	}
	// Miss (no such key) -> (def, nil), NOT an error.
	if v, err := cli.GetConfigBool(ctx, abctx, "ns1", "absent", true); err != nil || v != true {
		t.Fatalf("miss: got (%v,%v), want (true,nil)", v, err)
	}
}

func TestGetConfigBool_LenientMatrixNeverErrors(t *testing.T) {
	truthy := []string{"true", "TRUE", "True", "1", " true ", "\ttrue\n", " 1 "}
	falsy := []string{"false", "FALSE", "0", "yes", "no", "garbage", "t", "2", "1.0", "  "}

	for _, raw := range truthy {
		cli := seedDynamicFull(t, map[string]string{"b": raw})
		ctx, abctx := emptyCtx(cli)
		v, err := cli.GetConfigBool(ctx, abctx, "ns1", "b", false)
		if err != nil {
			t.Fatalf("truthy %q: bool must NEVER return a parse error, got %v", raw, err)
		}
		if v != true {
			t.Fatalf("truthy %q: got %v, want true", raw, v)
		}
	}
	for _, raw := range falsy {
		cli := seedDynamicFull(t, map[string]string{"b": raw})
		ctx, abctx := emptyCtx(cli)
		v, err := cli.GetConfigBool(ctx, abctx, "ns1", "b", true)
		if err != nil {
			t.Fatalf("falsy %q: bool must NEVER return a parse error, got %v", raw, err)
		}
		if v != false {
			t.Fatalf("falsy %q: got %v, want false", raw, v)
		}
	}
}

func TestGetConfigBool_AbtestPathResolvesVersion(t *testing.T) {
	h := newHarness(t)
	// full v=1 ("false"), ab v=2 ("true"): the abtest hit must win and parse to true.
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"flag": {full: 1, versions: map[int64]string{1: "false", 2: "true"}},
	}))
	h.abServer.SetResponse("ns1", &abtestv1.GetExperimentResultResponse{
		ConfigFlatKv: map[string]int64{"flag": 2},
	})
	cfg := h.baseConfig([]string{"ns1"})
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()

	abctx := cli.MockAbtestContext("u1", map[string]map[string]int64{"ns1": {"flag": 2}})
	v, err := cli.GetConfigBool(context.Background(), abctx, "ns1", "flag", false)
	if err != nil {
		t.Fatalf("abtest bool: unexpected err %v", err)
	}
	if v != true {
		t.Fatalf("abtest bool: expected ab v2 (true), got %v", v)
	}
}

// ---------------------------------------------------------------------------
// Dynamic — Int64 (hit / miss / parse error / precision)
// ---------------------------------------------------------------------------

func TestGetConfigInt64_HitMissParseError(t *testing.T) {
	cli := seedDynamicFull(t, map[string]string{
		"n":   "42",
		"bad": "12.5",
	})
	ctx, abctx := emptyCtx(cli)

	// Hit.
	if v, err := cli.GetConfigInt64(ctx, abctx, "ns1", "n", -1); err != nil || v != 42 {
		t.Fatalf("hit: got (%d,%v), want (42,nil)", v, err)
	}
	// Miss -> (def, nil), NOT an error.
	if v, err := cli.GetConfigInt64(ctx, abctx, "ns1", "absent", 99); err != nil || v != 99 {
		t.Fatalf("miss: got (%d,%v), want (99,nil)", v, err)
	}
	// Non-parseable cached value -> (def, non-nil wrapped error).
	v, err := cli.GetConfigInt64(ctx, abctx, "ns1", "bad", 7)
	if err == nil {
		t.Fatal("parse fail: expected a non-nil wrapped error")
	}
	if v != 7 {
		t.Fatalf("parse fail: expected def=7, got %d", v)
	}
	// Wrapped error should preserve the underlying strconv error.
	var numErr *strconv.NumError
	if !errors.As(err, &numErr) {
		t.Fatalf("parse fail: expected wrapped *strconv.NumError, got %v", err)
	}
}

func TestGetConfigInt64_PrecisionBeyond2Pow53(t *testing.T) {
	const big = int64(9007199254740993) // 2^53 + 1
	cli := seedDynamicFull(t, map[string]string{
		"big": strconv.FormatInt(big, 10),
		"max": strconv.FormatInt(math.MaxInt64, 10),
	})
	ctx, abctx := emptyCtx(cli)
	if v, err := cli.GetConfigInt64(ctx, abctx, "ns1", "big", 0); err != nil || v != big {
		t.Fatalf("big: got (%d,%v), want (%d,nil) — no float corruption", v, err, big)
	}
	if v, err := cli.GetConfigInt64(ctx, abctx, "ns1", "max", 0); err != nil || v != math.MaxInt64 {
		t.Fatalf("max: got (%d,%v), want (%d,nil)", v, err, int64(math.MaxInt64))
	}
}

// ---------------------------------------------------------------------------
// Dynamic — Float64
// ---------------------------------------------------------------------------

func TestGetConfigFloat64_HitMissParseError(t *testing.T) {
	cli := seedDynamicFull(t, map[string]string{
		"f":   "0.5",
		"bad": "notafloat",
	})
	ctx, abctx := emptyCtx(cli)
	if v, err := cli.GetConfigFloat64(ctx, abctx, "ns1", "f", -1); err != nil || v != 0.5 {
		t.Fatalf("hit: got (%v,%v), want (0.5,nil)", v, err)
	}
	if v, err := cli.GetConfigFloat64(ctx, abctx, "ns1", "absent", 9.9); err != nil || v != 9.9 {
		t.Fatalf("miss: got (%v,%v), want (9.9,nil)", v, err)
	}
	v, err := cli.GetConfigFloat64(ctx, abctx, "ns1", "bad", 3.3)
	if err == nil {
		t.Fatal("parse fail: expected non-nil wrapped error")
	}
	if v != 3.3 {
		t.Fatalf("parse fail: expected def=3.3, got %v", v)
	}
	var numErr *strconv.NumError
	if !errors.As(err, &numErr) {
		t.Fatalf("parse fail: expected wrapped *strconv.NumError, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Dynamic — String (legacy equivalence + miss semantics)
// ---------------------------------------------------------------------------

func TestGetConfigString_EquivalentToLegacyAndMiss(t *testing.T) {
	cli := seedDynamicFull(t, map[string]string{"s": "hello"})
	ctx, abctx := emptyCtx(cli)

	if v, err := cli.GetConfigString(ctx, abctx, "ns1", "s", "def"); err != nil || v != "hello" {
		t.Fatalf("hit: got (%q,%v), want (hello,nil)", v, err)
	}
	// Equivalence with legacy GetConfig on the same hit. Use a fresh
	// abctx because resultFor memoises per ns per request link.
	legacy, lerr := cli.GetConfig(ctx, cli.EmptyAbtestContext(), "ns1", "s", "def")
	typed, terr := cli.GetConfigString(ctx, cli.EmptyAbtestContext(), "ns1", "s", "def")
	if legacy != typed || (lerr == nil) != (terr == nil) {
		t.Fatalf("string accessor diverged from legacy: legacy=(%q,%v) typed=(%q,%v)", legacy, lerr, typed, terr)
	}
	// Miss -> (def, nil).
	if v, err := cli.GetConfigString(ctx, cli.EmptyAbtestContext(), "ns1", "absent", "def"); err != nil || v != "def" {
		t.Fatalf("miss: got (%q,%v), want (def,nil)", v, err)
	}
}

// ---------------------------------------------------------------------------
// Dynamic — JSON (hit / miss / parse error)
// ---------------------------------------------------------------------------

func TestGetConfigJSON_HitMissParseError(t *testing.T) {
	cli := seedDynamicFull(t, map[string]string{
		"obj": `{"name":"z","count":5,"enabled":false}`,
		"bad": `{broken`,
	})
	ctx, abctx := emptyCtx(cli)

	var p jsonPayload
	if err := cli.GetConfigJSON(ctx, abctx, "ns1", "obj", &p); err != nil {
		t.Fatalf("hit: unexpected err %v", err)
	}
	if p.Name != "z" || p.Count != 5 || p.Enabled {
		t.Fatalf("hit: got %+v", p)
	}

	// Miss -> nil error, out left untouched.
	var untouched = jsonPayload{Name: "sentinel"}
	if err := cli.GetConfigJSON(ctx, abctx, "ns1", "absent", &untouched); err != nil {
		t.Fatalf("miss: expected nil err, got %v", err)
	}
	if untouched.Name != "sentinel" {
		t.Fatalf("miss must not touch out, got %+v", untouched)
	}

	// Parse error -> non-nil wrapped error.
	var bad jsonPayload
	if err := cli.GetConfigJSON(ctx, abctx, "ns1", "bad", &bad); err == nil {
		t.Fatal("parse fail: expected a non-nil wrapped error")
	}
}

// ---------------------------------------------------------------------------
// Dynamic — underlying ctx/ns error path
// ---------------------------------------------------------------------------

// TestDynamicTyped_UnderlyingErrorSurfaces asserts every dynamic typed method
// returns (def, err) when the underlying GetConfig errors. We trigger the
// error deterministically with a nil *AbtestContext (ErrAbtestContextMissing) —
// the same path TestGetConfig_NilAbtestContextErr uses. This proves the typed
// wrappers surface the underlying error and return the default, INCLUDING the
// bool method (its only error is the underlying one).
func TestDynamicTyped_UnderlyingErrorSurfaces(t *testing.T) {
	cli := seedDynamicFull(t, map[string]string{"k": "123"})
	ctx := context.Background()
	var nilCtx *AbtestContext

	if v, err := cli.GetConfigBool(ctx, nilCtx, "ns1", "k", true); !errors.Is(err, ErrAbtestContextMissing) || v != true {
		t.Fatalf("bool err path: got (%v,%v), want (true, ErrAbtestContextMissing)", v, err)
	}
	if v, err := cli.GetConfigInt64(ctx, nilCtx, "ns1", "k", 7); !errors.Is(err, ErrAbtestContextMissing) || v != 7 {
		t.Fatalf("int64 err path: got (%d,%v), want (7, ErrAbtestContextMissing)", v, err)
	}
	if v, err := cli.GetConfigFloat64(ctx, nilCtx, "ns1", "k", 3.3); !errors.Is(err, ErrAbtestContextMissing) || v != 3.3 {
		t.Fatalf("float64 err path: got (%v,%v), want (3.3, ErrAbtestContextMissing)", v, err)
	}
	if v, err := cli.GetConfigString(ctx, nilCtx, "ns1", "k", "def"); !errors.Is(err, ErrAbtestContextMissing) || v != "def" {
		t.Fatalf("string err path: got (%q,%v), want (def, ErrAbtestContextMissing)", v, err)
	}
	var out map[string]any
	if err := cli.GetConfigJSON(ctx, nilCtx, "ns1", "k", &out); !errors.Is(err, ErrAbtestContextMissing) {
		t.Fatalf("json err path: got %v, want ErrAbtestContextMissing", err)
	}
}
