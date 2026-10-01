// Package agent talks to the per-node telemetry agent: dcgm-exporter in production, or the
// lab simulator (cmd/gpu-sim), which serves the same metrics plus /reset and /validate.
package agent

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/health"
	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/k8s"
	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/metrics"
)

// HTTP reads /metrics from the agent on each node's InternalIP (the agent uses hostNetwork).
type HTTP struct {
	Port   int
	Client *http.Client
}

// NewHTTP returns an agent client with sane timeouts.
func NewHTTP(port int) *HTTP {
	return &HTTP{Port: port, Client: &http.Client{Timeout: 5 * time.Second}}
}

func (a *HTTP) url(n k8s.Node, path string) (string, error) {
	if n.InternalIP == "" {
		return "", fmt.Errorf("node %s has no InternalIP", n.Name)
	}
	host := n.InternalIP
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("http://%s:%d%s", host, a.Port, path), nil
}

// GPUs scrapes the agent and groups the DCGM fields by GPU.
func (a *HTTP) GPUs(n k8s.Node) ([]health.GPU, error) {
	u, err := a.url(n, "/metrics")
	if err != nil {
		return nil, err
	}
	resp, err := a.Client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	samples, err := metrics.Parse(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	g := health.FromSamples(samples)
	if len(g) == 0 {
		return nil, fmt.Errorf("no DCGM GPU metrics at %s", u)
	}
	return g, nil
}

// Repair asks the agent to reset the GPUs. In the lab this clears injected faults; in production
// the equivalent is `nvidia-smi --gpu-reset`, a reboot, or a ticket to the hardware team.
func (a *HTTP) Repair(n k8s.Node) error {
	u, err := a.url(n, "/reset")
	if err != nil {
		return err
	}
	resp, err := a.Client.Post(u, "text/plain", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("POST %s: %s: %s", u, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// NoopRepairer only records that a repair would happen (for clusters where repair is manual).
type NoopRepairer struct{}

// Repair does nothing.
func (NoopRepairer) Repair(k8s.Node) error { return nil }
