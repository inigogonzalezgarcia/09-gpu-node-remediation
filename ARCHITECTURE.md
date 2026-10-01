# Architecture

```
            ┌────────────────────── control plane ──────────────────────┐
            │  gpu-remediation (Deployment, 1 replica, Recreate)         │
            │    every 3–15 s: list GPU nodes → per-node state machine   │
            │    /metrics  /healthz                                      │
            └───────┬───────────────────────┬────────────────────────────┘
       HTTP :9400   │                       │ Kubernetes API
   (node InternalIP)│                       │ patch node · evict · Job · Event
            ┌───────▼──────────┐   ┌────────▼──────────────────────────┐
            │ GPU node          │   │ API server                         │
            │  gpu-sim / dcgm-  │   │  enforces PodDisruptionBudgets on  │
            │  exporter (DS,    │   │  every eviction (429 when blocked) │
            │  hostNetwork)     │   └────────────────────────────────────┘
            │  validation Job   │
            │  (nodeName, all   │
            │   GPUs, /validate)│
            └───────────────────┘
```

## One pass

1. List nodes matching `-node-selector` and count those in `Draining`, `Repairing` or `Validating` (the budget in use).
2. For each node, act on its state:

| State | What happens | Next |
|---|---|---|
| Healthy | Scrape telemetry, evaluate. `none`: nothing. `watch`: one warning event. `cordon`: cordon. `drain`/`quarantine`: start a drain if the budget allows, otherwise a `RemediationDeferred` event | Cordoned / Draining |
| Cordoned | Escalate if the node now needs a drain; count clean checks; uncordon after `-clear-checks` | Healthy / Draining |
| Draining | Evict every evictable pod. 429 = PDB says wait; try again next pass. When empty: repair, or quarantine if that was the plan | Repairing / Quarantined |
| Repairing | Call the repairer (`POST /reset` on the agent), create the validation Job | Validating |
| Validating | Job succeeded and telemetry clean: record the incident, uncordon. Job failed, missing or timed out: quarantine | Healthy / Quarantined |
| Quarantined | Nothing. A human releases it (`scripts/release-node.sh`) | |

Each transition is one merge patch (state, reason, timestamp, `spec.unschedulable`) plus a Node event.

## State on the node

| Annotation | Meaning |
|---|---|
| `gpu-remediation.lab/state` | Current state; absent means Healthy |
| `gpu-remediation.lab/plan` | `repair` or `quarantine`, decided when the drain starts |
| `gpu-remediation.lab/reason` | Human-readable cause, e.g. `GPU 5: XID 79, GPU has fallen off the bus (drain)` |
| `gpu-remediation.lab/since` | When the node entered the state (used for the validation timeout) |
| `gpu-remediation.lab/incidents` | Timestamps of past repairs (repeat-offender rule) |
| `gpu-remediation.lab/validation-job` | Name of the running validation Job |
| `gpu-remediation.lab/clear-checks` | Consecutive clean checks while thermally cordoned |
| `gpu-remediation.lab/last-notice` | Last warning sent, so a stuck condition produces one event, not one per pass |

Label `gpu-remediation.lab/quarantined=true` makes quarantined nodes easy to select for the hardware team.

Keeping state in the API instead of in memory means the controller can crash or be redeployed at any point: the next pass reads the annotations and continues.

## What runs where

- **Controller:** control-plane node, so it is never on a node it drains. Metrics at `:8080/metrics` (`gpu_remediation_node_state`, `gpu_remediation_actions_total`, reconcile counters).
- **Telemetry agent:** one per GPU node with `hostNetwork` on port 9400, the same port as dcgm-exporter. In the lab it is `gpu-sim` in its own namespace (hostNetwork is not allowed by the `baseline` Pod Security level enforced on the controller's namespace).
- **Validation Job:** `nodeName` set directly (the node is cordoned, so the scheduler would refuse it), tolerates every taint, requests every GPU so nothing else can share the node while it runs, `backoffLimit: 0`, `activeDeadlineSeconds` = validation timeout.

## Failure handling

| Failure | Behaviour |
|---|---|
| Agent unreachable | `TelemetryUnavailable` warning once; node left as it is (no telemetry is not proof of a fault) |
| API error during a pass | Logged, counted in `gpu_remediation_reconcile_errors_total`, retried next pass |
| Eviction blocked by a PDB | Node stays Draining until the budget allows it; nothing is forced |
| Repair call fails | `GPURepairFailed` event; retried next pass |
| Controller restarts mid-cycle | Resumes from the annotations |
| `/healthz` | Fails if no pass completed for 4 intervals + 1 minute, so the liveness probe restarts a stuck loop |
