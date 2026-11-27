package tipsyabconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// GetConfigJSON / GetConfigStaticJSON decode-cache tests. Decodes are counted
// through types with a custom UnmarshalJSON. These tests must not run in
// parallel with each other (shared counters).

var countedDecodes atomic.Int64

type countedCfg struct {
	N    int               `json:"n"`
	Tags []string          `json:"tags"`
	Meta map[string]string `json:"meta"`
}

func (c *countedCfg) UnmarshalJSON(b []byte) error {
	countedDecodes.Add(1)
	type plain countedCfg
	return json.Unmarshal(b, (*plain)(c))
}

// hiddenCfg has an unexported field, so its decodes cannot be cached.
type hiddenCfg struct {
	N      int `json:"n"`
	hidden int
}

var hiddenDecodes atomic.Int64

func (h *hiddenCfg) UnmarshalJSON(b []byte) error {
	hiddenDecodes.Add(1)
	var w struct {
		N int `json:"n"`
	}
	err := json.Unmarshal(b, &w)
	h.N = w.N
	return err
}

var errRejected = errors.New("rejected by UnmarshalJSON")

type rejectingCfg struct{ N int }

var rejectingDecodes atomic.Int64

func (r *rejectingCfg) UnmarshalJSON([]byte) error {
	rejectingDecodes.Add(1)
	return errRejected
}

type panickingCfg struct{ N int }

var panickingDecodes atomic.Int64

func (p *panickingCfg) UnmarshalJSON([]byte) error {
	panickingDecodes.Add(1)
	panic("boom")
}

const countedRaw = `{"n":7,"tags":["a","b"],"meta":{"k":"v"}}`

func TestGetConfigStaticJSON_DecodesOncePerVersionUnderConcurrency(t *testing.T) {
	countedDecodes.Store(0)
	cli := seedStatic(t, map[string]string{"big": countedRaw})

	const readers = 64
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var c countedCfg
			if !cli.GetConfigStaticJSON("ns1", "big", &c) {
				t.Errorf("reader %d: miss", i)
				return
			}
			if c.N != 7 || len(c.Tags) != 2 || c.Meta["k"] != "v" {
				t.Errorf("reader %d: got %+v", i, c)
			}
			// Each reader owns its copy: writing to it must not race with or
			// leak into any other reader (run with -race).
			c.Tags[0] = fmt.Sprint(i)
			c.Meta["k"] = fmt.Sprint(i)
		}(i)
	}
	wg.Wait()

	if n := countedDecodes.Load(); n != 1 {
		t.Fatalf("decoded %d times for one version, want 1", n)
	}
	var again countedCfg
	cli.GetConfigStaticJSON("ns1", "big", &again)
	if again.Tags[0] != "a" || again.Meta["k"] != "v" {
		t.Fatalf("a reader's writes leaked into the cache: %+v", again)
	}
}

func TestGetConfigStaticJSON_InterfaceOutIsCopied(t *testing.T) {
	cli := seedStatic(t, map[string]string{"obj": `{"a":{"b":[1,2]}}`})

	var first any
	if !cli.GetConfigStaticJSON("ns1", "obj", &first) {
		t.Fatal("miss")
	}
	inner := first.(map[string]any)["a"].(map[string]any)
	inner["b"].([]any)[0] = "changed"
	inner["new"] = true

	var second any
	cli.GetConfigStaticJSON("ns1", "obj", &second)
	want := map[string]any{"a": map[string]any{"b": []any{float64(1), float64(2)}}}
	if !reflect.DeepEqual(second, want) {
		t.Fatalf("second read = %#v, want %#v (first reader's writes leaked)", second, want)
	}
}

func TestGetConfigStaticJSON_NonZeroOutKeepsMergeSemantics(t *testing.T) {
	countedDecodes.Store(0)
	cli := seedStatic(t, map[string]string{"partial": `{"tags":["x"]}`})

	for i := 0; i < 3; i++ {
		c := countedCfg{N: 99} // pre-filled default
		if !cli.GetConfigStaticJSON("ns1", "partial", &c) {
			t.Fatal("miss")
		}
		if c.N != 99 || len(c.Tags) != 1 || c.Tags[0] != "x" {
			t.Fatalf("read %d: got %+v, want the default N=99 kept and tags=[x]", i, c)
		}
	}
	if n := countedDecodes.Load(); n != 3 {
		t.Fatalf("non-zero out decoded %d times, want 3 (not cached)", n)
	}
}

