# Graceful Shutdown 실험 결과

> **실험일**: 2026-01-22
> **Go 버전**: 1.23.4
> **환경**: WSL2 Ubuntu

## 1. 실험 목적

`docs/01_http_server_overview.md`에서 분석한 Shutdown 메커니즘을 **실험으로 검증**한다:

> "Shutdown은 Active 연결이 완료될 때까지 대기하고, context timeout 시 반환한다"

## 2. Shutdown 동작 원리 (복습)

```go
// net/http/server.go:3049-3085
func (srv *Server) Shutdown(ctx context.Context) error {
    srv.inShutdown.Store(true)           // 1. shutdown 플래그
    srv.closeListenersLocked()           // 2. 새 연결 거부

    for {
        if srv.closeIdleConns() {        // 3. Idle 연결 닫기
            return nil                   // 4. 모든 연결 종료 → 성공
        }
        select {
        case <-ctx.Done():
            return ctx.Err()             // 5. timeout → 에러 반환
        case <-timer.C:
            continue                     // 6. Active 대기
        }
    }
}
```

**핵심**: timeout이 발생해도 `Shutdown()`은 **연결을 강제로 닫지 않는다**.

## 3. 실험 설계

### 독립 변수
- Shutdown timeout: 30초 vs 3초

### 종속 변수
- in-flight 요청 완료 여부
- curl 응답 코드
- 서버 종료 상태

### 공통 조건
- 10초 걸리는 요청 (`/sleep?ms=10000`)
- 요청 시작 2초 후 SIGTERM 발송
- 남은 요청 시간: ~8초

## 4. 실험 결과

### 실험 A: 충분한 timeout (30초)

```bash
./bin/server -shutdown-timeout=30s
curl "http://localhost:9090/sleep?ms=10000" &
# 2초 후 SIGTERM
```

**로그**:
```
01:58:06 INF shutdown signal received shutdown_timeout=30000
01:58:14 INF request completed latency=10000ms path=/sleep
01:58:14 INF shutdown completed gracefully elapsed=8435ms
```

| 측정 항목 | 결과 |
|-----------|------|
| 요청 완료 | ✅ 정상 (10초) |
| curl exit | 0 (성공) |
| Shutdown | gracefully |
| elapsed | 8.4초 |

### 실험 B: 부족한 timeout (3초)

```bash
./bin/server -shutdown-timeout=3s
curl "http://localhost:9090/sleep?ms=10000" &
# 2초 후 SIGTERM
```

**로그**:
```
01:58:53 INF shutdown signal received shutdown_timeout=3000
01:58:56 ERR shutdown error (timeout?) error="context deadline exceeded" elapsed=3000ms
01:58:56 INF server stopped
```

| 측정 항목 | 결과 |
|-----------|------|
| 요청 완료 | ❌ 중단 |
| curl exit | 52 (empty reply) |
| Shutdown | **timeout** |
| elapsed | 3.0초 |

## 5. 결과 비교

```
┌────────────────────────────────────────────────────────────────┐
│                    실험 A (30s timeout)                        │
│  ──────────────────────────────────────────────────────────    │
│  [요청 시작]──────────────────[요청 완료]──[서버 종료]         │
│       0s                        10s       10s                  │
│                                                                │
│  SIGTERM(2s)                                                   │
│       ↓                                                        │
│  대기 (8초)... ✅ gracefully                                   │
│                                                                │
├────────────────────────────────────────────────────────────────┤
│                    실험 B (3s timeout)                         │
│  ──────────────────────────────────────────────────────────    │
│  [요청 시작]─────[timeout!]                                    │
│       0s           5s                                          │
│                    ↑                                           │
│  SIGTERM(2s)──────┘                                            │
│       ↓            │                                           │
│  대기 (3초)... ⚠️ timeout!                                     │
│                    └→ 프로세스 종료 → 연결 끊김                │
│                                                                │
└────────────────────────────────────────────────────────────────┘
```

## 6. 핵심 발견

### 6.1 Shutdown timeout과 요청 완료의 관계

| shutdown timeout | 남은 요청 시간 | 결과 |
|-----------------|---------------|------|
| > 남은 시간 | - | ✅ 요청 완료, graceful 종료 |
| < 남은 시간 | - | ⚠️ timeout, 요청 중단 가능 |

### 6.2 timeout 발생 시 연결 처리

문서 분석에서 예측한 대로:
1. `Shutdown(ctx)`는 timeout 시 **에러만 반환**
2. 연결 자체는 **열린 상태 유지**
3. **하지만** 프로세스가 종료되면 OS가 TCP 연결 정리

```go
if err := server.Shutdown(ctx); err != nil {
    // err = "context deadline exceeded"
    // 이 시점에서 Active 연결은 아직 열려있음!
    // 하지만 main()이 끝나면 프로세스 종료 → 연결도 끊김
}
```

### 6.3 완전한 Graceful Shutdown 패턴

timeout 후에도 연결을 명시적으로 정리하려면:

```go
ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
defer cancel()

if err := server.Shutdown(ctx); err != nil {
    log.Error().Err(err).Msg("shutdown timeout, forcing close")
    server.Close()  // 강제 종료 추가!
}
```

## 7. 분산 시스템 관점

이 실험 결과는 분산 시스템 설계에 중요한 시사점을 준다:

### 7.1 RPC 노드의 Graceful Shutdown

블록체인 노드, RPC 서버 등에서:

```
Shutdown 시 고려사항:
├── HTTP 요청 완료 대기 (이 실험에서 검증)
├── mempool flush
├── WAL (Write-Ahead Log) flush
├── DB transaction commit
├── peer 연결 종료 알림
└── background goroutine drain
```

### 7.2 권장 Shutdown timeout 설정

| 서비스 유형 | 권장 timeout | 이유 |
|------------|-------------|------|
| 단순 API | 30초 | 대부분의 요청이 30초 내 완료 |
| 파일 업로드 | 5분+ | 대용량 파일 업로드 완료 대기 |
| 배치 처리 | 요청 특성에 따라 | 긴 작업은 context로 취소 처리 |

### 7.3 클라이언트 관점

서버 shutdown 시 클라이언트는:
- 연결 끊김 (empty reply, connection reset)
- 재시도 로직 필요
- idempotent API 설계 중요

## 8. 결론

### 검증된 동작

1. **충분한 timeout**: in-flight 요청이 완료되고 gracefully 종료
2. **부족한 timeout**: context deadline exceeded, 요청 중단 가능
3. **timeout 후 연결**: Shutdown은 안 닫음, 프로세스 종료가 닫음

### 실전 권장 사항

```go
server := &http.Server{
    // ... timeout 설정 ...
}

// Shutdown timeout = max(예상 요청 시간) + 여유
shutdownTimeout := 30 * time.Second

go func() {
    <-sigCh
    ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
    defer cancel()

    if err := server.Shutdown(ctx); err != nil {
        log.Error().Err(err).Msg("shutdown timeout")
        server.Close()  // 강제 종료
    }
}()
```

## 9. 추가 실험 아이디어

1. **다중 요청**: 여러 in-flight 요청이 있을 때 shutdown 동작
2. **WebSocket**: hijacked 연결의 shutdown 처리
3. **HTTP/2**: 다중화된 연결의 shutdown 동작
4. **메모리 영향**: 긴 shutdown 대기 시 메모리 사용량
