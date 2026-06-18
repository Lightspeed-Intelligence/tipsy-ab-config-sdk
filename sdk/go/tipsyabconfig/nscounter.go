package tipsyabconfig

import (
	"sync"
	"sync/atomic"
)

// nsCounter is a sync-protected map[string]*atomic.Uint64 used for per-ns
// monotonic metric counters. It is created via newNSCounter().
type nsCounter struct {
	mu sync.RWMutex
	m  map[string]*atomic.Uint64
}

func newNSCounter() *nsCounter { return &nsCounter{m: make(map[string]*atomic.Uint64)} }

func (c *nsCounter) inc(ns string) {
	c.mu.RLock()
	v, ok := c.m[ns]
	c.mu.RUnlock()
	if ok {
		v.Add(1)
		return
	}
	c.mu.Lock()
	v, ok = c.m[ns]
	if !ok {
		v = &atomic.Uint64{}
		c.m[ns] = v
	}
	c.mu.Unlock()
	v.Add(1)
}

func (c *nsCounter) get(ns string) uint64 {
	c.mu.RLock()
	v, ok := c.m[ns]
	c.mu.RUnlock()
	if !ok {
		return 0
	}
	return v.Load()
}

// nsGauge is a sync-protected map[string]*atomic.Uint64 used for per-ns
// last-write-wins gauges (e.g. local_cache_bytes).
type nsGauge struct {
	mu sync.RWMutex
	m  map[string]*atomic.Uint64
}

func newNSGauge() *nsGauge { return &nsGauge{m: make(map[string]*atomic.Uint64)} }

func (g *nsGauge) set(ns string, v uint64) {
	g.mu.RLock()
	cur, ok := g.m[ns]
	g.mu.RUnlock()
	if ok {
		cur.Store(v)
		return
	}
	g.mu.Lock()
	cur, ok = g.m[ns]
	if !ok {
		cur = &atomic.Uint64{}
		g.m[ns] = cur
	}
	g.mu.Unlock()
	cur.Store(v)
}

func (g *nsGauge) get(ns string) uint64 {
	g.mu.RLock()
	v, ok := g.m[ns]
	g.mu.RUnlock()
	if !ok {
		return 0
	}
	return v.Load()
}
