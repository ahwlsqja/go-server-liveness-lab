# Go net/http.Server 타임아웃 완전 가이드

> **Go 버전**: 1.23.4
> **분석 파일**: `$GOROOT/src/net/http/server.go`

## 1. 개요: 왜 타임아웃이 필요한가?

타임아웃 없는 서버는 **자원 고갈 공격**에 취약하다.

```
공격자가 할 수 있는 것들:

1. Slowloris (헤더 지연)
   - HTTP 헤더를 1바이트씩 천천히 보냄
   - 서버는 헤더 완료를 무한 대기

2. Slow Body (바디 지연)
   - POST 바디를 천천히 보냄
   - 서버는 바디 읽기를 무한 대기

3. Slow Read (응답 지연 읽기)
   - 서버 응답을 천천히 읽음
   - 서버는 응답 쓰기를 무한 대기

4. Idle Connection Exhaustion
   - keep-alive 연결을 열어두고 아무것도 안 함
   - 서버의 연결 풀 고갈
```

**타임아웃 = 자원 보호 메커니즘**

---

## 2. 네 가지 타임아웃

```go
server := &http.Server{
    ReadHeaderTimeout: 5 * time.Second,   // 헤더 읽기
    ReadTimeout:       30 * time.Second,  // 전체 요청 읽기
    WriteTimeout:      30 * time.Second,  // 응답 쓰기
    IdleTimeout:       60 * time.Second,  // keep-alive 유휴
}
```

### 시간 흐름에서의 위치

```
클라이언트 연결
     │
     ▼
┌────────────────────────────────────────────────────────────┐
│  ReadHeaderTimeout                                          │
│  ─────────────────                                          │
│  HTTP 헤더 읽기                                             │
│  "GET /path HTTP/1.1\r\n"                                   │
│  "Host: example.com\r\n"                                    │
│  "\r\n"  ← 이 빈 줄까지                                     │
└────────────────────────────────────────────────────────────┘
     │
     ▼
┌────────────────────────────────────────────────────────────┐
│  ReadTimeout (헤더 완료 후 전환)                            │
│  ───────────                                                │
│  HTTP 바디 읽기                                             │
│  (POST 데이터 등)                                           │
└────────────────────────────────────────────────────────────┘
     │
     ▼
┌────────────────────────────────────────────────────────────┐
│  WriteTimeout (핸들러 시작 시점)                            │
│  ────────────                                               │
│  핸들러 실행 + 응답 쓰기                                    │
│  (handler.ServeHTTP 전체)                                   │
└────────────────────────────────────────────────────────────┘
     │
     ▼
┌────────────────────────────────────────────────────────────┐
│  IdleTimeout (응답 완료 후)                                 │
│  ───────────                                                │
│  keep-alive 대기                                            │
│  (다음 요청의 첫 바이트 대기)                               │
└────────────────────────────────────────────────────────────┘
```

---

## 3. 각 타임아웃 상세 분석

### 3.1 ReadHeaderTimeout

**목적**: HTTP 헤더 파싱 시간 제한 (Slowloris 방어)

**소스 코드 위치**: `server.go:1029-1035`

```go
func (c *conn) readRequest(ctx context.Context) (w *response, err error) {
    if d := c.server.readHeaderTimeout(); d > 0 {
        hdrDeadline = t0.Add(d)
    }
    c.rwc.SetReadDeadline(hdrDeadline)  // ← 여기서 설정!

    req, err := readRequest(c.bufr)     // ← 이 함수에서 발동
    // ...
}
```

**언제 발동하나?**
- 클라이언트가 `\r\n\r\n` (헤더 끝)을 보내기 전에 시간 초과

**실험 결과** (`docs/03_slowloris_experiment.md`):
```
ReadHeaderTimeout=5s  → 5초 후 연결 강제 종료
ReadHeaderTimeout=0   → 무한 대기 (취약!)
```

**Fallback 규칙**:
```go
func (s *Server) readHeaderTimeout() time.Duration {
    if s.ReadHeaderTimeout != 0 {
        return s.ReadHeaderTimeout
    }
    return s.ReadTimeout  // fallback
}
```

**권장값**: `5초` (대부분의 정상 클라이언트는 1초 이내에 헤더 전송)

---

### 3.2 ReadTimeout

**목적**: 전체 요청 읽기 시간 제한 (헤더 + 바디)

**소스 코드 위치**: `server.go:1032-1034, 1092-1094`

