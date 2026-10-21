// Command loadtest drives concurrent chat requests and reports latency
// percentiles. It exists so performance claims are backed by a reproducible
// command, not by a screenshot.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	latency time.Duration
	status  int
	err     error
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8080/v1/chat/completions", "endpoint")
	key := flag.String("key", "", "bearer token")
	model := flag.String("model", "gpt-4o", "model name")
	total := flag.Int("total", 1000, "total requests")
	concurrency := flag.Int("concurrency", 100, "parallel workers")
	timeout := flag.Duration("timeout", 30*time.Second, "per-request timeout")
	flag.Parse()

	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			MaxIdleConns:        *concurrency,
			MaxIdleConnsPerHost: *concurrency,
			MaxConnsPerHost:     *concurrency,
		},
	}

	jobs := make(chan int)
	results := make(chan result, *total)
	var wg sync.WaitGroup
	var ok, failed atomic.Int64

	start := time.Now()
	for w := 0; w < *concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results <- one(client, *url, *key, *model, i, *timeout)
			}
		}()
	}
	go func() {
		for i := 0; i < *total; i++ {
			jobs <- i
		}
		close(jobs)
	}()
	go func() { wg.Wait(); close(results) }()

	latencies := make([]time.Duration, 0, *total)
	statuses := map[int]int{}
	var firstErr string
	for r := range results {
		if r.err != nil {
			failed.Add(1)
			if firstErr == "" {
				firstErr = r.err.Error()
			}
			continue
		}
		statuses[r.status]++
		if r.status == http.StatusOK {
			ok.Add(1)
			latencies = append(latencies, r.latency)
		} else {
			failed.Add(1)
		}
	}
	elapsed := time.Since(start)

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	pct := func(p float64) time.Duration {
		if len(latencies) == 0 {
			return 0
		}
		idx := int(float64(len(latencies)-1) * p)
		if idx < 0 {
			idx = 0
		}
		return latencies[idx]
	}

	out := map[string]any{
		"total":         *total,
		"concurrency":   *concurrency,
		"ok":            ok.Load(),
		"failed":        failed.Load(),
		"status_counts": statuses,
		"elapsed_s":     elapsed.Seconds(),
		"rps":           float64(*total) / elapsed.Seconds(),
		"p50_ms":        pct(0.50).Milliseconds(),
		"p95_ms":        pct(0.95).Milliseconds(),
		"p99_ms":        pct(0.99).Milliseconds(),
		"max_ms":        pct(1.0).Milliseconds(),
		"goroutines":    runtime.NumGoroutine(),
		"first_error":   firstErr,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)

	if failed.Load() > 0 {
		os.Exit(1)
	}
}

func one(client *http.Client, url, key, modelName string, i int, timeout time.Duration) result {
	payload, _ := json.Marshal(map[string]any{
		"model": modelName,
		"messages": []map[string]string{{
			"role": "user", "content": fmt.Sprintf("load test request %d", i),
		}},
		"max_tokens": 16,
	})
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return result{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return result{err: err}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return result{latency: time.Since(start), status: resp.StatusCode}
}
