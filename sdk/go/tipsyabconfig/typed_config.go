package tipsyabconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Typed config accessors.
//
// The platform stores every config value as a canonical string end-to-end
// (DB / proto / snapshot / SDK cache / wire are all string; value_type lives
// only on config_key as a console-side write contract and is NOT pushed to the
// SDK). These helpers wrap the string-returning GetConfigStatic / GetConfig and
// parse the value at the very edge, so callers get a typed value directly
// (mirrors Apollo's getIntProperty / getBooleanProperty surface).
//
// Read/write asymmetry (by design): the console writes bool via a true/false
// selector so stored bool values are always canonical "true"/"false", but the
// SDK parses bool leniently to tolerate any source (e.g. a hand-written SQL
// "TRUE"/"1"). Bool parsing therefore never fails.
//
// int64 precision: long values stay strings the whole way and are parsed once
// here with strconv.ParseInt(s, 10, 64) straight to int64 — no JSON/float64
// round-trip — so values beyond 2^53 are lossless.

// parseBoolLenient implements the lenient bool rule shared by the static and
// dynamic accessors. After trimming surrounding whitespace, "true"
// (case-insensitive) or "1" is true; everything else — including "false", "0",
// the empty string and arbitrary garbage — is false. It never reports an error.
func parseBoolLenient(raw string) bool {
	s := strings.TrimSpace(raw)
	return strings.EqualFold(s, "true") || s == "1"
}

// ---------------------------------------------------------------------------
// Static accessors (wrap GetConfigStatic; user-agnostic, no abtest RPC).
//
// They use the underlying ok bool to distinguish a cache hit from a miss — NOT
// a raw=="" heuristic. On a miss the typed default is returned with ok=false.
// On a hit the cached string is parsed; bool/string always succeed, while
// int64/float64/json return (def, false) when the cached value fails to parse.
// ---------------------------------------------------------------------------

// GetConfigStaticBool returns the full-release value for (ns, key) parsed as a
// bool. On a cache miss it returns (def, false). On a hit it returns
// (parsedBool, true); bool parsing is lenient and never fails (see
// parseBoolLenient).
func (c *Client) GetConfigStaticBool(ns, key string, def bool) (bool, bool) {
	raw, ok := c.GetConfigStatic(ns, key, "")
	if !ok {
		return def, false
	}
	return parseBoolLenient(raw), true
}

// GetConfigStaticInt64 returns the full-release value for (ns, key) parsed as an
// int64. On a cache miss it returns (def, false). On a hit it parses the trimmed
// value with strconv.ParseInt(.,10,64); on parse failure it returns (def, false).
func (c *Client) GetConfigStaticInt64(ns, key string, def int64) (int64, bool) {
	raw, ok := c.GetConfigStatic(ns, key, "")
	if !ok {
		return def, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return def, false
	}
	return v, true
}

// GetConfigStaticFloat64 returns the full-release value for (ns, key) parsed as a
// float64. On a cache miss it returns (def, false). On a hit it parses the
// trimmed value with strconv.ParseFloat(.,64); on parse failure it returns
// (def, false).
func (c *Client) GetConfigStaticFloat64(ns, key string, def float64) (float64, bool) {
	raw, ok := c.GetConfigStatic(ns, key, "")
	if !ok {
		return def, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return def, false
	}
	return v, true
}

// GetConfigStaticString is the symmetry-named equivalent of GetConfigStatic: it
// returns the raw full-release value for (ns, key), or (def, false) on a miss.
func (c *Client) GetConfigStaticString(ns, key, def string) (string, bool) {
	raw, ok := c.GetConfigStatic(ns, key, "")
	if !ok {
		return def, false
	}
	return raw, true
}

