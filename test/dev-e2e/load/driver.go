package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// driver issues one load request to the configured endpoint with a random user
// id so traffic spreads across experiment buckets.
type driver struct {
	cfg config
	cli *http.Client
	url string
	// reqTemplate fields are constant per run; only userId varies.
	rng   *rand.Rand
	rngMu sync.Mutex
}

func newDriver(cfg config) *driver {
	var path string
	switch cfg.endpoint {
	case "dynamic":
		path = "/api/v1/config/dynamic"
	default:
		path = "/api/v1/abtest/experiment_result"
	}
	return &driver{
		cfg: cfg,
		cli: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        cfg.concurrency * 2,
				MaxIdleConnsPerHost: cfg.concurrency * 2,
				MaxConnsPerHost:     cfg.concurrency * 2,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		url: cfg.base + path,
		rng: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (d *driver) randUser() string {
	d.rngMu.Lock()
	n := d.rng.Int63()
	d.rngMu.Unlock()
	return "load-u" + strconv.FormatInt(n, 36)
}

// do issues one request and returns (httpStatus, latency, err). A transport
// error returns status 0.
func (d *driver) do(ctx context.Context) (int, time.Duration, error) {
	body := d.buildBody()
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return 0, time.Since(start), err
	}
	req.Header.Set("Authorization", "Bearer "+d.cfg.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.cli.Do(req)
	if err != nil {
		return 0, time.Since(start), err
	}
	// Drain + close so the connection is reused (keep-alive).
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, time.Since(start), nil
}

func (d *driver) buildBody() []byte {
	user := d.randUser()
	if d.cfg.endpoint == "dynamic" {
		b, _ := json.Marshal(map[string]any{
			"namespace": d.cfg.ns,
			"userId":    user,
			"keys":      []string{},
		})
		return b
	}
	b, _ := json.Marshal(map[string]any{
		"namespace":      d.cfg.ns,
		"userId":         user,
		"experimentType": "EXPERIMENT_TYPE_CONFIG_VERSION",
		"displayType":    "RESULT_DISPLAY_TYPE_FLAT_KV",
	})
	return b
}

// collector accumulates per-request metrics in a concurrency-safe way.
type collector struct {
	totalCount atomic.Int64
	errCount   atomic.Int64

	mu        sync.Mutex
	durations []time.Duration  // all latencies (volumes are bounded for a medium run)
	byStatus  map[int]int64    // http status → count (0 = transport error)
	errSample map[string]int64 // error string → count (capped sample)
}

func newCollector() *collector {
	return &collector{
		byStatus:  map[int]int64{},
		errSample: map[string]int64{},
	}
}

func (c *collector) record(status int, dur time.Duration, err error) {
	c.totalCount.Add(1)
	c.mu.Lock()
	c.durations = append(c.durations, dur)
	c.byStatus[status]++
	if err != nil {
		c.errCount.Add(1)
		key := err.Error()
		if len(key) > 120 {
			key = key[:120]
		}
		// Cap the distinct-error sample to bound memory; keep counting errors we
		// already know about.
		if len(c.errSample) < 50 || c.errSample[key] > 0 {
			c.errSample[key]++
		}
	} else if status >= 400 {
		// HTTP-level error (no transport error). Record it under its status code.
		c.errCount.Add(1)
	}
	c.mu.Unlock()
}

func (c *collector) total() int64  { return c.totalCount.Load() }
func (c *collector) errors() int64 { return c.errCount.Load() }
