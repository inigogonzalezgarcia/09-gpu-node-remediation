# Runbook

## Where is each node?

```bash
kubectl get nodes -l nvidia.com/gpu.present=true -o custom-columns=\
'NAME:.metadata.name,STATE:.metadata.annotations.gpu-remediation\.lab/state,REASON:.metadata.annotations.gpu-remediation\.lab/reason,UNSCHED:.spec.unschedulable'
kubectl describe node <node> | sed -n '/Events:/,$p'
```

No state annotation means Healthy.

## Alerts worth having

| Alert | Expression (sketch) | Meaning |
|---|---|---|
| Node quarantined | `gpu_remediation_node_state{state="Quarantined"} == 1` | Hardware ticket needed |
| Remediation stuck | `gpu_remediation_node_state{state="Draining"} == 1` for 30m | A PDB blocks the drain; talk to the workload owner |
| Fleet-wide errors | rate of `RemediationDeferred` events, or several nodes needing drain at once | Suspect a driver/firmware rollout, not hardware |
| Controller stuck | `time() - gpu_remediation_last_reconcile_timestamp_seconds > 300` | Loop not running |
| API errors | `increase(gpu_remediation_reconcile_errors_total[15m]) > 0` | Check controller logs and RBAC |

## Release a quarantined node

After the hardware is fixed or replaced:

```bash
bash scripts/release-node.sh <node>                 # also forgets past incidents
bash scripts/release-node.sh <node> --keep-history  # keep the repeat-offender history
```

It removes the state annotations and the quarantine label, then uncordons.

## Take a node out for planned maintenance

Cordon it yourself (`kubectl cordon`). The controller ignores nodes that are unschedulable without its own state annotation.

## Pause automation

Restart the controller with `-dry-run` (or scale it to 0). In dry-run it still evaluates and logs every decision.

## Drain stuck in Draining

```bash
kubectl get pdb -A
kubectl get pods -A --field-selector spec.nodeName=<node>
```

The controller never forces an eviction. Either the workload owner relaxes the PDB, more replicas become ready elsewhere, or you decide to delete the pod by hand.

## Check a single machine with a real GPU

```bash
gpuremediate check            # table, exit code = action
gpuremediate check -json
```

See the README for the Windows binary.
