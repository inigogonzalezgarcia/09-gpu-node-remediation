// Package smi reads GPU health from nvidia-smi, so the same rules that drive the controller can be
// checked on a single real machine (a workstation, a gaming PC, a bare-metal node) without Kubernetes.
//
// It asks nvidia-smi for a fixed list of fields in CSV:
//
//	nvidia-smi --query-gpu=<Fields> --format=csv,noheader,nounits
//
// Consumer GPUs report "[N/A]" or "[Not Supported]" for ECC and row remapping; those fields are
// marked as not seen rather than read as zero.
//
// nvidia-smi does not report XIDs. On Linux they are in the kernel log ("NVRM: Xid (PCI:...): 79, ...");
// ParseXIDLog reads that format and attributes each XID to a GPU by PCI bus id.
package smi

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/health"
)

// Fields is the query order. remapped_rows.* needs an Ampere-or-newer data-centre GPU to return values.
var Fields = []string{
	"index", "uuid", "name", "pci.bus_id", "temperature.gpu",
	"ecc.errors.uncorrected.volatile.total", "remapped_rows.failure",
}

// fallback is used when the driver rejects a field name (older drivers).
var fallback = Fields[:6]

// Query runs nvidia-smi and parses its output.
func Query(ctx context.Context, binary string) ([]health.GPU, error) {
	out, err := run(ctx, binary, Fields)
	fields := Fields
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "not a valid field") {
		out, err = run(ctx, binary, fallback)
		fields = fallback
	}
	if err != nil {
		return nil, err
	}
	return Parse(strings.NewReader(out), fields)
}

func run(ctx context.Context, binary string, fields []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--query-gpu="+strings.Join(fields, ","), "--format=csv,noheader,nounits")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", binary, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func missing(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || strings.HasPrefix(v, "[") || strings.EqualFold(v, "N/A")
}

// Parse reads `--format=csv,noheader,nounits` output for the given fields.
func Parse(r io.Reader, fields []string) ([]health.GPU, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	cr.FieldsPerRecord = len(fields)
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("unexpected nvidia-smi output: %w", err)
	}
	var out []health.GPU
	for _, row := range rows {
		g := health.GPU{Seen: map[string]bool{}}
		for i, f := range fields {
			v := strings.TrimSpace(row[i])
			if missing(v) {
				continue
			}
			switch f {
			case "index":
				g.Index = v
			case "uuid":
				g.UUID = v
			case "name":
				g.Model = v
			case "pci.bus_id":
				g.Seen["pci:"+normalizeBus(v)] = true
			case "temperature.gpu":
				t, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return nil, fmt.Errorf("temperature %q: %w", v, err)
				}
				g.TempC = t
				g.Seen["DCGM_FI_DEV_GPU_TEMP"] = true
			case "ecc.errors.uncorrected.volatile.total":
				n, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return nil, fmt.Errorf("ECC count %q: %w", v, err)
				}
				g.DBE = n
				g.Seen["DCGM_FI_DEV_ECC_DBE_VOL_TOTAL"] = true
			case "remapped_rows.failure":
				g.RemapFailure = strings.EqualFold(v, "yes") || v == "1"
				g.Seen["DCGM_FI_DEV_ROW_REMAP_FAILURE"] = true
			}
		}
		if g.Index == "" {
			return nil, fmt.Errorf("row without a GPU index: %v", row)
		}
		out = append(out, g)
	}
	return out, nil
}

// BusID returns the normalised PCI bus id recorded by Parse, or "".
func BusID(g health.GPU) string {
	for k := range g.Seen {
		if strings.HasPrefix(k, "pci:") {
			return strings.TrimPrefix(k, "pci:")
		}
	}
	return ""
}

// normalizeBus turns "00000000:3B:00.0" and "0000:3b:00" into "3b:00".
func normalizeBus(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.LastIndex(s, "."); i > 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ":")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + ":" + parts[len(parts)-1]
	}
	return s
}

var xidLine = regexp.MustCompile(`NVRM: Xid \(PCI:([0-9a-fA-F:.]+)\): (\d+)`)

// ParseXIDLog reads kernel log lines (dmesg / journalctl -k) and sets each GPU's XID to the last one
// reported for its PCI bus. XIDs for unknown buses are returned separately.
func ParseXIDLog(r io.Reader, gpus []health.GPU) (unmatched []string, err error) {
	byBus := map[string]int{}
	for i, g := range gpus {
		if b := BusID(g); b != "" {
			byBus[b] = i
		}
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		m := xidLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		xid, _ := strconv.Atoi(m[2])
		if i, ok := byBus[normalizeBus(m[1])]; ok {
			gpus[i].XID = xid
			gpus[i].Seen["DCGM_FI_DEV_XID_ERRORS"] = true
		} else {
			unmatched = append(unmatched, fmt.Sprintf("PCI %s: XID %d", m[1], xid))
		}
	}
	return unmatched, sc.Err()
}
