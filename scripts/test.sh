#!/usr/bin/env bash
# "cond && pass ... || fail ..." is safe here: pass always returns 0.
# shellcheck disable=SC2015
# End-to-end scenarios against the lab built by up.sh. Each scenario injects a fault through the
# GPU simulator and checks what the controller does to the node.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
# shellcheck source=scripts/lib.sh
source scripts/lib.sh

mapfile -t NODES < <(gpu_nodes)
[ "${#NODES[@]}" -eq 3 ] || { echo "expected 3 GPU nodes, found ${#NODES[@]}"; exit 1; }
for n in "${NODES[@]}"; do sim "$n" "/reset?all=1" >/dev/null; done

# The node running trainer replica 0: draining it is what the PodDisruptionBudget protects.
trainer_node() { kubectl -n ml get pods -l app=trainer -o jsonpath='{.items[0].spec.nodeName}'; }

# ---------------------------------------------------------------------------------------------
step "1. XID 13 (application error) on ${NODES[0]}: no action"
sim "${NODES[0]}" "/inject?gpu=2&xid=13" >/dev/null
sleep 12
if [ "$(state "${NODES[0]}")" = Healthy ] && schedulable "${NODES[0]}"; then
  pass "node untouched by an application XID"
else
  fail "node changed: $(state "${NODES[0]}")"
fi
sim "${NODES[0]}" /reset >/dev/null

# ---------------------------------------------------------------------------------------------
A=$(trainer_node)
step "2. XID 79 (fallen off the bus) on $A: full cycle, PodDisruptionBudget respected"
kubectl -n ml patch pdb trainer --type=json \
  -p '[{"op":"remove","path":"/spec/maxUnavailable"},{"op":"add","path":"/spec/minAvailable","value":3}]' >/dev/null
sim "$A" "/inject?gpu=5&xid=79" >/dev/null
if wait_state "$A" Draining 30 && ! schedulable "$A"; then pass "cordoned and draining"; else fail "did not start draining"; fi
sleep 15
on_node=$(kubectl -n ml get pods -l app=trainer --field-selector "spec.nodeName=$A" -o name | wc -l)
if [ "$(state "$A")" = Draining ] && [ "$on_node" -ge 1 ]; then
  pass "drain waits while the PDB allows no disruption ($on_node trainer pod(s) still on $A)"
else
  fail "PDB not respected: state $(state "$A"), $on_node trainer pod(s) on node"
fi
kubectl -n ml patch pdb trainer --type=json \
  -p '[{"op":"remove","path":"/spec/minAvailable"},{"op":"add","path":"/spec/maxUnavailable","value":1}]' >/dev/null
seen_validating=no
for ((i = 0; i < 120; i += 2)); do
  s=$(state "$A")
  [ "$s" = Validating ] && seen_validating=yes
  [ "$s" = Healthy ] && break
  sleep 2
done
if [ "$(state "$A")" = Healthy ] && schedulable "$A" && [ "$seen_validating" = yes ]; then
  pass "repaired, validated by a Job and returned to service"
else
  fail "cycle did not complete: state $(state "$A"), validating seen: $seen_validating"
fi
[ -n "$(annotation "$A" incidents)" ] && pass "incident recorded on the node" || fail "no incident recorded"
ev=$(events_for "$A" | tr '\n' ' ')
case "$ev" in *GPUDrainStarted*GPUReturnedToService* | *GPUReturnedToService*GPUDrainStarted*) pass "events: $ev" ;; *) fail "missing events: $ev" ;; esac
kubectl -n ml rollout status deploy/trainer --timeout=90s >/dev/null && [ "$(trainer_ready)" = 3 ] &&
  pass "workload back to 3/3 ready" || fail "workload not ready: $(trainer_ready)/3"

# ---------------------------------------------------------------------------------------------
B=${NODES[1]}
C=${NODES[2]}
[ "$B" = "$A" ] && B=${NODES[0]}
[ "$C" = "$A" ] && C=${NODES[0]}
step "3. Faults on $B and $C at once: only one node in remediation at a time (max-unavailable=1)"
sim "$B" "/inject?gpu=0&dbe=2" >/dev/null
sim "$C" "/inject?gpu=1&xid=48" >/dev/null
max=0
for ((i = 0; i < 240; i += 2)); do
  busy=0
  for n in "$B" "$C"; do
    case "$(state "$n")" in Draining | Repairing | Validating) busy=$((busy + 1)) ;; esac
  done
  [ "$busy" -gt "$max" ] && max=$busy
  [ "$(state "$B")" = Healthy ] && [ "$(state "$C")" = Healthy ] && [ "$i" -gt 6 ] && break
  sleep 2
done
[ "$max" -eq 1 ] && pass "never more than one node in remediation" || fail "max concurrent remediation was $max"
[ "$(state "$B")" = Healthy ] && [ "$(state "$C")" = Healthy ] && pass "both nodes repaired one after the other" ||
  fail "states: $B=$(state "$B") $C=$(state "$C")"
