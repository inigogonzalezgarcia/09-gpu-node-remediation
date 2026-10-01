# GPU Node Remediation

A small Kubernetes controller that takes unhealthy GPU nodes out of service, repairs them, proves they work and puts them back, without breaking the workloads running on them. Tested end to end on every push against a kind cluster with simulated GPUs.

**DCGM telemetry → health rules (XID, ECC, row remapping, temperature) → cordon / drain / repair / validate / quarantine**

![ci](https://github.com/inigogonzalezgarcia/09-gpu-node-remediation/actions/workflows/ci.yml/badge.svg)

> A learning-in-public lab about "day 2" operations for GPU clusters. I don't have production GPU fleet experience; this project is how I am learning the problem. There is no real GPU in CI: the nodes run a simulator that serves the same metric names as NVIDIA's dcgm-exporter. The `check` mode below runs against a real GPU through `nvidia-smi`.

```
Healthy ──(XID 79, ECC…)──▶ Draining ──▶ Repairing ──▶ Validating ──▶ Healthy
   │                           │                          │
   │ (≥ 90 °C)                 │ (XID 64, repeat          │ (validation fails
   ▼                           ▼  offender)               ▼  or times out)
Cordoned ──(cooled down)──▶ Healthy                  Quarantined ──(human)──▶ Healthy
```

| Piece | What it does |
|---|---|
| `internal/health` | Turns DCGM fields into one action per node. The XID policy is built from NVIDIA's documented causes for each XID ([docs/xid-policy.md](docs/xid-policy.md)) |
| `internal/remediation` | The state machine. State lives in node annotations, so a restart resumes where it left off and `kubectl describe node` explains everything |
| `internal/k8s` | Under 300 lines of Kubernetes API calls with the standard library: list/patch nodes, evictions, Jobs, events ([why no client-go](docs/decisions.md#1-standard-library-only)) |
| `internal/smi` | Reads `nvidia-smi` CSV and kernel-log XIDs for the `check` mode |
| `cmd/gpu-sim` | Lab stand-in for dcgm-exporter, with fault injection (`/inject`), a fake GPU reset (`/reset`) and a burn-in endpoint (`/validate`) |

## Safety controls

- **Disruption budget for the fleet:** at most `-max-unavailable` nodes in remediation at once; the rest wait with a `RemediationDeferred` event.
- **Evictions, not deletions:** every pod goes through the Eviction API, so PodDisruptionBudgets are enforced by the API server. DaemonSet and static pods are skipped.
- **Thermal is not a hardware fault:** an overheating node is only cordoned; running work stays, and the cordon is lifted after N clean checks.
- **Prove it before trusting it:** after a repair a validation Job pinned to the node takes every GPU and runs the agent's diagnostic. Failure or timeout means quarantine.
- **Repeat offenders:** a second incident on the same node within the window goes straight to quarantine instead of another repair loop.
- **Hands off what isn't ours:** a node cordoned by someone else is ignored. Quarantine is never lifted automatically.
- **Dry-run mode** logs every decision without changing the cluster. Least-privilege RBAC; the controller runs on the control plane, so it can never drain itself.

## Run it

Needs Docker, [kind](https://kind.sigs.k8s.io/) and kubectl.

```bash
bash scripts/up.sh      # kind cluster with 3 "GPU" workers, simulator, controller, workload + PDB
bash scripts/test.sh    # the scenarios below
bash scripts/down.sh
```

Inject a fault yourself and watch:

```bash
docker exec gpu-lab-worker2 curl -s -X POST 'http://127.0.0.1:9400/inject?gpu=3&xid=79'
kubectl get nodes -o custom-columns='NAME:.metadata.name,STATE:.metadata.annotations.gpu-remediation\.lab/state,UNSCHEDULABLE:.spec.unschedulable' -w
```

## What the tests prove

`scripts/test.sh` runs in GitHub Actions on every push:

| # | Scenario | Expected |
|---|---|---|
| 1 | XID 13 (application error) | Node untouched |
| 2 | XID 79 while the PDB allows no disruption | Node cordoned, drain **waits**; once the PDB allows it: evict → reset → validation Job → back in service, workload 3/3 |
| 3 | Faults on two nodes at once | Never more than one in remediation; the second waits, then gets fixed |
| 4 | XID 64 (row remapping failure) | Quarantined and labelled, no reset attempted, stays quarantined until released by a human |
| 5 | Second XID 79 within the repeat window | Quarantined as a repeat offender |
| 6 | Fault that survives the reset | Validation Job fails → quarantined |
| 7 | GPU at 95 °C | Cordon only, nothing evicted, released after cooling down |
| 8 | Controller metrics | Every action counted, no reconcile errors |

Plus unit tests for the rules, the state machine (with a fake cluster), the API client, the parsers and the simulator.

Excerpt from a CI run (28 checks, 0 failed):

```
==> 2. XID 79 (fallen off the bus) on gpu-lab-worker: full cycle, PodDisruptionBudget respected
  PASS  cordoned and draining
  PASS  drain waits while the PDB allows no disruption (1 trainer pod(s) still on gpu-lab-worker)
  PASS  repaired, validated by a Job and returned to service
==> 5. Second XID 79 on gpu-lab-worker within the repeat window: repeat offender goes to quarantine
  PASS  reason: GPU 5: XID 79, GPU has fallen off the bus (drain); repeat offender: 2 incident(s) in the last 1h0m0s
==> 8. Controller metrics
    gpu_remediation_actions_total{action="drain"} 6
    gpu_remediation_actions_total{action="evict"} 8
    gpu_remediation_actions_total{action="repair"} 4
    gpu_remediation_actions_total{action="quarantine"} 3
```

## Check a real GPU (Linux or Windows)

The same health rules can run on any machine with an NVIDIA driver, no Kubernetes needed. CI builds `gpuremediate-windows-amd64.exe` and a Linux binary on every push (Actions → latest run → *gpuremediate-binaries*).

```powershell
.\gpuremediate-windows-amd64.exe check
.\gpuremediate-windows-amd64.exe check -json
```

```bash
journalctl -k > kern.log && ./gpuremediate-linux-amd64 check -xid-log kern.log
```

The exit code is the action (0 none, 1 watch, 2 cordon, 3 drain, 4 quarantine, 10 error), so it can gate a script. Consumer GPUs report ECC and row remapping as "not supported"; these show as `n/a` instead of a misleading zero. `nvidia-smi` does not expose XIDs: on Linux they come from the kernel log; on Windows they are not read.

## Documentation

- [ARCHITECTURE.md](ARCHITECTURE.md): the loop, state annotations, failure handling
- [docs/xid-policy.md](docs/xid-policy.md): which XID leads to which action, and why
- [docs/decisions.md](docs/decisions.md): design decisions and trade-offs
- [docs/runbook.md](docs/runbook.md): operating it, releasing quarantined nodes, troubleshooting

## Roadmap

- Run against real dcgm-exporter metrics on a GPU node and compare with the simulator.
- Leader election so the controller can run with more than one replica.
- DCGM diagnostics (`dcgmi diag`) or an NCCL test as the validation Job.
- Health checks on NVLink/InfiniBand fabric, not just the GPU.

## Customisation and contact

Want to talk about GPU node health, Kubernetes day 2 operations or a lab like this for your team? Get in touch:

- Email: [inigogonzalezgarcia@yahoo.es](mailto:inigogonzalezgarcia@yahoo.es)
- LinkedIn: [linkedin.com/in/igonzalez93](https://www.linkedin.com/in/igonzalez93)

## License

MIT
