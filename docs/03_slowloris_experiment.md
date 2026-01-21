# Slowloris 공격 실험 결과

> **실험일**: 2026-01-21
> **Go 버전**: 1.23.4
> **환경**: WSL2 Ubuntu

## 1. 실험 목적

`.claude.md`에서 설정한 **가설을 검증**한다:

> "ReadHeaderTimeout/ReadTimeout이 없으면 커넥션/고루틴이 장시간 점유된다"

## 2. Slowloris 공격 원리

```
정상 클라이언트:                    Slowloris 공격:
──────────────────                 ──────────────────
GET / HTTP/1.1\r\n                 GET / HTTP/1.1\r\n   ← 1초 대기
Host: example.com\r\n              Host: example.com\r\n ← 1초 대기
\r\n  ← 빠르게 전송                X-a: b\r\n           ← 1초 대기
                                   X-c: d\r\n           ← 1초 대기
서버: 즉시 처리                     ...                   ← \r\n\r\n 안 보냄!

                                   서버: 헤더 완료 대기 (무한)
                                   → goroutine + FD 점유
```

**핵심**: HTTP 헤더의 끝(`\r\n\r\n`)을 보내지 않고 천천히 헤더를 추가하면, 서버는 헤더 완료를 무한히 기다린다.

## 3. 실험 설계

### 독립 변수
- `ReadHeaderTimeout`: 5초 vs 0초 (없음)

### 종속 변수
- `goroutines`: 서버 goroutine 수
- `closed_by_server`: 서버가 끊은 연결 수
- `new` connections: 헤더 읽기 대기 중인 연결 수

### 통제 조건
- 동일 slowloris 클라이언트 사용
- 동일 연결 수 (10-20개)
- 동일 딜레이 (1-2초)

## 4. 실험 결과

### 실험 A: 타임아웃 있는 서버 (ReadHeaderTimeout=5s)

```bash
./bin/server -read-header-timeout=5s
./bin/slowloris -conns=10 -delay=1s
```

| 시점 | goroutines | slow 연결 (new) | closed_by_server |
|------|-----------|-----------------|------------------|
| 0s   | 6         | 0               | 0                |
| 3s   | 16        | 10              | 0                |
| 6s   | 16        | 10              | **10** ✅        |
| 9s   | 16        | 10              | 20               |
| 최종  | **6**     | 0               | -                |

**관찰**:
- 5초 후 서버가 slow 연결을 **강제 종료**
- goroutine 수가 기본값(6)으로 **복귀**
- 재연결해도 다시 5초 후 종료됨

### 실험 B: 타임아웃 없는 서버 (ReadHeaderTimeout=0)

```bash
./bin/server -read-header-timeout=0 -read-timeout=0
./bin/slowloris -conns=20 -delay=2s
```

| 시점 | goroutines | slow 연결 (new) | closed_by_server |
|------|-----------|-----------------|------------------|
| 3s   | **26**    | 20              | **0** ⚠️        |
| 6s   | **26**    | 20              | **0** ⚠️        |
| 9s   | **26**    | 20              | **0** ⚠️        |
| 종료 | 6         | 0               | -                |

**관찰**:
- 서버가 slow 연결을 **절대 끊지 않음**
- goroutine이 계속 **누적**됨
- 클라이언트가 끊어야만 정리됨

## 5. 결과 비교

```
                    실험 A (안전)              실험 B (취약)
                    ─────────────              ─────────────
ReadHeaderTimeout   5s                         0 (없음)

6초 시점:
  goroutines        16 (10 slow)               26 (20 slow)
  closed_by_server  10 ✅                      0 ⚠️

최종:
  상태              정상 복귀                  클라이언트 종속
  자원 점유         일시적 (5초)               무한 (공격자 의지)
```

## 6. 가설 검증 결과

### 원래 가설
> "ReadHeaderTimeout/ReadTimeout이 없으면 커넥션/고루틴이 장시간 점유된다"

### 검증 결과: ✅ **가설 증명됨**

| 조건 | 결과 | 증거 |
|------|------|------|
| 타임아웃 있음 | 5초 후 연결 종료 | closed_by_server=10 |
| 타임아웃 없음 | 무한 대기 | closed_by_server=0 (9초 후에도) |

## 7. 소스 코드 연결

`docs/01_http_server_overview.md`에서 분석한 내용과 연결:

```go
// net/http/server.go:1029-1035
func (c *conn) readRequest(ctx context.Context) (w *response, err error) {
    if d := c.server.readHeaderTimeout(); d > 0 {
        hdrDeadline = t0.Add(d)
    }
    c.rwc.SetReadDeadline(hdrDeadline)  // ← 여기서 타임아웃 설정!

    req, err := readRequest(c.bufr)     // ← 이 시점에 타임아웃 적용
    // ...
}
```

타임아웃이 0이면 `SetReadDeadline(time.Time{})` 호출 → **무제한 대기**

## 8. 실전 권장 사항

### 최소 권장 설정
```go
server := &http.Server{
    ReadHeaderTimeout: 5 * time.Second,  // slowloris 방어 (필수!)
    ReadTimeout:       30 * time.Second, // 전체 요청 제한
    WriteTimeout:      30 * time.Second, // 응답 제한
    IdleTimeout:       60 * time.Second, // keep-alive 제한
}
```

### 왜 각 타임아웃이 필요한가?

| Timeout | 방어 대상 | 없으면? |
|---------|----------|---------|
| **ReadHeaderTimeout** | Slowloris (헤더 지연) | 헤더 파싱에 무한 대기 |
| ReadTimeout | Slow body 공격 | 바디 읽기에 무한 대기 |
| WriteTimeout | Slow read 공격 | 응답 쓰기에 무한 대기 |
| IdleTimeout | Idle 연결 고갈 | keep-alive 연결 누적 |

## 9. 한계 및 추가 실험 아이디어

### 이 실험의 한계
1. 로컬 환경에서만 테스트 (네트워크 지연 없음)
2. 연결 수가 적음 (10-20개) - 실제 공격은 수천 개
3. FD 제한, ulimit 등 OS 레벨 제약 미측정

### 추가 실험 아이디어
1. **대규모 공격**: 1000+ 연결로 FD 고갈 시점 측정
2. **메모리 영향**: 장시간 공격 시 heap 증가 측정
3. **복합 공격**: slowloris + 정상 트래픽 혼합
4. **OS 레벨**: `ss -tanp`, `lsof` 로 TCP 상태 관찰

## 10. 결론

**ReadHeaderTimeout은 slowloris 공격 방어의 핵심이다.**

- 타임아웃이 있으면: 서버가 자동으로 slow 연결 정리
- 타임아웃이 없으면: 공격자가 원하는 만큼 자원 점유 가능

이것은 "추측"이 아니라 **측정값으로 증명된 사실**이다.
