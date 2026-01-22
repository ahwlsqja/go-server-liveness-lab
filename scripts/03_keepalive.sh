#!/bin/bash
# Keep-Alive 성능 실험: 연결 재사용 효과 측정
#
# 실험 목적:
#   HTTP Keep-Alive가 성능에 미치는 영향 측정
#
# 사용법:
#   ./scripts/03_keepalive.sh
#
# 캡쳐 팁:
#   NO_COLOR=1 ./scripts/03_keepalive.sh 2>&1 | tee results/keepalive.txt

source "$(dirname "$0")/common.sh"

RESULT_FILE="$RESULTS_DIR/03_keepalive_$(date +%Y%m%d_%H%M%S).txt"

# 결과를 파일과 화면 모두에 출력
exec > >(tee -a "$RESULT_FILE") 2>&1

print_box "KEEP-ALIVE PERFORMANCE EXPERIMENT"
echo ""
echo "Purpose: Measure performance impact of HTTP Keep-Alive"
echo "Date: $(date)"
echo ""
separator

# 빌드
build_all

# 테스트 설정
CONCURRENCY=10
DURATION="5s"
TARGET="http://localhost:9090/health"

###############################################################################
# 실험 A: Keep-Alive ON
###############################################################################
log_step "Experiment A: Keep-Alive ON"

echo "Configuration:"
echo "  - Keep-Alive: ENABLED"
echo "  - Concurrency: $CONCURRENCY"
echo "  - Duration: $DURATION"
echo "  - Target: $TARGET"
echo ""
separator

SERVER_LOG_A="/tmp/keepalive_exp_a.log"
start_server 9090 "" "$SERVER_LOG_A"

log_info "Running load test with Keep-Alive ON..."
echo ""

# loadgen 실행
LOADGEN_OUTPUT_A=$("$BIN_DIR/loadgen" \
    -target="$TARGET" \
    -concurrency=$CONCURRENCY \
    -duration=$DURATION \
    -keep-alive=true 2>&1)

echo "$LOADGEN_OUTPUT_A" | grep -A 20 "LOAD TEST RESULTS"

# 결과 추출
RPS_A=$(echo "$LOADGEN_OUTPUT_A" | grep "RPS:" | awk '{print $2}')
AVG_LATENCY_A=$(echo "$LOADGEN_OUTPUT_A" | grep "Average:" | awk '{print $2}')
P99_LATENCY_A=$(echo "$LOADGEN_OUTPUT_A" | grep "P99:" | awk '{print $2}')
TOTAL_A=$(echo "$LOADGEN_OUTPUT_A" | grep "Total Requests:" | awk '{print $3}')

stop_server
sleep 2

###############################################################################
# 실험 B: Keep-Alive OFF
###############################################################################
log_step "Experiment B: Keep-Alive OFF"

echo "Configuration:"
echo "  - Keep-Alive: DISABLED"
echo "  - Concurrency: $CONCURRENCY"
echo "  - Duration: $DURATION"
echo "  - Target: $TARGET"
echo ""
separator

SERVER_LOG_B="/tmp/keepalive_exp_b.log"
start_server 9090 "" "$SERVER_LOG_B"

log_info "Running load test with Keep-Alive OFF..."
echo ""

# loadgen 실행
LOADGEN_OUTPUT_B=$("$BIN_DIR/loadgen" \
    -target="$TARGET" \
    -concurrency=$CONCURRENCY \
    -duration=$DURATION \
    -keep-alive=false 2>&1)

echo "$LOADGEN_OUTPUT_B" | grep -A 20 "LOAD TEST RESULTS"

# 결과 추출
RPS_B=$(echo "$LOADGEN_OUTPUT_B" | grep "RPS:" | awk '{print $2}')
AVG_LATENCY_B=$(echo "$LOADGEN_OUTPUT_B" | grep "Average:" | awk '{print $2}')
P99_LATENCY_B=$(echo "$LOADGEN_OUTPUT_B" | grep "P99:" | awk '{print $2}')
TOTAL_B=$(echo "$LOADGEN_OUTPUT_B" | grep "Total Requests:" | awk '{print $3}')

stop_server

###############################################################################
# 결과 비교
###############################################################################
log_step "RESULTS COMPARISON"

# 성능 차이 계산 (awk로)
if [[ -n "$RPS_A" && -n "$RPS_B" ]]; then
    RPS_DIFF=$(awk "BEGIN {printf \"%.1f\", (($RPS_A - $RPS_B) / $RPS_B) * 100}")
fi

cat << EOF
┌────────────────────────────────────────────────────────────────┐
│                 KEEP-ALIVE PERFORMANCE RESULTS                 │
├────────────────────────────────────────────────────────────────┤
│                                                                │
│  Test Configuration:                                           │
│    Concurrency: $CONCURRENCY workers                                      │
│    Duration: $DURATION                                              │
│    Target: /health endpoint                                    │
│                                                                │
├─────────────────────┬──────────────────┬───────────────────────┤
│     Metric          │  Keep-Alive ON   │   Keep-Alive OFF      │
├─────────────────────┼──────────────────┼───────────────────────┤
│  Total Requests     │  $(printf "%-16s" "${TOTAL_A:-N/A}")│   $(printf "%-19s" "${TOTAL_B:-N/A}")│
│  RPS                │  $(printf "%-16s" "${RPS_A:-N/A}")│   $(printf "%-19s" "${RPS_B:-N/A}")│
│  Avg Latency        │  $(printf "%-16s" "${AVG_LATENCY_A:-N/A}")│   $(printf "%-19s" "${AVG_LATENCY_B:-N/A}")│
│  P99 Latency        │  $(printf "%-16s" "${P99_LATENCY_A:-N/A}")│   $(printf "%-19s" "${P99_LATENCY_B:-N/A}")│
├─────────────────────┴──────────────────┴───────────────────────┤
│                                                                │
│  Performance Difference:                                       │
│    Keep-Alive ON is ~${RPS_DIFF:-?}% faster in RPS                         │
│                                                                │
├────────────────────────────────────────────────────────────────┤
│                                                                │
│  WHY KEEP-ALIVE IS FASTER:                                     │
│                                                                │
│  Without Keep-Alive (new connection per request):              │
│    [TCP SYN] → [SYN-ACK] → [ACK] → [HTTP] → [FIN]              │
│       ~1ms       ~1ms       ~1ms    ~1ms     ~1ms              │
│    Total overhead: ~4-5ms per request                          │
│                                                                │
│  With Keep-Alive (reuse connection):                           │
│    [TCP handshake once] → [HTTP] → [HTTP] → [HTTP] → ...       │
│         ~3ms (once)        ~1ms     ~1ms     ~1ms              │
│    Total overhead: ~3ms + 1ms per request                      │
│                                                                │
├────────────────────────────────────────────────────────────────┤
│                                                                │
│  CONCLUSION:                                                   │
│  Keep-Alive significantly improves throughput by eliminating   │
│  TCP handshake overhead for each request.                      │
│                                                                │
│  Recommended: Always enable Keep-Alive for high-throughput     │
│  scenarios. Set appropriate IdleTimeout (60s recommended).     │
│                                                                │
└────────────────────────────────────────────────────────────────┘
EOF

echo ""
log_info "Results saved to: $RESULT_FILE"
