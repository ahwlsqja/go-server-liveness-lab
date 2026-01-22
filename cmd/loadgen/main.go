// cmd/loadgen은 HTTP 부하 발생기다.
//
// 주요 기능:
//   - 동시 요청 수 (concurrency) 조절
//   - keep-alive on/off 토글
//   - latency 통계 (평균, p50, p95, p99)
//   - RPS (requests per second) 측정
//
// 실험 목적:
//   - keep-alive on/off에 따른 성능 차이 측정
//   - 연결 재사용이 latency와 throughput에 미치는 영향 확인
//
// 사용 예:
//
//	# keep-alive 활성화 (기본)
//	./loadgen -target=http://localhost:8080/health -concurrency=10 -duration=10s
//
//	# keep-alive 비활성화
//	./loadgen -target=http://localhost:8080/health -concurrency=10 -duration=10s -keep-alive=false
package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ahwlsqja/go-http-lab/internal/logger"
	"github.com/rs/zerolog"
)

// Config holds loadgen configuration.
type Config struct {
	Target      string        // 타겟 URL
	Concurrency int           // 동시 요청 수
	Duration    time.Duration // 총 실행 시간
	KeepAlive   bool          // keep-alive 활성화 여부
	Timeout     time.Duration // 요청 타임아웃
	Debug       bool          // 디버그 로깅
}

// Stats holds request statistics.
type Stats struct {
	totalRequests  atomic.Int64
	successCount   atomic.Int64
	errorCount     atomic.Int64
	totalLatencyNs atomic.Int64 // 나노초 단위 총 latency

	// latency 분포 (뮤텍스로 보호)
	latencies []time.Duration
	mu        sync.Mutex
}

var (
	log   zerolog.Logger
	stats Stats
)

func main() {
	cfg := parseFlags()

	log = logger.New(cfg.Debug)
	log.Info().
		Str("target", cfg.Target).
		Int("concurrency", cfg.Concurrency).
		Dur("duration", cfg.Duration).
		Bool("keep_alive", cfg.KeepAlive).
		Msg("starting load generator")

	// HTTP 클라이언트 생성
	client := createHTTPClient(cfg)

	// 종료 시그널 처리
	ctx := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Info().Msg("shutdown signal received")
		close(ctx)
	}()

	// Duration 타이머
	go func() {
		time.Sleep(cfg.Duration)
		log.Info().Msg("duration reached")
		close(ctx)
	}()

	// 시작 시간 기록
	startTime := time.Now()

	// 워커 시작
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			worker(ctx, client, cfg.Target, workerID)
		}(i)
	}

	// 진행 상황 리포터
	go progressReporter(ctx, startTime)

	// 종료 대기
	<-ctx
	log.Info().Msg("stopping workers...")

	// 워커들이 현재 요청 완료할 시간
	time.Sleep(500 * time.Millisecond)

	// 최종 결과 출력
	elapsed := time.Since(startTime)
	printResults(elapsed, cfg)
}

func parseFlags() Config {
	cfg := Config{}

	flag.StringVar(&cfg.Target, "target", "http://localhost:8080/health", "target URL")
	flag.IntVar(&cfg.Concurrency, "concurrency", 10, "number of concurrent workers")
	flag.DurationVar(&cfg.Duration, "duration", 10*time.Second, "test duration")
	flag.BoolVar(&cfg.KeepAlive, "keep-alive", true, "enable HTTP keep-alive")
	flag.DurationVar(&cfg.Timeout, "timeout", 10*time.Second, "request timeout")
	flag.BoolVar(&cfg.Debug, "debug", false, "enable debug logging")

	flag.Parse()
	return cfg
}

// createHTTPClient creates an HTTP client with the given configuration.
func createHTTPClient(cfg Config) *http.Client {
	transport := &http.Transport{
		// keep-alive 설정
		DisableKeepAlives: !cfg.KeepAlive,

		// 연결 풀 설정
		MaxIdleConns:        cfg.Concurrency * 2,
		MaxIdleConnsPerHost: cfg.Concurrency * 2,
		IdleConnTimeout:     90 * time.Second,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   cfg.Timeout,
	}
}

