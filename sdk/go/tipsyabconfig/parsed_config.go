package tipsyabconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// ParsedConfig reads a config value and returns it parsed, caching the parse
// result per version so a hot path that reads a large value (e.g. a 100 KB+
// JSON) does not parse it again on every request.
//
// Caching by version needs no invalidation: a published config_version never
// changes and its id is globally unique, so one version always parses to the
// same result. The cache lives on the SDK's in-memory namespace snapshot. When
// a newer snapshot replaces it (a publish, offline, experiment or whitelist
// change in that namespace), the old results go away with it and each version
// still in use is parsed once more on its next read. Concurrent first reads of
// a version parse it once. A parse error is cached too, so a bad value is not
// re-parsed on every read.
//
// Create one ParsedConfig per parse function, usually as a package-level var,
// and reuse it; results are cached per ParsedConfig, so two of them never share
// results even when T is the same:
//
//	var promptCfg = tipsyabconfig.NewParsedConfig(tipsyabconfig.ParseJSON[*PromptCfg])
//
//	cfg, ok, err := promptCfg.Get(ctx, cli, abctx, "tipsy-chat", "prompt_cfg")
//
// The returned value is shared by every caller that reads the same version.
// Callers MUST NOT modify it, including maps, slices and pointees reachable
// from it; copy first if a modified version is needed. A pointer T avoids
// copying a large struct on every read.
type ParsedConfig[T any] struct {
	parse func(raw string) (T, error)
}

// NewParsedConfig returns a ParsedConfig that parses values with parse. parse
// must be deterministic for a given raw value. It panics if parse is nil.
func NewParsedConfig[T any](parse func(raw string) (T, error)) *ParsedConfig[T] {
	if parse == nil {
		panic("tipsyabconfig: NewParsedConfig: parse must not be nil")
	}
	return &ParsedConfig[T]{parse: parse}
}

// ParseJSON is a parse function for NewParsedConfig that json.Unmarshals the
// value into a new T.
func ParseJSON[T any](raw string) (T, error) {
	var v T
	err := json.Unmarshal([]byte(raw), &v)
	return v, err
}

// Get resolves (ns, key) for the user exactly like Client.GetConfig (same
// abtest resolution, ns defaulting and hit / default log lines) and returns the
// parsed value of the resolved version.
//
//   - ok=false, err=nil: no value resolved (GetConfig would return its default).
//   - ok=false, err!=nil: the error GetConfig would return (ctx canceled or
//     deadline, namespace errors, nil abctx, nil client).
//   - ok=true, err=nil: v is the parsed value.
//   - ok=true, err!=nil: a value was resolved but parse failed; err wraps the
//     parse error and v is the zero T.
func (p *ParsedConfig[T]) Get(ctx context.Context, c *Client, abctx *AbtestContext, ns, key string) (v T, ok bool, err error) {
	r, err := c.resolveConfig(ctx, abctx, ns, key)
	if err != nil || r.res.source == keySourceNone {
		return v, false, err
	}
	v, err = p.load(r.snap, key, r.res.version, r.res.value)
	return v, true, err
}

// GetStatic is the full-release counterpart of Get: like Client.GetConfigStatic
// it is user-agnostic and makes no abtest call. ok=false means no full-release
// value is cached for (ns, key) (or c is nil); ok=true with err!=nil means the
// value failed to parse.
func (p *ParsedConfig[T]) GetStatic(c *Client, ns, key string) (v T, ok bool, err error) {
	snap, raw, version, hit := c.resolveStatic(ns, key)
	if !hit {
		return v, false, nil
	}
	v, err = p.load(snap, key, version, raw)
	return v, true, err
}

// parsedKey identifies one cached result inside a snapshot. owner is the
// *ParsedConfig[T], so entries for one key always hold a *parsedEntry[T].
// Version ids are globally unique, so the config key is not part of it.
type parsedKey struct {
	owner   any
	version int64
}

type parsedEntry[T any] struct {
	once  sync.Once
	value T
	err   error
}

// load returns the parse result for version, parsing raw at most once per
// (snapshot, ParsedConfig, version).
func (p *ParsedConfig[T]) load(snap *NamespaceSnapshot, key string, version int64, raw string) (T, error) {
	if snap == nil || snap.parsed == nil {
		return p.parseRaw(snap, key, version, raw)
	}
	k := parsedKey{owner: p, version: version}
	got, ok := snap.parsed.Load(k)
	if !ok {
		got, _ = snap.parsed.LoadOrStore(k, &parsedEntry[T]{})
	}
	e := got.(*parsedEntry[T])
	e.once.Do(func() { e.value, e.err = p.parseRaw(snap, key, version, raw) })
	return e.value, e.err
}

// parseRaw runs the caller's parse function. A panic is turned into an error:
// otherwise the entry's once would be spent with no result recorded, and later
// readers would get the zero T with a nil error.
func (p *ParsedConfig[T]) parseRaw(snap *NamespaceSnapshot, key string, version int64, raw string) (v T, err error) {
	ns := ""
	if snap != nil {
		ns = snap.Namespace
	}
	defer func() {
		if r := recover(); r != nil {
			var zero T
			v, err = zero, fmt.Errorf("tipsyabconfig: parse %s/%s version %d panicked: %v", ns, key, version, r)
		}
	}()
	v, err = p.parse(raw)
	if err != nil {
		var zero T
		return zero, fmt.Errorf("tipsyabconfig: parse %s/%s version %d: %w", ns, key, version, err)
	}
	return v, nil
}