// GetConfigStaticJSON unmarshals the full-release value for (ns, key) into out.
// It returns false on a cache miss OR an unmarshal failure (out is left in
// whatever state json.Unmarshal produced), and true only when the cached value
// successfully unmarshals.
func (c *Client) GetConfigStaticJSON(ns, key string, out any) bool {
	raw, ok := c.GetConfigStatic(ns, key, "")
	if !ok {
		return false
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Dynamic accessors (wrap GetConfig; user-scoped, honor abtest resolution).
//
// Per design Q4 these keep the (T, error) shape of the underlying GetConfig and
// do NOT add an ok bool. Semantics:
//   - underlying err != nil (ctx canceled/deadline, ns errors): return (def, err).
//   - raw == "" : treated as a miss — non-string types never publish an empty
//     value, so "" can only mean "no hit" — return (def, nil), no parse attempted.
//   - otherwise: parse the value. Bool never reports a parse error; int64/
//     float64/json return (def, wrappedParseErr) on failure.
// Callers that must distinguish a miss from a parsed default should use the
// static (T, ok) variants instead.
// ---------------------------------------------------------------------------

// GetConfigBool resolves (ns, key) for the user and parses the value as a bool.
// A missing key or any non-ctx condition yields (def, nil). Bool parsing is
// lenient and never produces a parse error; the only error returned is the
// underlying ctx/ns error from GetConfig.
func (c *Client) GetConfigBool(ctx context.Context, abctx *AbtestContext, ns, key string, def bool) (bool, error) {
	raw, err := c.GetConfig(ctx, abctx, ns, key, "")
	if err != nil {
		return def, err
	}
	if raw == "" {
		return def, nil
	}
	return parseBoolLenient(raw), nil
}

// GetConfigInt64 resolves (ns, key) for the user and parses the value as an
// int64 via strconv.ParseInt(.,10,64). Returns (def, nil) on a miss or
// (def, wrappedErr) when a non-empty value fails to parse.
func (c *Client) GetConfigInt64(ctx context.Context, abctx *AbtestContext, ns, key string, def int64) (int64, error) {
	raw, err := c.GetConfig(ctx, abctx, ns, key, "")
	if err != nil {
		return def, err
	}
	if raw == "" {
		return def, nil
	}
	v, perr := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if perr != nil {
		return def, fmt.Errorf("tipsyabconfig: parse %q as int64: %w", raw, perr)
	}
	return v, nil
}

// GetConfigFloat64 resolves (ns, key) for the user and parses the value as a
// float64 via strconv.ParseFloat(.,64). Returns (def, nil) on a miss or
// (def, wrappedErr) when a non-empty value fails to parse.
func (c *Client) GetConfigFloat64(ctx context.Context, abctx *AbtestContext, ns, key string, def float64) (float64, error) {
	raw, err := c.GetConfig(ctx, abctx, ns, key, "")
	if err != nil {
		return def, err
	}
	if raw == "" {
		return def, nil
	}
	v, perr := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if perr != nil {
		return def, fmt.Errorf("tipsyabconfig: parse %q as float64: %w", raw, perr)
	}
	return v, nil
}

// GetConfigString is the symmetry-named counterpart of GetConfig. It resolves
// (ns, key) for the user and returns the raw value, or (def, nil) on a miss and
// (def, err) on an underlying ctx/ns error. It differs from GetConfig in one
// case: a resolved-but-empty value ("") is treated as a miss and returns def,
// because the dynamic path has no separate miss signal (it shares the typed
// accessors' raw=="" ⇒ miss rule). Use the static GetConfigStaticString when
// you must distinguish a genuinely empty string value from a miss.
func (c *Client) GetConfigString(ctx context.Context, abctx *AbtestContext, ns, key, def string) (string, error) {
	raw, err := c.GetConfig(ctx, abctx, ns, key, "")
	if err != nil {
		return def, err
	}
	if raw == "" {
		return def, nil
	}
	return raw, nil
}

// GetConfigJSON resolves (ns, key) for the user and unmarshals the value into
// out. A miss returns nil and leaves out untouched. A non-empty value that
// fails to unmarshal returns a wrapped error. The only other error is the
// underlying ctx/ns error from GetConfig.
func (c *Client) GetConfigJSON(ctx context.Context, abctx *AbtestContext, ns, key string, out any) error {
	raw, err := c.GetConfig(ctx, abctx, ns, key, "")
	if err != nil {
		return err
	}
	if raw == "" {
		return nil
	}
	if uerr := json.Unmarshal([]byte(raw), out); uerr != nil {
		return fmt.Errorf("tipsyabconfig: parse %q as json: %w", raw, uerr)
	}
	return nil
}
