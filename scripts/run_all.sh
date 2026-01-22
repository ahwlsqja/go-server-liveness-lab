#!/bin/bash
# 모든 실험 한번에 실행
#
# 사용법:
#   ./scripts/run_all.sh
#
# 캡쳐 팁:
#   NO_COLOR=1 ./scripts/run_all.sh 2>&1 | tee results/all_experiments.txt

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "╔════════════════════════════════════════════════════════════════╗"
echo "║           GO HTTP SERVER EXPERIMENTS - FULL SUITE              ║"
echo "║                                                                ║"
echo "║  This will run all experiments sequentially:                   ║"
echo "║    1. Slowloris (ReadHeaderTimeout test)                       ║"
echo "║    2. Graceful Shutdown (Shutdown timeout test)                ║"
echo "║    3. Keep-Alive Performance (Connection reuse test)           ║"
echo "║                                                                ║"
echo "╚════════════════════════════════════════════════════════════════╝"
echo ""
echo "Press Enter to start, or Ctrl+C to cancel..."
read

echo ""
echo "Starting experiments at $(date)"
echo ""

# 실험 1: Slowloris
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Running Experiment 1: Slowloris"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
"$SCRIPT_DIR/01_slowloris.sh"
echo ""
echo "Press Enter to continue to next experiment..."
read

# 실험 2: Shutdown
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Running Experiment 2: Graceful Shutdown"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
"$SCRIPT_DIR/02_shutdown.sh"
echo ""
echo "Press Enter to continue to next experiment..."
read

# 실험 3: Keep-Alive
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "Running Experiment 3: Keep-Alive Performance"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
"$SCRIPT_DIR/03_keepalive.sh"

echo ""
echo "╔════════════════════════════════════════════════════════════════╗"
echo "║                    ALL EXPERIMENTS COMPLETE                    ║"
echo "╚════════════════════════════════════════════════════════════════╝"
echo ""
echo "Results saved in: $(dirname "$SCRIPT_DIR")/results/"
echo "Finished at $(date)"
