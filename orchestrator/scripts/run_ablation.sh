#!/usr/bin/env bash
# Run the controller-improvements ablation matrix against a 10 MiB target.
#
# For each named ablation we set the right ORCH_* env vars, tear down the
# stack, bring it up, wait for the orchestrator container to exit, archive
# the state-dir, and emit a per-ablation HTML report. The summary at the
# end prints one row per ablation (batches, stop_reason, axis deltas,
# F-coverage, total bytes).
#
# Usage:
#   cd orchestrator && ./scripts/run_ablation.sh
#
# Optional env:
#   ABLATION_OUT=/path  - archive root (default: ./ablation-runs)
#   QP_SCENARIOS_FILE   - if set, override target.yaml's qp_scenarios block
#                         (used to add/remove `noop` per ablation)

set -euo pipefail

cd "$(dirname "$0")/.."

ABLATION_OUT="${ABLATION_OUT:-./ablation-runs}"
mkdir -p "$ABLATION_OUT"

run_ablation() {
  local name="$1"
  local include_noop="$2"  # 0 or 1

  echo
  echo "================================================================"
  echo "ABLATION: $name"
  echo "  ORCH_EPSILON=${ORCH_EPSILON:-0.0}"
  echo "  ORCH_OVERSHOOT_PENALTY=${ORCH_OVERSHOOT_PENALTY:-0.0}"
  echo "  ORCH_GAS_AWARE_DISPATCH=${ORCH_GAS_AWARE_DISPATCH:-0}"
  echo "  ORCH_RESIDUAL_STOP_THRESHOLD=${ORCH_RESIDUAL_STOP_THRESHOLD:-0.0}"
  echo "  noop in qp_scenarios=$include_noop"
  echo "================================================================"

  # Swap qp_scenarios block in target.yaml by ADD/REMOVE the "  - noop"
  # line. Idempotent: removes any existing noop line first, then adds
  # back if requested. Avoids multi-line awk on macOS (which can't take
  # newlines in -v vars).
  sed -i.bak '/^  - noop$/d' target.yaml
  rm -f target.yaml.bak
  if [[ "$include_noop" == "1" ]]; then
    sed -i.bak '/^  - erc20tx$/a\
  - noop' target.yaml
    rm -f target.yaml.bak
  fi

  # Reset state.
  docker compose down -v >/dev/null 2>&1 || true
  rm -rf nethermind-data state
  mkdir -p nethermind-data state

  # Up.
  docker compose up -d >/dev/null
  echo "  waiting for nethermind health..."
  until docker inspect orchestrator-nethermind-1 \
    --format '{{.State.Health.Status}}' 2>/dev/null | grep -q healthy; do
    sleep 2
  done

  echo "  waiting for orchestrator..."
  local exit_code
  exit_code=$(docker wait orchestrator-orchestrator-1)
  echo "  orchestrator exit: $exit_code"

  if [[ "$exit_code" != "0" ]]; then
    echo "  WARNING: orchestrator did not exit 0; logs:"
    docker logs orchestrator-orchestrator-1 2>&1 | tail -20
  fi

  # Archive.
  local archive="$ABLATION_OUT/$name"
  rm -rf "$archive"
  mkdir -p "$archive"
  cp -r state "$archive/state"
  cp target.yaml "$archive/target.yaml"
  echo "  archived to $archive"
}

# ---- Ablation matrix ------------------------------------------------------

unset ORCH_EPSILON ORCH_OVERSHOOT_PENALTY ORCH_GAS_AWARE_DISPATCH \
      ORCH_RESIDUAL_STOP_THRESHOLD

# 0. baseline (current production: argmax, no extras)
run_ablation "0_baseline" 0

# 1. ε-greedy at 0.1
ORCH_EPSILON=0.1 run_ablation "1_epsilon" 0

# 2. overshoot penalty
ORCH_OVERSHOOT_PENALTY=1.0 run_ablation "2_penalty" 0

# 3. gas-aware dispatcher
ORCH_GAS_AWARE_DISPATCH=1 run_ablation "3_gas" 0

# 4. noop verb in registry
run_ablation "4_noop" 1

# 5. soft-stop on residual_norm
ORCH_RESIDUAL_STOP_THRESHOLD=8.0 run_ablation "5_soft_stop" 0

# 6. all together
ORCH_EPSILON=0.1 \
ORCH_OVERSHOOT_PENALTY=1.0 \
ORCH_GAS_AWARE_DISPATCH=1 \
ORCH_RESIDUAL_STOP_THRESHOLD=8.0 \
  run_ablation "6_all" 1

# ---- Tear down + summary --------------------------------------------------

docker compose down -v >/dev/null 2>&1 || true
echo
echo "================================================================"
echo "ABLATION SUMMARY"
echo "================================================================"
uv run python scripts/summarize_ablations.py "$ABLATION_OUT"