func TestGetConfigStaticJSON_ReplaceDecodesAgain(t *testing.T) {
	countedDecodes.Store(0)
	keys := map[string]typedKey{"big": {full: 7, versions: map[int64]string{7: countedRaw}}}
	cli := seedParsedClient(t, keys)

	read := func() countedCfg {
		var c countedCfg
		if !cli.GetConfigStaticJSON("ns1", "big", &c) {
			t.Fatal("miss")
		}
		return c
	}
	read()
	read()
	if replaced, _, _ := cli.cache.applyProto(makeSnapshot("ns1", 1, 1, keys)); replaced {
		t.Fatal("same seqs must not replace the snapshot")
	}
	read()
	if n := countedDecodes.Load(); n != 1 {
		t.Fatalf("before replace: decoded %d times, want 1", n)
	}

	cli.cache.applyProto(makeSnapshot("ns1", 2, 1, keys))
	read()
	read()
	if n := countedDecodes.Load(); n != 2 {
		t.Fatalf("after replace: decoded %d times, want 2", n)
	}

	cli.cache.applyProto(makeSnapshot("ns1", 3, 1, map[string]typedKey{
		"big": {full: 8, versions: map[int64]string{8: `{"n":8}`}},
	}))
	if c := read(); c.N != 8 {
		t.Fatalf("after publish: got %+v, want n=8", c)
	}
}

func TestGetConfigStaticJSON_ErrorsMatchUnmarshal(t *testing.T) {
	rejectingDecodes.Store(0)
	raws := map[string]string{
		"syntax":   `{broken`,
		"mismatch": `{"n":"not-a-number","tags":["kept"]}`,
		"rejected": `{"n":1}`,
	}
	cli := seedStatic(t, raws)

	for i := 0; i < 2; i++ {
		var s countedCfg
		if cli.GetConfigStaticJSON("ns1", "syntax", &s) {
			t.Fatal("syntax error must return false")
		}

		// A type mismatch still fills the other fields; the caller used to
		// see that partial state, and still does on cached reads.
		var got, want struct {
			N    int      `json:"n"`
			Tags []string `json:"tags"`
		}
		if cli.GetConfigStaticJSON("ns1", "mismatch", &got) {
			t.Fatal("type mismatch must return false")
		}
		_ = json.Unmarshal([]byte(raws["mismatch"]), &want)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("read %d: partial state %+v, want %+v (same as json.Unmarshal)", i, got, want)
		}

		var r rejectingCfg
		if cli.GetConfigStaticJSON("ns1", "rejected", &r) {
			t.Fatal("UnmarshalJSON error must return false")
		}
	}
	if n := rejectingDecodes.Load(); n != 1 {
		t.Fatalf("a failing decode ran %d times, want 1 (errors are cached)", n)
	}
}

func TestGetConfigStaticJSON_PanicStillPropagates(t *testing.T) {
	panickingDecodes.Store(0)
	cli := seedStatic(t, map[string]string{"p": `{}`})
	for i := 0; i < 2; i++ {
		func() {
			defer func() {
				if r := recover(); r != "boom" {
					t.Fatalf("read %d: recovered %v, want the UnmarshalJSON panic", i, r)
				}
			}()
			var p panickingCfg
			cli.GetConfigStaticJSON("ns1", "p", &p)
		}()
	}
	if n := panickingDecodes.Load(); n != 1 {
		t.Fatalf("panicking decode ran %d times, want 1", n)
	}
}

func TestGetConfigStaticJSON_UncacheableTypeDecodesEveryTime(t *testing.T) {
	hiddenDecodes.Store(0)
	cli := seedStatic(t, map[string]string{"h": `{"n":3}`})
	for i := 0; i < 3; i++ {
		var h hiddenCfg
		if !cli.GetConfigStaticJSON("ns1", "h", &h) || h.N != 3 {
			t.Fatalf("read %d: got %+v", i, h)
		}
	}
	if n := hiddenDecodes.Load(); n != 3 {
		t.Fatalf("uncacheable type decoded %d times, want 3", n)
	}
}

