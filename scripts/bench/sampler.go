package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// metricSample holds a single point-in-time snapshot of VoidLLM process metrics.
type metricSample struct {
	T             int     `json:"t"`              // seconds since sampler start
	RSSMB         float64 `json:"rss_mb"`         // resident set size in MB
	HeapMB        float64 `json:"heap_mb"`        // heap allocated bytes in MB
	Goroutines    int     `json:"goroutines"`     // live goroutine count
	RPS           float64 `json:"rps"`            // upstream requests per second (delta from previous sample)
	ActiveStreams int     `json:"active_streams"` // current in-flight streaming responses
}

// sampler polls a Prometheus /metrics endpoint at a fixed interval and
// accumulates metricSample values. Call Stop to halt polling and retrieve
// the collected data.
type sampler struct {
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	samples []metricSample
}

// startSampler begins polling the metrics endpoint at the given interval.
// The returned *sampler is already running; call Stop when the benchmark
// phase ends to halt it and collect the data.
func startSampler(metricsURL string, interval time.Duration) *sampler {
	ctx, cancel := context.WithCancel(context.Background())
	s := &sampler{
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go s.run(ctx, metricsURL, interval)
	return s
}

// Stop halts the background polling goroutine and returns all collected samples.
// It is safe to call Stop more than once; subsequent calls are no-ops.
func (s *sampler) Stop() []metricSample {
	s.cancel()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]metricSample, len(s.samples))
	copy(out, s.samples)
	return out
}

// run is the background goroutine. It ticks at interval, fetches /metrics,
// parses the Prometheus text exposition format, and appends a metricSample.
func (s *sampler) run(ctx context.Context, metricsURL string, interval time.Duration) {
	defer close(s.done)

	timeout := interval - (interval / 4)
	if timeout < 100*time.Millisecond {
		timeout = 100 * time.Millisecond
	}
	client := &http.Client{Timeout: timeout}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	start := time.Now()
	var prevRequests float64
	var prevAt time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			raw, err := fetchMetrics(ctx, client, metricsURL)
			if err != nil {
				// Skip this sample; polling continues on next tick.
				continue
			}

			parsed := parsePrometheusText(raw)

			rss := parsed["process_resident_memory_bytes"] / (1024 * 1024)
			heap := parsed["go_memstats_heap_alloc_bytes"] / (1024 * 1024)
			goroutines := int(parsed["go_goroutines"])
			activeStreams := int(parsed["voidllm_active_streams"])
			totalRequests := parsed["voidllm_upstream_requests_total"]

			var rps float64
			if !prevAt.IsZero() {
				elapsed := now.Sub(prevAt).Seconds()
				if elapsed > 0 {
					rps = (totalRequests - prevRequests) / elapsed
					if rps < 0 {
						rps = 0
					}
				}
			}
			prevRequests = totalRequests
			prevAt = now

			sample := metricSample{
				T:             int(now.Sub(start).Seconds()),
				RSSMB:         rss,
				HeapMB:        heap,
				Goroutines:    goroutines,
				RPS:           rps,
				ActiveStreams: activeStreams,
			}

			s.mu.Lock()
			s.samples = append(s.samples, sample)
			s.mu.Unlock()
		}
	}
}

// fetchMetrics performs a single GET request to the metrics endpoint and
// returns the raw response body. The caller is responsible for any parsing.
func fetchMetrics(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// parsePrometheusText parses the Prometheus text exposition format and
// returns a map of metric name to value. Counter metrics with labels (e.g.
// voidllm_upstream_requests_total{...}) are accumulated: all label variants
// for the same base name are summed into a single entry.
//
// Lines beginning with '#' are skipped. Each non-comment line is expected to
// have the form:
//
//	metric_name[{labels}] value [timestamp]
func parsePrometheusText(body string) map[string]float64 {
	result := make(map[string]float64)
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Split into at most 3 fields: name+labels, value, optional timestamp.
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}

		nameWithLabels := parts[0]
		valueStr := parts[1]

		// Strip label block to get the base metric name.
		baseName := nameWithLabels
		if idx := strings.IndexByte(nameWithLabels, '{'); idx != -1 {
			baseName = nameWithLabels[:idx]
		}

		val, err := strconv.ParseFloat(valueStr, 64)
		if err != nil {
			continue
		}

		// Sum all label variants under the same base name.
		result[baseName] += val
	}
	return result
}
