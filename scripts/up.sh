#!/usr/bin/env bash
# Creates the lab on a local kind cluster: 3 "GPU" workers with simulated GPUs, the controller,
# and a workload with a PodDisruptionBudget. Safe to re-run. Needs: docker, kind, kubectl.
set -euo pipefail
cd "$(dirname "$0")/.." || exit 1
# shellcheck source=scripts/lib.sh
source scripts/lib.sh

render() {
  sed -e "s|VALIDATION_IMAGE|${VALIDATION_IMAGE}|g" -e "s|PAUSE_IMAGE|${PAUSE_IMAGE}|g" \
      -e "s|IMAGE|${IMAGE}|g" "$1"
}

step "kind cluster ${CLUSTER_NAME}"
if ! kind get clusters | grep -qx "${CLUSTER_NAME}"; then
  kind create cluster --name "${CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" --config kind/cluster.yaml
fi
kubectl config use-context "kind-${CLUSTER_NAME}"
kubectl wait --for=condition=Ready nodes --all --timeout=5m

step "Build ${IMAGE} and load it into the nodes"
docker build -t "${IMAGE}" --build-arg VERSION="lab-$(date +%Y%m%d)" .
kind load docker-image "${IMAGE}" --name "${CLUSTER_NAME}"
docker pull -q "${VALIDATION_IMAGE}"
kind load docker-image "${VALIDATION_IMAGE}" --name "${CLUSTER_NAME}"

step "Advertise ${FAKE_GPUS_PER_NODE} fake nvidia.com/gpu on each GPU node"
# The documented way to advertise an extended resource without a device plugin.
for n in $(gpu_nodes); do
  kubectl patch node "$n" --subresource=status --type=json -p "[
    {\"op\":\"add\",\"path\":\"/status/capacity/nvidia.com~1gpu\",\"value\":\"${FAKE_GPUS_PER_NODE}\"},
    {\"op\":\"add\",\"path\":\"/status/allocatable/nvidia.com~1gpu\",\"value\":\"${FAKE_GPUS_PER_NODE}\"}]" >/dev/null
  echo "  $n: $(kubectl get node "$n" -o jsonpath='{.status.allocatable.nvidia\.com/gpu}') GPUs"
done

step "GPU simulator (stands in for dcgm-exporter)"
render lab/gpu-sim.yaml | kubectl apply -f -
kubectl -n gpu-telemetry rollout status ds/gpu-sim --timeout=3m

step "Remediation controller"
kubectl apply -f deploy/00-namespace.yaml -f deploy/10-rbac.yaml
render deploy/20-controller.yaml | kubectl apply -f -
kubectl -n gpu-remediation rollout status deploy/gpu-remediation --timeout=3m

step "Workload with a PodDisruptionBudget"
render lab/workload.yaml | kubectl apply -f -
kubectl -n ml rollout status deploy/trainer --timeout=3m
kubectl -n ml get pods -o wide

echo
echo "Lab ready. Run: bash scripts/test.sh"
