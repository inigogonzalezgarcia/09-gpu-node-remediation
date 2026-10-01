package remediation

import (
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/health"
	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/k8s"
)

// fakeCluster keeps nodes, pods and jobs in memory and applies merge patches the way the API server does
// for the fields the controller touches.
type fakeCluster struct {
	nodes   map[string]*k8s.Node
	pods    map[string][]k8s.Pod // node -> pods
	blocked map[string]bool      // pod name -> eviction refused (PDB)
	jobs    map[string]string    // job name -> "", "ok", "failed"
	events  []string
	evicted []string
}

func newFake(names ...string) *fakeCluster {
	f := &fakeCluster{nodes: map[string]*k8s.Node{}, pods: map[string][]k8s.Pod{}, blocked: map[string]bool{}, jobs: map[string]string{}}
	for _, n := range names {
		f.nodes[n] = &k8s.Node{Name: n, Labels: map[string]string{}, Annotations: map[string]string{}}
	}
	return f
}

func (f *fakeCluster) ListNodes(string) ([]k8s.Node, error) {
	var out []k8s.Node
	for _, n := range f.nodes {
		c := *n
		c.Annotations = map[string]string{}
		for k, v := range n.Annotations {
			c.Annotations[k] = v
		}
		out = append(out, c)
	}
	return out, nil
}

func (f *fakeCluster) PatchNode(name string, patch map[string]any) error {
	n := f.nodes[name]
	if md, ok := patch["metadata"].(map[string]any); ok {
		for field, target := range map[string]map[string]string{"annotations": n.Annotations, "labels": n.Labels} {
			if m, ok := md[field].(map[string]any); ok {
				for k, v := range m {
					if v == nil {
						delete(target, k)
					} else {
						target[k] = v.(string)
					}
				}
			}
		}
	}
	if spec, ok := patch["spec"].(map[string]any); ok {
		n.Unschedulable = spec["unschedulable"].(bool)
	}
	return nil
}

func (f *fakeCluster) PodsOnNode(node string) ([]k8s.Pod, error) { return f.pods[node], nil }

func (f *fakeCluster) Evict(ns, name string) error {
	if f.blocked[name] {
		return k8s.ErrBlocked
	}
	for node, pods := range f.pods {
		for i, p := range pods {
			if p.Name == name {
				f.pods[node] = append(pods[:i:i], pods[i+1:]...)
			}
		}
	}
	f.evicted = append(f.evicted, name)
	return nil
}

func (f *fakeCluster) CreateJob(ns string, job map[string]any) error {
	name := job["metadata"].(map[string]any)["name"].(string)
	f.jobs[name] = ""
	return nil
}

func (f *fakeCluster) JobState(ns, name string) (bool, bool, error) {
	s, ok := f.jobs[name]
	if !ok {
		return false, false, k8s.ErrNotFound
	}
	return s == "ok", s == "failed", nil
}

func (f *fakeCluster) DeleteJob(ns, name string) error { delete(f.jobs, name); return nil }

func (f *fakeCluster) NodeEvent(ns, node, typ, reason, msg string) error {
	f.events = append(f.events, node+" "+reason)
	return nil
}

func (f *fakeCluster) finishJobs(result string) {
	for k := range f.jobs {
		f.jobs[k] = result
	}
}

type fakeTelemetry map[string][]health.GPU

func (t fakeTelemetry) GPUs(n k8s.Node) ([]health.GPU, error) {
	g, ok := t[n.Name]
	if !ok {
		return nil, errors.New("agent unreachable")
	}
	return g, nil
}

// fakeRepairer clears the faults of a node, like a GPU reset that works.
type fakeRepairer struct {
	tel      fakeTelemetry
	repaired []string
	fail     bool
}

func (r *fakeRepairer) Repair(n k8s.Node) error {
	if r.fail {
		return errors.New("reset failed")
	}
	r.repaired = append(r.repaired, n.Name)
	r.tel[n.Name] = []health.GPU{{Index: "0", TempC: 40}}
	return nil
}

type harness struct {
	f   *fakeCluster
	tel fakeTelemetry
	rep *fakeRepairer
	c   *Controller
	now time.Time
}