```go
// 헤더 읽기 전
if d := c.server.ReadTimeout; d > 0 {
    wholeReqDeadline = t0.Add(d)
}

// 헤더 읽기 완료 후, ReadTimeout으로 전환
if !hdrDeadline.Equal(wholeReqDeadline) {
    c.rwc.SetReadDeadline(wholeReqDeadline)  // ← 바디 읽기용
}
```

**언제 발동하나?**
- 헤더 + 바디 전체 읽기가 시간 초과

**주의**: 핸들러에서 `req.Body`를 읽을 때도 이 타임아웃이 적용됨!

```go
func myHandler(w http.ResponseWriter, r *http.Request) {
    // 이 읽기도 ReadTimeout에 영향받음!
    body, err := io.ReadAll(r.Body)
    if err != nil {
        // ReadTimeout 발동 시 여기로 옴
    }
}
```

**권장값**: `30초` (큰 파일 업로드 고려)

---

### 3.3 WriteTimeout

**목적**: 응답 쓰기 시간 제한 (Slow Read 공격 방어)

**소스 코드 위치**: `server.go:1036-1040`

```go
if d := c.server.WriteTimeout; d > 0 {
    defer func() {
        c.rwc.SetWriteDeadline(time.Now().Add(d))  // ← defer로 설정!
    }()
}
```

**핵심 포인트**: `defer`로 설정되므로, **핸들러 시작 직전**에 설정됨

**언제 발동하나?**
- 핸들러 실행 + 응답 쓰기 전체가 시간 초과

**⚠️ 흔한 실수**:
```go
server := &http.Server{
    WriteTimeout: 10 * time.Second,
}

func slowHandler(w http.ResponseWriter, r *http.Request) {
    time.Sleep(15 * time.Second)  // ❌ WriteTimeout 발동!
    w.Write([]byte("done"))       // 이미 timeout
}
```

**해결책**: 긴 작업은 WriteTimeout보다 짧게, 또는 스트리밍으로

```go
func streamHandler(w http.ResponseWriter, r *http.Request) {
    flusher, _ := w.(http.Flusher)

    for i := 0; i < 10; i++ {
        fmt.Fprintf(w, "chunk %d\n", i)
        flusher.Flush()  // 주기적으로 flush → timeout 리셋 안 됨!
        time.Sleep(2 * time.Second)
    }
}
// WriteTimeout=10s면 20초 후 timeout 발생
```

**권장값**: `30초` (가장 긴 핸들러 실행 시간 + 여유)

---

### 3.4 IdleTimeout

**목적**: keep-alive 유휴 연결 시간 제한

**소스 코드 위치**: `server.go:2117-2121`

```go
// 응답 완료 후, keep-alive 대기 시작
c.setState(c.rwc, StateIdle, runHooks)

if d := c.server.idleTimeout(); d > 0 {
    c.rwc.SetReadDeadline(time.Now().Add(d))  // ← 여기서 설정
}

// 다음 요청의 첫 바이트 대기
if _, err := c.bufr.Peek(4); err != nil {
    return  // timeout → goroutine 종료
}
```

**언제 발동하나?**
- keep-alive 연결에서 다음 요청이 IdleTimeout 내에 안 오면

**Fallback 규칙**:
```go
func (s *Server) idleTimeout() time.Duration {
    if s.IdleTimeout != 0 {
        return s.IdleTimeout
    }
    return s.ReadTimeout  // fallback
}
```

**권장값**: `60초` (너무 짧으면 keep-alive 효과 감소)

---

## 4. 타임아웃 상호작용

### 4.1 Fallback 체인

```
ReadHeaderTimeout ─┬─▶ 명시적 값
                   └─▶ ReadTimeout (fallback)

IdleTimeout ───────┬─▶ 명시적 값
                   └─▶ ReadTimeout (fallback)

ReadTimeout ───────────▶ 명시적 값만

WriteTimeout ──────────▶ 명시적 값만
```

### 4.2 설정 조합 예시

