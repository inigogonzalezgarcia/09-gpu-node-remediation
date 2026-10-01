// Command gpuremediate has two modes:
//
//	gpuremediate controller   run the remediation loop inside a Kubernetes cluster
//	gpuremediate check        evaluate the GPUs of this machine with nvidia-smi (Linux or Windows)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/agent"
	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/health"
	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/k8s"
	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/remediation"
	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/smi"
)

var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `gpuremediate %s

Usage:
  gpuremediate controller [flags]   run the remediation loop in a cluster
  gpuremediate check [flags]        check this machine's GPUs with nvidia-smi
  gpuremediate version

Run "gpuremediate <command> -h" for flags.
`, version)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "controller":
		os.Exit(controller(os.Args[2:]))
	case "check":
		os.Exit(check(os.Args[2:], os.Stdout))
	case "version", "--version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func controller(args []string) int {
	fs := flag.NewFlagSet("controller", flag.ExitOnError)
	var cfg remediation.Config
	fs.StringVar(&cfg.NodeSelector, "node-selector", env("NODE_SELECTOR", "nvidia.com/gpu.present=true"), "label selector for GPU nodes")
	fs.IntVar(&cfg.MaxUnavailable, "max-unavailable", 1, "nodes allowed in remediation at the same time")
	fs.IntVar(&cfg.RepeatLimit, "repeat-limit", 2, "incidents within -repeat-window that quarantine a node (0 = off)")
	fs.DurationVar(&cfg.RepeatWindow, "repeat-window", 24*time.Hour, "window for -repeat-limit")
	fs.IntVar(&cfg.ClearChecks, "clear-checks", 3, "consecutive clean checks before a thermal cordon is lifted")
	fs.DurationVar(&cfg.ValidationTimeout, "validation-timeout", 10*time.Minute, "how long the validation Job may take")
	fs.StringVar(&cfg.Namespace, "namespace", env("POD_NAMESPACE", "gpu-remediation"), "namespace for validation Jobs")
	fs.StringVar(&cfg.ValidationImage, "validation-image", "busybox:1.37", "image for the validation Job")
	fs.IntVar(&cfg.ValidationGPUs, "validation-gpus", 0, "nvidia.com/gpu the validation Job requests (0 = none)")
	fs.IntVar(&cfg.AgentPort, "agent-port", 9400, "port of the telemetry agent on each node")
	fs.StringVar(&cfg.EventNamespace, "event-namespace", "default", "namespace for Node events")
	fs.BoolVar(&cfg.DryRun, "dry-run", false, "log decisions without changing the cluster")
	temp := fs.Float64("temp-cordon", 90, "cordon at or above this GPU temperature (°C)")
	interval := fs.Duration("interval", 15*time.Second, "time between passes")
	listen := fs.String("listen", ":8080", "address for /metrics and /healthz")
	repair := fs.String("repair", "agent", "repair action: agent (POST /reset to the node agent) or none")
	_ = fs.Parse(args)

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg.Health = health.DefaultConfig()
	cfg.Health.TempCordonC = *temp

	client, err := k8s.InCluster()
	if err != nil {
		log.Error("kubernetes client", "error", err)
		return 1
	}
	tel := agent.NewHTTP(cfg.AgentPort)
	var rep remediation.Repairer = tel
	if *repair == "none" {
		rep = agent.NoopRepairer{}
	}
	c := remediation.New(cfg, client, tel, rep, log)

	var passes, failures atomic.Int64
	var last atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if t := last.Load(); t > 0 && time.Since(time.Unix(t, 0)) > 4**interval+time.Minute {
			http.Error(w, "reconcile loop is stuck", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		states, actions := c.Snapshot()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintln(w, "# HELP gpu_remediation_node_state Remediation state of each GPU node (1 = current state).")
		fmt.Fprintln(w, "# TYPE gpu_remediation_node_state gauge")
		nodes := make([]string, 0, len(states))
		for n := range states {
			nodes = append(nodes, n)
		}
		sort.Strings(nodes)
		all := []string{remediation.Healthy, remediation.Cordoned, remediation.Draining, remediation.Repairing, remediation.Validating, remediation.Quarantined}
		for _, n := range nodes {
			for _, s := range all {
				v := 0
				if states[n] == s {
					v = 1
				}
				fmt.Fprintf(w, "gpu_remediation_node_state{node=%q,state=%q} %d\n", n, s, v)
			}
		}
		fmt.Fprintln(w, "# HELP gpu_remediation_actions_total Actions taken by the controller.")
		fmt.Fprintln(w, "# TYPE gpu_remediation_actions_total counter")
		for _, a := range []string{"cordon", "uncordon", "drain", "evict", "repair", "quarantine"} {
			fmt.Fprintf(w, "gpu_remediation_actions_total{action=%q} %d\n", a, actions[a])
		}
		fmt.Fprintln(w, "# TYPE gpu_remediation_reconcile_total counter")
		fmt.Fprintf(w, "gpu_remediation_reconcile_total %d\n", passes.Load())
		fmt.Fprintln(w, "# TYPE gpu_remediation_reconcile_errors_total counter")
		fmt.Fprintf(w, "gpu_remediation_reconcile_errors_total %d\n", failures.Load())
		fmt.Fprintln(w, "# TYPE gpu_remediation_last_reconcile_timestamp_seconds gauge")
		fmt.Fprintf(w, "gpu_remediation_last_reconcile_timestamp_seconds %d\n", last.Load())
	})
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("metrics server", "error", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("starting", "version", version, "selector", cfg.NodeSelector, "maxUnavailable", cfg.MaxUnavailable, "dryRun", cfg.DryRun)
	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		passes.Add(1)
		if err := c.Reconcile(); err != nil {
			failures.Add(1)
			log.Error("reconcile", "error", err)
		}
		last.Store(time.Now().Unix())
		select {
		case <-ctx.Done():
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
			log.Info("stopped")
			return 0
		case <-t.C:
		}
	}
}

