#!/usr/bin/env bash
# What to look at when the lab misbehaves. Never fails.
cd "$(dirname "$0")/.." || exit 1
set -x
kubectl get nodes -o wide -L nvidia.com/gpu.present -L gpu-remediation.lab/quarantined
kubectl get nodes -o custom-columns='NAME:.metadata.name,STATE:.metadata.annotations.gpu-remediation\.lab/state,REASON:.metadata.annotations.gpu-remediation\.lab/reason,UNSCHED:.spec.unschedulable'
kubectl get pods -A -o wide
kubectl get pdb -A
kubectl -n gpu-remediation get jobs,pods
kubectl -n gpu-remediation logs deploy/gpu-remediation --tail=200
kubectl -n gpu-telemetry logs ds/gpu-sim --tail=50 --all-containers --prefix
kubectl get events -n default --sort-by=.lastTimestamp | tail -60
kubectl get --raw "/api/v1/namespaces/gpu-remediation/services/gpu-remediation:metrics/proxy/metrics"
true
