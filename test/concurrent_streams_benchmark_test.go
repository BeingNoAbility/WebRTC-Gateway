package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"webrtc-gateway/pkg/proxy"
)

// StreamBenchmarkResult aggregates empirical performance metrics for concurrent streams.
type StreamBenchmarkResult struct {
	Concurrency       int
	TotalRequests     int
	SuccessCount      int
	FailedCount       int
	Duration          time.Duration
	ThroughputQPS     float64
	MinLatency        time.Duration
	AvgLatency        time.Duration
	P50Latency        time.Duration
	P90Latency        time.Duration
	P95Latency        time.Duration
	P99Latency        time.Duration
	MaxLatency        time.Duration
	MemAllocMBBefore  float64
	MemAllocMBPeak    float64
	MemAllocMBAfter   float64
	ActiveStreamsLeak int
}

func runConcurrentStreamsBenchmark(t *testing.T, concurrency int) StreamBenchmarkResult {
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok",
			"stream": req.Header.Get("X-Stream-Index"),
			"ts":     time.Now().UnixNano(),
		})
	})

	backend, signalServer, ag, clientMux, cleanup := setupE2EEnvironment(t, handler)
	defer cleanup()
	_ = backend
	_ = signalServer

	// Allow enough concurrent streams on the agent side
	ag.SetMaxConcurrentStreams(concurrency + 500)

	var memStatsBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memStatsBefore)

	latencies := make([]time.Duration, concurrency)
	var successCount int64
	var failedCount int64
	var firstErr error
	var errMu sync.Mutex
	recordErr := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
		}
		errMu.Unlock()
	}

	start := time.Now()
	var wg sync.WaitGroup

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			t0 := time.Now()
			stream, err := clientMux.OpenStream()
			if err != nil {
				atomic.AddInt64(&failedCount, 1)
				recordErr(fmt.Errorf("OpenStream error: %w", err))
				return
			}
			defer stream.Close()

			reqMeta := proxy.RequestMetadata{
				Method: "GET",
				Path:   "/api/status",
				Headers: map[string]string{
					"X-Stream-Index": fmt.Sprintf("%d", idx),
				},
			}
			metaBytes, _ := json.Marshal(reqMeta)
			if err := stream.WriteHeaders(metaBytes); err != nil {
				atomic.AddInt64(&failedCount, 1)
				recordErr(fmt.Errorf("WriteHeaders error: %w", err))
				return
			}
			_ = stream.Close() // send FlagFin for request body

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, err = stream.ReadHeaders(ctx)
			if err != nil {
				atomic.AddInt64(&failedCount, 1)
				recordErr(fmt.Errorf("stream %d ReadHeaders error: %w, ctx.Err=%v", stream.ID(), err, stream.Context().Err()))
				return
			}

			var respBuf bytes.Buffer
			buf := make([]byte, 1024)
			for {
				n, err := stream.Read(buf)
				if n > 0 {
					respBuf.Write(buf[:n])
				}
				if err != nil {
					break
				}
			}

			lat := time.Since(t0)
			latencies[idx] = lat

			if bytes.Contains(respBuf.Bytes(), []byte(`"status":"ok"`)) {
				atomic.AddInt64(&successCount, 1)
			} else {
				atomic.AddInt64(&failedCount, 1)
				recordErr(fmt.Errorf("unexpected body: %s", respBuf.String()))
			}
		}(i)
	}

	wg.Wait()
	duration := time.Since(start)

	var memStatsPeak runtime.MemStats
	runtime.ReadMemStats(&memStatsPeak)

	// Clean garbage to measure steady-state memory
	runtime.GC()
	var memStatsAfter runtime.MemStats
	runtime.ReadMemStats(&memStatsAfter)

	succ := int(atomic.LoadInt64(&successCount))
	fail := int(atomic.LoadInt64(&failedCount))

	// Filter valid latencies
	validLats := make([]time.Duration, 0, succ)
	var totalLat time.Duration
	for _, l := range latencies {
		if l > 0 {
			validLats = append(validLats, l)
			totalLat += l
		}
	}
	sort.Slice(validLats, func(i, j int) bool { return validLats[i] < validLats[j] })

	res := StreamBenchmarkResult{
		Concurrency:       concurrency,
		TotalRequests:     concurrency,
		SuccessCount:      succ,
		FailedCount:       fail,
		Duration:          duration,
		ThroughputQPS:     float64(succ) / duration.Seconds(),
		MemAllocMBBefore:  float64(memStatsBefore.Alloc) / (1024 * 1024),
		MemAllocMBPeak:    float64(memStatsPeak.Alloc) / (1024 * 1024),
		MemAllocMBAfter:   float64(memStatsAfter.Alloc) / (1024 * 1024),
		ActiveStreamsLeak: int(clientMux.InboundBytes()), // Must be 0
	}

	if len(validLats) > 0 {
		res.MinLatency = validLats[0]
		res.MaxLatency = validLats[len(validLats)-1]
		res.AvgLatency = totalLat / time.Duration(len(validLats))
		res.P50Latency = validLats[int(math.Floor(float64(len(validLats))*0.50))]
		res.P90Latency = validLats[int(math.Floor(float64(len(validLats))*0.90))]
		res.P95Latency = validLats[int(math.Floor(float64(len(validLats))*0.95))]
		res.P99Latency = validLats[int(math.Floor(float64(len(validLats))*0.99))]
	}

	t.Logf("=================================================================")
	t.Logf("📊 Benchmark [%d Concurrent Streams]: %d Success, %d Failed in %v", concurrency, succ, fail, duration)
	t.Logf("   Throughput: %.2f QPS (streams/sec)", res.ThroughputQPS)
	t.Logf("   Latencies: Min=%.2fms, Avg=%.2fms, P50=%.2fms, P90=%.2fms, P95=%.2fms, P99=%.2fms, Max=%.2fms",
		float64(res.MinLatency.Microseconds())/1000.0,
		float64(res.AvgLatency.Microseconds())/1000.0,
		float64(res.P50Latency.Microseconds())/1000.0,
		float64(res.P90Latency.Microseconds())/1000.0,
		float64(res.P95Latency.Microseconds())/1000.0,
		float64(res.P99Latency.Microseconds())/1000.0,
		float64(res.MaxLatency.Microseconds())/1000.0,
	)
	t.Logf("   Memory Alloc: Before=%.2fMB, Peak=%.2fMB, After=%.2fMB, InboundLeak=%d bytes",
		res.MemAllocMBBefore, res.MemAllocMBPeak, res.MemAllocMBAfter, res.ActiveStreamsLeak)
	t.Logf("=================================================================")

	if fail > 0 {
		t.Fatalf("[%d Concurrency] %d streams failed out of %d: %v", concurrency, fail, concurrency, firstErr)
	}
	if res.ActiveStreamsLeak > 0 {
		t.Fatalf("[%d Concurrency] Inbound buffer leak detected: %d bytes unread", concurrency, res.ActiveStreamsLeak)
	}

	return res
}

