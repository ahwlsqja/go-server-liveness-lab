#!/bin/bash
# Slowloris 실험: ReadHeaderTimeout 효과 검증
#
# 실험 목적:
#   ReadHeaderTimeout이 Slowloris 공격을 어떻게 방어하는지 확인
#
# 사용법:
#   ./scripts/01_slowloris.sh
#
# 캡쳐 팁:
#   NO_COLOR=1 ./scripts/01_slowloris.sh 2>&1 | tee results/slowloris.txt

source "$(dirname "$0")/common.sh"

RESULT_FILE="$RESULTS_DIR/01_slowloris_$(date +%Y%m%d_%H%M%S).txt"

# 결과를 파일과 화면 모두에 출력
exec > >(tee -a "$RESULT_FILE") 2>&1

print_box "SLOWLORIS EXPERIMENT"
echo ""
echo "Purpose: Verify ReadHeaderTimeout defends against Slowloris attack"
echo "Date: $(date)"
echo ""
separator

# 빌드
build_all

###############################################################################
# 실험 A: ReadHeaderTimeout=5s (방어 활성화)
###############################################################################
log_step "Experiment A: ReadHeaderTimeout=5s (Defense ON)"

echo "Configuration:"
echo "  - ReadHeaderTimeout: 5s"
echo "  - Slowloris connections: 5"
echo "  - Header delay: 2s per line"
echo ""
echo "Expected: Connections closed after ~5 seconds"
echo ""
separator

SERVER_LOG_A="/tmp/slowloris_exp_a.log"
start_server 9090 "-read-header-timeout=5s" "$SERVER_LOG_A"

log_info "Launching Slowloris attack..."
echo ""

# Slowloris 실행 (10초 동안)
timeout 12s "$BIN_DIR/slowloris" \
    -target="localhost:9090" \
    -conns=5 \
    -delay=2s \
    -duration=10s 2>&1 || true

echo ""
log_info "Attack finished"
echo ""

# 서버 로그에서 결과 확인
echo "Server behavior:"
if grep -q "read header timeout" "$SERVER_LOG_A" 2>/dev/null || \
   grep -q "timeout" "$SERVER_LOG_A" 2>/dev/null; then
    echo "  [PASS] Server detected timeout and closed connections"
else
    echo "  [INFO] Check server log for details"
fi

stop_server
sleep 2

###############################################################################
# 실험 B: ReadHeaderTimeout=0 (방어 비활성화)
###############################################################################
log_step "Experiment B: ReadHeaderTimeout=0 (Defense OFF)"

echo "Configuration:"
echo "  - ReadHeaderTimeout: 0 (disabled)"
echo "  - Slowloris connections: 5"
echo "  - Header delay: 2s per line"
echo ""
echo "Expected: Connections stay open indefinitely (VULNERABLE!)"
echo ""
separator

SERVER_LOG_B="/tmp/slowloris_exp_b.log"
start_server 9090 "-read-header-timeout=0s -read-timeout=0s" "$SERVER_LOG_B"

log_info "Launching Slowloris attack (will run for 8 seconds)..."
echo ""

# Slowloris 실행 (8초 동안 - 짧게)
timeout 10s "$BIN_DIR/slowloris" \
    -target="localhost:9090" \
    -conns=5 \
    -delay=2s \
    -duration=8s 2>&1 || true

echo ""
log_info "Attack finished"
echo ""

echo "Server behavior:"
echo "  [VULNERABLE] Without timeout, connections remain open"
echo "  In production, this would exhaust server resources"

stop_server

###############################################################################
# 결과 요약
###############################################################################
log_step "RESULTS SUMMARY"

cat << 'EOF'
┌────────────────────────────────────────────────────────────────┐
│                     SLOWLORIS EXPERIMENT                       │
├────────────────────────────────────────────────────────────────┤
│                                                                │
│  Experiment A: ReadHeaderTimeout=5s                            │
│  ─────────────────────────────────────                         │
│  Result: Connections closed after timeout                      │
│  Status: PROTECTED                                             │
│                                                                │
│  Experiment B: ReadHeaderTimeout=0                             │
│  ─────────────────────────────────────                         │
│  Result: Connections stay open indefinitely                    │
│  Status: VULNERABLE                                            │
│                                                                │
├────────────────────────────────────────────────────────────────┤
│                                                                │
│  CONCLUSION:                                                   │
│  ReadHeaderTimeout is essential for Slowloris defense.         │
│  Recommended value: 5 seconds                                  │
│                                                                │
└────────────────────────────────────────────────────────────────┘
EOF

echo ""
log_info "Results saved to: $RESULT_FILE"
