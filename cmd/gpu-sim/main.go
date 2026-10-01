// Command gpu-sim is a lab stand-in for dcgm-exporter on nodes without GPUs. It serves the same
// DCGM metric names for a configurable number of fake GPUs, and lets tests inject faults.
//
//	GET  /metrics                       DCGM_FI_DEV_* gauges for every fake GPU
//	POST /inject?gpu=3&xid=79           set a fault (also temp=95, dbe=2, remap=1)
//	POST /inject?gpu=3&xid=79&sticky=1  a fault that survives /reset (validation will fail)
//	POST /reset                         "GPU reset": clears faults that are not sticky
//	POST /reset?all=1                   clears everything (the "hardware was replaced" button)
//	GET  /validate                      200 if every GPU is clean, 500 otherwise (used by the validation Job)
//	GET  /healthz
//
// It never touches real hardware.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type gpu struct {
	temp   float64
	xid    int
	dbe    float64
	remap  bool
	sticky bool
}

type sim struct {
	mu      sync.Mutex
	node    string
	gpus    []gpu
	baseTmp float64
	resets  int
	delay   time.Duration // how long /validate takes
}

func (s *sim) metrics(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	type field struct{ name, help string }
	fields := []field{
		{"DCGM_FI_DEV_GPU_TEMP", "GPU temperature (in C)."},
		{"DCGM_FI_DEV_XID_ERRORS", "Value of the last XID error encountered."},
		{"DCGM_FI_DEV_ECC_DBE_VOL_TOTAL", "Total number of double-bit volatile ECC errors."},
		{"DCGM_FI_DEV_ROW_REMAP_FAILURE", "Whether remapping of rows has failed."},
	}
	for _, f := range fields {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", f.name, f.help, f.name)
		for i, g := range s.gpus {
			var v float64
			switch f.name {
			case "DCGM_FI_DEV_GPU_TEMP":
				v = g.temp
			case "DCGM_FI_DEV_XID_ERRORS":
				v = float64(g.xid)
			case "DCGM_FI_DEV_ECC_DBE_VOL_TOTAL":
				v = g.dbe
			case "DCGM_FI_DEV_ROW_REMAP_FAILURE":
				if g.remap {
					v = 1
				}
			}
			fmt.Fprintf(w, "%s{gpu=\"%d\",UUID=\"GPU-sim-%s-%d\",modelName=\"Simulated GPU\",Hostname=%q} %g\n",
				f.name, i, s.node, i, s.node, v)
		}
	}
	fmt.Fprintf(w, "# TYPE gpu_sim_resets_total counter\ngpu_sim_resets_total %d\n", s.resets)
}

func (s *sim) inject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	i, err := strconv.Atoi(q.Get("gpu"))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil || i < 0 || i >= len(s.gpus) {
		http.Error(w, fmt.Sprintf("gpu must be 0..%d", len(s.gpus)-1), http.StatusBadRequest)
		return
	}
	g := &s.gpus[i]
	num := func(k string) (float64, bool) {
		v := q.Get(k)
		if v == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	}
	if v, ok := num("xid"); ok {
		g.xid = int(v)
	}
	if v, ok := num("temp"); ok {
		g.temp = v
	}
	if v, ok := num("dbe"); ok {
		g.dbe = v
	}
	if v, ok := num("remap"); ok {
		g.remap = v > 0
	}
	g.sticky = q.Get("sticky") == "1"
	slog.Info("inject", "gpu", i, "xid", g.xid, "temp", g.temp, "dbe", g.dbe, "remap", g.remap, "sticky", g.sticky)
	fmt.Fprintf(w, "gpu %d: xid=%d temp=%g dbe=%g remap=%v sticky=%v\n", i, g.xid, g.temp, g.dbe, g.remap, g.sticky)
}

func (s *sim) reset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resets++
	all := r.URL.Query().Get("all") == "1"
	for i := range s.gpus {
		if all || !s.gpus[i].sticky {
			s.gpus[i] = gpu{temp: s.baseTmp}
		}
	}
	slog.Info("reset")
	fmt.Fprintln(w, "reset")
}

func (s *sim) validate(w http.ResponseWriter, _ *http.Request) {
	time.Sleep(s.delay) // a real burn-in takes minutes; keep the lab fast
	s.mu.Lock()
	defer s.mu.Unlock()
	var bad []string
	for i, g := range s.gpus {
		if g.xid != 0 || g.dbe > 0 || g.remap {
			bad = append(bad, fmt.Sprintf("gpu %d: xid=%d dbe=%g remap=%v", i, g.xid, g.dbe, g.remap))
		}
	}
	if len(bad) > 0 {
		slog.Warn("validate failed", "faults", bad)
		http.Error(w, "FAIL\n"+strings.Join(bad, "\n"), http.StatusInternalServerError)
		return
	}
	slog.Info("validate passed")
	fmt.Fprintln(w, "PASS")
}

func main() {
	listen := flag.String("listen", ":9400", "address to listen on")
	count := flag.Int("gpus", 8, "number of fake GPUs")
	temp := flag.Float64("base-temp", 42, "normal temperature")
	delay := flag.Duration("validate-delay", 2*time.Second, "how long /validate takes")
	flag.Parse()
	node := os.Getenv("NODE_NAME")
	if node == "" {
		node, _ = os.Hostname()
	}
	s := &sim{node: node, baseTmp: *temp, gpus: make([]gpu, *count), delay: *delay}
	for i := range s.gpus {
		s.gpus[i].temp = *temp
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", s.metrics)
	mux.HandleFunc("/inject", s.inject)
	mux.HandleFunc("/reset", s.reset)
	mux.HandleFunc("/validate", s.validate)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	slog.Info("gpu-sim listening", "addr", *listen, "node", node, "gpus", *count)
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("listen", "error", err)
		os.Exit(1)
	}
}
