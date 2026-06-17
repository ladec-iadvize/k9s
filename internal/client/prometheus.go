// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// prometheusPort is the conventional Prometheus HTTP port.
const prometheusPort = 9090

// prometheusExcludes are service-name fragments that look prometheus-ish but
// are not the query endpoint (operator, exporters, alerting, long-term store).
var prometheusExcludes = []string{
	"operator", "pushgateway", "node-exporter", "kube-state",
	"alertmanager", "thanos", "blackbox", "operated", "adapter",
}

// Prometheus queries a Prometheus server reachable through the kube API-server
// service proxy — no port-forward, no extra config, reusing the already
// authenticated kube connection. The endpoint is auto-discovered once.
type Prometheus struct {
	conn      Connection
	proxyBase string
}

// DialPrometheus returns a Prometheus client bound to the given connection.
func DialPrometheus(c Connection) *Prometheus {
	return &Prometheus{conn: c}
}

// promVectorResponse is the subset of the Prometheus query API we consume.
type promVectorResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []any             `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// QueryVector runs an instant PromQL query and returns a map keyed by
// "<namespace>/<pod>" to the scalar sample value. Series without a pod label
// are skipped.
func (p *Prometheus) QueryVector(ctx context.Context, promQL string) (map[string]float64, error) {
	if err := p.ensureEndpoint(ctx); err != nil {
		return nil, err
	}
	cs, err := p.conn.Dial()
	if err != nil {
		return nil, err
	}

	raw, err := cs.CoreV1().RESTClient().Get().
		AbsPath(p.proxyBase + "/api/v1/query").
		Param("query", promQL).
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("prometheus query failed: %w", err)
	}

	var resp promVectorResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("prometheus response decode failed: %w", err)
	}
	if resp.Status != "success" {
		return nil, fmt.Errorf("prometheus query status: %s", resp.Status)
	}

	out := make(map[string]float64, len(resp.Data.Result))
	for _, r := range resp.Data.Result {
		pod := r.Metric["pod"]
		if pod == "" || len(r.Value) < 2 {
			continue
		}
		s, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		out[FQN(r.Metric["namespace"], pod)] = f
	}

	return out, nil
}

// ensureEndpoint auto-discovers the Prometheus service proxy path once and
// caches it. It looks cluster-wide for a ClusterIP service whose name contains
// "prometheus" (minus the known non-query variants) exposing port 9090.
func (p *Prometheus) ensureEndpoint(ctx context.Context) error {
	if p.proxyBase != "" {
		return nil
	}
	cs, err := p.conn.Dial()
	if err != nil {
		return err
	}
	svcs, err := cs.CoreV1().Services(BlankNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}

	for i := range svcs.Items {
		s := &svcs.Items[i]
		name := strings.ToLower(s.Name)
		if !strings.Contains(name, "prometheus") || excluded(name) {
			continue
		}
		if s.Spec.ClusterIP == "" || s.Spec.ClusterIP == "None" {
			continue
		}
		if port := prometheusServicePort(s.Spec.Ports); port != 0 {
			p.proxyBase = fmt.Sprintf(
				"/api/v1/namespaces/%s/services/%s:%d/proxy", s.Namespace, s.Name, port,
			)
			return nil
		}
	}

	return errors.New("no Prometheus service found in cluster (need a ClusterIP service named *prometheus* on port 9090)")
}

func excluded(name string) bool {
	for _, frag := range prometheusExcludes {
		if strings.Contains(name, frag) {
			return true
		}
	}
	return false
}

// prometheusServicePort prefers the conventional 9090 port, then a port named
// web/http-web/http, returning 0 when none looks right.
func prometheusServicePort(ports []v1.ServicePort) int32 {
	var named int32
	for i := range ports {
		if ports[i].Port == prometheusPort {
			return prometheusPort
		}
		switch strings.ToLower(ports[i].Name) {
		case "web", "http-web", "http":
			if named == 0 {
				named = ports[i].Port
			}
		}
	}
	return named
}
