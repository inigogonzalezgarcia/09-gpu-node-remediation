// Package remediation runs the loop that takes unhealthy GPU nodes out of service,
// repairs and validates them, and puts them back:
//
//	Healthy ──(XID / ECC / ...)──▶ Draining ──▶ Repairing ──▶ Validating ──▶ Healthy
//	   │                              │                           │
//	   │ (thermal)                    │ (unrepairable or          │ (validation fails
//	   ▼                              ▼  repeat offender)         ▼  or times out)
//	Cordoned ──(cooled down)──▶ Healthy                     Quarantined (human / RMA)
//
// State lives in node annotations, so the controller can restart at any point and
// `kubectl get node -o yaml` shows exactly where each node is.
package remediation

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/health"
	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/k8s"
)

// Annotation and label keys.
const (
	prefix          = "gpu-remediation.lab/"
	AnnState        = prefix + "state"
	AnnPlan         = prefix + "plan"
	AnnReason       = prefix + "reason"
	AnnSince        = prefix + "since"
	AnnIncidents    = prefix + "incidents"
	AnnJob          = prefix + "validation-job"
	AnnClearChecks  = prefix + "clear-checks"
	AnnLastNotice   = prefix + "last-notice"
	LabelQuarantine = prefix + "quarantined"
)

// States.
const (
	Healthy     = "Healthy"
	Cordoned    = "Cordoned"
	Draining    = "Draining"
	Repairing   = "Repairing"
	Validating  = "Validating"
	Quarantined = "Quarantined"
)

const (
	planRepair     = "repair"
	planQuarantine = "quarantine"
)

// Cluster is what the controller needs from Kubernetes. *k8s.Client implements it.
type Cluster interface {
	ListNodes(selector string) ([]k8s.Node, error)
	PatchNode(name string, patch map[string]any) error
	PodsOnNode(node string) ([]k8s.Pod, error)
	Evict(namespace, name string) error
	CreateJob(namespace string, job map[string]any) error
	JobState(namespace, name string) (succeeded, failed bool, err error)
	DeleteJob(namespace, name string) error
	NodeEvent(namespace, node, eventType, reason, message string) error
}

// Telemetry returns the current GPU telemetry of a node.
type Telemetry interface {
	GPUs(node k8s.Node) ([]health.GPU, error)
}

// Repairer performs the repair step (GPU reset, reboot, vendor ticket...).
type Repairer interface {
	Repair(node k8s.Node) error
}

// Config controls the loop.
type Config struct {
	NodeSelector      string        // which nodes are GPU nodes
	MaxUnavailable    int           // nodes allowed out of service for remediation at the same time
	RepeatLimit       int           // incidents within RepeatWindow that send a node to quarantine
	RepeatWindow      time.Duration //
	ClearChecks       int           // consecutive clean checks before a cordoned node is released
	ValidationTimeout time.Duration
	Namespace         string // where validation Jobs run
	ValidationImage   string
	ValidationGPUs    int    // GPUs the validation Job requests (all of them, like a burn-in)
	AgentPort         int    // port of the telemetry agent on each node, used by the validation Job
	EventNamespace    string // Node events are written here
	DryRun            bool   // evaluate and log only; change nothing
	Health            health.Config
}

// Controller is the remediation loop.
type Controller struct {
	cfg      Config
	cluster  Cluster
	tel      Telemetry
	repairer Repairer
	log      *slog.Logger
	now      func() time.Time

	mu      sync.Mutex
	actions map[string]int    // action -> count, for /metrics
	states  map[string]string // node -> state, for /metrics
}

// New builds a controller.
func New(cfg Config, cluster Cluster, tel Telemetry, repairer Repairer, log *slog.Logger) *Controller {
	return &Controller{cfg: cfg, cluster: cluster, tel: tel, repairer: repairer, log: log, now: time.Now,
		actions: map[string]int{}, states: map[string]string{}}
}

func stateOf(n k8s.Node) string {
	if s := n.Annotations[AnnState]; s != "" {
		return s
	}
	return Healthy
}

func (c *Controller) count(action string) {
	c.mu.Lock()
	c.actions[action]++
	c.mu.Unlock()
}