```go
// 최소 설정 (ReadTimeout만)
server := &http.Server{
    ReadTimeout: 30 * time.Second,
    // ReadHeaderTimeout = 30s (fallback)
    // IdleTimeout = 30s (fallback)
    // WriteTimeout = 무제한!
}

// 권장 설정 (명시적으로 모두 지정)
server := &http.Server{
    ReadHeaderTimeout: 5 * time.Second,   // slowloris 방어
    ReadTimeout:       30 * time.Second,  // 큰 업로드 허용
    WriteTimeout:      30 * time.Second,  // 긴 응답 허용
    IdleTimeout:       60 * time.Second,  // keep-alive 유지
}

// 보안 강화 설정
server := &http.Server{
    ReadHeaderTimeout: 2 * time.Second,   // 빠른 차단
    ReadTimeout:       10 * time.Second,  // 짧은 요청만
    WriteTimeout:      10 * time.Second,  // 짧은 응답만
    IdleTimeout:       30 * time.Second,  // 빠른 정리
}
```

---

## 5. 공격 시나리오별 방어

### 5.1 Slowloris (헤더 지연)

```
공격: HTTP 헤더를 1초에 1줄씩 보냄
방어: ReadHeaderTimeout

설정: ReadHeaderTimeout: 5 * time.Second
효과: 5초 내에 헤더 완료 안 되면 연결 끊음
```

### 5.2 Slow POST (바디 지연)

```
공격: POST 바디를 천천히 보냄
방어: ReadTimeout

설정: ReadTimeout: 30 * time.Second
효과: 30초 내에 바디 읽기 완료 안 되면 연결 끊음
```

### 5.3 Slow Read (응답 지연 읽기)

```
공격: 서버 응답을 천천히 읽음 (receive buffer 꽉 참)
방어: WriteTimeout

설정: WriteTimeout: 30 * time.Second
효과: 30초 내에 응답 쓰기 완료 안 되면 연결 끊음
```

### 5.4 Idle Connection Exhaustion

```
공격: keep-alive 연결 열어두고 아무것도 안 함
방어: IdleTimeout

설정: IdleTimeout: 60 * time.Second
효과: 60초 동안 요청 없으면 연결 끊음
```

---

## 6. 실전 트러블슈팅

### 6.1 "context deadline exceeded" 에러

```go
// 클라이언트에서 발생
resp, err := client.Get(url)
// err = "context deadline exceeded"

// 원인: 서버의 ReadTimeout/WriteTimeout보다 클라이언트 timeout이 길어서
// 서버가 먼저 연결을 끊음

// 해결: 클라이언트 timeout < 서버 timeout
```

### 6.2 큰 파일 업로드 실패

```go
// 문제: 100MB 파일 업로드 시 timeout
server := &http.Server{
    ReadTimeout: 10 * time.Second,  // 너무 짧음!
}

// 해결: 충분한 ReadTimeout 또는 스트리밍 처리
server := &http.Server{
    ReadTimeout: 5 * time.Minute,  // 큰 파일 허용
}
```

### 6.3 긴 작업 핸들러 timeout

```go
// 문제: 보고서 생성에 1분 걸림
func reportHandler(w http.ResponseWriter, r *http.Request) {
    report := generateReport()  // 1분 소요
    json.NewEncoder(w).Encode(report)
}

// WriteTimeout=30s면 실패!

// 해결 1: WriteTimeout 늘리기 (권장 안 함)
// 해결 2: 비동기 처리
func reportHandler(w http.ResponseWriter, r *http.Request) {
    jobID := queueReportJob()  // 큐에 넣고
    json.NewEncoder(w).Encode(map[string]string{
        "job_id": jobID,
        "status": "processing",
    })
}
```

---

## 7. 요약 표

| 타임아웃 | 목적 | 방어 대상 | 권장값 | Fallback |
|---------|------|----------|--------|----------|
| **ReadHeaderTimeout** | 헤더 읽기 | Slowloris | 5초 | ReadTimeout |
| **ReadTimeout** | 헤더+바디 읽기 | Slow POST | 30초 | 없음 |
| **WriteTimeout** | 핸들러+응답 쓰기 | Slow Read | 30초 | 없음 |
| **IdleTimeout** | keep-alive 유휴 | Idle 고갈 | 60초 | ReadTimeout |

---

## 8. 참고: 타임아웃 설정 위치

```go
// 서버 레벨 설정
server := &http.Server{
    ReadHeaderTimeout: 5 * time.Second,
    ReadTimeout:       30 * time.Second,
    WriteTimeout:      30 * time.Second,
    IdleTimeout:       60 * time.Second,
}

// 핸들러 레벨에서는 설정 불가!
// 하지만 context로 개별 요청 제어 가능:
func myHandler(w http.ResponseWriter, r *http.Request) {
    ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
    defer cancel()

    result := doWorkWithContext(ctx)
    // ...
}
```
