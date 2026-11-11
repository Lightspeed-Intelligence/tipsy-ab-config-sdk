// Command bucketfind reverse-solves deterministic user IDs for the dev e2e
// topology and emits the golden expectations fixture.
//
// It replicates the platform's unified bucket formula exactly (verified against
// internal/abtest/bucket/strategy.go):
//
//	bucket = xxhash.Sum64String(uid + "-" + salt) % trafficTotal
//
// and the two-level routing of internal/abtest/compute/topology.go: a user is
// routed by the LAYER bucket (salt = layer salt) into a slot's [lo,hi], THEN by
// the EXPERIMENT bucket (salt = experiment salt) into a group's [lo,hi]. The
// FIRST covering inclusive range wins; no cover → gap (skip).
//
// For every target the tool brute-forces candidate UIDs from a small enumerable
// space ("u0","u1",...) and keeps those that satisfy BOTH levels simultaneously
// (layer bucket in the slot range AND experiment bucket in the group range, or
// for layer-gap targets the layer bucket in the requested layer range). It then
// writes test/dev-e2e/fixtures/expectations.json and prints a readable summary.
//
// Run:  go run ./test/dev-e2e/tools/bucketfind [-out <path>]
//
// The fixture is written next to this source file
// (../../fixtures/expectations.json) regardless of the working directory;
// -out writes it elsewhere instead.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"

	"github.com/cespare/xxhash/v2"
)

// bucketOf replicates internal/abtest/bucket/strategy.go BucketOf for xxhash.
func bucketOf(uid, salt string, trafficTotal int64) int64 {
	if trafficTotal <= 0 || uid == "" {
		return -1
	}
	h := xxhash.Sum64String(uid + "-" + salt)
	return int64(h % uint64(trafficTotal))
}

// rng is an inclusive [lo,hi] traffic range.
type rng struct{ lo, hi int64 }

func (r rng) covers(b int64) bool { return b >= 0 && b >= r.lo && b <= r.hi }

// layerSpec describes the layer-level routing constraint.
type layerSpec struct {
	salt  string
	total int64
	slot  rng // the slot range that routes into the experiment
}

// expSpec describes the experiment-level routing constraint.
type expSpec struct {
	salt  string
	total int64
}

// target is one reverse-solve request: find `want` UIDs satisfying the layer
// slot constraint and (when exp != nil) the experiment group constraint.
type target struct {
	name  string
	layer layerSpec
	exp   *expSpec
	group rng
	want  int
	uids  []string
}

// candidateSpace enumerates the UID search space. We use "u<N>" which is a
// simple, deterministic, infinitely enumerable namespace.
const maxCandidates = 5_000_000

func candidate(i int) string { return "u" + strconv.Itoa(i) }

// findUIDs brute-forces up to maxCandidates UIDs satisfying both the layer slot
// constraint and the experiment group constraint, returning up to want UIDs.
// When exp is nil only the layer constraint is checked (layer-gap targets).
func findUIDs(layer layerSpec, exp *expSpec, group rng, want int) []string {
	out := make([]string, 0, want)
	for i := 0; i < maxCandidates && len(out) < want; i++ {
		uid := candidate(i)
		lb := bucketOf(uid, layer.salt, layer.total)
		if !layer.slot.covers(lb) {
			continue
		}
		if exp != nil {
			eb := bucketOf(uid, exp.salt, exp.total)
			if !group.covers(eb) {
				continue
			}
		}
		out = append(out, uid)
	}
	return out
}

// expGroupOf returns the index of the first covering group for a given UID at
// the experiment level, or -1 for a gap. groups must be checked in declared
// order (first covering wins), matching CoveringIndex.
func expGroupOf(uid string, exp expSpec, groups []rng) int {
	eb := bucketOf(uid, exp.salt, exp.total)
	for i, g := range groups {
		if g.covers(eb) {
			return i
		}
	}
	return -1
}

