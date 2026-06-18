package tipsyabconfig

import (
	"sync"
	"testing"

	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
)

// makeSnapshot builds a proto NamespaceSnapshot for the tests.
//   - keys: map of key -> (fullVer, versions). fullVer==0 means "unset"
//     (we omit the optional pointer).
func makeSnapshot(ns string, biz, exp int64, keys map[string]struct {
	full     int64
	versions map[int64]string
}) *configv1.NamespaceSnapshot {
	s := &configv1.NamespaceSnapshot{
		Namespace:             ns,
		BusinessSnapshotSeq:   biz,
		ExperimentSnapshotSeq: exp,
	}
	for k, v := range keys {
		ks := &configv1.KeyState{Key: k, Versions: map[int64]string{}}
		if v.full != 0 {
			fv := v.full
			ks.FullReleaseVersion = &fv
		}
		for vid, val := range v.versions {
			ks.Versions[vid] = val
		}
		s.Keys = append(s.Keys, ks)
	}
	return s
}

func TestCache_SetAndGetRoundTrip(t *testing.T) {
	c := newConfigCache(newMetrics())
	pb := makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{
		"k1": {full: 10, versions: map[int64]string{10: "v10"}},
	})
	replaced, bMoved, eMoved := c.applyProto(pb)
	if !replaced || !bMoved || !eMoved {
		t.Fatalf("expected first apply to be replaced+both moved, got replaced=%v biz=%v exp=%v", replaced, bMoved, eMoved)
	}
	if v, ok := c.fullReleaseVersion("ns1", "k1"); !ok || v != 10 {
		t.Fatalf("fullReleaseVersion: got (%d,%v) want (10,true)", v, ok)
	}
	if val, ok := c.valueOf("ns1", "k1", 10); !ok || val != "v10" {
		t.Fatalf("valueOf: got (%q,%v) want (v10,true)", val, ok)
	}
}

func TestCache_MissingNamespace(t *testing.T) {
	c := newConfigCache(newMetrics())
	if s := c.snapshot("missing"); s != nil {
		t.Fatalf("expected nil snapshot, got %+v", s)
	}
	if v, ok := c.fullReleaseVersion("missing", "k"); ok || v != 0 {
		t.Fatalf("expected miss, got (%d,%v)", v, ok)
	}
	if val, ok := c.valueOf("missing", "k", 1); ok || val != "" {
		t.Fatalf("expected miss, got (%q,%v)", val, ok)
	}
}

func TestCache_DualSeqAdvanceOnlyOnSeqMove(t *testing.T) {
	c := newConfigCache(newMetrics())
	first := makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 100, versions: map[int64]string{100: "v"}}})
	if r, _, _ := c.applyProto(first); !r {
		t.Fatal("first apply must succeed")
	}

	// Same seqs again - should NOT replace.
	again := makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 200, versions: map[int64]string{200: "new"}}})
	r, bMoved, eMoved := c.applyProto(again)
	if r || bMoved || eMoved {
		t.Fatalf("same seqs must be rejected: replaced=%v biz=%v exp=%v", r, bMoved, eMoved)
	}
	if v, _ := c.fullReleaseVersion("ns1", "k"); v != 100 {
		t.Fatalf("expected unchanged version=100, got %d", v)
	}

	// Business only advances.
	bizOnly := makeSnapshot("ns1", 2, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 200, versions: map[int64]string{200: "biz"}}})
	r, bMoved, eMoved = c.applyProto(bizOnly)
	if !r || !bMoved || eMoved {
		t.Fatalf("biz-only advance: replaced=%v biz=%v exp=%v", r, bMoved, eMoved)
	}
	if v, _ := c.fullReleaseVersion("ns1", "k"); v != 200 {
		t.Fatalf("expected version=200 after biz advance, got %d", v)
	}

	// Experiment only advances.
	expOnly := makeSnapshot("ns1", 2, 5, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 300, versions: map[int64]string{300: "exp"}}})
	r, bMoved, eMoved = c.applyProto(expOnly)
	if !r || bMoved || !eMoved {
		t.Fatalf("exp-only advance: replaced=%v biz=%v exp=%v", r, bMoved, eMoved)
	}
	if v, _ := c.fullReleaseVersion("ns1", "k"); v != 300 {
		t.Fatalf("expected version=300 after exp advance, got %d", v)
	}
}

func TestCache_RejectOlderSeq(t *testing.T) {
	c := newConfigCache(newMetrics())
	c.applyProto(makeSnapshot("ns1", 5, 5, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 100, versions: map[int64]string{100: "v"}}}))

	older := makeSnapshot("ns1", 3, 3, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 99, versions: map[int64]string{99: "x"}}})
	r, bMoved, eMoved := c.applyProto(older)
	if r || bMoved || eMoved {
		t.Fatalf("expected older snapshot rejected, got replaced=%v biz=%v exp=%v", r, bMoved, eMoved)
	}
	if v, _ := c.fullReleaseVersion("ns1", "k"); v != 100 {
		t.Fatalf("expected version unchanged at 100, got %d", v)
	}
}

