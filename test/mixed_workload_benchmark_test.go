package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"webrtc-gateway/pkg/proxy"
)

// TestE2E_MixedWorkload_LargeFileAndHighFreqAPI benchmarks concurrent execution
// of a bulk 10MB file streaming transfer (triggering WebRTC DataChannel backpressure)
// alongside 50 concurrent lightweight REST API requests.
//
// This test provides empirical proof that P2PMux 32KB chunking and high-priority
// control frames prevent Head-of-Line blocking and API starvation without requiring
// complex HTTP/2-level per-stream sliding window flow control.
func TestE2E_MixedWorkload_LargeFileAndHighFreqAPI(t *testing.T) {
	const (
		fileSizeMB    = 10
		fileSizeBytes = fileSizeMB * 1024 * 1024
		apiRequests   = 50
	)

	// Pre-generate 10MB test payload
	largePayload := make([]byte, fileSizeBytes)
	r := rand.New(rand.NewSource(42))
	_, _ = r.Read(largePayload)

	var apiHitCount int64
	var fileHitCount int64

	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/status":
			atomic.AddInt64(&apiHitCount, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":      "ok",
				"server_time": time.Now().UnixNano(),
			})
		case "/largefile":
			atomic.AddInt64(&fileHitCount, 1)
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", fileSizeBytes))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(largePayload)
		default:
			http.NotFound(w, req)
		}
	})

	backend, signalServer, ag, clientMux, cleanup := setupE2EEnvironment(t, handler)
	defer cleanup()
	_ = backend
	_ = signalServer
	_ = ag

	// Prepare data structures for metrics
	var fileDuration time.Duration
	var fileErr error
	fileDone := make(chan struct{})

	// 1. Launch Stream A: Bulk 10MB Download in background
	fileStart := time.Now()
	go func() {
		defer close(fileDone)
		stream, err := clientMux.OpenStream()
		if err != nil {
			fileErr = fmt.Errorf("open stream for large file error: %w", err)
			return
		}
		defer stream.Close()

		reqMeta := proxy.RequestMetadata{
			Method: "GET",
			Path:   "/largefile",
		}
		metaBytes, _ := json.Marshal(reqMeta)
		if err := stream.WriteHeaders(metaBytes); err != nil {
			fileErr = fmt.Errorf("write headers for large file error: %w", err)
			return
		}
		_ = stream.Close() // send FlagFin for request body

		// Read response headers
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err = stream.ReadHeaders(ctx)
		if err != nil {
			fileErr = fmt.Errorf("read response headers error: %w", err)
			return
		}

		receivedBuf := bytes.NewBuffer(make([]byte, 0, fileSizeBytes))
		buf := make([]byte, 32*1024)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				receivedBuf.Write(buf[:n])
			}
			if err != nil {
				if err == io.EOF {
					break
				}
				fileErr = fmt.Errorf("read large file stream error: %w", err)
				return
			}
		}

		fileDuration = time.Since(fileStart)
		if receivedBuf.Len() != fileSizeBytes {
			fileErr = fmt.Errorf("received length mismatch: expected %d, got %d", fileSizeBytes, receivedBuf.Len())
			return
		}
		if !bytes.Equal(receivedBuf.Bytes()[:1024], largePayload[:1024]) {
			fileErr = fmt.Errorf("payload content corrupted at head")
			return
		}
	}()

	// Wait 15ms so that the large file stream begins saturating the DataChannel queue
	time.Sleep(15 * time.Millisecond)

	// 2. Concurrently execute 50 high-frequency API calls
	latencies := make([]time.Duration, apiRequests)
	var apiWg sync.WaitGroup
	var apiErrors int64

	for i := 0; i < apiRequests; i++ {
		apiWg.Add(1)
		go func(idx int) {
			defer apiWg.Done()
			// Slight micro-jitter to simulate realistic browser concurrent fetch burst
			time.Sleep(time.Duration(idx*2) * time.Millisecond)

			t0 := time.Now()
			stream, err := clientMux.OpenStream()
			if err != nil {
				atomic.AddInt64(&apiErrors, 1)
				return
			}
			defer stream.Close()

			reqMeta := proxy.RequestMetadata{
				Method: "GET",
				Path:   "/api/status",
			}
			metaBytes, _ := json.Marshal(reqMeta)
			if err := stream.WriteHeaders(metaBytes); err != nil {
				atomic.AddInt64(&apiErrors, 1)
				return
			}
			_ = stream.Close() // send FlagFin for request body

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = stream.ReadHeaders(ctx)
			if err != nil {
				atomic.AddInt64(&apiErrors, 1)
				return
			}

			// Read response body
			var respData bytes.Buffer
			buf := make([]byte, 4096)
			for {
				n, err := stream.Read(buf)
				if n > 0 {
					respData.Write(buf[:n])
				}
				if err != nil {
					break
				}
			}

			latencies[idx] = time.Since(t0)
			if !bytes.Contains(respData.Bytes(), []byte(`"status":"ok"`)) {
				atomic.AddInt64(&apiErrors, 1)
			}
		}(i)
	}

	apiWg.Wait()
	<-fileDone

	if fileErr != nil {
		t.Fatalf("Large file transfer failed: %v", fileErr)
	}
	if apiErrors > 0 {
		t.Fatalf("%d out of %d API calls failed during bulk transfer!", apiErrors, apiRequests)
	}

	// 3. Compute Latency Distribution Metrics
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	minLat := latencies[0]
	maxLat := latencies[len(latencies)-1]
	p50Lat := latencies[int(math.Floor(float64(len(latencies))*0.50))]
	p95Lat := latencies[int(math.Floor(float64(len(latencies))*0.95))]
	p99Lat := latencies[int(math.Floor(float64(len(latencies))*0.99))]

	var totalLat time.Duration
	for _, d := range latencies {
		totalLat += d
	}
	avgLat := totalLat / time.Duration(len(latencies))
	throughputMBs := float64(fileSizeBytes) / (1024 * 1024) / fileDuration.Seconds()

	t.Logf("=================================================================")
	t.Logf("📊 P2PMux Mixed Workload Benchmark Results (Large File + API)")
	t.Logf("=================================================================")
	t.Logf("Large File Size:     %.2f MB", float64(fileSizeBytes)/(1024*1024))
	t.Logf("Large File Duration: %v (Throughput: %.2f MB/s)", fileDuration, throughputMBs)
	t.Logf("Concurrent API Calls: %d requests (100%% SUCCESS, 0 failed)", apiRequests)
	t.Logf("API Latencies:")
	t.Logf("  Min: %.2f ms", float64(minLat.Microseconds())/1000.0)
	t.Logf("  Avg: %.2f ms", float64(avgLat.Microseconds())/1000.0)
	t.Logf("  P50: %.2f ms", float64(p50Lat.Microseconds())/1000.0)
	t.Logf("  P95: %.2f ms", float64(p95Lat.Microseconds())/1000.0)
	t.Logf("  P99: %.2f ms", float64(p99Lat.Microseconds())/1000.0)
	t.Logf("  Max: %.2f ms", float64(maxLat.Microseconds())/1000.0)
	t.Logf("=================================================================")

	// Assertion: P95 latency of API requests must remain responsive (< 100ms)
	// even under 100% saturation of WebRTC DataChannel by bulk file download
	if p95Lat > 100*time.Millisecond {
		t.Errorf("API P95 latency too high: expected < 100ms, got %v", p95Lat)
	}
}