func TestGetConfigStaticJSON_InvalidOutMatchesUnmarshal(t *testing.T) {
	cli := seedStatic(t, map[string]string{"obj": `{"n":1}`})
	if cli.GetConfigStaticJSON("ns1", "obj", nil) {
		t.Fatal("nil out must return false")
	}
	var c countedCfg
	if cli.GetConfigStaticJSON("ns1", "obj", c) {
		t.Fatal("non-pointer out must return false")
	}
}

func TestGetConfigJSON_CachesPerResolvedVersion(t *testing.T) {
	countedDecodes.Store(0)
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
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		var ab, full countedCfg
		abctx := cli.MockAbtestContext("u1", map[string]map[string]int64{"ns1": {"big": 2}})
		if err := cli.GetConfigJSON(ctx, abctx, "ns1", "big", &ab); err != nil || ab.N != 2 {
			t.Fatalf("abtest hit: got (%+v, %v), want n=2", ab, err)
		}
		if err := cli.GetConfigJSON(ctx, cli.EmptyAbtestContext(), "ns1", "big", &full); err != nil || full.N != 1 {
			t.Fatalf("full release: got (%+v, %v), want n=1", full, err)
		}
	}
	if n := countedDecodes.Load(); n != 2 {
		t.Fatalf("two versions read 3 times each were decoded %d times, want 2", n)
	}
}

func TestGetConfigJSON_ErrorsKeepWrapping(t *testing.T) {
	cli := seedDynamicFull(t, map[string]string{"rejected": `{"n":1}`, "obj": `{"n":1}`})
	ctx, abctx := emptyCtx(cli)
	for i := 0; i < 2; i++ {
		var r rejectingCfg
		err := cli.GetConfigJSON(ctx, abctx, "ns1", "rejected", &r)
		if !errors.Is(err, errRejected) || !strings.Contains(err.Error(), `parse "{\"n\":1}" as json`) {
			t.Fatalf("read %d: err = %v, want the wrapped UnmarshalJSON error", i, err)
		}
	}
	var c countedCfg
	var invalid *json.InvalidUnmarshalError
	if err := cli.GetConfigJSON(ctx, abctx, "ns1", "obj", c); !errors.As(err, &invalid) {
		t.Fatalf("non-pointer out: err = %v, want *json.InvalidUnmarshalError", err)
	}
}

func TestGetConfigJSON_SeparateTypesCachedSeparately(t *testing.T) {
	countedDecodes.Store(0)
	cli := seedDynamicFull(t, map[string]string{"big": countedRaw})
	ctx, abctx := emptyCtx(cli)
	for i := 0; i < 3; i++ {
		var c countedCfg
		var m map[string]any
		if err := cli.GetConfigJSON(ctx, abctx, "ns1", "big", &c); err != nil || c.N != 7 {
			t.Fatalf("struct: (%+v, %v)", c, err)
		}
		if err := cli.GetConfigJSON(ctx, abctx, "ns1", "big", &m); err != nil || m["n"] != float64(7) {
			t.Fatalf("map: (%v, %v)", m, err)
		}
	}
	if n := countedDecodes.Load(); n != 1 {
		t.Fatalf("struct decoded %d times, want 1", n)
	}
}

