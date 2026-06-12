// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package model1

import (
	"fmt"
	"log/slog"
	"slices"
	"sort"

	"github.com/fvbommel/sortorder"
	"k8s.io/apimachinery/pkg/util/sets"
)

type ReRangeFn func(int, RowEvent) bool

// ResEvent represents a resource event.
type ResEvent int

// RowEvent tracks resource instance events.
type RowEvent struct {
	Kind   ResEvent
	Row    Row
	Deltas DeltaRow
}

// NewRowEvent returns a new row event.
func NewRowEvent(kind ResEvent, row Row) RowEvent {
	return RowEvent{
		Kind: kind,
		Row:  row,
	}
}

// NewRowEventWithDeltas returns a new row event with deltas.
func NewRowEventWithDeltas(row Row, delta DeltaRow) RowEvent {
	return RowEvent{
		Kind:   EventUpdate,
		Row:    row,
		Deltas: delta,
	}
}

// Clone returns a row event deep copy.
func (r RowEvent) Clone() RowEvent {
	return RowEvent{
		Kind:   r.Kind,
		Row:    r.Row.Clone(),
		Deltas: r.Deltas.Clone(),
	}
}

// Customize returns a new subset based on the given column indices.
func (r RowEvent) Customize(cols []int) RowEvent {
	delta := r.Deltas
	if !r.Deltas.IsBlank() {
		delta = make(DeltaRow, len(cols))
		r.Deltas.Customize(cols, delta)
	}

	return RowEvent{
		Kind:   r.Kind,
		Deltas: delta,
		Row:    r.Row.Customize(cols),
	}
}

// ExtractHeaderLabels extract collection of fields into header.
func (r RowEvent) ExtractHeaderLabels(labelCol int) []string {
	hh, _ := sortLabels(labelize(r.Row.Fields[labelCol]))
	return hh
}

// Labelize returns a new row event based on labels.
func (r RowEvent) Labelize(cols []int, labelCol int, labels []string) RowEvent {
	return RowEvent{
		Kind:   r.Kind,
		Deltas: r.Deltas.Labelize(cols, labelCol),
		Row:    r.Row.Labelize(cols, labelCol, labels),
	}
}

// Diff returns true if the row changed.
func (r RowEvent) Diff(re RowEvent, ageCol int) bool {
	if r.Kind != re.Kind {
		return true
	}
	if r.Deltas.Diff(re.Deltas, ageCol) {
		return true
	}
	return r.Row.Diff(re.Row, ageCol)
}

// ----------------------------------------------------------------------------

type reIndex map[string]int

// RowEvents a collection of row events.
type RowEvents struct {
	events []RowEvent
	index  reIndex
}

func NewRowEvents(size int) *RowEvents {
	return &RowEvents{
		events: make([]RowEvent, 0, size),
		index:  make(reIndex, size),
	}
}

func NewRowEventsWithEvts(ee ...RowEvent) *RowEvents {
	re := NewRowEvents(len(ee))
	for _, e := range ee {
		re.Add(e)
	}

	return re
}

func (r *RowEvents) reindex() {
	for i, e := range r.events {
		r.index[e.Row.ID] = i
	}
}

func (r *RowEvents) At(i int) (RowEvent, bool) {
	if i < 0 || i > len(r.events) {
		return RowEvent{}, false
	}

	return r.events[i], true
}

func (r *RowEvents) Set(i int, re RowEvent) {
	r.events[i] = re
	r.index[re.Row.ID] = i
}

func (r *RowEvents) Add(re RowEvent) {
	r.events = append(r.events, re)
	r.index[re.Row.ID] = len(r.events) - 1
}

// ExtractHeaderLabels extract header labels.
func (r *RowEvents) ExtractHeaderLabels(labelCol int) []string {
	ll := make([]string, 0, 10)
	for _, re := range r.events {
		ll = append(ll, re.ExtractHeaderLabels(labelCol)...)
	}

	return ll
}

// Labelize converts labels into a row event.
func (r *RowEvents) Labelize(cols []int, labelCol int, labels []string) *RowEvents {
	out := make([]RowEvent, 0, len(r.events))
	for _, re := range r.events {
		out = append(out, re.Labelize(cols, labelCol, labels))
	}

	return NewRowEventsWithEvts(out...)
}

// Customize returns custom row events based on columns layout.
func (r *RowEvents) Customize(cols []int) *RowEvents {
	ee := make([]RowEvent, 0, len(cols))
	for _, re := range r.events {
		ee = append(ee, re.Customize(cols))
	}

	return NewRowEventsWithEvts(ee...)
}

