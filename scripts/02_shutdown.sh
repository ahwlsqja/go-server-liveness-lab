#!/bin/bash
# Graceful Shutdown 실험: Shutdown timeout 효과 검증
#
# 실험 목적:
#   Shutdown timeout이 in-flight 요청에 어떤 영향을 주는지 확인
#
# 사용법:
#   ./scripts/02_shutdown.sh
#
# 캡쳐 팁:
#   NO_COLOR=1 ./scripts/02_shutdown.sh 2>&1 | tee results/shutdown.txt

source "$(dirname "$0")/common.sh"

RESULT_FILE="$RESULTS_DIR/02_shutdown_$(date +%Y%m%d_%H%M%S).txt"

# 결과를 파일과 화면 모두에 출력
exec > >(tee -a "$RESULT_FILE") 2>&1

print_box "GRACEFUL SHUTDOWN EXPERIMENT"
echo ""
echo "Purpose: Verify shutdown timeout behavior with in-flight requests"
echo "Date: $(date)"
echo ""
separator

# 빌드
build_all

###############################################################################
# 실험 A: 충분한 Shutdown Timeout (30초)
###############################################################################
log_step "Experiment A: Shutdown Timeout=30s (Sufficient)"

echo "Configuration:"
echo "  - Shutdown timeout: 30s"
echo "  - Request duration: 5s (/sleep?ms=5000)"
echo "  - SIGTERM sent: 2s after request starts"
echo "  - Remaining time: ~3s (< 30s timeout)"
echo ""
echo "Expected: Request completes successfully"
echo ""
separator

SERVER_LOG_A="/tmp/shutdown_exp_a.log"
start_server 9090 "-shutdown-timeout=30s -write-timeout=60s" "$SERVER_LOG_A"

log_info "Sending 5-second request..."

# 백그라운드에서 curl 실행
CURL_OUTPUT_A="/tmp/curl_exp_a.txt"
curl -s -w "\nHTTP_CODE:%{http_code}\nTIME_TOTAL:%{time_total}" \
    "http://localhost:9090/sleep?ms=5000" > "$CURL_OUTPUT_A" 2>&1 &
CURL_PID=$!

# 2초 대기 후 SIGTERM
sleep 2
log_info "Sending SIGTERM to server..."
kill -TERM $SERVER_PID 2>/dev/null || true

# curl 완료 대기
wait $CURL_PID 2>/dev/null || true
CURL_EXIT_A=$?

echo ""
echo "Results:"
echo "  curl exit code: $CURL_EXIT_A"

if [[ -f "$CURL_OUTPUT_A" ]]; then
    HTTP_CODE=$(grep "HTTP_CODE:" "$CURL_OUTPUT_A" | cut -d: -f2)
    TIME_TOTAL=$(grep "TIME_TOTAL:" "$CURL_OUTPUT_A" | cut -d: -f2)
    echo "  HTTP status: ${HTTP_CODE:-N/A}"
    echo "  Total time: ${TIME_TOTAL:-N/A}s"
fi

if [[ "$CURL_EXIT_A" -eq 0 ]]; then
    echo ""
    echo "  [PASS] Request completed successfully during shutdown"
else
    echo ""
    echo "  [FAIL] Request was interrupted"
fi

# 서버 로그 확인
echo ""
echo "Server shutdown log:"
grep -E "(shutdown|gracefully|elapsed)" "$SERVER_LOG_A" 2>/dev/null | \
    sed 's/\x1b\[[0-9;]*m//g' | tail -3 | sed 's/^/  /'

wait $SERVER_PID 2>/dev/null || true
unset SERVER_PID
sleep 2

###############################################################################
# 실험 B: 부족한 Shutdown Timeout (2초)
###############################################################################
log_step "Experiment B: Shutdown Timeout=2s (Insufficient)"

echo "Configuration:"
echo "  - Shutdown timeout: 2s"
echo "  - Request duration: 5s (/sleep?ms=5000)"
echo "  - SIGTERM sent: 1s after request starts"
echo "  - Remaining time: ~4s (> 2s timeout)"
echo ""
echo "Expected: Request interrupted, timeout error"
echo ""
separator