func setup(t *testing.T, mutate func(*Config), nodes ...string) *harness {
	t.Helper()
	f := newFake(nodes...)
	tel := fakeTelemetry{}
	for _, n := range nodes {
		tel[n] = []health.GPU{{Index: "0", TempC: 40}}
	}
	rep := &fakeRepairer{tel: tel}
	cfg := Config{
		MaxUnavailable: 1, RepeatLimit: 2, RepeatWindow: 24 * time.Hour, ClearChecks: 2,
		ValidationTimeout: 10 * time.Minute, Namespace: "gpu-remediation", ValidationImage: "busybox",
		ValidationGPUs: 8, AgentPort: 9400, EventNamespace: "default", Health: health.DefaultConfig(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h := &harness{f: f, tel: tel, rep: rep, now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	h.c = New(cfg, f, tel, rep, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.c.now = func() time.Time { return h.now }
	return h
}

func (h *harness) reconcile(t *testing.T) {
	t.Helper()
	if err := h.c.Reconcile(); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(30 * time.Second)
}

func (h *harness) state(node string) string { return stateOf(*h.f.nodes[node]) }

func TestApplicationXIDTakesNoAction(t *testing.T) {
	h := setup(t, nil, "gpu-a")
	h.tel["gpu-a"] = []health.GPU{{Index: "0", XID: 13}}
	h.reconcile(t)
	if h.state("gpu-a") != Healthy || h.f.nodes["gpu-a"].Unschedulable {
		t.Fatalf("XID 13 should not touch the node: state %s", h.state("gpu-a"))
	}
}

func TestFullRepairCycleRespectsPDB(t *testing.T) {
	h := setup(t, nil, "gpu-a")
	h.f.pods["gpu-a"] = []k8s.Pod{
		{Namespace: "ml", Name: "trainer-0", OwnerKinds: []string{"StatefulSet"}, Phase: "Running"},
		{Namespace: "kube-system", Name: "dcgm-x", OwnerKinds: []string{"DaemonSet"}, Phase: "Running"},
	}
	h.f.blocked["trainer-0"] = true
	h.tel["gpu-a"] = []health.GPU{{Index: "3", XID: 79}}

	h.reconcile(t) // Healthy -> Draining
	if h.state("gpu-a") != Draining || !h.f.nodes["gpu-a"].Unschedulable {
		t.Fatalf("want Draining and cordoned, got %s", h.state("gpu-a"))
	}
	h.reconcile(t) // PDB blocks
	if h.state("gpu-a") != Draining || len(h.f.evicted) != 0 {
		t.Fatalf("drain must wait for the PDB: state %s evicted %v", h.state("gpu-a"), h.f.evicted)
	}
	h.f.blocked["trainer-0"] = false
	h.reconcile(t) // evicts
	h.reconcile(t) // only the DaemonSet pod is left -> Repairing -> Validating
	if h.state("gpu-a") != Validating {
		t.Fatalf("want Validating, got %s", h.state("gpu-a"))
	}
	if len(h.rep.repaired) != 1 || len(h.f.jobs) != 1 {
		t.Fatalf("want one repair and one validation job, got %v %v", h.rep.repaired, h.f.jobs)
	}
	for _, e := range h.f.evicted {
		if e == "dcgm-x" {
			t.Fatal("DaemonSet pods must not be evicted")
		}
	}
	h.f.finishJobs("ok")
	h.reconcile(t)
	n := h.f.nodes["gpu-a"]
	if h.state("gpu-a") != Healthy || n.Unschedulable {
		t.Fatalf("want back in service, got %s unschedulable=%v", h.state("gpu-a"), n.Unschedulable)
	}
	if n.Annotations[AnnIncidents] == "" || len(h.f.jobs) != 0 {
		t.Fatalf("incident should be recorded and the job cleaned up: %v %v", n.Annotations, h.f.jobs)
	}
}

func TestBudgetLimitsConcurrentRemediation(t *testing.T) {
	h := setup(t, nil, "gpu-a", "gpu-b")
	h.tel["gpu-a"] = []health.GPU{{Index: "0", XID: 79}}
	h.tel["gpu-b"] = []health.GPU{{Index: "0", XID: 48}}
	h.reconcile(t) // a -> Draining, b deferred
	h.reconcile(t) // a drained -> Repairing -> Validating, b still deferred
	if h.state("gpu-a") != Validating || h.state("gpu-b") != Healthy {
		t.Fatalf("only one node may be in remediation: a=%s b=%s", h.state("gpu-a"), h.state("gpu-b"))
	}
	if h.f.nodes["gpu-b"].Unschedulable {
		t.Fatal("deferred node must stay schedulable")
	}
	deferred := 0
	for _, e := range h.f.events {
		if e == "gpu-b RemediationDeferred" {
			deferred++
		}
	}
	if deferred != 1 {
		t.Fatalf("the deferral should be reported exactly once, got %d", deferred)
	}
	h.f.finishJobs("ok")
	h.reconcile(t) // a returns
	h.reconcile(t) // b starts
	if h.state("gpu-a") != Healthy || h.state("gpu-b") == Healthy {
		t.Fatalf("b should start once a is back: a=%s b=%s", h.state("gpu-a"), h.state("gpu-b"))
	}
}

func TestUnrepairableXIDQuarantines(t *testing.T) {
	h := setup(t, nil, "gpu-a")
	h.tel["gpu-a"] = []health.GPU{{Index: "1", XID: 64}}
	h.reconcile(t)
	h.reconcile(t)
	n := h.f.nodes["gpu-a"]
	if h.state("gpu-a") != Quarantined || n.Labels[LabelQuarantine] != "true" || !n.Unschedulable {
		t.Fatalf("want quarantined, got %s %v", h.state("gpu-a"), n.Labels)
	}
	if len(h.rep.repaired) != 0 {
		t.Fatal("a quarantined node must not be repaired automatically")
	}
	h.reconcile(t)
	if h.state("gpu-a") != Quarantined {
		t.Fatal("quarantine is sticky")
	}
}

func TestThermalCordonsThenReleases(t *testing.T) {
	h := setup(t, nil, "gpu-a")
	h.f.pods["gpu-a"] = []k8s.Pod{{Namespace: "ml", Name: "trainer-0", Phase: "Running"}}
	h.tel["gpu-a"] = []health.GPU{{Index: "0", TempC: 94}}
	h.reconcile(t)
	if h.state("gpu-a") != Cordoned || !h.f.nodes["gpu-a"].Unschedulable || len(h.f.evicted) != 0 {
		t.Fatalf("thermal should cordon without evicting: %s %v", h.state("gpu-a"), h.f.evicted)
	}
	h.tel["gpu-a"] = []health.GPU{{Index: "0", TempC: 70}}
	h.reconcile(t)
	if h.state("gpu-a") != Cordoned {
		t.Fatal("one clean check is not enough")
	}
	h.reconcile(t)
	if h.state("gpu-a") != Healthy || h.f.nodes["gpu-a"].Unschedulable {
		t.Fatalf("want released, got %s", h.state("gpu-a"))
	}
}

func TestRepeatOffenderIsQuarantined(t *testing.T) {
	h := setup(t, nil, "gpu-a")
	h.f.nodes["gpu-a"].Annotations[AnnIncidents] = h.now.Add(-2 * time.Hour).Format(time.RFC3339)
	h.tel["gpu-a"] = []health.GPU{{Index: "0", XID: 79}}
	h.reconcile(t)
	h.reconcile(t)
	if h.state("gpu-a") != Quarantined {
		t.Fatalf("second incident in the window should quarantine, got %s", h.state("gpu-a"))
	}
	if !strings.Contains(h.f.nodes["gpu-a"].Annotations[AnnReason], "repeat offender") {
		t.Fatalf("reason should say why: %q", h.f.nodes["gpu-a"].Annotations[AnnReason])
	}
}

func TestOldIncidentsDoNotCount(t *testing.T) {
	h := setup(t, nil, "gpu-a")
	h.f.nodes["gpu-a"].Annotations[AnnIncidents] = h.now.Add(-48 * time.Hour).Format(time.RFC3339)
	h.tel["gpu-a"] = []health.GPU{{Index: "0", XID: 79}}
	h.reconcile(t)
	h.reconcile(t)
	if h.state("gpu-a") != Validating {
		t.Fatalf("an incident outside the window should not quarantine, got %s", h.state("gpu-a"))
	}
}

func TestFailedValidationQuarantines(t *testing.T) {
	h := setup(t, nil, "gpu-a")
	h.tel["gpu-a"] = []health.GPU{{Index: "0", XID: 79}}
	h.reconcile(t)
	h.reconcile(t)
	h.f.finishJobs("failed")
	h.reconcile(t)
	if h.state("gpu-a") != Quarantined {
		t.Fatalf("want quarantined after failed validation, got %s", h.state("gpu-a"))
	}
}

func TestValidationTimeoutQuarantines(t *testing.T) {
	h := setup(t, nil, "gpu-a")
	h.tel["gpu-a"] = []health.GPU{{Index: "0", XID: 79}}
	h.reconcile(t)
	h.reconcile(t)
	h.now = h.now.Add(11 * time.Minute)
	h.reconcile(t)
	if h.state("gpu-a") != Quarantined {
		t.Fatalf("want quarantined after timeout, got %s", h.state("gpu-a"))
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	h := setup(t, func(c *Config) { c.DryRun = true }, "gpu-a")
	h.tel["gpu-a"] = []health.GPU{{Index: "0", XID: 79}}
	h.reconcile(t)
	n := h.f.nodes["gpu-a"]
	if h.state("gpu-a") != Healthy || n.Unschedulable || len(n.Annotations) != 0 || len(h.f.events) != 0 {
		t.Fatalf("dry-run must not change the cluster: %v %v", n.Annotations, h.f.events)
	}
}

func TestManuallyCordonedNodeIsLeftAlone(t *testing.T) {
	h := setup(t, nil, "gpu-a")
	h.f.nodes["gpu-a"].Unschedulable = true
	h.tel["gpu-a"] = []health.GPU{{Index: "0", XID: 79}}
	h.reconcile(t)
	if h.state("gpu-a") != Healthy || len(h.f.events) != 0 {
		t.Fatal("a node someone else cordoned is not ours to remediate")
	}
}

func TestValidationJobIsPinnedAndTolerant(t *testing.T) {
	h := setup(t, nil, "gpu-a")
	job := h.c.validationJob("gpu-validate-x", "gpu-a")
	spec := job["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	if spec["nodeName"] != "gpu-a" {
		t.Fatal("job must be pinned to the node")
	}
	if len(spec["tolerations"].([]map[string]string)) == 0 {
		t.Fatal("job must tolerate the cordon taint")
	}
	c := spec["containers"].([]map[string]any)[0]
	if c["resources"].(map[string]any)["limits"].(map[string]any)["nvidia.com/gpu"] != "8" {
		t.Fatal("job should request every GPU")
	}
}
