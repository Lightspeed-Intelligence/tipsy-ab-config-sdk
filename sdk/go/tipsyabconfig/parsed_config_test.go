package tipsyabconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// ParsedConfig tests. Snapshots are seeded through the same fake-config
// harness as typed_config_test.go; snapshot replaces are driven straight
// through cli.cache.applyProto with a higher business seq.

type parsedTestCfg struct {
	N    int      `json:"n"`
	Tags []string `json:"tags"`
}

// countingParser returns a ParsedConfig over ParseJSON[*parsedTestCfg] and a
// counter of how many times it actually parsed.
func countingParser() (*ParsedConfig[*parsedTestCfg], *atomic.Int64) {
	var n atomic.Int64
	p := NewParsedConfig(func(raw string) (*parsedTestCfg, error) {
		n.Add(1)
		return ParseJSON[*parsedTestCfg](raw)
	})
	return p, &n
}

// seedParsed builds a Client whose ns1 snapshot (biz=1, exp=1) holds keys.
// withAbtest wires the abtest channel (needed for the dynamic path).
func seedParsed(t *testing.T, keys map[string]typedKey, withAbtest bool) *Client {
	t.Helper()
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, keys))
	cfg := h.baseConfigNoAbtest([]string{"ns1"})
	if withAbtest {
		cfg = h.baseConfig([]string{"ns1"})
	}
	cli, err := Init(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func TestParsedConfig_StaticParsesOncePerVersionUnderConcurrency(t *testing.T) {
	cli := seedParsed(t, map[string]typedKey{
		"big": {full: 7, versions: map[int64]string{7: `{"n":7,"tags":["a"]}`}},
	}, false)
	p, parses := countingParser()

	const readers = 64
	got := make([]*parsedTestCfg, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, ok, err := p.GetStatic(cli, "ns1", "big")
			if !ok || err != nil {
				t.Errorf("GetStatic: ok=%v err=%v", ok, err)
				return
			}
			got[i] = v
		}(i)
	}
	wg.Wait()

	if n := parses.Load(); n != 1 {
		t.Fatalf("parse ran %d times for one version, want 1", n)
	}
	for i, v := range got {
		if v != got[0] {
			t.Fatalf("reader %d got a different object; every reader of one version must share the cached result", i)
		}
	}
	if got[0].N != 7 || len(got[0].Tags) != 1 {
		t.Fatalf("parsed value = %+v, want n=7 tags=[a]", got[0])
	}
}

func TestParsedConfig_ReparsesOnlyAfterSnapshotReplace(t *testing.T) {
	keys := map[string]typedKey{"big": {full: 7, versions: map[int64]string{7: `{"n":7}`}}}
	cli := seedParsed(t, keys, false)
	p, parses := countingParser()

	first, _, _ := p.GetStatic(cli, "ns1", "big")
	_, _, _ = p.GetStatic(cli, "ns1", "big")
	if n := parses.Load(); n != 1 {
		t.Fatalf("before replace: parse ran %d times, want 1", n)
	}

	// Same seqs: applyProto keeps the current snapshot, so the cache survives.
	if replaced, _, _ := cli.cache.applyProto(makeSnapshot("ns1", 1, 1, keys)); replaced {
		t.Fatal("same seqs must not replace the snapshot")
	}
	_, _, _ = p.GetStatic(cli, "ns1", "big")
	if n := parses.Load(); n != 1 {
		t.Fatalf("after no-op apply: parse ran %d times, want 1", n)
	}

	// Newer snapshot with the same version: parsed once more, then cached again.
	cli.cache.applyProto(makeSnapshot("ns1", 2, 1, keys))
	second, _, _ := p.GetStatic(cli, "ns1", "big")
	_, _, _ = p.GetStatic(cli, "ns1", "big")
	if n := parses.Load(); n != 2 {
		t.Fatalf("after replace: parse ran %d times, want 2", n)
	}
	if second == first {
		t.Fatal("a replaced snapshot must not hand out results cached on the old one")
	}

	// New full-release version: the new value is served.
	cli.cache.applyProto(makeSnapshot("ns1", 3, 1, map[string]typedKey{
		"big": {full: 8, versions: map[int64]string{8: `{"n":8}`}},
	}))
	if v, ok, err := p.GetStatic(cli, "ns1", "big"); !ok || err != nil || v.N != 8 {
		t.Fatalf("after publish: got (%+v,%v,%v), want n=8", v, ok, err)
	}
}

