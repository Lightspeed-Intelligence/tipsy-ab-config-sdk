package tipsyabconfig

import (
	"sync"
	"time"
)

// BackgroundErrorEvent describes a single background-link failure delivered to
// Config.OnBackgroundError. The callback is invoked synchronously on the SDK's
// background goroutine; see Config.OnBackgroundError for the contract.
type BackgroundErrorEvent struct {
	// Phase is the background link that failed; one of:
	//   "startup_pull"  — the startup PullAll sweep failed and was absorbed
	//                      by StartupFailOpen (aggregate; Namespace is "").
	//   "periodic_pull" — a periodic (10 s fallback) PullAll for Namespace failed.
	//   "subscribe"     — the Subscribe stream errored and will reconnect.
	Phase string
	// Namespace is the namespace associated with the failure. It is "" for the
	// aggregate startup_pull failure and for subscribe failures (the stream is
	// multi-namespace).
	Namespace string
	// Err is the underlying error.
	Err error
	// Time is when the SDK observed the failure.
	Time time.Time
}

// Health is a snapshot of the SDK's background-link health. Obtain a copy via
// Client.Health. The zero value means "all healthy / no failures observed".
//
// HTTP transport mode (Config.Transport == TransportHTTP): the Subscribe stream
// is never opened, so SubscribeConnected is always false and LastSubscribeErr
// is always nil; the SDK relies solely on periodic PullAll, so only LastPullErr
// / StartupCacheEmpty are meaningful there.
type Health struct {
	// StartupCacheEmpty is true when StartupFailOpen absorbed a startup PullAll
	// failure and the client started with an empty cache.
	StartupCacheEmpty bool
	// LastPullErr is the most recent periodic PullAll error (nil = no failure
	// recorded yet). It is not cleared on subsequent success.
	LastPullErr error
	// LastPullErrTime is when LastPullErr was recorded.
	LastPullErrTime time.Time
	// LastSubscribeErr is the most recent Subscribe stream error (nil = none).
	// Always nil in HTTP transport mode (no Subscribe stream).
	LastSubscribeErr error
	// LastSubscribeErrTime is when LastSubscribeErr was recorded.
	LastSubscribeErrTime time.Time
	// SubscribeConnected reflects whether the Subscribe stream is currently
	// established (true after a successful stream open, false after a stream
	// error until the next successful open). Always false in HTTP transport
	// mode (no Subscribe stream is opened).
	SubscribeConnected bool
}

// healthState is the mutable, mutex-protected backing store for Client.Health.
// It is owned by the Client and accessed from the pull loop, the subscribe
// loop, Init, and external Health calls; all access goes through the helpers
// below so it is data-race-free under `go test -race`.
type healthState struct {
	mu   sync.Mutex
	snap Health
}

func (h *healthState) snapshot() Health {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snap
}

func (h *healthState) setStartupCacheEmpty() {
	h.mu.Lock()
	h.snap.StartupCacheEmpty = true
	h.mu.Unlock()
}

func (h *healthState) recordPullErr(err error, t time.Time) {
	h.mu.Lock()
	h.snap.LastPullErr = err
	h.snap.LastPullErrTime = t
	h.mu.Unlock()
}

func (h *healthState) recordSubscribeErr(err error, t time.Time) {
	h.mu.Lock()
	h.snap.LastSubscribeErr = err
	h.snap.LastSubscribeErrTime = t
	h.snap.SubscribeConnected = false
	h.mu.Unlock()
}

func (h *healthState) setSubscribeConnected(connected bool) {
	h.mu.Lock()
	h.snap.SubscribeConnected = connected
	h.mu.Unlock()
}

// Health returns a snapshot of the SDK's background-link health. Safe for
// concurrent use.
func (c *Client) Health() Health {
	return c.health.snapshot()
}

// fireBackgroundError records the failure into health state and invokes the
// user's OnBackgroundError callback synchronously. The callback is wrapped in
// recover() so that a panicking consumer callback cannot kill the SDK's
// background goroutine — an intentional boundary exception to the "trust
// internal code" norm, since this callback is external consumer code.
//
// The callback stays synchronous by design: a slow callback will slow the
// pull/subscribe loop, which is documented on Config.OnBackgroundError.
func (c *Client) fireBackgroundError(ev BackgroundErrorEvent) {
	switch ev.Phase {
	case "startup_pull":
		c.health.setStartupCacheEmpty()
	case "periodic_pull":
		c.health.recordPullErr(ev.Err, ev.Time)
	case "subscribe":
		c.health.recordSubscribeErr(ev.Err, ev.Time)
	}
	cb := c.cfg.OnBackgroundError
	if cb == nil {
		return
	}
	defer func() { _ = recover() }()
	cb(ev)
}
