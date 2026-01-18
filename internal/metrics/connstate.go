// Package metrics provides connection state tracking for http.Server.
//
// 이 패키지는 http.Server의 ConnState 훅과 연동하여
// TCP 연결의 상태 전이(NEW → ACTIVE → IDLE → CLOSED)를 추적한다.
//
// Go net/http의 ConnState 상태 정의 (net/http/server.go):
//   - StateNew:    새 연결이 accept됨, 아직 데이터 없음
//   - StateActive: 요청을 처리 중 (헤더 읽기 시작 ~ 응답 완료)
//   - StateIdle:   keep-alive 상태로 다음 요청 대기 중
//   - StateHijacked: 연결이 hijack됨 (WebSocket 등)
//   - StateClosed: 연결 종료됨
package metrics

import (
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog"
)

// ConnStateCounter tracks the count of connections in each state.
type ConnStateCounter struct {
	// 각 상태별 현재 연결 수 (atomic으로 thread-safe)
	stateNew      atomic.Int64
	stateActive   atomic.Int64
	stateIdle     atomic.Int64
	stateHijacked atomic.Int64

	// 누적 카운터 (총 연결 수 추적)
	totalAccepted atomic.Int64
	totalClosed   atomic.Int64

	// 연결별 현재 상태 추적 (상태 전이 시 이전 상태 감소 위해 필요)
	connStates map[net.Conn]http.ConnState
	mu         sync.RWMutex

	logger zerolog.Logger
}

// NewConnStateCounter creates a new connection state counter.
func NewConnStateCounter(logger zerolog.Logger) *ConnStateCounter {
	return &ConnStateCounter{
		connStates: make(map[net.Conn]http.ConnState),
		logger:     logger,
	}
}

// TrackConnState is the callback function for http.Server.ConnState.
// http.Server가 연결 상태 변경 시 이 함수를 호출한다.
//
// 호출 시점 (Go 1.23.4 기준 net/http/server.go):
//   - StateNew:    Accept 직후, serve() goroutine 시작 전
//   - StateActive: 첫 바이트 읽기 시작 시
//   - StateIdle:   응답 완료 후 keep-alive 대기 진입 시
//   - StateClosed: 연결 종료 시 (정상 종료, 타임아웃, 에러 등)
func (c *ConnStateCounter) TrackConnState(conn net.Conn, state http.ConnState) {
	c.mu.Lock()
	prevState, existed := c.connStates[conn]

	// 이전 상태 카운터 감소
	if existed {
		c.decrementState(prevState)
	}

	// 새 상태 카운터 증가
	switch state {
	case http.StateNew:
		c.stateNew.Add(1)
		c.totalAccepted.Add(1)
		c.connStates[conn] = state

	case http.StateActive:
		c.stateActive.Add(1)
		c.connStates[conn] = state

	case http.StateIdle:
		c.stateIdle.Add(1)
		c.connStates[conn] = state

	case http.StateHijacked:
		c.stateHijacked.Add(1)
		c.connStates[conn] = state

	case http.StateClosed:
		// Closed 상태는 맵에서 제거 (메모리 누수 방지)
		delete(c.connStates, conn)
		c.totalClosed.Add(1)
	}
	c.mu.Unlock()

	// 로깅 (lock 밖에서)
	remoteAddr := "unknown"
	if conn != nil {
		remoteAddr = conn.RemoteAddr().String()
	}

	c.logger.Debug().
		Str("remote_addr", remoteAddr).
		Str("prev_state", stateName(prevState)).
		Str("new_state", stateName(state)).
		Msg("connection state changed")
}

// decrementState decreases the counter for the given state.
// 호출자가 lock을 이미 획득한 상태여야 함.
func (c *ConnStateCounter) decrementState(state http.ConnState) {
	switch state {
	case http.StateNew:
		c.stateNew.Add(-1)
	case http.StateActive:
		c.stateActive.Add(-1)
	case http.StateIdle:
		c.stateIdle.Add(-1)
	case http.StateHijacked:
		c.stateHijacked.Add(-1)
	}
}

// Snapshot returns the current state of all counters.
type Snapshot struct {
	New      int64 `json:"new"`
	Active   int64 `json:"active"`
	Idle     int64 `json:"idle"`
	Hijacked int64 `json:"hijacked"`

	TotalAccepted int64 `json:"total_accepted"`
	TotalClosed   int64 `json:"total_closed"`
}

// GetSnapshot returns a point-in-time snapshot of connection states.
func (c *ConnStateCounter) GetSnapshot() Snapshot {
	return Snapshot{
		New:           c.stateNew.Load(),
		Active:        c.stateActive.Load(),
		Idle:          c.stateIdle.Load(),
		Hijacked:      c.stateHijacked.Load(),
		TotalAccepted: c.totalAccepted.Load(),
		TotalClosed:   c.totalClosed.Load(),
	}
}

// stateName returns human-readable name for http.ConnState.
func stateName(state http.ConnState) string {
	switch state {
	case http.StateNew:
		return "new"
	case http.StateActive:
		return "active"
	case http.StateIdle:
		return "idle"
	case http.StateHijacked:
		return "hijacked"
	case http.StateClosed:
		return "closed"
	default:
		return "unknown"
	}
}