SERVER_LOG_B="/tmp/shutdown_exp_b.log"
start_server 9090 "-shutdown-timeout=2s -write-timeout=60s" "$SERVER_LOG_B"

log_info "Sending 5-second request..."

# 백그라운드에서 curl 실행
CURL_OUTPUT_B="/tmp/curl_exp_b.txt"
curl -s -w "\nHTTP_CODE:%{http_code}\nTIME_TOTAL:%{time_total}" \
    "http://localhost:9090/sleep?ms=5000" > "$CURL_OUTPUT_B" 2>&1 &
CURL_PID=$!

# 1초 대기 후 SIGTERM
sleep 1
log_info "Sending SIGTERM to server..."
kill -TERM $SERVER_PID 2>/dev/null || true

# curl 완료 대기
wait $CURL_PID 2>/dev/null || true
CURL_EXIT_B=$?

echo ""
echo "Results:"
echo "  curl exit code: $CURL_EXIT_B"

if [[ -f "$CURL_OUTPUT_B" ]]; then
    HTTP_CODE=$(grep "HTTP_CODE:" "$CURL_OUTPUT_B" | cut -d: -f2)
    TIME_TOTAL=$(grep "TIME_TOTAL:" "$CURL_OUTPUT_B" | cut -d: -f2)
    echo "  HTTP status: ${HTTP_CODE:-N/A}"
    echo "  Total time: ${TIME_TOTAL:-N/A}s"
fi

if [[ "$CURL_EXIT_B" -ne 0 ]]; then
    echo ""
    echo "  [EXPECTED] Request was interrupted (timeout)"
else
    echo ""
    echo "  [UNEXPECTED] Request completed despite short timeout"
fi

# 서버 로그 확인
echo ""
echo "Server shutdown log:"
grep -E "(shutdown|timeout|deadline|elapsed)" "$SERVER_LOG_B" 2>/dev/null | \
    sed 's/\x1b\[[0-9;]*m//g' | tail -3 | sed 's/^/  /'

wait $SERVER_PID 2>/dev/null || true
unset SERVER_PID

###############################################################################
# 결과 요약
###############################################################################
log_step "RESULTS SUMMARY"

cat << 'EOF'
┌────────────────────────────────────────────────────────────────┐
│                  GRACEFUL SHUTDOWN EXPERIMENT                  │
├────────────────────────────────────────────────────────────────┤
│                                                                │
│  Experiment A: Shutdown Timeout > Request Time                 │
│  ─────────────────────────────────────────────                 │
│  Shutdown: 30s, Request: 5s, SIGTERM at: 2s                    │
│  Result: Request completed successfully                        │
│  Server: Gracefully shutdown                                   │
│                                                                │
│  Experiment B: Shutdown Timeout < Request Time                 │
│  ─────────────────────────────────────────────                 │
│  Shutdown: 2s, Request: 5s, SIGTERM at: 1s                     │
│  Result: Request interrupted                                   │
│  Server: context deadline exceeded                             │
│                                                                │
├────────────────────────────────────────────────────────────────┤
│                                                                │
│  TIMELINE:                                                     │
│                                                                │
│  Exp A (30s timeout):                                          │
│  [Request]─────────────────[Complete]──[Shutdown]              │
│     0s          2s            5s          5s                   │
│              SIGTERM                                           │
│                                                                │
│  Exp B (2s timeout):                                           │
│  [Request]────[Timeout!]                                       │
│     0s    1s     3s                                            │
│        SIGTERM   ↑                                             │
│                  └─ Connection closed                          │
│                                                                │
├────────────────────────────────────────────────────────────────┤
│                                                                │
│  CONCLUSION:                                                   │
│  Set shutdown timeout > max expected request duration          │
│  Recommended: 30 seconds for typical APIs                      │
│                                                                │
└────────────────────────────────────────────────────────────────┘
EOF

echo ""
log_info "Results saved to: $RESULT_FILE"
