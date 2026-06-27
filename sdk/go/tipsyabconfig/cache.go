package tipsyabconfig

import (
	"sort"
	"sync"

	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
)

// KeyState is the SDK-local mirror of configv1.KeyState. It is a value type
// embedded in NamespaceSnapshot so reads do not need pointer indirection.
type KeyState struct {
	// FullReleaseVersion is the version_id of the active full release for
	// this key, or 0 if none. We use 0 as the sentinel because proto3
	// `optional int64` is normalised here: the SDK only stores positive
	// version ids on the wire; the host caller never sees the raw struct.
	FullReleaseVersion int64
	// Versions maps version_id → value. Contains the full-release version
	// (if any) plus every version that abtest reports as "possibly
	// applicable" for the namespace.
	Versions map[int64]string
	// HasDynamicResolution mirrors the proto's `optional bool` presence
	// semantics: nil = field absent (old server), &false = explicit "pure
	// full-release, no gray/experiment", &true = needs abtest resolution.
	// GetConfig only takes the fast-path (skip abtest) when this is present
	// AND false; nil (old server) or true keeps the existing abtest path so a
	// new SDK against an old server never silently skips a live experiment.
	HasDynamicResolution *bool
}

// NamespaceSnapshot is the per-ns immutable view the cache hands out to
// readers. The dual snapshot_seq pair (business + experiment) is the freshness
// vector — either advancing triggers a cache replace.
type NamespaceSnapshot struct {
	Namespace             string
	BusinessSnapshotSeq   int64
	ExperimentSnapshotSeq int64
	Keys                  map[string]KeyState
}

// configCache is the per-process in-memory cache of NamespaceSnapshot.
//
// Concurrency model: many readers, one writer per namespace at a time. We use
// a sync.RWMutex on the namespace map; the per-ns snapshot pointer itself is
// atomic by virtue of the map slot write under the writer lock. Readers grab
// an RLock, fetch the pointer, then read its fields lock-free (the snapshot
// is treated as immutable once published).
type configCache struct {
	mu      sync.RWMutex
	byNS    map[string]*NamespaceSnapshot
	metrics *Metrics
}

func newConfigCache(m *Metrics) *configCache {
	return &configCache{byNS: make(map[string]*NamespaceSnapshot), metrics: m}
}

// snapshot returns the current snapshot for ns, or nil if absent. Safe for
// concurrent use; the returned pointer is treated as immutable.
func (c *configCache) snapshot(ns string) *NamespaceSnapshot {
	c.mu.RLock()
	s := c.byNS[ns]
	c.mu.RUnlock()
	return s
}

// fullReleaseVersion returns (versionID, true) when an active full-release
// version is present for (ns, key). It returns (0, false) on a miss; callers
// MUST NOT treat 0 as "no full release" — use the second return value.
func (c *configCache) fullReleaseVersion(ns, key string) (int64, bool) {
	s := c.snapshot(ns)
	if s == nil {
		return 0, false
	}
	ks, ok := s.Keys[key]
	if !ok || ks.FullReleaseVersion == 0 {
		return 0, false
	}
	return ks.FullReleaseVersion, true
}

// hasDynamicResolution reports the cached has_dynamic_resolution flag for
// (ns, key) with presence semantics. It returns (val, true) only when a
// snapshot for ns exists, the key is present, AND the field was explicitly set
// by the server. It returns (false, false) when the ns, key, or field is
// absent — i.e. present == false means "unknown / old server", NOT false.
// GetConfig MUST gate its fast-path on present == true && val == false so an
// absent field never triggers a skip.
func (c *configCache) hasDynamicResolution(ns, key string) (val bool, present bool) {
	s := c.snapshot(ns)
	if s == nil {
		return false, false
	}
	ks, ok := s.Keys[key]
	if !ok || ks.HasDynamicResolution == nil {
		return false, false
	}
	return *ks.HasDynamicResolution, true
}