if (events_for "$B"; events_for "$C") | grep -q RemediationDeferred; then pass "the waiting node got a RemediationDeferred event"; else fail "no RemediationDeferred event"; fi

# ---------------------------------------------------------------------------------------------
step "4. XID 64 (row remapping failure) on $B: quarantine, no automatic repair"
bash scripts/release-node.sh "$B" >/dev/null # forget the incident from scenario 3
resets_before=$(docker exec "$B" curl -fsS http://127.0.0.1:9400/metrics | awk '/^gpu_sim_resets_total/ {print $2}')
sim "$B" "/inject?gpu=4&xid=64" >/dev/null
if wait_state "$B" Quarantined 60; then pass "quarantined"; else fail "not quarantined"; fi
[ "$(kubectl get node "$B" -o jsonpath='{.metadata.labels.gpu-remediation\.lab/quarantined}')" = true ] &&
  pass "labelled gpu-remediation.lab/quarantined=true" || fail "quarantine label missing"
resets_after=$(docker exec "$B" curl -fsS http://127.0.0.1:9400/metrics | awk '/^gpu_sim_resets_total/ {print $2}')
[ "$resets_before" = "$resets_after" ] && pass "no reset attempted" || fail "reset was attempted"
sleep 8
[ "$(state "$B")" = Quarantined ] && ! schedulable "$B" && pass "quarantine is sticky" || fail "quarantine was lifted"
sim "$B" "/reset?all=1" >/dev/null
bash scripts/release-node.sh "$B" >/dev/null
sleep 8
[ "$(state "$B")" = Healthy ] && schedulable "$B" && pass "released by a human (scripts/release-node.sh)" || fail "release failed"

# ---------------------------------------------------------------------------------------------
step "5. Second XID 79 on $A within the repeat window: repeat offender goes to quarantine"
sim "$A" "/inject?gpu=5&xid=79" >/dev/null
if wait_state "$A" Quarantined 90; then pass "quarantined"; else fail "not quarantined"; fi
case "$(annotation "$A" reason)" in *"repeat offender"*) pass "reason: $(annotation "$A" reason)" ;; *) fail "reason: $(annotation "$A" reason)" ;; esac
sim "$A" "/reset?all=1" >/dev/null
bash scripts/release-node.sh "$A" >/dev/null

# ---------------------------------------------------------------------------------------------
step "6. Fault that survives the reset on $B: validation Job fails, node quarantined"
sim "$B" "/inject?gpu=3&xid=79&sticky=1" >/dev/null
if wait_state "$B" Quarantined 150; then pass "quarantined"; else fail "not quarantined"; fi
case "$(annotation "$B" reason)" in *validation*) pass "reason: $(annotation "$B" reason)" ;; *) fail "reason: $(annotation "$B" reason)" ;; esac
sim "$B" "/reset?all=1" >/dev/null
bash scripts/release-node.sh "$B" >/dev/null

# ---------------------------------------------------------------------------------------------
T=$(trainer_node)
step "7. GPU at 95°C on $T: cordon only, running work stays, released after it cools down"
pods_before=$(kubectl -n ml get pods -l app=trainer --field-selector "spec.nodeName=$T" -o name | sort)
sim "$T" "/inject?gpu=0&temp=95" >/dev/null
if wait_state "$T" Cordoned 30 && ! schedulable "$T"; then pass "cordoned"; else fail "not cordoned"; fi
sleep 6
pods_after=$(kubectl -n ml get pods -l app=trainer --field-selector "spec.nodeName=$T" -o name | sort)
[ "$pods_before" = "$pods_after" ] && [ -n "$pods_after" ] && pass "nothing evicted" || fail "pods changed on a thermal cordon"
sim "$T" "/inject?gpu=0&temp=60" >/dev/null
if wait_state "$T" Healthy 30 && schedulable "$T"; then pass "uncordoned after consecutive clean checks"; else fail "still $(state "$T")"; fi

# ---------------------------------------------------------------------------------------------
step "8. Controller metrics"
m=$(controller_metrics)
echo "$m" | grep -E '^gpu_remediation_actions_total' | sed 's/^/    /'
for a in drain quarantine uncordon cordon repair; do
  v=$(echo "$m" | awk -v a="$a" -F' ' '$1 ~ "action=\""a"\"" {print $2}')
  [ "${v:-0}" -ge 1 ] && pass "actions_total{action=$a} = $v" || fail "actions_total{action=$a} = ${v:-missing}"
done
echo "$m" | grep -q '^gpu_remediation_reconcile_errors_total 0$' && pass "no reconcile errors" ||
  fail "reconcile errors: $(echo "$m" | grep reconcile_errors)"

printf '\n%d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
