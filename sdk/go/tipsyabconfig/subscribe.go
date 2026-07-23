package tipsyabconfig

import (
	"context"
	"errors"
	"io"
	"time"

	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
	"github.com/google/uuid"
)

// initialSubscribeBackoff is the starting reconnect backoff for runSubscribe
// (also the value the backoff resets to after a clean EOF and after a healthy
// connection is torn down — see resetBackoffIfStable).
const initialSubscribeBackoff = time.Second

// defaultSubscribeStableResetThreshold is the default minimum uptime for a
// just-ended connection to count as "healthy" so the reconnect backoff resets
// to the initial value. It sits above the 20 s server heartbeat interval and
// clearly above the dial+instant-failure band, yet below the observed CF
// ~124 s stream lifetime, so a CF idle-timeout teardown after minutes of
// uptime reconnects promptly instead of inheriting an escalated backoff.
const defaultSubscribeStableResetThreshold = 60 * time.Second

// runSubscribe maintains a long-lived ConfigService.Subscribe stream with
// exponential backoff (1 s → 2 s → 4 s → … capped at 30 s) per design §5.2.
// It exits when the SDK root context is cancelled.
//
// Backoff reset on the real-error path: a connection that stayed up at least
// c.stableResetThreshold before being torn down (e.g. an edge-proxy idle
// timeout after minutes of uptime) is treated as "healthy then dropped" and
// reconnects at the initial backoff instead of inheriting the escalated
// backoff meant for a server that cannot be reached at all.
func (c *Client) runSubscribe() {
	defer c.wg.Done()
	backoff := initialSubscribeBackoff
	const maxBackoff = 30 * time.Second
	for {
		if err := c.rootCtx.Err(); err != nil {
			return
		}
		start := time.Now()
		err := c.subscribeOnce(c.rootCtx)
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
			// Server closed cleanly; reconnect immediately on EOF, exit
			// on Canceled.
			if errors.Is(err, context.Canceled) {
				return
			}
			backoff = initialSubscribeBackoff
			continue
		}
		// Real error path: bump disconnect metric and back off. Reset the
		// backoff BEFORE sleeping when the just-ended connection was healthy,
		// so the logged backoff matches the actual time.After wait below.
		backoff = resetBackoffIfStable(backoff, time.Since(start), c.stableResetThreshold)
		for _, ns := range c.subscribedNamespaces {
			c.metrics.subscribeDisc.inc(ns)
		}
		c.logger.Error("tipsyabconfig: Subscribe stream error; reconnecting", "err", err, "backoff", backoff.String())
		// Record the disconnect for observability. recordSubscribeErr (inside
		// fireBackgroundError) also flips SubscribeConnected back to false.
		c.fireBackgroundError(BackgroundErrorEvent{
			Phase: "subscribe",
			Err:   err,
			Time:  time.Now(),
		})
		select {
		case <-c.rootCtx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// resetBackoffIfStable returns the initial backoff when the just-ended
// connection was up long enough to count as healthy (uptime >= threshold);
// otherwise it keeps the current backoff (which the caller then doubles).
// A healthy connection that is later torn down (e.g. an edge-proxy idle
// timeout after minutes of uptime) should reconnect promptly, not inherit the
// escalated backoff meant for a server that cannot be reached at all.
//
// A non-positive threshold (threshold <= 0) never counts as stable, so it
// falls through to the escalating backoff. This is a defensive guard: a
// hand-built Client that forgot to set stableResetThreshold (zero value) must
// not turn every failure against an unreachable server into a ~1s hot
// reconnect loop. Production always seeds it to 60s via Init, so this path is
// unreachable there.
func resetBackoffIfStable(backoff, uptime, threshold time.Duration) time.Duration {
	if threshold > 0 && uptime >= threshold {
		return initialSubscribeBackoff
	}
	return backoff
}

// subscribeOnce opens one Subscribe stream and pumps events into the cache.
// It returns the recv error (or nil on a clean EOF).
//
// trace_id semantics (sdk-trace-id §3, §4): a fresh trace_id is generated per
// subscribe attempt and stamped onto SubscribeRequest. It identifies "this
// subscription attempt" only — subsequent push events on the stream do not
// re-use it (they are inherently many-to-one with the connect).
func (c *Client) subscribeOnce(ctx context.Context) error {
	traceID := uuid.New().String()
	req := &configv1.SubscribeRequest{
		Namespaces: append([]string(nil), c.subscribedNamespaces...),
		KnownSeqs:  c.cache.knownSeqs(c.subscribedNamespaces),
		TraceId:    traceID,
		Env:        c.cfg.Env,
	}
	c.logger.Debug("tipsyabconfig: Subscribe", "namespaces", req.Namespaces, "trace_id", traceID)
	stream, err := c.configCli.Subscribe(ctx, req)
	if err != nil {
		return err
	}
	// Stream opened successfully — mark the subscribe link connected.
	c.health.setSubscribeConnected(true)
	for {
		ev, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return io.EOF
			}
			return err
		}
		c.handleEvent(ev)
	}
}

// handleEvent applies a ConfigUpdateEvent to the cache. Unknown oneof
// branches are silently skipped (design §5.2 forward-compat clause).
func (c *Client) handleEvent(ev *configv1.ConfigUpdateEvent) {
	if ev == nil {
		return
	}
	switch payload := ev.Payload.(type) {
	case *configv1.ConfigUpdateEvent_Snapshot:
		s := payload.Snapshot
		if s == nil {
			return
		}
		c.metrics.subscribeEvent.inc(s.Namespace)
		replaced, _, _ := c.cache.applyProto(s)
		if replaced {
			c.logger.Debug("tipsyabconfig: subscribe applied snapshot",
				"ns", s.Namespace,
				"business_seq", s.BusinessSnapshotSeq,
				"experiment_seq", s.ExperimentSnapshotSeq,
			)
		}
	case *configv1.ConfigUpdateEvent_Heartbeat:
		// Liveness only — no cache work, no seq advance, not counted as a
		// config event. Keeps the stream non-idle for edge proxies (e.g.
		// Cloudflare).
	default:
		// Unknown payload — silently skip.
	}
}