// check exits with the action level: 0 none, 1 watch, 2 cordon, 3 drain, 4 quarantine; 10 on errors.
func check(args []string, out io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	bin := fs.String("nvidia-smi", "nvidia-smi", "path to nvidia-smi")
	fromFile := fs.String("from-file", "", "read saved `nvidia-smi --query-gpu=... --format=csv,noheader,nounits` output instead of running it")
	xidLog := fs.String("xid-log", "", "kernel log to scan for XIDs (e.g. the output of `journalctl -k`); Linux only")
	temp := fs.Float64("temp-cordon", 90, "cordon threshold (°C)")
	asJSON := fs.Bool("json", false, "print JSON")
	_ = fs.Parse(args)

	var gpus []health.GPU
	var err error
	if *fromFile != "" {
		var f *os.File
		if f, err = os.Open(*fromFile); err == nil {
			gpus, err = smi.Parse(f, smi.Fields)
			f.Close()
		}
	} else {
		gpus, err = smi.Query(context.Background(), *bin)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 10
	}
	var unmatched []string
	if *xidLog != "" {
		f, err := os.Open(*xidLog)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 10
		}
		unmatched, err = smi.ParseXIDLog(f, gpus)
		f.Close()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 10
		}
	}
	cfg := health.DefaultConfig()
	cfg.TempCordonC = *temp
	v := health.Evaluate(gpus, cfg)

	if *asJSON {
		type gpuOut struct {
			Index, UUID, Model string
			TempC              float64
			UncorrectableECC   *float64 `json:",omitempty"`
			RowRemapFailure    *bool    `json:",omitempty"`
			XID                int      `json:",omitempty"`
		}
		res := struct {
			Action        string
			Findings      []health.Finding
			GPUs          []gpuOut
			UnmatchedXIDs []string `json:",omitempty"`
		}{Action: v.Action.String(), Findings: v.Findings, UnmatchedXIDs: unmatched}
		for _, g := range gpus {
			o := gpuOut{Index: g.Index, UUID: g.UUID, Model: g.Model, TempC: g.TempC, XID: g.XID}
			if g.Seen["DCGM_FI_DEV_ECC_DBE_VOL_TOTAL"] {
				d := g.DBE
				o.UncorrectableECC = &d
			}
			if g.Seen["DCGM_FI_DEV_ROW_REMAP_FAILURE"] {
				r := g.RemapFailure
				o.RowRemapFailure = &r
			}
			res.GPUs = append(res.GPUs, o)
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		return int(v.Action)
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "GPU\tMODEL\tTEMP\tUNCORR. ECC\tROW REMAP FAIL\tXID")
	na := func(seen bool, s string) string {
		if !seen {
			return "n/a"
		}
		return s
	}
	for _, g := range gpus {
		xid := "-"
		if g.XID != 0 {
			xid = fmt.Sprint(g.XID)
		} else if *xidLog == "" {
			xid = "not read"
		}
		fmt.Fprintf(tw, "%s\t%s\t%.0f°C\t%s\t%s\t%s\n", g.Index, g.Model, g.TempC,
			na(g.Seen["DCGM_FI_DEV_ECC_DBE_VOL_TOTAL"], fmt.Sprintf("%.0f", g.DBE)),
			na(g.Seen["DCGM_FI_DEV_ROW_REMAP_FAILURE"], fmt.Sprint(g.RemapFailure)), xid)
	}
	tw.Flush()
	fmt.Fprintf(out, "\nVerdict: %s\n", strings.ToUpper(v.Action.String()))
	for _, f := range v.Findings {
		fmt.Fprintf(out, "  - GPU %s: %s -> %s\n", f.GPU, f.Reason, f.Action)
	}
	for _, u := range unmatched {
		fmt.Fprintf(out, "  ! XID for a GPU not in nvidia-smi output: %s\n", u)
	}
	if *xidLog == "" {
		fmt.Fprintln(out, "\nNote: XIDs are not visible to nvidia-smi. On Linux pass -xid-log with the kernel log.")
	}
	return int(v.Action)
}
