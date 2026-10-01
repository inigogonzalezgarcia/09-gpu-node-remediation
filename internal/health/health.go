// Package health turns GPU telemetry into a decision for the whole node.
//
// Input is the DCGM field set exposed by NVIDIA dcgm-exporter (or the lab simulator):
//
//	DCGM_FI_DEV_XID_ERRORS        last XID reported for the GPU (0 = none)
//	DCGM_FI_DEV_GPU_TEMP          GPU temperature in °C
//	DCGM_FI_DEV_ECC_DBE_VOL_TOTAL uncorrectable (double-bit) ECC errors since the last reset
//	DCGM_FI_DEV_ROW_REMAP_FAILURE 1 if the GPU could not remap a faulty memory row
//
// The mapping from XID to action is this project's policy, built from the possible causes
// NVIDIA lists for each XID (see docs/xid-policy.md). Tune it for your fleet.
package health

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/metrics"
)

// Action is what the node needs, ordered from least to most disruptive.
type Action int

const (
	None       Action = iota // healthy, or a problem attributed to the workload
	Watch                    // record and alert, no change to the node
	Cordon                   // stop new work; running work continues (e.g. thermal)
	Drain                    // move work away, reset/repair, validate, return to service
	Quarantine               // take out of service until a human (or the vendor) fixes it
)

func (a Action) String() string {
	return [...]string{"none", "watch", "cordon", "drain", "quarantine"}[a]
}

// XIDPolicy maps an XID to an action and a short reason.
type XIDPolicy struct {
	Action Action
	Reason string
}

// DefaultXIDPolicy is the starting policy. XIDs not listed get Watch.
var DefaultXIDPolicy = map[int]XIDPolicy{
	13:  {None, "graphics engine exception (usually the application)"},
	31:  {None, "GPU memory page fault (usually the application)"},
	43:  {None, "GPU stopped processing (application)"},
	45:  {None, "preemptive cleanup after a previous error"},
	94:  {Watch, "contained memory error: restart the affected application"},
	48:  {Drain, "double-bit ECC error"},
	63:  {Drain, "row remapping pending: needs a GPU reset"},
	74:  {Drain, "NVLink error"},
	79:  {Drain, "GPU has fallen off the bus"},
	95:  {Drain, "uncontained memory error"},
	119: {Drain, "GSP RPC timeout"},
	120: {Drain, "GSP error"},
	64:  {Quarantine, "row remapping failure: memory cannot be repaired in place"},
	92:  {Quarantine, "high single-bit ECC error rate"},
}

// Config holds thresholds.
type Config struct {
	TempCordonC float64 // cordon at or above this GPU temperature
	XID         map[int]XIDPolicy
}

// DefaultConfig returns the thresholds used in the lab.
func DefaultConfig() Config {
	return Config{TempCordonC: 90, XID: DefaultXIDPolicy}
}

// GPU is the latest telemetry for one GPU.
type GPU struct {
	Index        string
	UUID         string
	Model        string
	TempC        float64
	XID          int
	DBE          float64
	RemapFailure bool
	Seen         map[string]bool // which fields were present
}

// Finding is one reason for an action.
type Finding struct {
	GPU    string
	Action Action
	Reason string
}

// Verdict is the decision for one node.
type Verdict struct {
	Action   Action
	Findings []Finding
}

// Summary is a one-line description for events and logs.
func (v Verdict) Summary() string {
	if len(v.Findings) == 0 {
		return "healthy"
	}
	s := ""
	for i, f := range v.Findings {
		if i > 0 {
			s += "; "
		}
		s += fmt.Sprintf("GPU %s: %s (%s)", f.GPU, f.Reason, f.Action)
	}
	return s
}

// FromSamples groups DCGM samples by GPU.
func FromSamples(samples []metrics.Sample) []GPU {
	byIndex := map[string]*GPU{}
	for _, s := range samples {
		idx, ok := s.Labels["gpu"]
		if !ok {
			continue
		}
		g := byIndex[idx]
		if g == nil {
			g = &GPU{Index: idx, Seen: map[string]bool{}}
			byIndex[idx] = g
		}
		if u := s.Labels["UUID"]; u != "" {
			g.UUID = u
		}
		if m := s.Labels["modelName"]; m != "" {
			g.Model = m
		}
		switch s.Name {
		case "DCGM_FI_DEV_GPU_TEMP":
			g.TempC = s.Value
		case "DCGM_FI_DEV_XID_ERRORS":
			g.XID = int(s.Value)
		case "DCGM_FI_DEV_ECC_DBE_VOL_TOTAL":
			g.DBE = s.Value
		case "DCGM_FI_DEV_ROW_REMAP_FAILURE":
			g.RemapFailure = s.Value > 0
		default:
			continue
		}
		g.Seen[s.Name] = true
	}
	out := make([]GPU, 0, len(byIndex))
	for _, g := range byIndex {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		a, errA := strconv.Atoi(out[i].Index)
		b, errB := strconv.Atoi(out[j].Index)
		if errA == nil && errB == nil {
			return a < b
		}
		return out[i].Index < out[j].Index
	})
	return out
}

// Evaluate decides what a node needs from the telemetry of its GPUs.
func Evaluate(gpus []GPU, cfg Config) Verdict {
	var v Verdict
	add := func(gpu string, a Action, reason string) {
		v.Findings = append(v.Findings, Finding{GPU: gpu, Action: a, Reason: reason})
		if a > v.Action {
			v.Action = a
		}
	}
	for _, g := range gpus {
		if g.XID != 0 {
			p, known := cfg.XID[g.XID]
			if !known {
				p = XIDPolicy{Watch, "unclassified XID"}
			}
			add(g.Index, p.Action, fmt.Sprintf("XID %d, %s", g.XID, p.Reason))
		}
		if g.RemapFailure {
			add(g.Index, Quarantine, "row remapping failure reported by DCGM")
		}
		if g.DBE > 0 {
			add(g.Index, Drain, fmt.Sprintf("%.0f uncorrectable ECC error(s) since last reset", g.DBE))
		}
		if cfg.TempCordonC > 0 && g.TempC >= cfg.TempCordonC {
			add(g.Index, Cordon, fmt.Sprintf("temperature %.0f°C (limit %.0f°C)", g.TempC, cfg.TempCordonC))
		}
	}
	// Findings that need no action are kept for the record but should not read as problems.
	return v
}