// seedParsedClient is seedStatic with explicit versions.
func seedParsedClient(t *testing.T, keys map[string]typedKey) *Client {
	t.Helper()
	h := newHarness(t)
	h.cfgServer.SetPullSnapshot(makeSnapshot("ns1", 1, 1, keys))
	cli, err := Init(context.Background(), h.baseConfigNoAbtest([]string{"ns1"}))
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

type deepNode struct {
	Name     string
	When     time.Time
	Ptr      *int
	Arr      [2][]int
	Kids     []deepNode
	ByName   map[string]*deepNode
	Any      any
	Raw      json.RawMessage
	Flat     [3]int
	FlatList []struct{ A, B string }
}

func TestDeepCopyJSONValue_SharesNoMutableMemory(t *testing.T) {
	seven := 7
	src := deepNode{
		Name:     "root",
		When:     time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC),
		Ptr:      &seven,
		Arr:      [2][]int{{1}, {2, 3}},
		Kids:     []deepNode{{Name: "kid", ByName: map[string]*deepNode{"x": {Name: "x"}}}},
		ByName:   map[string]*deepNode{"a": {Name: "a", Any: []any{"s", map[string]any{"k": 1.0}}}},
		Any:      map[string]any{"list": []any{1.0, "two"}},
		Raw:      json.RawMessage(`{"r":1}`),
		Flat:     [3]int{1, 2, 3},
		FlatList: []struct{ A, B string }{{"a", "b"}},
	}
	if !jsonTypeCacheable(reflect.TypeOf(src)) {
		t.Fatal("deepNode should be cacheable")
	}
	cp := deepCopyJSONValue(reflect.ValueOf(src)).Interface().(deepNode)
	if !reflect.DeepEqual(cp, src) {
		t.Fatalf("copy differs:\n got %#v\nwant %#v", cp, src)
	}

	*cp.Ptr = 8
	cp.Arr[1][0] = 99
	cp.Kids[0].ByName["x"].Name = "changed"
	cp.ByName["a"].Any.([]any)[1].(map[string]any)["k"] = 2.0
	cp.Any.(map[string]any)["list"].([]any)[0] = "changed"
	cp.Raw[2] = 'X'
	cp.FlatList[0].A = "changed"

	if *src.Ptr != 7 || src.Arr[1][0] != 2 || src.Kids[0].ByName["x"].Name != "x" ||
		src.ByName["a"].Any.([]any)[1].(map[string]any)["k"] != 1.0 ||
		src.Any.(map[string]any)["list"].([]any)[0] != 1.0 || string(src.Raw) != `{"r":1}` ||
		src.FlatList[0].A != "a" {
		t.Fatalf("writes to the copy reached the source: %#v", src)
	}
}

func TestJSONTypeCacheable(t *testing.T) {
	type withChan struct{ C chan int }
	type withUnexported struct{ a int }
	type embedsUnexported struct {
		withUnexported
		B int
	}
	cases := []struct {
		v    any
		want bool
	}{
		{map[string]any{}, true},
		{[]string{}, true},
		{deepNode{}, true},
		{time.Time{}, true},
		{withChan{}, false},
		{withUnexported{}, false},
		{embedsUnexported{}, false},
		{map[string]withUnexported{}, false},
		{(*func())(nil), false},
	}
	for _, c := range cases {
		if got := jsonTypeCacheable(reflect.TypeOf(c.v)); got != c.want {
			t.Errorf("jsonTypeCacheable(%T) = %v, want %v", c.v, got, c.want)
		}
	}
}

// benchJSON builds a JSON object of at least n bytes.
func benchJSON(n int) string {
	var b strings.Builder
	b.WriteString(`{"items":[`)
	for i := 0; b.Len() < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%d,"name":"persona_%d","prompt":%q,"tags":["a","b"]}`, i, i, strings.Repeat("你是一个温柔的角色，回答要简短自然。", 8))
	}
	b.WriteString(`]}`)
	return b.String()
}

type benchCfg struct {
	Items []struct {
		ID     int      `json:"id"`
		Name   string   `json:"name"`
		Prompt string   `json:"prompt"`
		Tags   []string `json:"tags"`
	} `json:"items"`
}

// BenchmarkStaticJSON100KB compares the old behaviour (json.Unmarshal on every
// read) with the cached GetConfigStaticJSON on a ~100 KB value.
//
//	go test -run '^$' -bench StaticJSON100KB -benchmem ./sdk/go/tipsyabconfig/
func BenchmarkStaticJSON100KB(b *testing.B) {
	raw := benchJSON(100 << 10)
	c := &Client{cache: newConfigCache(newMetrics()), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	c.cache.applyProto(makeSnapshot("ns1", 1, 1, map[string]typedKey{"big": {full: 1, versions: map[int64]string{1: raw}}}))

	b.Run("struct/json.Unmarshal", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var out benchCfg
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("struct/GetConfigStaticJSON", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var out benchCfg
			if !c.GetConfigStaticJSON("ns1", "big", &out) {
				b.Fatal("miss")
			}
		}
	})
	b.Run("map/json.Unmarshal", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var out map[string]any
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("map/GetConfigStaticJSON", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			var out map[string]any
			if !c.GetConfigStaticJSON("ns1", "big", &out) {
				b.Fatal("miss")
			}
		}
	})
}