// Expectation is one row in expectations.json (the golden expected result).
type Expectation struct {
	NS                string         `json:"ns"`
	UserID            string         `json:"user_id"`
	UserAttrs         map[string]any `json:"user_attrs"`
	Key               string         `json:"key"`
	ExpectedVersionID string         `json:"expected_version_id"`
	ExpectedValue     any            `json:"expected_value"`
	Source            string         `json:"source"` // experiment_group|whitelist|full
	Note              string         `json:"note"`
	AppliesTo         []string       `json:"applies_to"`
	// ExpectedConfigFlatKV is set only on retired_custom raw-result probes; an
	// empty map (no config hit) must still be emitted, hence omitzero.
	ExpectedConfigFlatKV map[string]string `json:"expected_config_flat_kv,omitzero"`
}

// allClients is the standard client matrix every expectation applies to unless
// a case is server-path-only (e.g. sticky, handled by the test driver, not
// expressed here).
var allClients = []string{"http", "grpc", "go_sdk_grpc", "go_sdk_http", "py_sdk_grpc", "py_sdk_http", "java_sdk_grpc", "java_sdk_http"}

func noAttrs() map[string]any { return map[string]any{} }

func usAttrs() map[string]any { return map[string]any{"country": map[string]any{"s": "US"}} }

// rawExp is one config experiment as seen by a raw GetExperimentResult call:
// the layer slot and first covering group route the user, and that group's
// key→version map lands in config_flat_kv. Uncovered layer or group buckets
// (gaps) contribute nothing; the full release is not part of raw results.
type rawExp struct {
	layer    layerSpec
	exp      expSpec
	groups   []rng
	versions []map[string]string // per group, same order as groups
	needUS   bool                // experiment admission country==US
}

func countryIsUS(attrs map[string]any) bool {
	country, _ := attrs["country"].(map[string]any)
	return country["s"] == "US"
}

// rawConfigFlatKV derives the expected raw config_flat_kv for one user from
// the same bucket routing the per-key expectations use.
func rawConfigFlatKV(uid string, attrs map[string]any, exps []rawExp) map[string]string {
	out := map[string]string{}
	for _, e := range exps {
		if !e.layer.slot.covers(bucketOf(uid, e.layer.salt, e.layer.total)) {
			continue
		}
		if e.needUS && !countryIsUS(attrs) {
			continue
		}
		g := expGroupOf(uid, e.exp, e.groups)
		if g < 0 {
			continue
		}
		for key, version := range e.versions[g] {
			out[key] = version
		}
	}
	return out
}