// Diff returns true if the event changed.
func (r *RowEvents) Diff(re *RowEvents, ageCol int) bool {
	if len(r.events) != len(re.events) {
		return true
	}
	for i := range r.events {
		if r.events[i].Diff(re.events[i], ageCol) {
			return true
		}
	}

	return false
}

// Clone returns a deep copy.
func (r *RowEvents) Clone() *RowEvents {
	re := make([]RowEvent, 0, len(r.events))
	for _, e := range r.events {
		re = append(re, e.Clone())
	}

	return NewRowEventsWithEvts(re...)
}

// Upsert add or update a row if it exists.
func (r *RowEvents) Upsert(re RowEvent) {
	if idx, ok := r.FindIndex(re.Row.ID); ok {
		r.events[idx] = re
	} else {
		r.Add(re)
	}
}

// DeleteAll removes all rows whose id is in victims, reindexing once.
func (r *RowEvents) DeleteAll(victims sets.Set[string]) {
	if victims.Len() == 0 {
		return
	}
	out := r.events[:0]
	for _, e := range r.events {
		if victims.Has(e.Row.ID) {
			delete(r.index, e.Row.ID)
			continue
		}
		out = append(out, e)
	}
	r.events = out
	r.reindex()
}

// Delete removes an element by id.
func (r *RowEvents) Delete(fqn string) error {
	victim, ok := r.FindIndex(fqn)
	if !ok {
		return fmt.Errorf("unable to delete row with fqn: %q", fqn)
	}
	r.events = append(r.events[0:victim], r.events[victim+1:]...)
	delete(r.index, fqn)
	r.reindex()

	return nil
}

func (r *RowEvents) Len() int {
	return len(r.events)
}

func (r *RowEvents) Empty() bool {
	return len(r.events) == 0
}

// Clear delete all row events.
func (r *RowEvents) Clear() {
	r.events = r.events[:0]
	for k := range r.index {
		delete(r.index, k)
	}
}

func (r *RowEvents) Range(f ReRangeFn) {
	for i, e := range r.events {
		if !f(i, e) {
			return
		}
	}
}

func (r *RowEvents) Get(id string) (RowEvent, bool) {
	i, ok := r.index[id]
	if !ok {
		return RowEvent{}, false
	}

	return r.At(i)
}

// FindIndex locates a row index by id. Returns false is not found.
func (r *RowEvents) FindIndex(id string) (int, bool) {
	i, ok := r.index[id]

	return i, ok
}

// Same reports whether both collections hold exactly the same events,
// regardless of ordering.
func (r *RowEvents) Same(re *RowEvents) bool {
	if len(r.events) != len(re.events) {
		return false
	}
	for i := range re.events {
		e := &re.events[i]
		o, ok := r.Get(e.Row.ID)
		if !ok || o.Kind != e.Kind ||
			!slices.Equal(o.Row.Fields, e.Row.Fields) ||
			!slices.Equal(o.Deltas, e.Deltas) {
			return false
		}
	}

	return true
}

// HasChanges returns true if any row event has a kind other than EventUnchanged.
// Used to skip unnecessary UI refreshes when the informer cache is idle.
func (r *RowEvents) HasChanges() bool {
	for _, e := range r.events {
		if e.Kind != EventUnchanged {
			return true
		}
	}
	return false
}

// Sort rows based on column index and order.
// Sort keys are computed once per row: parsing durations/quantities in the
// comparator would cost O(n log n) parses on every refresh.
func (r *RowEvents) Sort(_ string, sortCol int, isDuration, numCol, isCapacity, asc bool) {
	if r == nil || sortCol == -1 {
		return
	}

	ee := r.events
	keys := make([]sortKey, len(ee))
	for i := range ee {
		var v string
		if sortCol < len(ee[i].Row.Fields) {
			v = ee[i].Row.Fields[sortCol]
		}
		keys[i] = makeSortKey(v, isDuration, numCol, isCapacity)
	}

	idx := make([]int, len(ee))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		i, j := idx[a], idx[b]
		c := keys[i].cmp(&keys[j])
		if c != 0 {
			return c < 0
		}
		return sortorder.NaturalLess(ee[i].Row.ID, ee[j].Row.ID)
	})
	// Descending is the exact mirror of ascending, ties included.
	if !asc {
		slices.Reverse(idx)
	}

	out := make([]RowEvent, len(ee))
	for a, i := range idx {
		out[a] = ee[i]
	}
	r.events = out
	r.reindex()
}

// For debugging...
func (re RowEvents) Dump(msg string) {
	slog.Debug("[DEBUG] RowEvents" + msg)
	for _, r := range re.events {
		slog.Debug(fmt.Sprintf("   %#v", r))
	}
}
