#!/usr/bin/env bash
# Helpers shared by the lab scripts.
# shellcheck source=scripts/versions.env
source "$(dirname "${BASH_SOURCE[0]}")/versions.env"

step() { printf '\n==> %s\n' "$*"; }
pass() { printf '  PASS  %s\n' "$*"; PASSED=$((PASSED + 1)); }
fail() { printf '  FAIL  %s\n' "$*"; FAILED=$((FAILED + 1)); }
PASSED=0
FAILED=0

gpu_nodes() { kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort; }

# Talk to the simulator on a node through the kind node container (it listens on the host network).
sim() {
  local node=$1 path=$2
  docker exec "$node" curl -fsS -X POST "http://127.0.0.1:9400${path}"
}

state() {
  local s
  s=$(kubectl get node "$1" -o jsonpath="{.metadata.annotations.gpu-remediation\.lab/state}")
  echo "${s:-Healthy}"
}

schedulable() { [ "$(kubectl get node "$1" -o jsonpath='{.spec.unschedulable}')" != "true" ]; }

annotation() { kubectl get node "$1" -o jsonpath="{.metadata.annotations.gpu-remediation\.lab/$2}"; }

# wait_state NODE STATE TIMEOUT_SECONDS
wait_state() {
  local node=$1 want=$2 timeout=${3:-120} i
  for ((i = 0; i < timeout; i += 2)); do
    [ "$(state "$node")" = "$want" ] && return 0
    sleep 2
  done
  echo "  timed out waiting for $node to be $want (is $(state "$node"))"
  return 1
}

events_for() {
  kubectl get events -n default --field-selector "involvedObject.kind=Node,involvedObject.name=$1" \
    -o jsonpath='{range .items[*]}{.reason}{"\n"}{end}'
}

trainer_ready() { kubectl -n ml get deploy trainer -o jsonpath='{.status.readyReplicas}'; }

controller_metrics() {
  kubectl get --raw "/api/v1/namespaces/gpu-remediation/services/gpu-remediation:metrics/proxy/metrics"
}