func TestParsedConfig_ParseErrorIsCachedAndOmitsRawValue(t *testing.T) {
	raw := `not json ` + strings.Repeat("x", 1000)
	cli := seedParsed(t, map[string]typedKey{"bad": {full: 9, versions: map[int64]string{9: raw}}}, false)
	p, parses := countingParser()

	for i := 0; i < 3; i++ {
		v, ok, err := p.GetStatic(cli, "ns1", "bad")
		if !ok || err == nil || v != nil {
			t.Fatalf("read %d: got (%v,%v,%v), want (nil,true,parse error)", i, v, ok, err)
		}
		var syn *json.SyntaxError
		if !errors.As(err, &syn) {
			t.Fatalf("err must wrap the parse error, got %v", err)
		}
		if !strings.Contains(err.Error(), "ns1/bad version 9") {
			t.Fatalf("err should name ns/key/version, got %q", err)
		}
		if strings.Contains(err.Error(), "xxxx") {
			t.Fatal("err must not embed the raw value (it can be 100 KB+)")
		}
	}
	if n := parses.Load(); n != 1 {
		t.Fatalf("a bad value was parsed %d times, want 1", n)
	}
}

func TestParsedConfig_MissDoesNotParse(t *testing.T) {
	cli := seedParsed(t, map[string]typedKey{
		"nofull": {full: 0, versions: map[int64]string{3: `{"n":3}`}},
	}, true)
	p, parses := countingParser()
	ctx, abctx := emptyCtx(cli)

	for _, key := range []string{"absent", "nofull"} {
		if v, ok, err := p.GetStatic(cli, "ns1", key); ok || err != nil || v != nil {
			t.Fatalf("static %q: got (%v,%v,%v), want miss", key, v, ok, err)
		}
		if v, ok, err := p.Get(ctx, cli, abctx, "ns1", key); ok || err != nil || v != nil {
			t.Fatalf("dynamic %q: got (%v,%v,%v), want miss", key, v, ok, err)
		}
	}
	if v, ok, err := p.GetStatic(cli, "other-ns", "absent"); ok || err != nil || v != nil {
		t.Fatalf("unknown ns: got (%v,%v,%v), want miss", v, ok, err)
	}
	if n := parses.Load(); n != 0 {
		t.Fatalf("misses ran parse %d times, want 0", n)
	}
}

func TestParsedConfig_DynamicUsesResolvedVersion(t *testing.T) {
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, map[string]typedKey{
		"big": {full: 1, versions: map[int64]string{1: `{"n":1}`, 2: `{"n":2}`}},
	}))
	h.abServer.SetResponse("ns1", perGroupResponse(map[string]int64{"big": 2}))
	cli, err := Init(context.Background(), h.baseConfig([]string{"ns1"}))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer cli.Close()
	p, parses := countingParser()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		ab := cli.MockAbtestContext("u1", map[string]map[string]int64{"ns1": {"big": 2}})
		if v, ok, err := p.Get(ctx, cli, ab, "ns1", "big"); !ok || err != nil || v.N != 2 {
			t.Fatalf("abtest hit: got (%+v,%v,%v), want n=2", v, ok, err)
		}
		if v, ok, err := p.Get(ctx, cli, cli.EmptyAbtestContext(), "ns1", "big"); !ok || err != nil || v.N != 1 {
			t.Fatalf("full release: got (%+v,%v,%v), want n=1", v, ok, err)
		}
	}
	if n := parses.Load(); n != 2 {
		t.Fatalf("two versions read 3 times each were parsed %d times, want 2", n)
	}
}

func TestParsedConfig_DynamicErrorsMatchGetConfig(t *testing.T) {
	cli := seedParsed(t, map[string]typedKey{"big": {full: 1, versions: map[int64]string{1: `{"n":1}`}}}, true)
	p, parses := countingParser()
	ctx := context.Background()

	if _, ok, err := p.Get(ctx, cli, nil, "ns1", "big"); ok || !errors.Is(err, ErrAbtestContextMissing) {
		t.Fatalf("nil abctx: got ok=%v err=%v, want ErrAbtestContextMissing", ok, err)
	}
	if _, ok, err := p.Get(ctx, cli, cli.EmptyAbtestContext(), "not-subscribed", "big"); ok || err == nil {
		t.Fatalf("unsubscribed ns: got ok=%v err=%v, want the GetConfig error", ok, err)
	}
	var nilClient *Client
	if _, ok, err := p.Get(ctx, nilClient, cli.EmptyAbtestContext(), "ns1", "big"); ok || !errors.Is(err, ErrClosed) {
		t.Fatalf("nil client: got ok=%v err=%v, want ErrClosed", ok, err)
	}
	if _, ok, err := p.GetStatic(nilClient, "ns1", "big"); ok || err != nil {
		t.Fatalf("nil client static: got ok=%v err=%v, want miss", ok, err)
	}
	if n := parses.Load(); n != 0 {
		t.Fatalf("error paths ran parse %d times, want 0", n)
	}
}

