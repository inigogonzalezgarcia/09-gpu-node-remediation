package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckFromFile(t *testing.T) {
	dir := t.TempDir()
	smiOut := filepath.Join(dir, "smi.csv")
	os.WriteFile(smiOut, []byte("0, GPU-33333333-cccc, NVIDIA GeForce RTX 4070, 00000000:01:00.0, 52, [N/A], [N/A]\n"), 0o600)
	var buf bytes.Buffer
	if code := check([]string{"-from-file", smiOut}, &buf); code != 0 {
		t.Fatalf("healthy consumer GPU should exit 0, got %d\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "n/a") || !strings.Contains(buf.String(), "Verdict: NONE") {
		t.Fatalf("unexpected output:\n%s", buf.String())
	}

	log := filepath.Join(dir, "kern.log")
	os.WriteFile(log, []byte("NVRM: Xid (PCI:0000:01:00): 79, pid=1, GPU has fallen off the bus.\n"), 0o600)
	buf.Reset()
	if code := check([]string{"-from-file", smiOut, "-xid-log", log, "-json"}, &buf); code != 3 {
		t.Fatalf("XID 79 should exit 3 (drain), got %d\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), `"Action": "drain"`) {
		t.Fatalf("unexpected JSON:\n%s", buf.String())
	}
}