func main() {
	out := flag.String("out", "", "write the fixture here instead of ../../fixtures/expectations.json next to this source file")
	flag.Parse()

	// ---- Topology constants (EXACTLY from the design doc) -----------------

	// NS demo-test --------------------------------------------------------
	const nsDemo = "demo-test"

	lCfg := layerSpec{salt: "L_cfg_demo", total: 10000, slot: rng{0, 9999}}
	eCfg := expSpec{salt: "E_cfg_demo", total: 10000}
	cfgA := rng{0, 4999}    // welcome-A + red
	cfgB := rng{5000, 9999} // welcome-B + blue

	// L_gap slot covers ONLY [0,4999]; experiment group G covers [0,9999].
	lGap := layerSpec{salt: "L_gap_demo", total: 10000, slot: rng{0, 4999}}
	eGap := expSpec{salt: "E_gap_demo", total: 10000}
	gapG := rng{0, 9999}
	// layer-gap target: UIDs whose LAYER bucket lands in [5000,9999] (no slot
	// cover → layer gap → no hit → full value).
	lGapLayerFull := layerSpec{salt: "L_gap_demo", total: 10000, slot: rng{5000, 9999}}

	lAdmit := layerSpec{salt: "L_admit_demo", total: 10000, slot: rng{0, 9999}}
	eAdmit := expSpec{salt: "E_admit_demo", total: 10000}
	admitG := rng{0, 9999}

	// NS for_dev_agent_test ----------------------------------------------
	const nsFda = "for_dev_agent_test"

	lCfgFda := layerSpec{salt: "L_cfg_fda", total: 10000, slot: rng{0, 9999}}
	eCfg2 := expSpec{salt: "E_cfg_fda", total: 10000}
	g1 := rng{0, 3332}      // c1
	g2 := rng{3333, 6665}   // c2
	g3 := rng{6666, 9998}   // c3
	gGap := rng{9999, 9999} // bucket 9999 → uncovered → c-FULL

	// Experiment-group config versions (key → version id): the single source
	// for both the per-key GetConfig rows and the raw-result probes.
	cfgVer := []map[string]string{
		{"welcome_text": "900200011", "banner_color": "900200021"}, // E_cfg A
		{"welcome_text": "900200012", "banner_color": "900200022"}, // E_cfg B
	}
	gapVer := map[string]string{"gap_key": "900200031"}
	admitVer := map[string]string{"admit_key": "900200041"}
	colorVer := []map[string]string{{"color": "900400011"}, {"color": "900400012"}, {"color": "900400013"}} // G1..G3

	// ---- Reverse-solve UIDs for each target ------------------------------

	targets := []*target{
		// demo-test E_cfg
		{name: "demo/E_cfg/A", layer: lCfg, exp: &eCfg, group: cfgA, want: 2},
		{name: "demo/E_cfg/B", layer: lCfg, exp: &eCfg, group: cfgB, want: 2},
		// demo-test E_gap: (a) hits experiment, (b) layer gap → full
		{name: "demo/E_gap/EXP", layer: lGap, exp: &eGap, group: gapG, want: 2},
		{name: "demo/E_gap/LAYER_FULL", layer: lGapLayerFull, exp: nil, group: rng{}, want: 2},
		// demo-test E_admit (bucket only; admission handled via attrs)
		{name: "demo/E_admit/HIT", layer: lAdmit, exp: &eAdmit, group: admitG, want: 2},
		// for_dev_agent_test E_cfg2 G1/G2/G3 + experiment-internal gap (bucket 9999)
		{name: "fda/E_cfg2/G1", layer: lCfgFda, exp: &eCfg2, group: g1, want: 2},
		{name: "fda/E_cfg2/G2", layer: lCfgFda, exp: &eCfg2, group: g2, want: 2},
		{name: "fda/E_cfg2/G3", layer: lCfgFda, exp: &eCfg2, group: g3, want: 2},
		{name: "fda/E_cfg2/GAP9999", layer: lCfgFda, exp: &eCfg2, group: gGap, want: 2},
	}

	for _, t := range targets {
		t.uids = findUIDs(t.layer, t.exp, t.group, t.want)
	}

	// ---- Special fixed UIDs (NOT bucket-derived) -------------------------
	// gray-user-1 also needs its E_cfg group resolved so we can assert that
	// banner_color still follows the experiment while welcome_text is gray.
	grayUser1Group := expGroupOf("gray-user-1", eCfg, []rng{cfgA, cfgB}) // 0=A,1=B

	// ---- Build expectations ----------------------------------------------
	exps := make([]Expectation, 0, 64)

	add := func(e Expectation) {
		if e.UserAttrs == nil {
			e.UserAttrs = noAttrs()
		}
		if e.AppliesTo == nil {
			e.AppliesTo = allClients
		}
		exps = append(exps, e)
	}

	uidByTarget := func(name string) []string {
		for _, t := range targets {
			if t.name == name {
				return t.uids
			}
		}
		return nil
	}

	// demo-test E_cfg group A → welcome_text=welcome-A(900200011), banner_color=red(900200021)
	for _, uid := range uidByTarget("demo/E_cfg/A") {
		add(Expectation{NS: nsDemo, UserID: uid, Key: "welcome_text",
			ExpectedVersionID: cfgVer[0]["welcome_text"], ExpectedValue: "welcome-A",
			Source: "experiment_group", Note: "E_cfg group A (bucket-derived)"})
		add(Expectation{NS: nsDemo, UserID: uid, Key: "banner_color",
			ExpectedVersionID: cfgVer[0]["banner_color"], ExpectedValue: "red",
			Source: "experiment_group", Note: "E_cfg group A (bucket-derived)"})
	}
	// demo-test E_cfg group B → welcome-B(900200012), blue(900200022)
	for _, uid := range uidByTarget("demo/E_cfg/B") {
		add(Expectation{NS: nsDemo, UserID: uid, Key: "welcome_text",
			ExpectedVersionID: cfgVer[1]["welcome_text"], ExpectedValue: "welcome-B",
			Source: "experiment_group", Note: "E_cfg group B (bucket-derived)"})
		add(Expectation{NS: nsDemo, UserID: uid, Key: "banner_color",
			ExpectedVersionID: cfgVer[1]["banner_color"], ExpectedValue: "blue",
			Source: "experiment_group", Note: "E_cfg group B (bucket-derived)"})
	}

	// demo-test E_gap experiment hit → gap_key=gap-EXP(900200031)
	for _, uid := range uidByTarget("demo/E_gap/EXP") {
		add(Expectation{NS: nsDemo, UserID: uid, Key: "gap_key",
			ExpectedVersionID: gapVer["gap_key"], ExpectedValue: "gap-EXP",
			Source: "experiment_group", Note: "E_gap hit: layer bucket in [0,4999] AND experiment group G"})
	}
	// demo-test layer gap (layer bucket in [5000,9999], no slot) → gap_key=gap-FULL(900200039)
	for _, uid := range uidByTarget("demo/E_gap/LAYER_FULL") {
		add(Expectation{NS: nsDemo, UserID: uid, Key: "gap_key",
			ExpectedVersionID: "900200039", ExpectedValue: "gap-FULL",
			Source: "full", Note: "layer gap: L_gap bucket in [5000,9999] uncovered → fall back to full release"})
	}

	// demo-test E_admit: with country=US → admit-EXP(900200041); without → admit-FULL(900200049)
	for _, uid := range uidByTarget("demo/E_admit/HIT") {
		add(Expectation{NS: nsDemo, UserID: uid, UserAttrs: usAttrs(), Key: "admit_key",
			ExpectedVersionID: admitVer["admit_key"], ExpectedValue: "admit-EXP",
			Source: "experiment_group", Note: "E_admit admission country==US passes"})
		add(Expectation{NS: nsDemo, UserID: uid, UserAttrs: noAttrs(), Key: "admit_key",
			ExpectedVersionID: "900200049", ExpectedValue: "admit-FULL",
			Source: "full", Note: "E_admit admission fails (no country) → fall back to full release"})
	}

	// demo-test group whitelist: wl-force-B forced into E_cfg group B regardless of bucket.
	add(Expectation{NS: nsDemo, UserID: "wl-force-B", Key: "welcome_text",
		ExpectedVersionID: cfgVer[1]["welcome_text"], ExpectedValue: "welcome-B",
		Source: "experiment_group", Note: "group whitelist: forced into E_cfg group B (overrides bucket)"})
	add(Expectation{NS: nsDemo, UserID: "wl-force-B", Key: "banner_color",
		ExpectedVersionID: cfgVer[1]["banner_color"], ExpectedValue: "blue",
		Source: "experiment_group", Note: "group whitelist: forced into E_cfg group B (overrides bucket)"})

	// demo-test gray release: gray-user-1 / gray-user-2 welcome_text=welcome-GRAY(900200013).
	for _, uid := range []string{"gray-user-1", "gray-user-2"} {
		add(Expectation{NS: nsDemo, UserID: uid, Key: "welcome_text",
			ExpectedVersionID: "900200013", ExpectedValue: "welcome-GRAY",
			Source: "whitelist", Note: "gray release on welcome_text takes priority over experiment"})
	}
	// gray-user-1 banner_color still follows its experiment group (gray only owns welcome_text).
	{
		var verID, val, grp string
		if grayUser1Group == 1 { // group B
			verID, val, grp = cfgVer[1]["banner_color"], "blue", "B"
		} else { // group A (also covers gap → but bucket always covers A or B here since A∪B=[0,9999])
			verID, val, grp = cfgVer[0]["banner_color"], "red", "A"
		}
		add(Expectation{NS: nsDemo, UserID: "gray-user-1", Key: "banner_color",
			ExpectedVersionID: verID, ExpectedValue: val,
			Source: "experiment_group",
			Note:   fmt.Sprintf("gray owns only welcome_text; banner_color still follows E_cfg group %s (bucket-derived)", grp)})
	}

	// for_dev_agent_test E_cfg2 G1/G2/G3 → color c1/c2/c3
	for _, uid := range uidByTarget("fda/E_cfg2/G1") {
		add(Expectation{NS: nsFda, UserID: uid, Key: "color",
			ExpectedVersionID: colorVer[0]["color"], ExpectedValue: "c1",
			Source: "experiment_group", Note: "E_cfg2 group G1 (bucket-derived)"})
	}
	for _, uid := range uidByTarget("fda/E_cfg2/G2") {
		add(Expectation{NS: nsFda, UserID: uid, Key: "color",
			ExpectedVersionID: colorVer[1]["color"], ExpectedValue: "c2",
			Source: "experiment_group", Note: "E_cfg2 group G2 (bucket-derived)"})
	}
	for _, uid := range uidByTarget("fda/E_cfg2/G3") {
		add(Expectation{NS: nsFda, UserID: uid, Key: "color",
			ExpectedVersionID: colorVer[2]["color"], ExpectedValue: "c3",
			Source: "experiment_group", Note: "E_cfg2 group G3 (bucket-derived)"})
	}
	// for_dev_agent_test experiment-internal gap (bucket 9999) → color c-FULL(900400019)
	for _, uid := range uidByTarget("fda/E_cfg2/GAP9999") {
		add(Expectation{NS: nsFda, UserID: uid, Key: "color",
			ExpectedVersionID: "900400019", ExpectedValue: "c-FULL",
			Source: "full", Note: "experiment-internal gap: E_cfg2 bucket==9999 uncovered → fall back to full release"})
	}

	// for_dev_agent_test gray release: gray-fda-1 greeting=hi-GRAY(900400021)
	add(Expectation{NS: nsFda, UserID: "gray-fda-1", Key: "greeting",
		ExpectedVersionID: "900400021", ExpectedValue: "hi-GRAY",
		Source: "whitelist", Note: "gray release on greeting takes priority over (no) experiment"})

	// ---- Raw-result probes (retired custom compatibility) -----------------
	// Each probe pins raw types 0/1/3 to the config-only result for one exact
	// namespace/user/attrs; the expected map is derived, not hand-written.
	rawExps := map[string][]rawExp{
		nsDemo: {
			{layer: lCfg, exp: eCfg, groups: []rng{cfgA, cfgB}, versions: cfgVer},
			{layer: lGap, exp: eGap, groups: []rng{gapG}, versions: []map[string]string{gapVer}},
			{layer: lAdmit, exp: eAdmit, groups: []rng{admitG}, versions: []map[string]string{admitVer}, needUS: true},
		},
		nsFda: {
			{layer: lCfgFda, exp: eCfg2, groups: []rng{g1, g2, g3}, versions: colorVer},
		},
	}
	const rawNote = "Raw type contract for this exact namespace/user/attrs: 0/1/3 match expected_config_flat_kv; 2 rejects; full fallbacks excluded."
	probes := []struct {
		ns, uid string
		attrs   map[string]any
		note    string
	}{
		{nsDemo, "u0", noAttrs(), rawNote},
		{nsDemo, "u0", usAttrs(), rawNote},
		{nsDemo, "u1", noAttrs(), rawNote},
		{nsDemo, "u2", noAttrs(), rawNote},
		{nsFda, "u0", noAttrs(), rawNote},
		{nsFda, "u1", noAttrs(), rawNote},
		{nsFda, "u2", noAttrs(), rawNote},
		{nsFda, "u3", noAttrs(), rawNote},
		{nsFda, uidByTarget("fda/E_cfg2/GAP9999")[0], noAttrs(),
			"Experiment-internal gap: E_cfg2 bucket 9999 is uncovered, so 0/1/3 return empty raw config; GetConfig resolves the full release separately."},
	}
	for _, p := range probes {
		add(Expectation{NS: p.ns, UserID: p.uid, UserAttrs: p.attrs, Key: "__custom__",
			ExpectedVersionID: "",
			ExpectedValue:     map[string]any{},
			Source:            "retired_custom", Note: p.note,
			ExpectedConfigFlatKV: rawConfigFlatKV(p.uid, p.attrs, rawExps[p.ns])})
	}

	// ---- Deterministic ordering ------------------------------------------
	sort.SliceStable(exps, func(i, j int) bool {
		a, b := exps[i], exps[j]
		if a.NS != b.NS {
			return a.NS < b.NS
		}
		if a.UserID != b.UserID {
			return a.UserID < b.UserID
		}
		if a.Key != b.Key {
			return a.Key < b.Key
		}
		// stabilize US vs no-attrs duplicates by source
		return a.Source < b.Source
	})

	// ---- Write fixture ----------------------------------------------------
	outPath, err := resolveFixturePath(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve fixture path:", err)
		os.Exit(1)
	}
	buf, err := json.MarshalIndent(exps, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal:", err)
		os.Exit(1)
	}
	buf = append(buf, '\n')
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(outPath, buf, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}

	// ---- Print summary table ---------------------------------------------
	printSummary(targets, grayUser1Group, eCfg)
	fmt.Printf("\nWrote %d expectations to %s\n", len(exps), outPath)
}