func TestParsedConfig_SeparateParsersDoNotShareResults(t *testing.T) {
	cli := seedParsed(t, map[string]typedKey{"big": {full: 1, versions: map[int64]string{1: `{"n":1}`}}}, false)
	p1, c1 := countingParser()
	p2 := NewParsedConfig(func(raw string) (*parsedTestCfg, error) {
		v, err := ParseJSON[*parsedTestCfg](raw)
		if err == nil {
			v.N *= 100
		}
		return v, err
	})

	for i := 0; i < 2; i++ {
		if v, _, _ := p1.GetStatic(cli, "ns1", "big"); v.N != 1 {
			t.Fatalf("p1 got n=%d, want 1", v.N)
		}
		if v, _, _ := p2.GetStatic(cli, "ns1", "big"); v.N != 100 {
			t.Fatalf("p2 got n=%d, want 100 (results of p1 leaked into p2)", v.N)
		}
	}
	if n := c1.Load(); n != 1 {
		t.Fatalf("p1 parsed %d times, want 1", n)
	}
}

func TestParsedConfig_PanicBecomesCachedError(t *testing.T) {
	cli := seedParsed(t, map[string]typedKey{"big": {full: 1, versions: map[int64]string{1: `{}`}}}, false)
	var calls atomic.Int64
	p := NewParsedConfig(func(string) (map[string]int, error) {
		calls.Add(1)
		panic("boom")
	})
	for i := 0; i < 2; i++ {
		v, ok, err := p.GetStatic(cli, "ns1", "big")
		if !ok || err == nil || !strings.Contains(err.Error(), "panicked: boom") || v != nil {
			t.Fatalf("read %d: got (%v,%v,%v), want a cached panic error", i, v, ok, err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("panicking parse ran %d times, want 1", n)
	}
}

func TestParsedConfig_SnapshotWithoutCacheParsesEveryTime(t *testing.T) {
	p, parses := countingParser()
	snap := &NamespaceSnapshot{Namespace: "ns1"} // built outside applyProto: parsed == nil
	for i := 0; i < 2; i++ {
		if v, err := p.load(snap, "big", 1, `{"n":5}`); err != nil || v.N != 5 {
			t.Fatalf("got (%+v,%v), want n=5", v, err)
		}
	}
	if n := parses.Load(); n != 2 {
		t.Fatalf("parse ran %d times, want 2 (no cache without applyProto)", n)
	}
}

func TestNewParsedConfig_NilParsePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewParsedConfig(nil) must panic")
		}
	}()
	NewParsedConfig[int](nil)
}

// benchJSON builds a JSON object of at least n bytes.
func benchJSON(n int) string {
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := 0; b.Len() < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"name":"persona_%d","prompt":%q}`, i, i, strings.Repeat("你是一个温柔的角色，回答要简短自然。", 8))
	}
	b.WriteString(`]}`)
	return b.String()
}

type benchCfg struct {
	Items []struct {
		ID     int    `json:"id"`
		Name   string `json:"name"`
		Prompt string `json:"prompt"`
	} `json:"items"`
}

func benchStaticClient(b *testing.B, raw string) *Client {
	b.Helper()
	c := &Client{cache: newConfigCache(newMetrics()), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	fv := int64(1)
	c.cache.applyProto(makeSnapshot("ns1", 1, 1, map[string]typedKey{"big": {full: fv, versions: map[int64]string{fv: raw}}}))
	return c
}

// BenchmarkStaticJSON100KB compares re-parsing on every read
// (GetConfigStaticJSON) with ParsedConfig.GetStatic on a ~100 KB value.
//
//	go test -run '^$' -bench StaticJSON100KB -benchmem ./sdk/go/tipsyabconfig/
func BenchmarkStaticJSON100KB(b *testing.B) {
	raw := benchJSON(100 << 10)
	c := benchStaticClient(b, raw)
	b.Run("GetConfigStaticJSON", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var out benchCfg
			if !c.GetConfigStaticJSON("ns1", "big", &out) {
				b.Fatal("miss")
			}
		}
	})
	p := NewParsedConfig(ParseJSON[*benchCfg])
	b.Run("ParsedConfig.GetStatic", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, ok, err := p.GetStatic(c, "ns1", "big"); !ok || err != nil {
				b.Fatal(ok, err)
			}
		}
	})
}
