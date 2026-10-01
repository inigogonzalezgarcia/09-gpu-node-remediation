package k8s

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientCalls(t *testing.T) {
	var gotPatch map[string]any
	var patchType, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/nodes":
			if r.URL.Query().Get("labelSelector") != "nvidia.com/gpu.present=true" {
				http.Error(w, "bad selector", 400)
				return
			}
			io.WriteString(w, `{"items":[{"metadata":{"name":"gpu-a","annotations":{"x":"y"}},"spec":{"unschedulable":true},
				"status":{"addresses":[{"type":"Hostname","address":"gpu-a"},{"type":"InternalIP","address":"10.0.0.5"}]}}]}`)
		case r.Method == "PATCH" && r.URL.Path == "/api/v1/nodes/gpu-a":
			patchType = r.Header.Get("Content-Type")
			json.NewDecoder(r.Body).Decode(&gotPatch)
			io.WriteString(w, `{}`)
		case r.Method == "GET" && r.URL.Path == "/api/v1/pods":
			io.WriteString(w, `{"items":[{"metadata":{"namespace":"ml","name":"p","ownerReferences":[{"kind":"ReplicaSet"}]},"status":{"phase":"Running"}}]}`)
		case r.URL.Path == "/api/v1/namespaces/ml/pods/blocked/eviction":
			http.Error(w, `{"reason":"TooManyRequests"}`, http.StatusTooManyRequests)
		case r.URL.Path == "/api/v1/namespaces/ml/pods/gone/eviction":
			http.NotFound(w, r)
		case r.URL.Path == "/apis/batch/v1/namespaces/ns/jobs/done":
			io.WriteString(w, `{"status":{"succeeded":1,"conditions":[{"type":"Complete","status":"True"}]}}`)
		case r.URL.Path == "/apis/batch/v1/namespaces/ns/jobs/bad":
			io.WriteString(w, `{"status":{"conditions":[{"type":"Failed","status":"True"}]}}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), 500)
		}
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, token: "t0k", http: srv.Client()}

	nodes, err := c.ListNodes("nvidia.com/gpu.present=true")
	if err != nil || len(nodes) != 1 || nodes[0].InternalIP != "10.0.0.5" || !nodes[0].Unschedulable {
		t.Fatalf("ListNodes: %v %+v", err, nodes)
	}
	if auth != "Bearer t0k" {
		t.Fatalf("missing bearer token: %q", auth)
	}
	if err := c.PatchNode("gpu-a", map[string]any{"spec": map[string]any{"unschedulable": true}}); err != nil {
		t.Fatal(err)
	}
	if patchType != "application/merge-patch+json" || gotPatch["spec"].(map[string]any)["unschedulable"] != true {
		t.Fatalf("patch: %s %v", patchType, gotPatch)
	}
	pods, err := c.PodsOnNode("gpu-a")
	if err != nil || len(pods) != 1 || pods[0].OwnerKinds[0] != "ReplicaSet" {
		t.Fatalf("PodsOnNode: %v %+v", err, pods)
	}
	if err := c.Evict("ml", "blocked"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("429 should be ErrBlocked, got %v", err)
	}
	if err := c.Evict("ml", "gone"); err != nil {
		t.Fatalf("evicting a pod that is gone is fine, got %v", err)
	}
	if ok, failed, err := c.JobState("ns", "done"); !ok || failed || err != nil {
		t.Fatalf("done job: %v %v %v", ok, failed, err)
	}
	if ok, failed, err := c.JobState("ns", "bad"); ok || !failed || err != nil {
		t.Fatalf("failed job: %v %v %v", ok, failed, err)
	}
	if _, _, err := c.JobState("ns", "missing"); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("server errors should surface, got %v", err)
	}
}
