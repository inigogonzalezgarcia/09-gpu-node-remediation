// Package k8s is a deliberately small Kubernetes API client: only the calls the
// remediation loop needs, with the standard library. See docs/decisions.md for why
// this project does not use client-go.
package k8s

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// ErrBlocked means the API refused an eviction because it would break a PodDisruptionBudget.
var ErrBlocked = errors.New("eviction blocked by a PodDisruptionBudget")

// ErrNotFound is returned for 404 responses.
var ErrNotFound = errors.New("not found")

// Node is the part of a Node object the controller uses.
type Node struct {
	Name          string
	Labels        map[string]string
	Annotations   map[string]string
	Unschedulable bool
	InternalIP    string
}

// Pod is the part of a Pod object the controller uses.
type Pod struct {
	Namespace   string
	Name        string
	OwnerKinds  []string
	Annotations map[string]string
	Phase       string
}

// Client talks to the API server with a bearer token.
type Client struct {
	base  string
	token string
	http  *http.Client
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// InCluster builds a client from the service account mounted in the pod.
func InCluster() (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster (KUBERNETES_SERVICE_HOST is empty)")
	}
	token, err := os.ReadFile(saDir + "/token")
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("could not parse the cluster CA")
	}
	return &Client{
		base:  "https://" + host + ":" + port,
		token: string(token),
		http: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		},
	}, nil
}

func (c *Client) do(method, path, contentType string, body []byte, out any) error {
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusTooManyRequests:
		return ErrBlocked
	case resp.StatusCode >= 300:
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, truncate(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300]) + "..."
	}
	return string(b)
}

type objectMeta struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
	OwnerReferences []struct {
		Kind string `json:"kind"`
	} `json:"ownerReferences"`
}

// ListNodes returns the nodes matching a label selector.
func (c *Client) ListNodes(selector string) ([]Node, error) {
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				Unschedulable bool `json:"unschedulable"`
			} `json:"spec"`
			Status struct {
				Addresses []struct {
					Type    string `json:"type"`
					Address string `json:"address"`
				} `json:"addresses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := c.do("GET", "/api/v1/nodes?labelSelector="+url.QueryEscape(selector), "", nil, &list); err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(list.Items))
	for _, it := range list.Items {
		n := Node{Name: it.Metadata.Name, Labels: it.Metadata.Labels, Annotations: it.Metadata.Annotations,
			Unschedulable: it.Spec.Unschedulable}
		for _, a := range it.Status.Addresses {
			if a.Type == "InternalIP" {
				n.InternalIP = a.Address
			}
		}
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		out = append(out, n)
	}
	return out, nil
}

// PatchNode applies a JSON merge patch to a node (labels, annotations, spec.unschedulable).
func (c *Client) PatchNode(name string, patch map[string]any) error {
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return c.do("PATCH", "/api/v1/nodes/"+url.PathEscape(name), "application/merge-patch+json", body, nil)
}

// PodsOnNode lists the pods scheduled on a node.
func (c *Client) PodsOnNode(node string) ([]Pod, error) {
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Status   struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	q := url.QueryEscape("spec.nodeName=" + node)
	if err := c.do("GET", "/api/v1/pods?fieldSelector="+q, "", nil, &list); err != nil {
		return nil, err
	}
	out := make([]Pod, 0, len(list.Items))
	for _, it := range list.Items {
		p := Pod{Namespace: it.Metadata.Namespace, Name: it.Metadata.Name, Annotations: it.Metadata.Annotations,
			Phase: it.Status.Phase}
		for _, o := range it.Metadata.OwnerReferences {
			p.OwnerKinds = append(p.OwnerKinds, o.Kind)
		}
		out = append(out, p)
	}
	return out, nil
}

// Evict asks the API to evict a pod. PodDisruptionBudgets are enforced by the API server.
func (c *Client) Evict(namespace, name string) error {
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "policy/v1",
		"kind":       "Eviction",
		"metadata":   map[string]string{"name": name, "namespace": namespace},
	})
	err := c.do("POST", fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/eviction", url.PathEscape(namespace), url.PathEscape(name)),
		"application/json", body, nil)
	if errors.Is(err, ErrNotFound) {
		return nil // already gone
	}
	return err
}

// CreateJob creates a batch/v1 Job from a JSON manifest.
func (c *Client) CreateJob(namespace string, job map[string]any) error {
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return c.do("POST", fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs", url.PathEscape(namespace)), "application/json", body, nil)
}

// JobState reports whether a Job succeeded, failed, or is still running.
func (c *Client) JobState(namespace, name string) (succeeded, failed bool, err error) {
	var job struct {
		Status struct {
			Succeeded  int `json:"succeeded"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := c.do("GET", fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs/%s", url.PathEscape(namespace), url.PathEscape(name)), "", nil, &job); err != nil {
		return false, false, err
	}
	for _, cond := range job.Status.Conditions {
		if cond.Type == "Failed" && cond.Status == "True" {
			return false, true, nil
		}
		if cond.Type == "Complete" && cond.Status == "True" {
			return true, false, nil
		}
	}
	return job.Status.Succeeded > 0, false, nil
}

// DeleteJob removes a Job and its pods.
func (c *Client) DeleteJob(namespace, name string) error {
	body, _ := json.Marshal(map[string]string{"propagationPolicy": "Background"})
	err := c.do("DELETE", fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs/%s", url.PathEscape(namespace), url.PathEscape(name)),
		"application/json", body, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// NodeEvent records an event on a node, visible in `kubectl describe node`.
func (c *Client) NodeEvent(namespace, node, eventType, reason, message string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Event",
		"metadata":   map[string]string{"generateName": node + ".", "namespace": namespace},
		"involvedObject": map[string]string{
			"kind": "Node", "name": node, "uid": node, "apiVersion": "v1",
		},
		"type":           eventType,
		"reason":         reason,
		"message":        message,
		"source":         map[string]string{"component": "gpu-remediation"},
		"firstTimestamp": now,
		"lastTimestamp":  now,
		"count":          1,
	})
	return c.do("POST", fmt.Sprintf("/api/v1/namespaces/%s/events", url.PathEscape(namespace)), "application/json", body, nil)
}
