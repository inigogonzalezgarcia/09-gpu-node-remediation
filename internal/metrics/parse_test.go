package metrics

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	page := `# HELP DCGM_FI_DEV_GPU_TEMP GPU temperature (in C).
# TYPE DCGM_FI_DEV_GPU_TEMP gauge
DCGM_FI_DEV_GPU_TEMP{gpu="0",UUID="GPU-abc",Hostname="node-a",modelName="NVIDIA H100 80GB HBM3"} 41
DCGM_FI_DEV_XID_ERRORS{gpu="1",UUID="GPU-def",err_msg="has \"quotes\""} 79 1700000000000
up 1
`
	samples, err := Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 3 {
		t.Fatalf("want 3 samples, got %d", len(samples))
	}
	if samples[0].Labels["modelName"] != "NVIDIA H100 80GB HBM3" || samples[0].Value != 41 {
		t.Errorf("unexpected first sample: %+v", samples[0])
	}
	if samples[1].Labels["err_msg"] != `has "quotes"` || samples[1].Value != 79 {
		t.Errorf("unexpected second sample: %+v", samples[1])
	}
	if samples[2].Name != "up" || samples[2].Value != 1 {
		t.Errorf("unexpected third sample: %+v", samples[2])
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, page := range []string{`metric{gpu="0"`, `metric{gpu=0} 1`, `metric`, `metric{gpu="0"} abc`} {
		if _, err := Parse(strings.NewReader(page)); err == nil {
			t.Errorf("expected an error for %q", page)
		}
	}
}
