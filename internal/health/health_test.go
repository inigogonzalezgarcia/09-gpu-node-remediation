package health

import (
	"strings"
	"testing"

	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/metrics"
)

func gpus(t *testing.T, page string) []GPU {
	t.Helper()
	s, err := metrics.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	return FromSamples(s)
}

func TestHealthyNode(t *testing.T) {
	g := gpus(t, `
DCGM_FI_DEV_GPU_TEMP{gpu="0"} 45
DCGM_FI_DEV_XID_ERRORS{gpu="0"} 0
DCGM_FI_DEV_GPU_TEMP{gpu="1"} 47
DCGM_FI_DEV_XID_ERRORS{gpu="1"} 0
`)
	v := Evaluate(g, DefaultConfig())
	if v.Action != None || len(v.Findings) != 0 {
		t.Fatalf("expected healthy, got %v %v", v.Action, v.Findings)
	}
}

func TestXIDClassification(t *testing.T) {
	cases := map[int]Action{13: None, 31: None, 43: None, 94: Watch, 48: Drain, 79: Drain, 74: Drain, 64: Quarantine, 92: Quarantine, 999: Watch}
	for xid, want := range cases {
		v := Evaluate([]GPU{{Index: "3", XID: xid}}, DefaultConfig())
		if v.Action != want {
			t.Errorf("XID %d: want %v, got %v (%s)", xid, want, v.Action, v.Summary())
		}
	}
}

func TestWorstFindingWins(t *testing.T) {
	g := gpus(t, `
DCGM_FI_DEV_GPU_TEMP{gpu="0"} 93
DCGM_FI_DEV_XID_ERRORS{gpu="1"} 79
DCGM_FI_DEV_ROW_REMAP_FAILURE{gpu="2"} 1
`)
	v := Evaluate(g, DefaultConfig())
	if v.Action != Quarantine {
		t.Fatalf("want quarantine, got %v", v.Action)
	}
	if len(v.Findings) != 3 {
		t.Fatalf("want 3 findings, got %d: %s", len(v.Findings), v.Summary())
	}
	if !strings.Contains(v.Summary(), "GPU 1: XID 79") {
		t.Errorf("summary should name the GPU and XID: %s", v.Summary())
	}
}

func TestThermalOnlyCordons(t *testing.T) {
	v := Evaluate([]GPU{{Index: "0", TempC: 90}}, DefaultConfig())
	if v.Action != Cordon {
		t.Fatalf("want cordon at the threshold, got %v", v.Action)
	}
	v = Evaluate([]GPU{{Index: "0", TempC: 89}}, DefaultConfig())
	if v.Action != None {
		t.Fatalf("want none below the threshold, got %v", v.Action)
	}
}

func TestDoubleBitECCDrains(t *testing.T) {
	v := Evaluate([]GPU{{Index: "0", DBE: 2}}, DefaultConfig())
	if v.Action != Drain {
		t.Fatalf("want drain, got %v", v.Action)
	}
}

func TestGPUsAreSortedNumerically(t *testing.T) {
	g := gpus(t, `
DCGM_FI_DEV_GPU_TEMP{gpu="10"} 40
DCGM_FI_DEV_GPU_TEMP{gpu="2"} 40
`)
	if g[0].Index != "2" || g[1].Index != "10" {
		t.Fatalf("unexpected order: %v, %v", g[0].Index, g[1].Index)
	}
}
