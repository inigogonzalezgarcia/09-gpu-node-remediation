package smi

import (
	"strings"
	"testing"

	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/health"
)

const datacentre = `0, GPU-11111111-aaaa, NVIDIA H100 80GB HBM3, 00000000:1B:00.0, 41, 0, No
1, GPU-22222222-bbbb, NVIDIA H100 80GB HBM3, 00000000:3B:00.0, 44, 2, No
`

const consumer = `0, GPU-33333333-cccc, NVIDIA GeForce RTX 4070, 00000000:01:00.0, 52, [N/A], [N/A]
`

func TestParseDataCentre(t *testing.T) {
	g, err := Parse(strings.NewReader(datacentre), Fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(g) != 2 || g[1].DBE != 2 || g[0].Model != "NVIDIA H100 80GB HBM3" || BusID(g[1]) != "3b:00" {
		t.Fatalf("unexpected: %+v", g)
	}
	if v := health.Evaluate(g, health.DefaultConfig()); v.Action != health.Drain {
		t.Fatalf("2 uncorrectable ECC errors should drain, got %v", v.Action)
	}
}

func TestConsumerGPUNotSupportedIsNotZero(t *testing.T) {
	g, err := Parse(strings.NewReader(consumer), Fields)
	if err != nil {
		t.Fatal(err)
	}
	if g[0].Seen["DCGM_FI_DEV_ECC_DBE_VOL_TOTAL"] || g[0].Seen["DCGM_FI_DEV_ROW_REMAP_FAILURE"] {
		t.Fatal("[N/A] must be reported as not available, not as zero")
	}
	if !g[0].Seen["DCGM_FI_DEV_GPU_TEMP"] || g[0].TempC != 52 {
		t.Fatalf("temperature should be read: %+v", g[0])
	}
}

func TestXIDLogMatchesByBus(t *testing.T) {
	g, _ := Parse(strings.NewReader(datacentre), Fields)
	log := `[  12.3] nvidia 0000:3b:00.0: enabling device
[ 812.1] NVRM: Xid (PCI:0000:3b:00): 79, pid='<unknown>', name=<unknown>, GPU has fallen off the bus.
[ 900.0] NVRM: Xid (PCI:0000:af:00): 13, pid=1234, name=python
`
	unmatched, err := ParseXIDLog(strings.NewReader(log), g)
	if err != nil {
		t.Fatal(err)
	}
	if g[1].XID != 79 || g[0].XID != 0 {
		t.Fatalf("XID should land on GPU 1 only: %d %d", g[0].XID, g[1].XID)
	}
	if len(unmatched) != 1 {
		t.Fatalf("want one unmatched XID, got %v", unmatched)
	}
}

func TestParseRejectsWrongShape(t *testing.T) {
	if _, err := Parse(strings.NewReader("0, GPU-x, 41\n"), Fields); err == nil {
		t.Fatal("expected an error for a short row")
	}
}
