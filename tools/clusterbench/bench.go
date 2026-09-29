package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Hypothesis targets from the cluster-mode PRD.
const (
	minThroughputRatio = 2.5
	maxP99Overhead     = 3 * time.Millisecond
	maxErrorRate       = 0.001
	recommendedCPUs    = 8
)

type throughputResult struct {
	rps       float64
	requests  int64
	errors    int64
	errorRate float64
}

type latencyResult struct {
	p50, p99 time.Duration
	samples  int
	errors   int
}

func runBench(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("bench", flag.ContinueOnError)
	single := flags.String("single", "http://gpt-load-single:3001", "single-instance baseline base URL")
	instances := flags.String("instances", "http://gpt-load-1:3001,http://gpt-load-2:3001,http://gpt-load-3:3001", "cluster instance base URLs")
	lb := flags.String("lb", "http://nginx", "cluster load balancer base URL")
	upstream := flags.String("upstream", "http://fake-upstream:8080", "fake upstream base URL for the openai channel (the channel appends /v1)")
	concurrency := flags.Int("concurrency", 512, "closed-loop workers for the throughput test")
	duration := flags.Duration("duration", 60*time.Second, "measured throughput duration per target")
	warmup := flags.Duration("warmup", 10*time.Second, "unmeasured throughput warm-up per target")
	rate := flags.Int("rate", 200, "open-loop request rate for the latency test")
	rateDuration := flags.Duration("rate-duration", 60*time.Second, "latency test duration per target")
	if err := flags.Parse(args); err != nil {
		return err
	}
	authKey := os.Getenv("AUTH_KEY")
	cluster := splitList(*instances)
	if authKey == "" || len(cluster) == 0 {
		return errors.New("bench needs AUTH_KEY and -instances")
	}
	singleURL, lbURL := strings.TrimRight(*single, "/"), strings.TrimRight(*lb, "/")

	cpus := runtime.NumCPU()
	fmt.Printf("host CPUs: %d, GATEWAY_CPUS: %s\n", cpus, os.Getenv("GATEWAY_CPUS"))
	if cpus < recommendedCPUs {
		fmt.Printf("WARNING: fewer than %d CPUs; gateways, Redis, PostgreSQL and the load generator share the host, so the throughput ratio understates scaling\n", recommendedCPUs)
	}

	suffix := fmt.Sprintf("bench-%d", time.Now().UnixNano())
	adminHTTP := &http.Client{Timeout: 30 * time.Second}
	singleSet, err := seed(ctx, adminClient{baseURL: singleURL, authKey: authKey, http: adminHTTP}, []string{singleURL}, *upstream, suffix)
	if err != nil {
		return fmt.Errorf("seed single instance: %w", err)
	}
	clusterSet, err := seed(ctx, adminClient{baseURL: cluster[0], authKey: authKey, http: adminHTTP}, append([]string{lbURL}, cluster...), *upstream, suffix)
	if err != nil {
		return fmt.Errorf("seed cluster: %w", err)
	}
	client := newDataPlaneClient()

	fmt.Println("measuring single-instance throughput ...")
	singleThroughput := closedLoop(ctx, client, singleURL, singleSet.bench.Secret, *concurrency, *warmup, *duration)
	fmt.Println("measuring cluster throughput through the load balancer ...")
	clusterThroughput := closedLoop(ctx, client, lbURL, clusterSet.bench.Secret, *concurrency, *warmup, *duration)
	fmt.Println("measuring single-instance latency ...")
	singleLatency := openLoop(ctx, client, singleURL, singleSet.bench.Secret, *rate, *rateDuration)
	fmt.Println("measuring cluster-instance latency ...")
	clusterLatency := openLoop(ctx, client, cluster[0], clusterSet.bench.Secret, *rate, *rateDuration)

	checks := benchChecks(singleThroughput, clusterThroughput, singleLatency, clusterLatency)
	printChecks(os.Stdout, checks)
	if !allPass(checks) {
		return errors.New("cluster benchmark missed a target")
	}
	return nil
}

