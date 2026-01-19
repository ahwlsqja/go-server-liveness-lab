# Go net/http.Server 내부 구조 분석

> **Go 버전**: 1.23.4
> **분석 파일**: `$GOROOT/src/net/http/server.go` (3908줄)
> **작성일**: 2026-01-19

## 개요

이 문서는 Go 표준 라이브러리 `net/http.Server`의 내부 구현을 코드 레벨로 추적한 결과다.
"그럴 것 같다"가 아니라 **소스 코드 라인 번호**로 증명한다.

---

## 1. 전체 아키텍처

```
┌─────────────────────────────────────────────────────────────────┐
│                    http.Server.Serve()                          │
│                      (main goroutine)                           │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │  for {                          ← Accept Loop            │   │
│  │      conn, _ := listener.Accept()  ← 블록킹              │   │
│  │      c.setState(StateNew)          ← ConnState 훅        │   │
│  │      go c.serve(ctx)               ← goroutine 생성!     │   │
│  │  }                                                       │   │
│  └─────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
                              │
                              │ goroutine per connection
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                    c.serve() goroutine                          │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │  for {                          ← Request Read Loop      │   │
│  │      req := c.readRequest()       ← 헤더+바디 읽기       │   │
│  │      c.setState(StateActive)      ← ConnState 훅         │   │
│  │      handler.ServeHTTP(w, req)    ← 사용자 핸들러        │   │
│  │                                                          │   │
│  │      if !keepAlive { return }     ← 연결 종료?           │   │
│  │      c.setState(StateIdle)        ← ConnState 훅         │   │
│  │      SetReadDeadline(IdleTimeout) ← 타임아웃 설정        │   │
│  │      Peek(4)                      ← 다음 요청 대기       │   │
│  │  }                                                       │   │
│  └─────────────────────────────────────────────────────────┘   │
│  defer c.setState(StateClosed)       ← 종료 시 훅           │   │
└─────────────────────────────────────────────────────────────────┘
```

---

## 2. Accept Loop 분석

### 2.1 ListenAndServe() - 진입점

```go
// net/http/server.go:3247-3260
func (srv *Server) ListenAndServe() error {
    if srv.shuttingDown() {
        return ErrServerClosed
    }
    addr := srv.Addr
    if addr == "" {
        addr = ":http"
    }
    ln, err := net.Listen("tcp", addr)  // TCP 리스너 생성
    if err != nil {
        return err
    }
    return srv.Serve(ln)  // 실제 서빙 위임
}
```

**핵심**: `ListenAndServe()`는 단순히 TCP 리스너를 만들고 `Serve()`에 위임한다.

### 2.2 Serve() - Accept Loop의 핵심

```go
// net/http/server.go:3300-3362
func (srv *Server) Serve(l net.Listener) error {
    // ... 초기화 생략 ...

    for {  // ← 무한 루프 = Accept Loop
        rw, err := l.Accept()  // ← 블록킹! 새 연결 대기

        if err != nil {
            if srv.shuttingDown() {
                return ErrServerClosed  // shutdown 시 루프 탈출
            }
            // 임시 에러면 exponential backoff (5ms → 10ms → ... → 1s)
            if ne, ok := err.(net.Error); ok && ne.Temporary() {
                time.Sleep(tempDelay)
                continue
            }
            return err
        }

        c := srv.newConn(rw)                       // conn 구조체 생성
        c.setState(c.rwc, StateNew, runHooks)      // ← ConnState 훅 호출!
        go c.serve(connCtx)                        // ← goroutine-per-connection!
    }
}
```

**핵심 포인트**:
1. `for { l.Accept() }` = 무한 루프로 연결 수락
2. `go c.serve(connCtx)` = **연결당 하나의 goroutine 생성**
3. `c.setState(StateNew)` = ConnState 훅 호출 (우리 서버의 추적 기능)

---

## 3. Goroutine per Connection 모델

### 3.1 c.serve() - 연결 goroutine의 메인 루프

