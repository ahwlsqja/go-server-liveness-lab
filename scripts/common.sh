#!/bin/bash
# 공통 함수 및 설정

set -e

# 색상 (NO_COLOR 환경변수로 비활성화 가능)
if [[ -z "$NO_COLOR" ]]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[0;33m'
    BLUE='\033[0;34m'
    CYAN='\033[0;36m'
    BOLD='\033[1m'
    NC='\033[0m' # No Color
else
    RED=''
    GREEN=''
    YELLOW=''
    BLUE=''
    CYAN=''
    BOLD=''
    NC=''
fi

# 프로젝트 루트
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
BIN_DIR="$PROJECT_ROOT/bin"
RESULTS_DIR="$PROJECT_ROOT/results"

# 결과 디렉토리 생성
mkdir -p "$RESULTS_DIR"

# 타임스탬프
timestamp() {
    date "+%Y-%m-%d %H:%M:%S"
}

# 로그 함수
log_info() {
    echo -e "${BLUE}[$(timestamp)]${NC} ${GREEN}INFO${NC}  $1"
}

log_warn() {
    echo -e "${BLUE}[$(timestamp)]${NC} ${YELLOW}WARN${NC}  $1"
}

log_error() {
    echo -e "${BLUE}[$(timestamp)]${NC} ${RED}ERROR${NC} $1"
}

log_step() {
    echo -e "\n${BOLD}${CYAN}=== $1 ===${NC}\n"
}

# 구분선
separator() {
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
}

# 박스 출력
print_box() {
    local title="$1"
    local width=60
    local padding=$(( (width - ${#title} - 2) / 2 ))

    echo "┌$(printf '─%.0s' $(seq 1 $width))┐"
    printf "│%*s %s %*s│\n" $padding "" "$title" $padding ""
    echo "└$(printf '─%.0s' $(seq 1 $width))┘"
}

# 서버 시작 및 대기
start_server() {
    local port="${1:-9090}"
    local extra_args="${2:-}"
    local log_file="${3:-/tmp/server_experiment.log}"

    log_info "Starting server on port $port..."

    # 기존 서버 종료
    pkill -f "bin/server" 2>/dev/null || true
    sleep 1

    # 서버 시작
    "$BIN_DIR/server" -port="$port" $extra_args > "$log_file" 2>&1 &
    SERVER_PID=$!

    # 서버 준비 대기
    for i in {1..30}; do
        if curl -s "http://localhost:$port/health" > /dev/null 2>&1; then
            log_info "Server ready (PID: $SERVER_PID)"
            return 0
        fi
        sleep 0.2
    done

    log_error "Server failed to start"
    return 1
}

# 서버 종료
stop_server() {
    local signal="${1:-TERM}"

    if [[ -n "${SERVER_PID:-}" ]]; then
        log_info "Stopping server (PID: $SERVER_PID) with SIG$signal..."
        kill -$signal $SERVER_PID 2>/dev/null || true
        wait $SERVER_PID 2>/dev/null || true
        unset SERVER_PID
    fi

    # 혹시 남아있는 프로세스 정리
    pkill -f "bin/server" 2>/dev/null || true
}

# 빌드
build_all() {
    log_step "Building binaries"

    cd "$PROJECT_ROOT"

    log_info "Building server..."
    go build -o "$BIN_DIR/server" ./cmd/server

    log_info "Building slowloris..."
    go build -o "$BIN_DIR/slowloris" ./cmd/slowloris

    log_info "Building loadgen..."
    go build -o "$BIN_DIR/loadgen" ./cmd/loadgen

    log_info "Build complete"
}

# 정리 (스크립트 종료 시)
cleanup() {
    stop_server
}

trap cleanup EXIT
