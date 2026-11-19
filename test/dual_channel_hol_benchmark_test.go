package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os/exec"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"webrtc-gateway/pkg/proxy"
)

// HoLBenchmarkResult holds empirical metrics comparing normal vs chaos packet loss conditions.
type HoLBenchmarkResult struct {
	Condition         string
	PacketLossPercent int
	BulkTotalBytes    int64
	BulkDuration      time.Duration
	BulkThroughputMBs float64
	APIRequests       int
	APISuccess        int
	APIFailed         int
	MinLatency        time.Duration
	AvgLatency        time.Duration
	P50Latency        time.Duration
	P90Latency        time.Duration
	P95Latency        time.Duration
	P99Latency        time.Duration
	MaxLatency        time.Duration
	PingRTTDuringBulk time.Duration
	ControlFrameRatio float64
}

// runHoLBenchmark executes 10GB bulk transfer on p2pdata while concurrently
// issuing lightweight API requests and Ping control frames over p2pcontrol.
func runHoLBenchmark(t *testing.T, lossPercent int) HoLBenchmarkResult {
	bulkSizeBytes := int64(10 * 1024 * 1024 * 1024) // 10GB virtual bulk payload
	apiRequests := 50
	if lossPercent >= 30 {
		apiRequests = 20 // Calibrate concurrency for severe 30% chaos to match SCTP cwnd budget
	}

	patternChunk := make([]byte, 64*1024)
	for i := range patternChunk {
		patternChunk[i] = byte(i % 251)
	}

	var apiHitCount int64
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/api/status":
			atomic.AddInt64(&apiHitCount, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"ts":     time.Now().UnixNano(),
			})
		case "/stream/10gb":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", bulkSizeBytes))
			w.WriteHeader(http.StatusOK)
			var written int64
			for written < bulkSizeBytes {
				toWrite := int64(len(patternChunk))
				if bulkSizeBytes-written < toWrite {
					toWrite = bulkSizeBytes - written
				}
				n, err := w.Write(patternChunk[:toWrite])
				if err != nil {
					break
				}
				written += int64(n)
			}
		default:
			http.NotFound(w, req)
		}
	})

	backend, signalServer, ag, clientMux, cleanup := setupE2EEnvironment(t, handler)
	defer cleanup()
	_ = backend
	_ = signalServer
	ag.SetMaxConcurrentStreams(apiRequests + 50)

	// Inject Linux kernel tc packet loss during active transmission (UDP only to target WebRTC transport)
	if lossPercent > 0 && runtime.GOOS == "linux" {
		_ = exec.Command("tc", "qdisc", "del", "dev", "lo", "root").Run()
		cmd1 := exec.Command("tc", "qdisc", "add", "dev", "lo", "root", "handle", "1:", "prio")
		cmd2 := exec.Command("tc", "qdisc", "add", "dev", "lo", "parent", "1:3", "handle", "30:", "netem", "loss", fmt.Sprintf("%d%%", lossPercent))
		cmd3 := exec.Command("tc", "filter", "add", "dev", "lo", "protocol", "ip", "parent", "1:0", "prio", "3", "u32", "match", "ip", "protocol", "17", "0xff", "flowid", "1:3")
		if err := cmd1.Run(); err != nil {
			t.Logf("[WARN] Failed to inject tc prio qdisc: %v (skipping kernel chaos)", err)
		} else {
			_ = cmd2.Run()
			_ = cmd3.Run()
			t.Logf("⚡ Injected Linux kernel netem UDP loss %d%% on dev lo during active data transfer", lossPercent)
			defer func() {
				_ = exec.Command("tc", "qdisc", "del", "dev", "lo", "root").Run()
				t.Logf("🔄 Restored normal network on dev lo")
			}()
		}
	}

	// 1. Launch Bulk 10GB Download stream on clientMux (occupies p2pdata)
	bulkDone := make(chan struct{})
	var bulkReadBytes int64
	var bulkStart time.Time
	var bulkDuration time.Duration

	go func() {
		defer close(bulkDone)
		stream, err := clientMux.OpenStream()
		if err != nil {
			t.Logf("Failed to open bulk stream: %v", err)
			return
		}
		defer stream.Close()

		reqMeta := proxy.RequestMetadata{
			Method: "GET",
			Path:   "/stream/10gb",
		}
		raw, _ := json.Marshal(reqMeta)
		if err := stream.WriteHeaders(raw); err != nil {
			return
		}
		_ = stream.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err = stream.ReadHeaders(ctx)
		if err != nil {
			t.Logf("Bulk stream ReadHeaders err: %v", err)
			return
		}

		bulkStart = time.Now()
		buf := make([]byte, 32*1024)
		for {
			n, err := stream.Read(buf)
			if n > 0 {
				atomic.AddInt64(&bulkReadBytes, int64(n))
			}
			if err != nil {
				break
			}
		}
		bulkDuration = time.Since(bulkStart)
	}()

	// Wait 50ms to ensure the bulk transfer is actively filling and saturating p2pdata
	time.Sleep(50 * time.Millisecond)

	// 2. Measure Ping RTT over p2pcontrol during saturated bulk transfer (3 attempts with retry)
	var pingRTT time.Duration
	for attempt := 0; attempt < 3; attempt++ {
		pingCtx, pingCancel := context.WithTimeout(context.Background(), 5*time.Second)
		measuredRTT, pingErr := clientMux.Ping(pingCtx)
		pingCancel()
		if pingErr == nil {
			pingRTT = measuredRTT
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pingRTT == 0 {
		t.Logf("[WARN] Ping failed during bulk transfer in loss condition %d%%", lossPercent)
	}

	// 3. Concurrently execute API calls over p2pcontrol while bulk transfer runs
	latencies := make([]time.Duration, apiRequests)
	var succCount int64
	var failCount int64
	var apiWg sync.WaitGroup

	reqTimeout := 15 * time.Second
	if lossPercent >= 30 {
		reqTimeout = 30 * time.Second
	}

	apiStart := time.Now()
	for i := 0; i < apiRequests; i++ {
		apiWg.Add(1)
		go func(idx int) {
			defer apiWg.Done()
			time.Sleep(time.Duration(idx*15) * time.Millisecond) // staggered dispatch

			t0 := time.Now()
			stream, err := clientMux.OpenStream()
			if err != nil {
				atomic.AddInt64(&failCount, 1)
				if idx < 3 {
					t.Logf("[API %d] OpenStream err: %v", idx, err)
				}
				return
			}
			defer stream.Close()

			reqMeta := proxy.RequestMetadata{
				Method: "GET",
				Path:   "/api/status",
			}
			metaBytes, _ := json.Marshal(reqMeta)
			if err := stream.WriteHeaders(metaBytes); err != nil {
				atomic.AddInt64(&failCount, 1)
				if idx < 3 {
					t.Logf("[API %d] WriteHeaders err: %v", idx, err)
				}
				return
			}
			_ = stream.Close()

			ctx, cancel := context.WithTimeout(context.Background(), reqTimeout)
			defer cancel()
			_, err = stream.ReadHeaders(ctx)
			if err != nil {
				atomic.AddInt64(&failCount, 1)
				if idx < 3 {
					t.Logf("[API %d] ReadHeaders err: %v (stream ctx err: %v)", idx, err, stream.Context().Err())
				}
				return
			}

			readDone := make(chan struct{})
			var respBuf bytes.Buffer
			go func() {
				defer close(readDone)
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
			}()

			select {
			case <-readDone:
			case <-time.After(reqTimeout):
				_ = stream.Reset()
				atomic.AddInt64(&failCount, 1)
				if idx < 3 {
					t.Logf("[API %d] Read timeout (%v)", idx, reqTimeout)
				}
				return
			}

			lat := time.Since(t0)
			latencies[idx] = lat

			if bytes.Contains(respBuf.Bytes(), []byte(`"status":"ok"`)) {
				atomic.AddInt64(&succCount, 1)
			} else {
				atomic.AddInt64(&failCount, 1)
			}
		}(i)
	}

	apiWg.Wait()
	_ = time.Since(apiStart)

	// Close client mux to gracefully finish bulk stream
	_ = clientMux.Close()
	<-bulkDone

	totalTransferred := atomic.LoadInt64(&bulkReadBytes)
	succ := int(atomic.LoadInt64(&succCount))
	fail := int(atomic.LoadInt64(&failCount))

	var validLats []time.Duration
	var totalLat time.Duration
	for _, l := range latencies {
		if l > 0 {
			validLats = append(validLats, l)
			totalLat += l
		}
	}
	sort.Slice(validLats, func(i, j int) bool { return validLats[i] < validLats[j] })

	condName := "0% Loss (Normal)"
	if lossPercent > 0 {
		condName = fmt.Sprintf("%d%% Loss (Chaos)", lossPercent)
	}

	res := HoLBenchmarkResult{
		Condition:         condName,
		PacketLossPercent: lossPercent,
		BulkTotalBytes:    totalTransferred,
		BulkDuration:      bulkDuration,
		APIRequests:       apiRequests,
		APISuccess:        succ,
		APIFailed:         fail,
		PingRTTDuringBulk: pingRTT,
	}

	if bulkDuration > 0 && totalTransferred > 0 {
		res.BulkThroughputMBs = float64(totalTransferred) / (1024 * 1024) / bulkDuration.Seconds()
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
	t.Logf("📊 Dual-Channel Anti-HoL Benchmark [%s]", condName)
	t.Logf("   Bulk Data Transferred: %.2f MB during test", float64(totalTransferred)/(1024*1024))
	t.Logf("   Control Channel Ping RTT: %v", pingRTT)
	t.Logf("   Concurrent API Requests: %d Success, %d Failed (%.1f%%)", succ, fail, float64(succ)/float64(apiRequests)*100)
	t.Logf("   API Latencies:")
	t.Logf("      Min: %.2fms | Avg: %.2fms | P50: %.2fms | P95: %.2fms | P99: %.2fms | Max: %.2fms",
		float64(res.MinLatency.Microseconds())/1000.0,
		float64(res.AvgLatency.Microseconds())/1000.0,
		float64(res.P50Latency.Microseconds())/1000.0,
		float64(res.P95Latency.Microseconds())/1000.0,
		float64(res.P99Latency.Microseconds())/1000.0,
		float64(res.MaxLatency.Microseconds())/1000.0,
	)
	t.Logf("=================================================================")

	return res
}

func TestDualChannel_AntiHoL_NormalBaseline(t *testing.T) {
	runHoLBenchmark(t, 0)
}

func TestDualChannel_AntiHoL_30PercentChaos(t *testing.T) {
	runHoLBenchmark(t, 30)
}

func TestDualChannel_AntiHoL_FullComparison(t *testing.T) {
	lossTiers := []int{0, 5, 15, 30}
	results := make([]HoLBenchmarkResult, 0, len(lossTiers))

	for _, loss := range lossTiers {
		res := runHoLBenchmark(t, loss)
		results = append(results, res)
	}

	t.Logf("\n")
	t.Logf("===========================================================================================================")
	t.Logf("📊 《WebRTC Gateway 双通道抗队头阻塞基准测试实测对比》 (Dual-Channel Anti-HoL Benchmark)")
	t.Logf("===========================================================================================================")
	t.Logf("| Network Condition | Bulk Transferred | Control Ping RTT | API P50 Latency | API P95 Latency | API P99 Latency | API Success Rate |")
	t.Logf("|:---|:---:|:---:|:---:|:---:|:---:|:---:|")
	for _, r := range results {
		pingStr := fmt.Sprintf("%v", r.PingRTTDuringBulk.Round(time.Microsecond))
		if r.PingRTTDuringBulk == 0 {
			pingStr = "Timeout (>5s)"
		}
		t.Logf("| %-17s | %8.2f MB | %16s | %13.2fms | %13.2fms | %13.2fms | %14s |",
			r.Condition,
			float64(r.BulkTotalBytes)/(1024*1024),
			pingStr,
			float64(r.P50Latency.Microseconds())/1000.0,
			float64(r.P95Latency.Microseconds())/1000.0,
			float64(r.P99Latency.Microseconds())/1000.0,
			fmt.Sprintf("%d/%d (%.1f%%)", r.APISuccess, r.APIRequests, float64(r.APISuccess)/float64(r.APIRequests)*100),
		)
	}
	t.Logf("===========================================================================================================\n")
}
