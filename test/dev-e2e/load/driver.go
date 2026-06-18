package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptrace"
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

// phaseSample captures per-request timings from net/http/httptrace.ClientTrace
// so we can decompose "where the wall-clock went" into:
//
//	dns   : DNS lookup (0 when not performed — e.g. cached / IP)
//	tcp   : TCP connect (0 when conn was reused)
//	tls   : TLS handshake (0 when conn was reused)
//	wait  : request fully written → first response byte
//	         (= server think time + return-leg network)
//	read  : first byte → body fully drained
//
// reused indicates whether the underlying TCP+TLS connection was reused via
// keep-alive (i.e. the request paid no dns/tcp/tls cost). Tracking that ratio
// separately is critical: on a Cloudflare-fronted endpoint, the first request
// per worker pays one expensive TLS handshake, then every subsequent request
// on that conn pays only `wait+read`.
type phaseSample struct {
	total  time.Duration
	dns    time.Duration
	tcp    time.Duration
	tls    time.Duration
	wait   time.Duration
	read   time.Duration
	reused bool
}

// do issues one request and returns (httpStatus, sample, err). A transport
// error returns status 0.
func (d *driver) do(ctx context.Context) (int, phaseSample, error) {
	body := d.buildBody()

	var (
		sample        phaseSample
		dnsStart      time.Time
		connStart     time.Time
		tlsStart      time.Time
		wroteReq      time.Time
		firstByteSeen time.Time
	)

	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone: func(httptrace.DNSDoneInfo) {
			if !dnsStart.IsZero() {
				sample.dns = time.Since(dnsStart)
			}
		},
		ConnectStart: func(network, addr string) { connStart = time.Now() },
		ConnectDone: func(network, addr string, err error) {
			if !connStart.IsZero() {
				sample.tcp = time.Since(connStart)
			}
		},
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			if !tlsStart.IsZero() {
				sample.tls = time.Since(tlsStart)
			}
		},
		GotConn: func(info httptrace.GotConnInfo) {
			sample.reused = info.Reused
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			wroteReq = time.Now()
		},
		GotFirstResponseByte: func() {
			firstByteSeen = time.Now()
			if !wroteReq.IsZero() {
				sample.wait = firstByteSeen.Sub(wroteReq)
			}
		},
	}

	start := time.Now()
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		sample.total = time.Since(start)
		return 0, sample, err
	}
	req.Header.Set("Authorization", "Bearer "+d.cfg.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.cli.Do(req)
	if err != nil {
		sample.total = time.Since(start)
		return 0, sample, err
	}
	// Drain + close so the connection is reused (keep-alive).
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if !firstByteSeen.IsZero() {
		sample.read = time.Since(firstByteSeen)
	}
	sample.total = time.Since(start)
	return resp.StatusCode, sample, nil
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
	reusedCnt  atomic.Int64
	freshCnt   atomic.Int64

	mu        sync.Mutex
	samples   []phaseSample    // all per-request samples (volumes are bounded for a medium run)
	byStatus  map[int]int64    // http status → count (0 = transport error)
	errSample map[string]int64 // error string → count (capped sample)
}

func newCollector() *collector {
	return &collector{
		byStatus:  map[int]int64{},
		errSample: map[string]int64{},
	}
}

func (c *collector) record(status int, s phaseSample, err error) {
	c.totalCount.Add(1)
	if s.reused {
		c.reusedCnt.Add(1)
	} else {
		c.freshCnt.Add(1)
	}
	c.mu.Lock()
	c.samples = append(c.samples, s)
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
