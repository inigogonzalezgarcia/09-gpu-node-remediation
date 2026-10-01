# Design decisions

## 1. Standard library only

The controller talks to the Kubernetes API with `net/http` (`internal/k8s`, under 300 lines) instead of client-go.

- **For:** no dependencies to audit or update, a ~10 MB static binary, and every API call is visible and tested (`client_test.go` checks paths, verbs, the merge-patch content type and that a 429 on eviction becomes `ErrBlocked`).
- **Against:** no informers or watch caches, so each pass lists nodes again. Fine for hundreds of nodes every few seconds; for thousands, client-go informers (or controller-runtime) would be the right tool, and the `Cluster` interface in `internal/remediation` is where that swap would happen.

## 2. Polling, not watching

A pass every few seconds over all GPU nodes. GPU faults are not millisecond events and the next step (draining) takes minutes. Polling keeps the loop simple to reason about and naturally retries everything.

## 3. State in annotations

See ARCHITECTURE.md. Alternatives considered: a CRD per node (cleaner API, but one more thing to install and version for a lab) and in-memory state (lost on restart, which is exactly when you need it).

## 4. Taints vs cordon

The controller uses `spec.unschedulable` (cordon), which Kubernetes turns into the `node.kubernetes.io/unschedulable:NoSchedule` taint. A custom `NoExecute` taint would evict pods too, but it bypasses PodDisruptionBudgets. Evictions through the API respect them.

## 5. One fleet-wide budget

`-max-unavailable` caps how many nodes are being remediated at once. A bad driver rollout can make every node report errors at the same time; the budget turns that into one node out of service plus a stream of `RemediationDeferred` events, which is the signal to stop and look. Thermal cordons are not counted: they don't evict anything.

## 6. Validation before trust

A reset that "worked" is not evidence the GPU is healthy. The validation Job uses the node exactly like a workload would (pinned, all GPUs) and the result decides between service and quarantine. In the lab it calls the simulator's `/validate`; on real hardware it would run DCGM diagnostics (`dcgmi diag -r 3`) or an NCCL all-reduce test.

## 7. Quarantine is a dead end on purpose

Once a node is quarantined, only a human puts it back (`scripts/release-node.sh`). Automatic retries on hardware that already failed validation waste GPU hours of whatever lands on it next.

## 8. Single replica, no leader election

Two controllers acting on the same node could double-evict or fight over state. The Deployment uses `replicas: 1` and `strategy: Recreate`, so an upgrade never runs two copies. Leader election (a `coordination.k8s.io` Lease) is on the roadmap.

## 9. A simulator instead of a GPU

CI has no GPU and renting one for a lab is not justified. `gpu-sim` serves the same metric names as dcgm-exporter, so the controller code path is identical; the gap is that real dcgm-exporter metrics have more labels and fields, and real faults are messier than injected ones. The `check` mode exists to close part of that gap on a real machine.