// worker는 지속적으로 요청을 보내는 워커 goroutine이다.
func worker(ctx <-chan struct{}, client *http.Client, target string, workerID int) {
	for {
		select {
		case <-ctx:
			return
		default:
		}

		start := time.Now()
		err := doRequest(client, target)
		latency := time.Since(start)

		stats.totalRequests.Add(1)
		stats.totalLatencyNs.Add(int64(latency))

		if err != nil {
			stats.errorCount.Add(1)
			log.Debug().Err(err).Int("worker", workerID).Msg("request failed")
		} else {
			stats.successCount.Add(1)

			// latency 기록 (통계용)
			stats.mu.Lock()
			stats.latencies = append(stats.latencies, latency)
			stats.mu.Unlock()
		}
	}
}

// doRequest는 단일 HTTP 요청을 수행한다.
func doRequest(client *http.Client, target string) error {
	resp, err := client.Get(target)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// 바디를 완전히 읽어야 연결이 재사용됨 (keep-alive)
	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return nil
}

// progressReporter는 주기적으로 진행 상황을 출력한다.
func progressReporter(ctx <-chan struct{}, startTime time.Time) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastTotal int64

	for {
		select {
		case <-ctx:
			return
		case <-ticker.C:
			total := stats.totalRequests.Load()
			success := stats.successCount.Load()
			errors := stats.errorCount.Load()
			elapsed := time.Since(startTime).Seconds()

			// 최근 2초간 RPS
			recentRequests := total - lastTotal
			recentRPS := float64(recentRequests) / 2.0
			lastTotal = total

			// 전체 평균 RPS
			avgRPS := float64(total) / elapsed

			log.Info().
				Int64("total", total).
				Int64("success", success).
				Int64("errors", errors).
				Float64("recent_rps", recentRPS).
				Float64("avg_rps", avgRPS).
				Msg("progress")
		}
	}
}

// printResults는 최종 결과를 출력한다.
func printResults(elapsed time.Duration, cfg Config) {
	total := stats.totalRequests.Load()
	success := stats.successCount.Load()
	errors := stats.errorCount.Load()

	// RPS 계산
	rps := float64(total) / elapsed.Seconds()

	// Latency 통계 계산
	stats.mu.Lock()
	latencies := make([]time.Duration, len(stats.latencies))
	copy(latencies, stats.latencies)
	stats.mu.Unlock()

	var avgLatency, p50, p95, p99 time.Duration
	if len(latencies) > 0 {
		// 정렬
		sort.Slice(latencies, func(i, j int) bool {
			return latencies[i] < latencies[j]
		})

		// 평균
		var sum time.Duration
		for _, l := range latencies {
			sum += l
		}
		avgLatency = sum / time.Duration(len(latencies))

		// 백분위수
		p50 = percentile(latencies, 50)
		p95 = percentile(latencies, 95)
		p99 = percentile(latencies, 99)
	}

	// 결과 출력
	fmt.Println()
	fmt.Println("============================================================")
	fmt.Println("LOAD TEST RESULTS")
	fmt.Println("============================================================")
	fmt.Printf("Target:       %s\n", cfg.Target)
	fmt.Printf("Concurrency:  %d\n", cfg.Concurrency)
	fmt.Printf("Duration:     %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("Keep-Alive:   %v\n", cfg.KeepAlive)
	fmt.Println("------------------------------------------------------------")
	fmt.Printf("Total Requests: %d\n", total)
	fmt.Printf("Successful:     %d (%.1f%%)\n", success, float64(success)/float64(total)*100)
	fmt.Printf("Errors:         %d (%.1f%%)\n", errors, float64(errors)/float64(total)*100)
	fmt.Println("------------------------------------------------------------")
	fmt.Printf("RPS:            %.2f req/sec\n", rps)
	fmt.Println("------------------------------------------------------------")
	fmt.Println("Latency:")
	fmt.Printf("  Average:      %s\n", avgLatency.Round(time.Microsecond))
	fmt.Printf("  P50:          %s\n", p50.Round(time.Microsecond))
	fmt.Printf("  P95:          %s\n", p95.Round(time.Microsecond))
	fmt.Printf("  P99:          %s\n", p99.Round(time.Microsecond))
	fmt.Println("============================================================")
}

// percentile calculates the p-th percentile of sorted durations.
func percentile(sorted []time.Duration, p int) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := (len(sorted) - 1) * p / 100
	return sorted[idx]
}