// resolveFixturePath returns out when set, else the fixture path derived from
// this source file's location. The working directory and the nearest go.mod
// are deliberately not consulted: the SDK copy is its own module under a
// go.work root, so either would resolve to the wrong tree.
func resolveFixturePath(out string) (string, error) {
	if out != "" {
		return filepath.Abs(out)
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(file) {
		return "", errors.New("source path unavailable (built with -trimpath?); pass -out")
	}
	return filepath.Join(filepath.Dir(file), "../../fixtures/expectations.json"), nil
}

func printSummary(targets []*target, grayUser1Group int, eCfg expSpec) {
	fmt.Println("bucketfind — reverse-solved UIDs (xxhash64(uid+\"-\"+salt) % total)")
	fmt.Println("================================================================")
	fmt.Printf("%-26s %-14s %s\n", "TARGET", "UID", "layer_bucket / exp_bucket")
	fmt.Println("----------------------------------------------------------------")
	for _, t := range targets {
		if len(t.uids) == 0 {
			fmt.Printf("%-26s %-14s %s\n", t.name, "<NONE FOUND>", "")
			continue
		}
		for _, uid := range t.uids {
			lb := bucketOf(uid, t.layer.salt, t.layer.total)
			eb := int64(-1)
			if t.exp != nil {
				eb = bucketOf(uid, t.exp.salt, t.exp.total)
			}
			fmt.Printf("%-26s %-14s lb=%-5d eb=%d\n", t.name, uid, lb, eb)
		}
	}
	fmt.Println("----------------------------------------------------------------")
	grp := "A"
	if grayUser1Group == 1 {
		grp = "B"
	}
	fmt.Printf("special: gray-user-1 E_cfg bucket=%d → group %s (banner_color follows experiment)\n",
		bucketOf("gray-user-1", eCfg.salt, eCfg.total), grp)
	fmt.Printf("special: wl-force-B forced into E_cfg group B by group whitelist (bucket ignored)\n")
}
