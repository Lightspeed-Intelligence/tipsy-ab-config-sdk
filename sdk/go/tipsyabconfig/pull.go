package tipsyabconfig

import (
	"context"
	"errors"
	"fmt"
	"time"

	configv1 "github.com/Lightspeed-Intelligence/tipsy-ab-config-sdk/api/gen/go/tipsy/config/v1"
	"github.com/google/uuid"
)

// startupPullAll performs the synchronous PullAll-per-ns sweep described in
// §3 / §5.1. It returns nil iff at least one namespace was successfully
// hydrated; partial failures are tolerated only when at least one ns
// succeeded. The strict policy is: every ns must succeed within retries,
// otherwise the function returns an aggregate error and the caller decides
// fail-close vs fail-open via Config.StartupFailOpen.
func (c *Client) startupPullAll(ctx context.Context) error {
	var firstErr error
	failed := make([]string, 0)
	for _, ns := range c.subscribedNamespaces {
		if err := c.pullOnceWithRetries(ctx, ns); err != nil {
			c.metrics.pullFailure.inc(ns)
			c.logger.Error("tipsyabconfig: startup PullAll failed for namespace", "ns", ns, "err", err)
			failed = append(failed, ns)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("startup PullAll failed for %v: %w", failed, firstErr)
	}
	return nil
}

func (c *Client) pullOnceWithRetries(ctx context.Context, ns string) error {
	backoff := 200 * time.Millisecond
	var lastErr error
	for attempt := 0; attempt < c.cfg.PullRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
		}
		err := c.pullOnce(ctx, ns)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

// pullOnce sends a single PullAll for the given namespace, applies the
// response to the cache, and returns any RPC error. It does NOT inject the
// known_seqs because the server may legitimately decide to omit an
// up-to-date ns from the response. The runtime path always passes the cached
// known_seqs through pullOnceWithKnown.
//
// Each call generates a fresh trace_id and emits a Debug log before the RPC
// so operators can grep both the SDK and the server log for the same id
// (sdk-trace-id §3 / §4). Background PullAll therefore appears under a NEW
// trace per tick — this is per design: trace_id is request-scoped, not
// SDK-instance-scoped.
func (c *Client) pullOnce(ctx context.Context, ns string) error {
	rpcCtx, cancel := context.WithTimeout(ctx, c.cfg.PullTimeout)
	defer cancel()
	traceID := uuid.New().String()
	c.logger.Debug("tipsyabconfig: PullAll", "ns", ns, "trace_id", traceID)
	resp, err := c.configTr.PullAll(rpcCtx, &configv1.PullAllRequest{
		Namespaces: []string{ns},
		TraceId:    traceID,
	})
	if err != nil {
		return err
	}
	c.applySnapshots(resp.GetSnapshots())
	return nil
}

// runPullLoop is the 10 s safety-net PullAll loop. It exits when the SDK
// root context is cancelled.
func (c *Client) runPullLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.cfg.PullInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.rootCtx.Done():
			return
		case <-ticker.C:
			for _, ns := range c.subscribedNamespaces {
				if err := c.rootCtx.Err(); err != nil {
					return
				}
				if err := c.pullOnce(c.rootCtx, ns); err != nil {
					if errors.Is(err, context.Canceled) {
						return
					}
					c.metrics.pullFailure.inc(ns)
					c.logger.Error("tipsyabconfig: periodic PullAll failed", "ns", ns, "err", err)
					c.fireBackgroundError(BackgroundErrorEvent{
						Phase:     "periodic_pull",
						Namespace: ns,
						Err:       err,
						Time:      time.Now(),
					})
				}
			}
		}
	}
}

func (c *Client) applySnapshots(snaps []*configv1.NamespaceSnapshot) {
	for _, s := range snaps {
		if s == nil {
			continue
		}
		replaced, _, _ := c.cache.applyProto(s)
		if !replaced {
			continue
		}
		c.logger.Debug("tipsyabconfig: cache replaced",
			"ns", s.Namespace,
			"business_seq", s.BusinessSnapshotSeq,
			"experiment_seq", s.ExperimentSnapshotSeq,
		)
	}
}