```go
// net/http/server.go:1937-2133
func (c *conn) serve(ctx context.Context) {
    // panic 복구 및 연결 정리
    defer func() {
        if !c.hijacked() {
            c.close()
            c.setState(c.rwc, StateClosed, runHooks)  // ← 종료 시 훅
        }
    }()

    // TLS 핸들링 (생략)

    // HTTP/1.x Request Read Loop
    for {
        w, err := c.readRequest(ctx)  // 요청 읽기

        if c.r.remain != c.server.initialReadLimitSize() {
            c.setState(c.rwc, StateActive, runHooks)  // ← 데이터 읽으면 Active
        }

        // 에러 처리 (생략)

        // 핸들러 실행
        serverHandler{c.server}.ServeHTTP(w, w.req)  // ← 사용자 핸들러!

        // keep-alive 판단
        if !w.shouldReuseConnection() {
            return  // 연결 종료 → goroutine 종료
        }

        c.setState(c.rwc, StateIdle, runHooks)  // ← Idle 상태로 전환

        // shutdown 중이면 종료
        if !w.conn.server.doKeepAlives() {
            return
        }

        // IdleTimeout 설정
        if d := c.server.idleTimeout(); d > 0 {
            c.rwc.SetReadDeadline(time.Now().Add(d))
        }

        // 다음 요청 대기
        if _, err := c.bufr.Peek(4); err != nil {
            return  // 타임아웃 또는 연결 끊김
        }
    }
}
```

### 3.2 ConnState 상태 전이

```
                    Accept()
                       │
                       ▼
                  ┌─────────┐
                  │ StateNew│ ← 연결 수락됨, 데이터 없음
                  └────┬────┘
                       │ 첫 바이트 읽기
                       ▼
                  ┌──────────┐
             ┌───▶│StateActive│ ← 요청 처리 중
             │    └────┬─────┘
             │         │ 응답 완료
             │         ▼
             │    ┌──────────┐
             │    │ StateIdle│ ← keep-alive 대기
             │    └────┬─────┘
             │         │
             └─────────┘ (다음 요청)
                       │
                       │ 연결 종료
                       ▼
                  ┌───────────┐
                  │StateClosed│
                  └───────────┘
```

---

## 4. Timeout 적용 시점

### 4.1 Timeout 종류

| Timeout | 용도 | 적용 시점 |
|---------|------|----------|
| `ReadHeaderTimeout` | HTTP 헤더 읽기 | 요청 시작 직후 |
| `ReadTimeout` | 전체 요청 (헤더+바디) | 헤더 완료 후 전환 |
| `WriteTimeout` | 응답 쓰기 | 핸들러 시작 직전 |
| `IdleTimeout` | keep-alive 유휴 | 응답 완료 후 |

### 4.2 readRequest() - Timeout 설정의 핵심

```go
// net/http/server.go:1019-1094
func (c *conn) readRequest(ctx context.Context) (w *response, err error) {
    var (
        wholeReqDeadline time.Time  // ReadTimeout용
        hdrDeadline      time.Time  // ReadHeaderTimeout용
    )
    t0 := time.Now()

    // 1. ReadHeaderTimeout 계산
    if d := c.server.readHeaderTimeout(); d > 0 {
        hdrDeadline = t0.Add(d)
    }

    // 2. ReadTimeout 계산
    if d := c.server.ReadTimeout; d > 0 {
        wholeReqDeadline = t0.Add(d)
    }

    // 3. 헤더 읽기 전에 ReadHeaderTimeout 설정!
    c.rwc.SetReadDeadline(hdrDeadline)

    // 4. WriteTimeout은 defer로 나중에 설정
    if d := c.server.WriteTimeout; d > 0 {
        defer func() {
            c.rwc.SetWriteDeadline(time.Now().Add(d))
        }()
    }

    // 5. 헤더 파싱 (이 시점에 ReadHeaderTimeout 적용됨!)
    req, err := readRequest(c.bufr)

    // ... 헤더 검증 ...

    // 6. 헤더 읽기 완료 후, ReadTimeout으로 전환
    if !hdrDeadline.Equal(wholeReqDeadline) {
        c.rwc.SetReadDeadline(wholeReqDeadline)
    }

    return w, nil
}
```

