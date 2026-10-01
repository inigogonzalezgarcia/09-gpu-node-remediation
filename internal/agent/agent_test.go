package agent

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/k8s"
)

func TestScrapeAndRepair(t *testing.T) {
	reset := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metrics":
			w.Write([]byte("DCGM_FI_DEV_XID_ERRORS{gpu=\"0\"} 79\nDCGM_FI_DEV_GPU_TEMP{gpu=\"0\"} 50\n"))
		case "/reset":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			reset++
		}
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p, _ := strconv.Atoi(port)
	a := NewHTTP(p)
	n := k8s.Node{Name: "gpu-a", InternalIP: host}

	g, err := a.GPUs(n)
	if err != nil {
		t.Fatal(err)
	}
	if len(g) != 1 || g[0].XID != 79 || g[0].TempC != 50 {
		t.Fatalf("unexpected telemetry: %+v", g)
	}
	if err := a.Repair(n); err != nil || reset != 1 {
		t.Fatalf("repair: %v (reset=%d)", err, reset)
	}
	if _, err := a.GPUs(k8s.Node{Name: "x"}); err == nil {
		t.Fatal("a node without an IP should be an error")
	}
}