// valueOf returns (value, true) when a specific (ns, key, versionID) is
// cached. It returns ("", false) on a cache miss. Per design §10.5 the empty
// string is a valid value; callers MUST gate on the second return value.
func (c *configCache) valueOf(ns, key string, versionID int64) (string, bool) {
	s := c.snapshot(ns)
	if s == nil {
		return "", false
	}
	ks, ok := s.Keys[key]
	if !ok {
		return "", false
	}
	v, ok := ks.Versions[versionID]
	return v, ok
}

// applyProto replaces the cached snapshot for ns iff either snapshot_seq
// advanced. It returns the (oldSnapshot, newSnapshot, replaced) triple so
// callers can fire metrics. Per design §4 either business OR experiment
// advancing — including 0 → 1 — triggers a replace.
func (c *configCache) applyProto(pb *configv1.NamespaceSnapshot) (replaced, businessMoved, experimentMoved bool) {
	if pb == nil || pb.Namespace == "" {
		return false, false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.byNS[pb.Namespace]

	newBiz := pb.BusinessSnapshotSeq
	newExp := pb.ExperimentSnapshotSeq

	var curBiz, curExp int64
	if cur != nil {
		curBiz = cur.BusinessSnapshotSeq
		curExp = cur.ExperimentSnapshotSeq
	}

	businessMoved = newBiz > curBiz
	experimentMoved = newExp > curExp
	if cur != nil && !businessMoved && !experimentMoved {
		return false, false, false
	}

	next := &NamespaceSnapshot{
		Namespace:             pb.Namespace,
		BusinessSnapshotSeq:   newBiz,
		ExperimentSnapshotSeq: newExp,
		Keys:                  make(map[string]KeyState, len(pb.Keys)),
	}
	var byteSize uint64
	for _, k := range pb.Keys {
		if k == nil || k.Key == "" {
			continue
		}
		ks := KeyState{Versions: make(map[int64]string, len(k.Versions))}
		if k.FullReleaseVersion != nil {
			ks.FullReleaseVersion = *k.FullReleaseVersion
		}
		// Preserve presence: copy the pointer's value into a fresh local
		// pointer iff set, leaving nil when the proto field is absent.
		if k.HasDynamicResolution != nil {
			hdr := *k.HasDynamicResolution
			ks.HasDynamicResolution = &hdr
		}
		for vid, val := range k.Versions {
			ks.Versions[vid] = val
			byteSize += uint64(len(val))
		}
		byteSize += uint64(len(k.Key))
		next.Keys[k.Key] = ks
	}
	c.byNS[pb.Namespace] = next
	if c.metrics != nil {
		c.metrics.localCacheBytes.set(pb.Namespace, byteSize)
		if businessMoved {
			c.metrics.businessSeqMoved.inc(pb.Namespace)
		}
		if experimentMoved {
			c.metrics.experimentSeqMov.inc(pb.Namespace)
		}
	}
	return true, businessMoved, experimentMoved
}

// knownSeqs returns the per-ns NamespaceSeqs pair the cache currently holds.
// Namespaces with no snapshot are still listed with the zero pair, so the
// server treats a 0/0 entry as "send me everything" per backend §4.1.2.
func (c *configCache) knownSeqs(namespaces []string) map[string]*configv1.NamespaceSeqs {
	out := make(map[string]*configv1.NamespaceSeqs, len(namespaces))
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, ns := range namespaces {
		s := c.byNS[ns]
		if s == nil {
			out[ns] = &configv1.NamespaceSeqs{}
			continue
		}
		out[ns] = &configv1.NamespaceSeqs{
			BusinessSnapshotSeq:   s.BusinessSnapshotSeq,
			ExperimentSnapshotSeq: s.ExperimentSnapshotSeq,
		}
	}
	return out
}

// listNamespaces returns a sorted snapshot of cached namespace keys, useful
// for diagnostics and tests.
func (c *configCache) listNamespaces() []string {
	c.mu.RLock()
	out := make([]string, 0, len(c.byNS))
	for k := range c.byNS {
		out = append(out, k)
	}
	c.mu.RUnlock()
	sort.Strings(out)
	return out
}