func benchChecks(singleThroughput, clusterThroughput throughputResult, singleLatency, clusterLatency latencyResult) []check {
	ratio := 0.0
	if singleThroughput.rps > 0 {
		ratio = clusterThroughput.rps / singleThroughput.rps
	}
	overhead := clusterLatency.p99 - singleLatency.p99
	return []check{
		{
			Name:     "throughput-ratio",
			Target:   fmt.Sprintf("≥ %.1f×", minThroughputRatio),
			Measured: fmt.Sprintf("%.2f× (single %.0f rps, cluster %.0f rps)", ratio, singleThroughput.rps, clusterThroughput.rps),
			Pass:     ratio >= minThroughputRatio,
		},
		{
			Name:     "throughput-errors",
			Target:   fmt.Sprintf("≤ %.1f%%", maxErrorRate*100),
			Measured: fmt.Sprintf("single %.3f%%, cluster %.3f%%", singleThroughput.errorRate*100, clusterThroughput.errorRate*100),
			Pass:     singleThroughput.errorRate <= maxErrorRate && clusterThroughput.errorRate <= maxErrorRate,
		},
		{
			Name:   "p99-overhead",
			Target: "≤ " + maxP99Overhead.String(),
			Measured: fmt.Sprintf("%s (single p50 %s p99 %s, cluster p50 %s p99 %s)",
				overhead.Round(10*time.Microsecond),
				singleLatency.p50.Round(10*time.Microsecond), singleLatency.p99.Round(10*time.Microsecond),
				clusterLatency.p50.Round(10*time.Microsecond), clusterLatency.p99.Round(10*time.Microsecond)),
			Pass: overhead <= maxP99Overhead && singleLatency.errors == 0 && clusterLatency.errors == 0,
		},
	}
}

// closedLoop keeps workers busy for warmup+duration and counts only the
// completions after warm-up.
func closedLoop(ctx context.Context, client *http.Client, target, secret string, workers int, warmup, duration time.Duration) throughputResult {
	measureFrom := time.Now().Add(warmup)
	deadline := measureFrom.Add(duration)
	var requests, failures atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for ctx.Err() == nil {
				status, err := chat(ctx, client, target, secret, benchModel)
				now := time.Now()
				if now.After(deadline) {
					return
				}
				if now.Before(measureFrom) {
					continue
				}
				requests.Add(1)
				if err != nil || status != http.StatusOK {
					failures.Add(1)
				}
			}
		})
	}
	wg.Wait()
	result := throughputResult{requests: requests.Load(), errors: failures.Load()}
	result.rps = float64(result.requests) / duration.Seconds()
	if result.requests > 0 {
		result.errorRate = float64(result.errors) / float64(result.requests)
	}
	return result
}

// openLoop issues requests at a fixed rate regardless of response time, so
// slow responses cannot hide latency by throttling the generator.
func openLoop(ctx context.Context, client *http.Client, target, secret string, rate int, duration time.Duration) latencyResult {
	interval := time.Second / time.Duration(rate)
	total := int(duration / interval)
	var mu sync.Mutex
	latencies := make([]time.Duration, 0, total)
	failures := 0
	var wg sync.WaitGroup
	started := time.Now()
	for i := range total {
		if wait := time.Until(started.Add(time.Duration(i) * interval)); wait > 0 {
			select {
			case <-ctx.Done():
				wg.Wait()
				return summarizeLatency(latencies, failures)
			case <-time.After(wait):
			}
		}
		wg.Go(func() {
			begin := time.Now()
			status, err := chat(ctx, client, target, secret, benchModel)
			elapsed := time.Since(begin)
			mu.Lock()
			defer mu.Unlock()
			if err != nil || status != http.StatusOK {
				failures++
				return
			}
			latencies = append(latencies, elapsed)
		})
	}
	wg.Wait()
	return summarizeLatency(latencies, failures)
}

func summarizeLatency(latencies []time.Duration, failures int) latencyResult {
	sorted := slices.Clone(latencies)
	slices.Sort(sorted)
	return latencyResult{p50: percentile(sorted, 0.50), p99: percentile(sorted, 0.99), samples: len(sorted), errors: failures}
}

// percentile uses the nearest-rank method on sorted samples.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(p * float64(len(sorted))))
	return sorted[max(rank, 1)-1]
}