### 4.3 Timeout 적용 순서 다이어그램

```
클라이언트 연결
│
├─────────────────────────────────────────────────────────────────┐
│ ① SetReadDeadline(ReadHeaderTimeout)                            │
│    ↓                                                            │
│    HTTP 헤더 읽기 (GET /path HTTP/1.1\r\nHost: ...\r\n\r\n)     │
│    ★ slowloris 공격 방어 지점!                                  │
│    ↓                                                            │
├─────────────────────────────────────────────────────────────────┤
│ ② SetReadDeadline(ReadTimeout)  ← 헤더 완료 후 전환             │
│    ↓                                                            │
│    HTTP 바디 읽기 (POST 데이터 등)                              │
│    ↓                                                            │
├─────────────────────────────────────────────────────────────────┤
│ ③ SetWriteDeadline(WriteTimeout)  ← 핸들러 시작 직전 (defer)   │
│    ↓                                                            │
│    handler.ServeHTTP() 실행                                     │
│    ↓                                                            │
│    응답 쓰기                                                    │
│    ↓                                                            │
├─────────────────────────────────────────────────────────────────┤
│ ④ 응답 완료                                                     │
│    ↓                                                            │
│    keep-alive? → SetReadDeadline(IdleTimeout)                   │
│    ↓                                                            │
│    Peek(4) 대기 (다음 요청 첫 바이트)                           │
└─────────────────────────────────────────────────────────────────┘
```

### 4.4 Timeout Fallback 체인

```go
// net/http/server.go:3446-3458
func (s *Server) idleTimeout() time.Duration {
    if s.IdleTimeout != 0 {
        return s.IdleTimeout
    }
    return s.ReadTimeout  // fallback
}

func (s *Server) readHeaderTimeout() time.Duration {
    if s.ReadHeaderTimeout != 0 {
        return s.ReadHeaderTimeout
    }
    return s.ReadTimeout  // fallback
}
```

**Fallback 규칙**:
- `ReadHeaderTimeout`: 명시적 값 → `ReadTimeout` → 0 (무제한)
- `IdleTimeout`: 명시적 값 → `ReadTimeout` → 0 (무제한)
- `ReadTimeout`: 명시적 값 → 0 (무제한)
- `WriteTimeout`: 명시적 값 → 0 (무제한)

---

## 5. Shutdown 메커니즘

### 5.1 Shutdown() vs Close()

| | `Shutdown(ctx)` | `Close()` |
|---|---|---|
| 새 연결 | 즉시 거부 | 즉시 거부 |
| Idle 연결 | 즉시 닫음 | 즉시 닫음 |
| Active 연결 | **완료 대기** | **강제 종료** |
| 데이터 유실 | 없음 (graceful) | 가능 |

### 5.2 Shutdown() 구현

```go
// net/http/server.go:3049-3085
func (srv *Server) Shutdown(ctx context.Context) error {
    // 1. shutdown 플래그 설정
    srv.inShutdown.Store(true)

    // 2. 모든 리스너 닫기 (새 연결 거부!)
    srv.closeListenersLocked()

    // 3. onShutdown 콜백 실행
    for _, f := range srv.onShutdown {
        go f()
    }

    // 4. Polling Loop (exponential backoff: 1ms → ... → 500ms)
    for {
        if srv.closeIdleConns() {  // 모든 연결 종료됨?
            return lnerr
        }
        select {
        case <-ctx.Done():         // context timeout!
            return ctx.Err()       // ⚠️ Active 연결은 여전히 열려있음!
        case <-timer.C:
            timer.Reset(nextPollInterval())
        }
    }
}
```

### 5.3 closeIdleConns() - Idle 연결 정리

