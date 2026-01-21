// cmd/server는 net/http.Server 동작을 실험하기 위한 서버다.
//
// 이 서버는 다음을 제공한다:
//   - 다양한 테스트 엔드포인트 (/sleep, /echo, /health 등)
//   - 모든 http.Server timeout 옵션을 플래그로 조절
//   - ConnState 훅을 통한 연결 상태 추적
//   - 요청별 구조화 로깅 (request_id, latency, bytes)
//   - pprof 엔드포인트 (별도 포트)
//
// 사용 예:
//
//	# 기본 실행 (타임아웃 적용)
//	./server -port=8080 -pprof-port=6060
//
//	# 타임아웃 없이 실행 (slowloris 취약 모드)
//	./server -port=8080 -read-timeout=0 -write-timeout=0
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	_ "net/http/pprof" // pprof 핸들러 자동 등록
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ahwlsqja/go-http-lab/internal/logger"
	"github.com/ahwlsqja/go-http-lab/internal/metrics"
	"github.com/rs/zerolog"
)

// Config holds server configuration from flags.
type Config struct {
	Port     int
	PprofPort int

	// http.Server timeout 설정
	// 각 타임아웃이 어디에 적용되는지는 docs/02_timeouts_read_write.md 참조
	ReadTimeout       time.Duration // 전체 요청 읽기 (헤더 + 바디)
	ReadHeaderTimeout time.Duration // 헤더만 읽기 (slowloris 방어 핵심)
	WriteTimeout      time.Duration // 응답 쓰기
	IdleTimeout       time.Duration // keep-alive 유휴 대기

	// 서버 동작 옵션
	MaxHeaderBytes  int
	ShutdownTimeout time.Duration // graceful shutdown 대기 시간
	Debug           bool
}

// 전역 상태 (실험용)
var (
	requestCounter atomic.Uint64
	connCounter    *metrics.ConnStateCounter
	log            zerolog.Logger
)

func main() {
	cfg := parseFlags()

	// 로거 초기화
	log = logger.New(cfg.Debug)
	log.Info().
		Int("port", cfg.Port).
		Int("pprof_port", cfg.PprofPort).
		Dur("read_timeout", cfg.ReadTimeout).
		Dur("read_header_timeout", cfg.ReadHeaderTimeout).
		Dur("write_timeout", cfg.WriteTimeout).
		Dur("idle_timeout", cfg.IdleTimeout).
		Str("go_version", runtime.Version()).
		Msg("starting server")

	// 연결 상태 카운터 초기화
	connCounter = metrics.NewConnStateCounter(log)

	// pprof 서버 (별도 goroutine)
	go runPprofServer(cfg.PprofPort)

	// 메인 HTTP 서버 설정
	mux := http.NewServeMux()
	registerHandlers(mux)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.Port),
		Handler: mux,

		// 타임아웃 설정
		// 0이면 타임아웃 없음 (무한 대기 - 취약)
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,

		MaxHeaderBytes: cfg.MaxHeaderBytes,

		// ConnState 훅 등록
		// 연결 상태가 변경될 때마다 호출됨
		ConnState: connCounter.TrackConnState,
	}

	// Graceful shutdown 설정
	// SIGINT, SIGTERM 수신 시 정상 종료
	done := make(chan struct{})
	shutdownTimeout := cfg.ShutdownTimeout // closure에서 사용할 값 캡처
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

		sig := <-sigCh
		log.Info().
			Str("signal", sig.String()).
			Dur("shutdown_timeout", shutdownTimeout).
			Msg("shutdown signal received, waiting for in-flight requests")

		// Shutdown은 새 연결 거부 + 기존 연결 완료 대기
		// context에 타임아웃을 주면 그 시간 내에 완료 안 된 연결은 강제로 반환
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		shutdownStart := time.Now()
		if err := server.Shutdown(ctx); err != nil {
			log.Error().Err(err).Dur("elapsed", time.Since(shutdownStart)).Msg("shutdown error (timeout?)")
		} else {
			log.Info().Dur("elapsed", time.Since(shutdownStart)).Msg("shutdown completed gracefully")
		}
		close(done)
	}()

	// 서버 시작
	log.Info().Msgf("listening on :%d", cfg.Port)
	// ListenAndServer가 돌면서 Accept 블록킹 -> 즉 연결 마다 고루틴 만들어서 계쏙 도는 겅미 그러다가 ListenAndServe가 ErrServerClosed 반환하면  <- done 으로감
	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatal().Err(err).Msg("server error")
	}

	<-done
	log.Info().Msg("server stopped")
}

// 프로그램 실행할 때 준 옵션들(예: -port=8080 -debug)을 읽어서 Config 구조체에 담아 돌려주는 함수
func parseFlags() Config {
	// Config 구조체를 만들고 모든 필드를 기본값(0, false, nil 등)으로 초기화
	cfg := Config{}

	flag.IntVar(&cfg.Port, "port", 8080, "main server port")
	flag.IntVar(&cfg.PprofPort, "pprof-port", 6060, "pprof server port")

	flag.DurationVar(&cfg.ReadTimeout, "read-timeout", 10*time.Second, "http.Server.ReadTimeout (0 = no timeout)")
	flag.DurationVar(&cfg.ReadHeaderTimeout, "read-header-timeout", 5*time.Second, "http.Server.ReadHeaderTimeout (0 = no timeout)")
	flag.DurationVar(&cfg.WriteTimeout, "write-timeout", 10*time.Second, "http.Server.WriteTimeout (0 = no timeout)")
	flag.DurationVar(&cfg.IdleTimeout, "idle-timeout", 60*time.Second, "http.Server.IdleTimeout (0 = no timeout)")

	flag.IntVar(&cfg.MaxHeaderBytes, "max-header-bytes", 1<<20, "http.Server.MaxHeaderBytes")
	flag.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", 30*time.Second, "graceful shutdown timeout")
	flag.BoolVar(&cfg.Debug, "debug", false, "enable debug logging")

	flag.Parse()
	return cfg
}

