#!/usr/bin/env bash
# Hand a quarantined node back to the controller after a human has fixed it (or the GPU was replaced).
# Usage: scripts/release-node.sh NODE [--keep-history]
set -euo pipefail
node=${1:?usage: release-node.sh NODE [--keep-history]}
ann=gpu-remediation.lab
remove=("$ann/state-" "$ann/plan-" "$ann/reason-" "$ann/since-" "$ann/validation-job-" "$ann/last-notice-" "$ann/clear-checks-")
if [ "${2:-}" != "--keep-history" ]; then
  remove+=("$ann/incidents-")
fi
kubectl annotate node "$node" "${remove[@]}" >/dev/null
kubectl label node "$node" "$ann/quarantined-" >/dev/null
kubectl uncordon "$node"
echo "released $node"
