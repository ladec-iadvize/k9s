// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package model1_test

import (
	"testing"

	"github.com/derailed/k9s/internal/client"
	"github.com/derailed/k9s/internal/model1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
)

func TestCompileFilterRx(t *testing.T) {
	rx1, err := model1.CompileFilterRx(`(?i)(fred)`)
	require.NoError(t, err)
	rx2, err := model1.CompileFilterRx(`(?i)(fred)`)
	require.NoError(t, err)
	assert.Same(t, rx1, rx2)
	assert.True(t, rx1.MatchString("FrEd-blee"))

	_, err = model1.CompileFilterRx(`(`)
	require.Error(t, err)
}

func TestRowEventsDeleteAll(t *testing.T) {
	re := model1.NewRowEventsWithEvts(
		model1.RowEvent{Row: model1.Row{ID: "A", Fields: model1.Fields{"1"}}},
		model1.RowEvent{Row: model1.Row{ID: "B", Fields: model1.Fields{"2"}}},
		model1.RowEvent{Row: model1.Row{ID: "C", Fields: model1.Fields{"3"}}},
		model1.RowEvent{Row: model1.Row{ID: "D", Fields: model1.Fields{"4"}}},
	)

	re.DeleteAll(sets.New("A", "C"))

	assert.Equal(t, 2, re.Len())
	_, ok := re.Get("A")
	assert.False(t, ok)
	_, ok = re.Get("C")
	assert.False(t, ok)
	b, ok := re.Get("B")
	assert.True(t, ok)
	assert.Equal(t, "B", b.Row.ID)
	d, ok := re.Get("D")
	assert.True(t, ok)
	assert.Equal(t, "D", d.Row.ID)

	// No-op delete keeps everything intact.
	re.DeleteAll(sets.New[string]())
	assert.Equal(t, 2, re.Len())
}

func TestTableDataSame(t *testing.T) {
	gvr := client.NewGVR("v1/pods")
	h := model1.Header{
		model1.HeaderColumn{Name: "NAME"},
		model1.HeaderColumn{Name: "AGE"},
	}
	mkTable := func(rr ...model1.RowEvent) *model1.TableData {
		return model1.NewTableDataFull(gvr, "ns1", h, model1.NewRowEventsWithEvts(rr...))
	}

	r1 := model1.RowEvent{Row: model1.Row{ID: "ns1/a", Fields: model1.Fields{"a", "2d"}}}
	r2 := model1.RowEvent{Row: model1.Row{ID: "ns1/b", Fields: model1.Fields{"b", "5m"}}}

	uu := map[string]struct {
		t1, t2 *model1.TableData
		e      bool
	}{
		"identical": {
			t1: mkTable(r1, r2),
			t2: mkTable(r1, r2),
			e:  true,
		},
		"reordered": {
			t1: mkTable(r2, r1),
			t2: mkTable(r1, r2),
			e:  true,
		},
		"age_changed": {
			t1: mkTable(r1, r2),
			t2: mkTable(r1, model1.RowEvent{Row: model1.Row{ID: "ns1/b", Fields: model1.Fields{"b", "6m"}}}),
			e:  false,
		},
		"kind_changed": {
			t1: mkTable(r1, r2),
			t2: mkTable(r1, model1.RowEvent{Kind: model1.EventUpdate, Row: r2.Row}),
			e:  false,
		},
		"row_added": {
			t1: mkTable(r1),
			t2: mkTable(r1, r2),
			e:  false,
		},
	}

	for k := range uu {
		u := uu[k]
		t.Run(k, func(t *testing.T) {
			assert.Equal(t, u.e, u.t1.Same(u.t2))
		})
	}
}