// Reconcile runs one pass over all GPU nodes.
func (c *Controller) Reconcile() error {
	nodes, err := c.cluster.ListNodes(c.cfg.NodeSelector)
	if err != nil {
		return fmt.Errorf("list nodes: %w", err)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	inFlight := 0
	for _, n := range nodes {
		switch stateOf(n) {
		case Draining, Repairing, Validating:
			inFlight++
		}
	}

	states := map[string]string{}
	var errs []error
	for _, n := range nodes {
		var err error
		switch stateOf(n) {
		case Healthy:
			err = c.healthy(n, &inFlight)
		case Cordoned:
			err = c.cordoned(n, &inFlight)
		case Draining:
			err = c.draining(n)
		case Repairing:
			err = c.repairing(n)
		case Validating:
			err = c.validating(n)
		case Quarantined:
			// Waits for a human. Clearing the state annotation and the label hands the node back.
		default:
			err = fmt.Errorf("unknown state %q", stateOf(n))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", n.Name, err))
		}
		states[n.Name] = stateOf(n)
	}
	// Re-read states for /metrics (transitions above patched the API, not the local copies).
	if fresh, err := c.cluster.ListNodes(c.cfg.NodeSelector); err == nil {
		states = map[string]string{}
		for _, n := range fresh {
			states[n.Name] = stateOf(n)
		}
	}
	c.mu.Lock()
	c.states = states
	c.mu.Unlock()
	return errors.Join(errs...)
}

func (c *Controller) evaluate(n k8s.Node) (health.Verdict, error) {
	gpus, err := c.tel.GPUs(n)
	if err != nil {
		return health.Verdict{}, err
	}
	return health.Evaluate(gpus, c.cfg.Health), nil
}

// healthy: decide whether a node in service needs to be taken out.
func (c *Controller) healthy(n k8s.Node, inFlight *int) error {
	if n.Unschedulable {
		// Cordoned by someone else (planned maintenance, an operator): not ours to touch.
		return nil
	}
	v, err := c.evaluate(n)
	if err != nil {
		return c.notice(n, "Warning", "TelemetryUnavailable", "cannot read GPU telemetry: "+err.Error())
	}
	switch v.Action {
	case health.None:
		return nil
	case health.Watch:
		return c.notice(n, "Warning", "GPUWarning", v.Summary())
	case health.Cordon:
		if c.cfg.DryRun {
			c.log.Info("dry-run: would cordon", "node", n.Name, "reason", v.Summary())
			return nil
		}
		c.count("cordon")
		return c.transition(n, Cordoned, "", v.Summary(), true, "Warning", "GPUCordoned")
	default: // Drain or Quarantine
		return c.startDrain(n, v, inFlight)
	}
}

func (c *Controller) startDrain(n k8s.Node, v health.Verdict, inFlight *int) error {
	plan := planRepair
	reason := v.Summary()
	if v.Action == health.Quarantine {
		plan = planQuarantine
	} else if recent := c.recentIncidents(n); c.cfg.RepeatLimit > 0 && len(recent)+1 >= c.cfg.RepeatLimit {
		plan = planQuarantine
		reason = fmt.Sprintf("%s; repeat offender: %d incident(s) in the last %s", reason, len(recent)+1, c.cfg.RepeatWindow)
	}
	if c.cfg.DryRun {
		c.log.Info("dry-run: would cordon and drain", "node", n.Name, "plan", plan, "reason", reason)
		return nil
	}
	if *inFlight >= c.cfg.MaxUnavailable {
		return c.notice(n, "Warning", "RemediationDeferred",
			fmt.Sprintf("needs %s but %d node(s) already in remediation (max %d): %s", plan, *inFlight, c.cfg.MaxUnavailable, reason))
	}
	*inFlight++
	c.count("drain")
	return c.transition(n, Draining, plan, reason, true, "Warning", "GPUDrainStarted")
}

// cordoned: a thermal cordon is released after enough clean checks, or escalates.
func (c *Controller) cordoned(n k8s.Node, inFlight *int) error {
	v, err := c.evaluate(n)
	if err != nil {
		return c.notice(n, "Warning", "TelemetryUnavailable", "cannot read GPU telemetry: "+err.Error())
	}
	if v.Action >= health.Drain {
		return c.startDrain(n, v, inFlight)
	}
	if v.Action == health.Cordon {
		return c.cluster.PatchNode(n.Name, annotations(map[string]any{AnnClearChecks: "0"}))
	}
	clean, _ := strconv.Atoi(n.Annotations[AnnClearChecks])
	clean++
	if clean < c.cfg.ClearChecks {
		return c.cluster.PatchNode(n.Name, annotations(map[string]any{AnnClearChecks: strconv.Itoa(clean)}))
	}
	c.count("uncordon")
	return c.transition(n, Healthy, "", fmt.Sprintf("clean for %d consecutive checks", clean), false, "Normal", "GPUReturnedToService")
}

// draining: evict what can be evicted; PodDisruptionBudgets are enforced by the API server.
func (c *Controller) draining(n k8s.Node) error {
	pods, err := c.cluster.PodsOnNode(n.Name)
	if err != nil {
		return err
	}
	remaining, blocked := 0, 0
	for _, p := range pods {
		if !evictable(p, c.cfg.Namespace) {
			continue
		}
		remaining++
		if err := c.cluster.Evict(p.Namespace, p.Name); err != nil {
			if errors.Is(err, k8s.ErrBlocked) {
				blocked++
				continue
			}
			return fmt.Errorf("evict %s/%s: %w", p.Namespace, p.Name, err)
		}
		c.count("evict")
	}
	if remaining > 0 {
		if blocked > 0 {
			c.log.Info("drain waiting for disruption budget", "node", n.Name, "blocked", blocked)
		}
		return nil // check again next pass
	}
	if n.Annotations[AnnPlan] == planQuarantine {
		c.count("quarantine")
		if err := c.cluster.PatchNode(n.Name, map[string]any{"metadata": map[string]any{
			"labels": map[string]any{LabelQuarantine: "true"}}}); err != nil {
			return err
		}
		return c.transition(n, Quarantined, planQuarantine, n.Annotations[AnnReason], true, "Warning", "GPUQuarantined")
	}
	if err := c.transition(n, Repairing, planRepair, n.Annotations[AnnReason], true, "Normal", "GPUDrained"); err != nil {
		return err
	}
	return c.repairing(n)
}

// repairing: run the repair action, then start validation.
func (c *Controller) repairing(n k8s.Node) error {
	if err := c.repairer.Repair(n); err != nil {
		return c.notice(n, "Warning", "GPURepairFailed", err.Error())
	}
	c.count("repair")
	job := fmt.Sprintf("gpu-validate-%s-%d", n.Name, c.now().Unix())
	if len(job) > 63 {
		job = job[len(job)-63:]
		job = strings.TrimLeft(job, "-")
	}
	if err := c.cluster.CreateJob(c.cfg.Namespace, c.validationJob(job, n.Name)); err != nil {
		return fmt.Errorf("create validation job: %w", err)
	}
	if err := c.cluster.PatchNode(n.Name, annotations(map[string]any{AnnJob: job})); err != nil {
		return err
	}
	return c.transition(n, Validating, planRepair, n.Annotations[AnnReason], true, "Normal", "GPUValidationStarted")
}

// validating: wait for the validation Job, then check telemetry once more.
func (c *Controller) validating(n k8s.Node) error {
	job := n.Annotations[AnnJob]
	ok, failed, err := c.cluster.JobState(c.cfg.Namespace, job)
	if err != nil && !errors.Is(err, k8s.ErrNotFound) {
		return err
	}
	since, _ := time.Parse(time.RFC3339, n.Annotations[AnnSince])
	timedOut := !since.IsZero() && c.now().Sub(since) > c.cfg.ValidationTimeout
	switch {
	case ok:
		_ = c.cluster.DeleteJob(c.cfg.Namespace, job)
		v, err := c.evaluate(n)
		if err != nil {
			return err
		}
		if v.Action >= health.Cordon {
			return c.quarantine(n, "validation passed but telemetry still unhealthy: "+v.Summary())
		}
		c.count("uncordon")
		incidents := append(c.recentIncidents(n), c.now().UTC().Format(time.RFC3339))
		if err := c.cluster.PatchNode(n.Name, annotations(map[string]any{AnnIncidents: strings.Join(incidents, ","), AnnJob: nil})); err != nil {
			return err
		}
		return c.transition(n, Healthy, "", "repaired and validated", false, "Normal", "GPUReturnedToService")
	case failed || errors.Is(err, k8s.ErrNotFound):
		return c.quarantine(n, "validation job failed after repair")
	case timedOut:
		return c.quarantine(n, fmt.Sprintf("validation did not finish within %s", c.cfg.ValidationTimeout))
	}
	return nil
}

func (c *Controller) quarantine(n k8s.Node, reason string) error {
	c.count("quarantine")
	if err := c.cluster.PatchNode(n.Name, map[string]any{"metadata": map[string]any{
		"labels": map[string]any{LabelQuarantine: "true"}}}); err != nil {
		return err
	}
	return c.transition(n, Quarantined, planQuarantine, reason, true, "Warning", "GPUQuarantined")
}

// transition patches state, reason, timestamps and schedulability in one call and records an event.
func (c *Controller) transition(n k8s.Node, state, plan, reason string, unschedulable bool, eventType, eventReason string) error {
	ann := map[string]any{
		AnnState:       state,
		AnnReason:      reason,
		AnnSince:       c.now().UTC().Format(time.RFC3339),
		AnnClearChecks: nil,
		AnnLastNotice:  nil,
	}
	if plan == "" {
		ann[AnnPlan] = nil
	} else {
		ann[AnnPlan] = plan
	}
	if state == Healthy {
		ann[AnnState] = nil // a healthy node carries no remediation state
		ann[AnnReason] = nil
		ann[AnnSince] = nil
		ann[AnnPlan] = nil
	}
	patch := map[string]any{
		"metadata": map[string]any{"annotations": ann},
		"spec":     map[string]any{"unschedulable": unschedulable},
	}
	if err := c.cluster.PatchNode(n.Name, patch); err != nil {
		return err
	}
	c.log.Info("transition", "node", n.Name, "from", stateOf(n), "to", state, "reason", reason)
	if err := c.cluster.NodeEvent(c.cfg.EventNamespace, n.Name, eventType, eventReason, state+": "+reason); err != nil {
		c.log.Warn("could not record event", "node", n.Name, "error", err)
	}
	return nil
}

// notice records a warning once per distinct message, so a stuck condition does not flood events.
func (c *Controller) notice(n k8s.Node, eventType, reason, msg string) error {
	key := reason + ": " + msg
	if len(key) > 250 {
		key = key[:250]
	}
	if n.Annotations[AnnLastNotice] == key {
		return nil
	}
	c.log.Warn(strings.ToLower(reason), "node", n.Name, "message", msg)
	if c.cfg.DryRun {
		return nil
	}
	if err := c.cluster.NodeEvent(c.cfg.EventNamespace, n.Name, eventType, reason, msg); err != nil {
		c.log.Warn("could not record event", "node", n.Name, "error", err)
	}
	return c.cluster.PatchNode(n.Name, annotations(map[string]any{AnnLastNotice: key}))
}

func (c *Controller) recentIncidents(n k8s.Node) []string {
	var out []string
	for _, ts := range strings.Split(n.Annotations[AnnIncidents], ",") {
		t, err := time.Parse(time.RFC3339, strings.TrimSpace(ts))
		if err == nil && c.now().Sub(t) <= c.cfg.RepeatWindow {
			out = append(out, t.UTC().Format(time.RFC3339))
		}
	}
	return out
}

func annotations(a map[string]any) map[string]any {
	return map[string]any{"metadata": map[string]any{"annotations": a}}
}

// evictable skips DaemonSet pods, static (mirror) pods, finished pods and the controller's own validation Jobs.
func evictable(p k8s.Pod, ownNamespace string) bool {
	if p.Phase == "Succeeded" || p.Phase == "Failed" {
		return false
	}
	if _, mirror := p.Annotations["kubernetes.io/config.mirror"]; mirror {
		return false
	}
	for _, k := range p.OwnerKinds {
		if k == "DaemonSet" {
			return false
		}
		if k == "Job" && p.Namespace == ownNamespace && strings.HasPrefix(p.Name, "gpu-validate-") {
			return false
		}
	}
	return true
}

// validationJob is a burn-in style check pinned to the node: it requests every GPU and asks the
// node's telemetry agent to run its diagnostic. In production this would run DCGM diagnostics
// (dcgmi diag -r 3) or a NCCL test instead.
func (c *Controller) validationJob(name, node string) map[string]any {
	container := map[string]any{
		"name":    "validate",
		"image":   c.cfg.ValidationImage,
		"command": []string{"sh", "-c", fmt.Sprintf(`wget -q -O- "http://${HOST_IP}:%d/validate"`, c.cfg.AgentPort)},
		"env": []map[string]any{{"name": "HOST_IP", "valueFrom": map[string]any{
			"fieldRef": map[string]string{"fieldPath": "status.hostIP"}}}},
		"securityContext": map[string]any{
			"allowPrivilegeEscalation": false, "runAsNonRoot": true, "runAsUser": 65534,
			"capabilities": map[string]any{"drop": []string{"ALL"}},
		},
	}
	if c.cfg.ValidationGPUs > 0 {
		container["resources"] = map[string]any{"limits": map[string]any{"nvidia.com/gpu": strconv.Itoa(c.cfg.ValidationGPUs)}}
	}
	return map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{"name": name, "namespace": c.cfg.Namespace,
			"labels": map[string]string{"app.kubernetes.io/name": "gpu-validate", "gpu-remediation.lab/node": node}},
		"spec": map[string]any{
			"backoffLimit":            0,
			"activeDeadlineSeconds":   int(c.cfg.ValidationTimeout.Seconds()),
			"ttlSecondsAfterFinished": 600,
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]string{"app.kubernetes.io/name": "gpu-validate"}},
				"spec": map[string]any{
					"nodeName":                     node, // bypasses the scheduler: the node is cordoned
					"restartPolicy":                "Never",
					"automountServiceAccountToken": false,
					"tolerations":                  []map[string]string{{"operator": "Exists"}},
					"containers":                   []map[string]any{container},
				},
			},
		},
	}
}

// Snapshot returns current counters for /metrics.
func (c *Controller) Snapshot() (states map[string]string, actions map[string]int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	states = make(map[string]string, len(c.states))
	for k, v := range c.states {
		states[k] = v
	}
	actions = make(map[string]int, len(c.actions))
	for k, v := range c.actions {
		actions[k] = v
	}
	return states, actions
}