func TestBenchmark_ConcurrentStreams_100(t *testing.T) {
	runConcurrentStreamsBenchmark(t, 100)
}

func TestBenchmark_ConcurrentStreams_500(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 500-stream benchmark in short mode")
	}
	runConcurrentStreamsBenchmark(t, 500)
}

func TestBenchmark_ConcurrentStreams_1000(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 1000-stream benchmark in short mode")
	}
	runConcurrentStreamsBenchmark(t, 1000)
}

func TestBenchmark_ConcurrentStreams_Matrix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping stream benchmark matrix in short mode")
	}
	c100 := runConcurrentStreamsBenchmark(t, 100)
	c500 := runConcurrentStreamsBenchmark(t, 500)
	c1000 := runConcurrentStreamsBenchmark(t, 1000)

	t.Logf("\n")
	t.Logf("| Concurrency | Success Rate | Duration | Throughput (QPS) | Avg Latency | P50 Latency | P95 Latency | P99 Latency | Max Latency | Peak Memory |")
	t.Logf("|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|:---:|")
	for _, r := range []StreamBenchmarkResult{c100, c500, c1000} {
		t.Logf("| %d | %d/%d (%.1f%%) | %v | %.1f streams/s | %.2fms | %.2fms | %.2fms | %.2fms | %.2fms | %.2f MB |",
			r.Concurrency,
			r.SuccessCount, r.TotalRequests,
			float64(r.SuccessCount)/float64(r.TotalRequests)*100.0,
			r.Duration.Round(time.Millisecond),
			r.ThroughputQPS,
			float64(r.AvgLatency.Microseconds())/1000.0,
			float64(r.P50Latency.Microseconds())/1000.0,
			float64(r.P95Latency.Microseconds())/1000.0,
			float64(r.P99Latency.Microseconds())/1000.0,
			float64(r.MaxLatency.Microseconds())/1000.0,
			r.MemAllocMBPeak,
		)
	}
}
