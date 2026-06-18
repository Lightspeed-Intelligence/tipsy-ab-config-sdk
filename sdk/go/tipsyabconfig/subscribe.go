package tipsyabconfig

import (
	"context"
	"errors"
	"io"
	"time"

	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
	"github.com/google/uuid"
)

// runSubscribe maintains a long-lived ConfigService.Subscribe stream with
// exponential backoff (1 s → 2 s → 4 s → … capped at 30 s) per design §5.2.
// It exits when the SDK root context is cancelled.
func (c *Client) runSubscribe() {
	defer c.wg.Done()
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if err := c.rootCtx.Err(); err != nil {
			return
		}
		err := c.subscribeOnce(c.rootCtx)
		if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
			// Server closed cleanly; reconnect immediately on EOF, exit
			// on Canceled.
			if errors.Is(err, context.Canceled) {
				return
			}
			backoff = time.Second
			continue
		}
		// Real error path: bump disconnect metric and back off.
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
	default:
		// Unknown payload — silently skip.
	}
}
