// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package view

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSizingReco(t *testing.T) {
	uu := map[string]struct {
		use, req int64
		want     int64
		ok       bool
	}{
		"under threshold recommends 50pct": {use: 10, req: 100, want: 20, ok: true},
		"just under threshold":             {use: 29, req: 100, want: 58, ok: true},
		"at threshold no reco":             {use: 30, req: 100, ok: false},
		"well used no reco":                {use: 50, req: 100, ok: false},
		"over committed no reco":           {use: 120, req: 100, ok: false},
		"no usage no reco":                 {use: 0, req: 100, ok: false},
		"no request no reco":               {use: 10, req: 0, ok: false},
	}

	for k := range uu {
		u := uu[k]
		t.Run(k, func(t *testing.T) {
			got, ok := reco(u.use, u.req)
			assert.Equal(t, u.ok, ok)
			if u.ok {
				assert.Equal(t, u.want, got)
			}
		})
	}
}

func TestSizingWaste(t *testing.T) {
	uu := map[string]struct {
		use, req int64
		pods     int
		want     int64
	}{
		"gap times pods":   {use: 20, req: 100, pods: 3, want: 240},
		"single pod":       {use: 20, req: 100, pods: 1, want: 80},
		"zero pods clamps": {use: 20, req: 100, pods: 0, want: 80},
		"fully used":       {use: 100, req: 100, pods: 3, want: 0},
		"over committed":   {use: 120, req: 100, pods: 3, want: 0},
		"no usage":         {use: 0, req: 100, pods: 3, want: 0},
	}

	for k := range uu {
		u := uu[k]
		t.Run(k, func(t *testing.T) {
			assert.Equal(t, u.want, waste(u.use, u.req, u.pods))
		})
	}
}

func TestFmtMem(t *testing.T) {
	uu := map[string]struct {
		bytes int64
		want  string
	}{
		"zero":          {bytes: 0, want: "0"},
		"round Mi":      {bytes: 384 * 1024 * 1024, want: "384Mi"},
		"round Gi":      {bytes: 5 * 1024 * 1024 * 1024, want: "5Gi"},
		"arbitrary Mi":  {bytes: 504334290, want: "481Mi"},
		"arbitrary Gi":  {bytes: 3286298316, want: "3.1Gi"},
		"just under Gi": {bytes: 1023 * 1024 * 1024, want: "1023Mi"},
	}

	for k := range uu {
		u := uu[k]
		t.Run(k, func(t *testing.T) {
			assert.Equal(t, u.want, fmtMem(u.bytes))
		})
	}
}

func TestSizingUsageQuery(t *testing.T) {
	uu := map[string]struct {
		metric    string
		ns        string
		isCounter bool
		want      string
	}{
		"cpu all namespaces": {
			metric: "container_cpu_usage_seconds_total", ns: "all", isCounter: true,
			want: `sum by (namespace, pod) (rate(container_cpu_usage_seconds_total{container!=""}[24h]))`,
		},
		"mem scoped namespace": {
			metric: "container_memory_working_set_bytes", ns: "ha-conversations", isCounter: false,
			want: `sum by (namespace, pod) (avg_over_time(container_memory_working_set_bytes{container!="",namespace="ha-conversations"}[24h]))`,
		},
	}

	for k := range uu {
		u := uu[k]
		t.Run(k, func(t *testing.T) {
			assert.Equal(t, u.want, sizingUsageQuery(u.metric, u.ns, u.isCounter))
		})
	}
}