func runPprofServer(port int) {
	addr := fmt.Sprintf(":%d", port)
	log.Info().Msgf("pprof listening on %s", addr)

	// pprof는 DefaultServeMux에 자동 등록됨
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Error().Err(err).Msg("pprof server error")
	}
}

func registerHandlers(mux *http.ServeMux) {
	// 미들웨어로 감싸서 로깅 추가
	mux.HandleFunc("/health", withLogging(healthHandler))
	mux.HandleFunc("/ready", withLogging(readyHandler))
	mux.HandleFunc("/sleep", withLogging(sleepHandler))
	mux.HandleFunc("/echo", withLogging(echoHandler))
	mux.HandleFunc("/readbody", withLogging(readBodyHandler))
	mux.HandleFunc("/stats", withLogging(statsHandler))
}

// withLogging은 요청 로깅 미들웨어다.
// 각 요청에 고유 ID를 부여하고, 완료 시 latency와 바이트 수를 로깅한다.
func withLogging(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := requestCounter.Add(1)

		// 응답 래퍼로 바이트 수 추적
		rw := &responseWriter{ResponseWriter: w}

		log.Debug().
			Uint64("request_id", reqID).
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Str("remote_addr", r.RemoteAddr).
			Msg("request started")

		handler(rw, r)

		log.Info().
			Uint64("request_id", reqID).
			Str("method", r.Method).
			Str("path", r.URL.Path).
			Int("status", rw.status).
			Int("bytes_written", rw.bytesWritten).
			Dur("latency", time.Since(start)).
			Msg("request completed")
	}
}

// responseWriter wraps http.ResponseWriter to track status and bytes written.
type responseWriter struct {
	http.ResponseWriter
	status       int
	bytesWritten int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if rw.status == 0 {
		rw.status = http.StatusOK
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.bytesWritten += n
	return n, err
}

// =============================================================================
// Handlers
// =============================================================================

// healthHandler는 단순 헬스체크 엔드포인트다.
// 항상 200 OK를 반환한다.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// readyHandler는 서버 준비 상태를 반환한다.
// 연결 통계도 함께 반환한다.
func readyHandler(w http.ResponseWriter, r *http.Request) {
	snap := connCounter.GetSnapshot()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status":      "ready",
		"connections": snap,
		"goroutines":  runtime.NumGoroutine(),
	})
}

// sleepHandler는 지정된 시간만큼 대기 후 응답한다.
// 긴 요청 시뮬레이션과 graceful shutdown 테스트에 사용.
//
// 사용: GET /sleep?ms=5000
func sleepHandler(w http.ResponseWriter, r *http.Request) {
	msStr := r.URL.Query().Get("ms")
	ms, err := strconv.Atoi(msStr)
	if err != nil || ms < 0 {
		ms = 1000 // 기본 1초
	}

	// 최대 30초로 제한 (실수 방지)
	if ms > 30000 {
		ms = 30000
	}

	duration := time.Duration(ms) * time.Millisecond

	// context 취소 감지 (shutdown 등)
	select {
	case <-time.After(duration):
		w.Write([]byte(fmt.Sprintf("slept for %dms\n", ms)))
	case <-r.Context().Done():
		// 클라이언트 연결 끊김 또는 서버 shutdown
		log.Debug().Dur("requested", duration).Msg("sleep interrupted by context cancellation")
		return
	}
}

// echoHandler는 요청 정보를 그대로 응답에 반영한다.
// 헤더, 쿼리 파라미터 등을 확인할 때 유용.
func echoHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	response := map[string]any{
		"method":      r.Method,
		"path":        r.URL.Path,
		"query":       r.URL.Query(),
		"headers":     r.Header,
		"remote_addr": r.RemoteAddr,
		"host":        r.Host,
	}

	json.NewEncoder(w).Encode(response)
}

// readBodyHandler는 요청 바디를 읽고 크기를 반환한다.
// ReadTimeout 테스트에 사용 - 클라이언트가 바디를 천천히 보내면 타임아웃 발생.
//
// 사용: POST /readbody (with body)
func readBodyHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	// io.ReadAll은 EOF까지 읽음
	// 클라이언트가 천천히 보내면 ReadTimeout이 발동할 수 있음
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Debug().Err(err).Msg("failed to read body")
		http.Error(w, fmt.Sprintf("read error: %v", err), http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"bytes_read": len(body),
		"content":    string(body), // 작은 바디만 에코 (큰 바디는 생략해야 함)
	})
}

// statsHandler는 서버 통계를 반환한다.
// 실험 중 서버 상태 확인에 사용.
func statsHandler(w http.ResponseWriter, r *http.Request) {
	snap := connCounter.GetSnapshot()

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"connections": snap,
		"goroutines":  runtime.NumGoroutine(),
		"requests":    requestCounter.Load(),
		"memory": map[string]any{
			"alloc_mb":       memStats.Alloc / 1024 / 1024,
			"total_alloc_mb": memStats.TotalAlloc / 1024 / 1024,
			"sys_mb":         memStats.Sys / 1024 / 1024,
			"num_gc":         memStats.NumGC,
		},
	})
}