```go
// net/http/server.go:3100-3122
func (s *Server) closeIdleConns() bool {
    quiescent := true
    for c := range s.activeConn {
        st, unixSec := c.getState()

        // Issue 22682: StateNew가 5초 이상이면 Idle로 취급
        if st == StateNew && unixSec < time.Now().Unix()-5 {
            st = StateIdle
        }

        if st != StateIdle || unixSec == 0 {
            quiescent = false  // Active 연결 있음
            continue
        }

        c.rwc.Close()  // Idle 연결은 닫기
        delete(s.activeConn, c)
    }
    return quiescent
}
```

### 5.4 Shutdown 중 Keep-Alive 비활성화

```go
// net/http/server.go:3460-3462
func (s *Server) doKeepAlives() bool {
    return !s.disableKeepAlives.Load() && !s.shuttingDown()
}
```

Shutdown이 호출되면:
1. `inShutdown` = true
2. `doKeepAlives()` = false
3. Active 요청 완료 후 keep-alive로 대기하지 않고 즉시 종료
4. 연결이 빠르게 Idle로 전환 → `closeIdleConns()`가 정리

### 5.5 Shutdown 전체 흐름

```
Shutdown(ctx) 호출
        │
        ▼
┌───────────────────────────────────────────────────────────────┐
│ ① inShutdown.Store(true)                                      │
│    └── Serve() 루프가 ErrServerClosed 반환                    │
│    └── doKeepAlives() = false                                 │
└───────────────────────────────────────────────────────────────┘
        │
        ▼
┌───────────────────────────────────────────────────────────────┐
│ ② closeListenersLocked()                                      │
│    └── 새 연결 즉시 거부                                      │
└───────────────────────────────────────────────────────────────┘
        │
        ▼
┌───────────────────────────────────────────────────────────────┐
│ ③ Polling Loop (1ms → 2ms → ... → 500ms max)                 │
│    ├── StateIdle → Close()                                    │
│    ├── StateNew > 5초 → Idle로 취급 → Close()                 │
│    ├── StateActive → 대기                                     │
│    │                                                          │
│    ├── 모든 연결 종료? → return (정상)                        │
│    └── ctx.Done()? → return ctx.Err() (timeout)               │
└───────────────────────────────────────────────────────────────┘
```

---

## 6. 실전 권장 설정

```go
server := &http.Server{
    Addr:    ":8080",
    Handler: mux,

    // Timeout 설정 (slowloris 방어)
    ReadHeaderTimeout: 5 * time.Second,   // 헤더 읽기 (짧게)
    ReadTimeout:       30 * time.Second,  // 전체 요청 (큰 업로드 허용)
    WriteTimeout:      30 * time.Second,  // 응답 쓰기
    IdleTimeout:       60 * time.Second,  // keep-alive 유휴

    // 연결 상태 추적 (선택)
    ConnState: func(conn net.Conn, state http.ConnState) {
        log.Printf("conn %s: %s", conn.RemoteAddr(), state)
    },
}
```

---

## 7. 핵심 요약

1. **Goroutine-per-Connection**: `go c.serve(ctx)`로 연결당 goroutine 생성
2. **Request Read Loop**: `for { c.readRequest() }` 루프로 keep-alive 지원
3. **Timeout 적용 순서**: ReadHeaderTimeout → ReadTimeout → WriteTimeout → IdleTimeout
4. **Timeout Fallback**: ReadHeaderTimeout과 IdleTimeout은 ReadTimeout을 fallback으로 사용
5. **Graceful Shutdown**: Active 연결 완료 대기, Idle 연결 즉시 종료

---

## 참고

- Go 소스 코드: `$GOROOT/src/net/http/server.go`
- 핵심 함수 위치:
  - `Server.Serve()`: 3300줄
  - `conn.serve()`: 1937줄
  - `conn.readRequest()`: 1019줄
  - `Server.Shutdown()`: 3049줄
  - `Server.Close()`: 2999줄