func TestCache_ZeroToOneReplaces(t *testing.T) {
	// Initial cache empty. Even snapshot with seqs (0,0) on a missing ns
	// is treated as a first replace (cur=nil branch).
	c := newConfigCache(newMetrics())
	r, _, _ := c.applyProto(makeSnapshot("ns1", 0, 0, map[string]struct {
		full     int64
		versions map[int64]string
	}{}))
	if !r {
		t.Fatal("expected first apply to replace even at seqs (0,0) when ns absent")
	}
	// Now move 0 -> 1 on biz, exp stays 0.
	r2, bMoved, eMoved := c.applyProto(makeSnapshot("ns1", 1, 0, nil))
	if !r2 || !bMoved || eMoved {
		t.Fatalf("0->1 on biz: replaced=%v biz=%v exp=%v", r2, bMoved, eMoved)
	}
}

func TestCache_FullReleaseVersionUnsetVsZero(t *testing.T) {
	// proto3 optional: when FullReleaseVersion is nil pointer the cache
	// must store sentinel 0 and report "not set".
	c := newConfigCache(newMetrics())
	pb := &configv1.NamespaceSnapshot{
		Namespace:             "ns1",
		BusinessSnapshotSeq:   1,
		ExperimentSnapshotSeq: 1,
		Keys: []*configv1.KeyState{
			{Key: "noFull", Versions: map[int64]string{42: "v42"}},
			{Key: "withFull", Versions: map[int64]string{}},
		},
	}
	fv := int64(99)
	pb.Keys[1].FullReleaseVersion = &fv
	pb.Keys[1].Versions[99] = "v99"

	c.applyProto(pb)
	if _, ok := c.fullReleaseVersion("ns1", "noFull"); ok {
		t.Fatal("expected noFull to report 'no full release'")
	}
	if v, ok := c.fullReleaseVersion("ns1", "withFull"); !ok || v != 99 {
		t.Fatalf("withFull: got (%d,%v) want (99,true)", v, ok)
	}
}

func TestCache_ConcurrentReadsDuringReplace(t *testing.T) {
	c := newConfigCache(newMetrics())
	c.applyProto(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "v1"}}}))

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = c.snapshot("ns1")
				_, _ = c.fullReleaseVersion("ns1", "k")
				_, _ = c.valueOf("ns1", "k", 1)
			}
		}()
	}
	for i := int64(2); i < 200; i++ {
		c.applyProto(makeSnapshot("ns1", i, i, map[string]struct {
			full     int64
			versions map[int64]string
		}{"k": {full: i, versions: map[int64]string{i: "v"}}}))
	}
	close(stop)
	wg.Wait()
}

func TestCache_KnownSeqsAndListNamespaces(t *testing.T) {
	c := newConfigCache(newMetrics())
	c.applyProto(makeSnapshot("nsA", 3, 4, nil))
	c.applyProto(makeSnapshot("nsB", 7, 1, nil))

	got := c.knownSeqs([]string{"nsA", "nsB", "missing"})
	if got["nsA"].BusinessSnapshotSeq != 3 || got["nsA"].ExperimentSnapshotSeq != 4 {
		t.Fatalf("nsA seqs wrong: %+v", got["nsA"])
	}
	if got["nsB"].BusinessSnapshotSeq != 7 || got["nsB"].ExperimentSnapshotSeq != 1 {
		t.Fatalf("nsB seqs wrong: %+v", got["nsB"])
	}
	if m, ok := got["missing"]; !ok || m == nil || m.BusinessSnapshotSeq != 0 || m.ExperimentSnapshotSeq != 0 {
		t.Fatalf("missing ns should be present with (0,0), got %+v", m)
	}

	list := c.listNamespaces()
	if len(list) != 2 || list[0] != "nsA" || list[1] != "nsB" {
		t.Fatalf("listNamespaces wrong: %v", list)
	}
}

func TestCache_NilSnapshotIgnored(t *testing.T) {
	c := newConfigCache(newMetrics())
	r, _, _ := c.applyProto(nil)
	if r {
		t.Fatal("nil pb must be ignored")
	}
	r, _, _ = c.applyProto(&configv1.NamespaceSnapshot{Namespace: ""})
	if r {
		t.Fatal("empty namespace must be ignored")
	}
}

func TestCache_MetricsBytesAndSeqCounters(t *testing.T) {
	m := newMetrics()
	c := newConfigCache(m)
	c.applyProto(makeSnapshot("ns1", 1, 1, map[string]struct {
		full     int64
		versions map[int64]string
	}{"k": {full: 1, versions: map[int64]string{1: "hello"}}}))
	if got := m.BusinessSeqChangeTotal("ns1"); got != 1 {
		t.Fatalf("BusinessSeqChangeTotal: %d", got)
	}
	if got := m.ExperimentSeqChangeTotal("ns1"); got != 1 {
		t.Fatalf("ExperimentSeqChangeTotal: %d", got)
	}
	if got := m.LocalCacheBytes("ns1"); got == 0 {
		t.Fatal("LocalCacheBytes should be > 0")
	}
}
