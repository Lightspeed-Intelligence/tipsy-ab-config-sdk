package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Summary is the serialized load-test result.
type Summary struct {
	Endpoint        string           `json:"endpoint"`
	Base            string           `json:"base"`
	Namespace       string           `json:"namespace"`
	Concurrency     int              `json:"concurrency"`
	TargetQPS       int              `json:"target_qps"`
	DurationSeconds float64          `json:"duration_seconds"`
	TotalRequests   int64            `json:"total_requests"`
	ErrorCount      int64            `json:"error_count"`
	ErrorRatePct    float64          `json:"error_rate_pct"`
	QPS             float64          `json:"qps"`
	StatusCounts    map[string]int64 `json:"status_counts"` // "200","500","transport_error" → count
	LatencyMs       LatencyMs        `json:"latency_ms"`
	ErrorSamples    map[string]int64 `json:"error_samples,omitempty"`
	StartedAt       string           `json:"started_at"`
}

// LatencyMs holds latency percentiles in milliseconds.
type LatencyMs struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	Max float64 `json:"max"`
}

func (c *collector) summarize(cfg config, elapsed time.Duration) Summary {
	c.mu.Lock()
	defer c.mu.Unlock()

	total := c.totalCount.Load()
	errCount := c.errCount.Load()

	statusCounts := map[string]int64{}
	for code, n := range c.byStatus {
		key := fmt.Sprintf("%d", code)
		if code == 0 {
			key = "transport_error"
		}
		statusCounts[key] = n
	}

	errSamples := map[string]int64{}
	for k, v := range c.errSample {
		errSamples[k] = v
	}

	secs := elapsed.Seconds()
	qps := 0.0
	if secs > 0 {
		qps = float64(total) / secs
	}
	errRate := 0.0
	if total > 0 {
		errRate = 100 * float64(errCount) / float64(total)
	}

	lat := percentiles(c.durations)

	return Summary{
		Endpoint:        cfg.endpoint,
		Base:            cfg.base,
		Namespace:       cfg.ns,
		Concurrency:     cfg.concurrency,
		TargetQPS:       cfg.targetQPS,
		DurationSeconds: secs,
		TotalRequests:   total,
		ErrorCount:      errCount,
		ErrorRatePct:    errRate,
		QPS:             qps,
		StatusCounts:    statusCounts,
		LatencyMs:       lat,
		ErrorSamples:    errSamples,
		StartedAt:       time.Now().Add(-elapsed).Format(time.RFC3339),
	}
}

// percentiles returns p50/p95/p99/max in milliseconds from a slice of durations.
func percentiles(durs []time.Duration) LatencyMs {
	if len(durs) == 0 {
		return LatencyMs{}
	}
	sorted := make([]time.Duration, len(durs))
	copy(sorted, durs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	pick := func(p float64) float64 {
		idx := int(p * float64(len(sorted)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(sorted) {
			idx = len(sorted) - 1
		}
		return float64(sorted[idx].Microseconds()) / 1000.0
	}
	return LatencyMs{
		P50: pick(0.50),
		P95: pick(0.95),
		P99: pick(0.99),
		Max: float64(sorted[len(sorted)-1].Microseconds()) / 1000.0,
	}
}

func printSummary(s Summary) {
	fmt.Println("----------------------------------------------------------------")
	fmt.Println("LOAD TEST SUMMARY")
	fmt.Printf("  endpoint        : %s (%s)\n", s.Endpoint, s.Namespace)
	fmt.Printf("  duration        : %.1fs\n", s.DurationSeconds)
	fmt.Printf("  total requests  : %d\n", s.TotalRequests)
	fmt.Printf("  achieved QPS    : %.1f\n", s.QPS)
	fmt.Printf("  errors          : %d (%.2f%%)\n", s.ErrorCount, s.ErrorRatePct)
	fmt.Printf("  latency p50     : %.1f ms\n", s.LatencyMs.P50)
	fmt.Printf("  latency p95     : %.1f ms\n", s.LatencyMs.P95)
	fmt.Printf("  latency p99     : %.1f ms\n", s.LatencyMs.P99)
	fmt.Printf("  latency max     : %.1f ms\n", s.LatencyMs.Max)
	fmt.Println("  status codes    :")
	// stable order
	codes := make([]string, 0, len(s.StatusCounts))
	for k := range s.StatusCounts {
		codes = append(codes, k)
	}
	sort.Strings(codes)
	for _, k := range codes {
		fmt.Printf("      %-16s %d\n", k, s.StatusCounts[k])
	}
	if len(s.ErrorSamples) > 0 {
		fmt.Println("  error samples   :")
		for k, v := range s.ErrorSamples {
			fmt.Printf("      [%d] %s\n", v, k)
		}
	}
}

func writeResults(path string, s Summary) error {
	abs := path
	if !filepath.IsAbs(path) {
		abs = resolveRepoPath(path)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	return os.WriteFile(abs, buf, 0o644)
}

// resolveRepoPath turns a repo-relative path into an absolute one by walking up
// to go.mod (so the JSON lands in the right place regardless of cwd).
func resolveRepoPath(rel string) string {
	dir, err := os.Getwd()
	if err != nil {
		return rel
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			if abs, err := filepath.Abs(rel); err == nil {
				return abs
			}
			return rel
		}
		dir = parent
	}
}
