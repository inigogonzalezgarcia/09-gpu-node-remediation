package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/health"
	"github.com/inigogonzalezgarcia/09-gpu-node-remediation/internal/metrics"
)

func call(t *testing.T, h http.HandlerFunc, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(method, target, nil))
	return w
}

func scrape(t *testing.T, s *sim) health.Verdict {
	t.Helper()
	w := call(t, s.metrics, "GET", "/metrics")
	samples, err := metrics.Parse(strings.NewReader(w.Body.String()))
	if err != nil {
		t.Fatalf("simulator output must parse: %v", err)
	}
	return health.Evaluate(health.FromSamples(samples), health.DefaultConfig())
}

func newSim() *sim {
	s := &sim{node: "n1", baseTmp: 42, gpus: make([]gpu, 4)}
	for i := range s.gpus {
		s.gpus[i].temp = 42
	}
	return s
}

func TestInjectResetValidate(t *testing.T) {
	s := newSim()
	if v := scrape(t, s); v.Action != health.None {
		t.Fatalf("fresh simulator should be healthy: %s", v.Summary())
	}
	call(t, s.inject, "POST", "/inject?gpu=2&xid=79")
	if v := scrape(t, s); v.Action != health.Drain {
		t.Fatalf("want drain, got %s", v.Summary())
	}
	if w := call(t, s.validate, "GET", "/validate"); w.Code != 500 {
		t.Fatalf("validation should fail with a fault, got %d", w.Code)
	}
	call(t, s.reset, "POST", "/reset")
	if w := call(t, s.validate, "GET", "/validate"); w.Code != 200 {
		t.Fatalf("validation should pass after reset, got %d", w.Code)
	}
}

func TestStickyFaultSurvivesReset(t *testing.T) {
	s := newSim()
	call(t, s.inject, "POST", "/inject?gpu=0&xid=79&sticky=1")
	call(t, s.reset, "POST", "/reset")
	if w := call(t, s.validate, "GET", "/validate"); w.Code != 500 {
		t.Fatalf("sticky fault should fail validation, got %d", w.Code)
	}
}

func TestInjectValidation(t *testing.T) {
	s := newSim()
	if w := call(t, s.inject, "POST", "/inject?gpu=9&xid=79"); w.Code != 400 {
		t.Fatalf("out of range GPU should be rejected, got %d", w.Code)
	}
	if w := call(t, s.inject, "GET", "/inject?gpu=0&xid=79"); w.Code != 405 {
		t.Fatalf("GET should be rejected, got %d", w.Code)
	}
}
