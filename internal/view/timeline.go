// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package view

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/derailed/k9s/internal/client"
	"github.com/derailed/k9s/internal/config"
	"github.com/derailed/k9s/internal/model"
	"github.com/derailed/k9s/internal/slogs"
	"github.com/derailed/k9s/internal/ui"
	"github.com/derailed/k9s/internal/view/cmd"
	"github.com/derailed/tcell/v2"
	"github.com/derailed/tview"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

const (
	timelineTitle = "Timeline"
	tlCols        = 60 // number of buckets drawn in a frieze band
	tlRefreshDur  = 30 * time.Second
)

// Event/band severity levels (carried forward to colorize the state band).
const (
	sevNone = iota // before object creation
	sevNormal
	sevWarning
	sevError
)

// tlWindows are the selectable look-back windows, cycled with +/-.
var tlWindows = []time.Duration{
	15 * time.Minute,
	30 * time.Minute,
	time.Hour,
	3 * time.Hour,
	6 * time.Hour,
	12 * time.Hour,
	24 * time.Hour,
}

// tlObject is one row of the timeline: the deployment, a replicaset or a pod.
type tlObject struct {
	kind   string
	name   string
	indent int
	uid    string
	birth  time.Time
	live   int // current severity derived from live status
	events []*v1.Event
}

// Timeline renders a per-pod chronological state band for a Deployment plus a
// detail pane listing the events of the selected row. It is a read-only,
// on-demand view: it fetches once on open, refreshes gently, and never streams.
type Timeline struct {
	*tview.Flex

	app      *App
	gvr      *client.GVR
	path     string
	selector labels.Selector

	axis     *tview.TextView
	table    *tview.Table
	detail   *tview.TextView
	actions  *ui.KeyActions
	objects  []tlObject
	windowIx int
	cancelFn context.CancelFunc
}

// NewTimeline returns a new deployment events timeline.
func NewTimeline(app *App, gvr *client.GVR, path string, sel labels.Selector) *Timeline {
	return &Timeline{
		Flex:     tview.NewFlex().SetDirection(tview.FlexRow),
		app:      app,
		gvr:      gvr,
		path:     path,
		selector: sel,
		axis:     tview.NewTextView(),
		table:    tview.NewTable(),
		detail:   tview.NewTextView(),
		actions:  ui.NewKeyActions(),
		windowIx: 2, // default 1h
	}
}

// Init initializes the view.
func (t *Timeline) Init(_ context.Context) error {
	t.SetBorder(true)
	t.SetBorderPadding(0, 0, 1, 1)
	t.updateTitle()

	t.axis.SetDynamicColors(true)
	t.table.SetBorder(false)
	t.table.SetSelectable(true, false)
	t.table.SetFixed(1, 0)
	t.table.SetSelectionChangedFunc(t.selectionChanged)
	t.table.SetInputCapture(t.keyboard)

	t.detail.SetDynamicColors(true).SetScrollable(true).SetWrap(true)
	t.detail.SetBorder(true)
	t.detail.SetTitle(" Events ")

	t.AddItem(t.axis, 1, 0, false)
	t.AddItem(t.table, 0, 3, true)
	t.AddItem(t.detail, 0, 2, false)

	t.app.Styles.AddListener(t)
	t.StylesChanged(t.app.Styles)

	t.load()
	t.app.SetFocus(t.table)

	return nil
}

func (t *Timeline) window() time.Duration { return tlWindows[t.windowIx] }

// load fetches pods, replicasets and events then (re)builds the bands.
func (t *Timeline) load() {
	objs, err := t.fetch()
	if err != nil {
		t.app.Flash().Err(err)
		return
	}
	t.objects = objs
	t.render()
}

func (t *Timeline) fetch() ([]tlObject, error) {
	dial, err := t.app.Conn().Dial()
	if err != nil {
		return nil, err
	}
	ns, name := client.Namespaced(t.path)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	opts := metav1.ListOptions{LabelSelector: t.selector.String()}
	pods, err := dial.CoreV1().Pods(ns).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	rsList, err := dial.AppsV1().ReplicaSets(ns).List(ctx, opts)
	if err != nil {
		return nil, err
	}
	dp, err := dial.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	evs, err := dial.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	// Index events by the involved object UID.
	byUID := make(map[string][]*v1.Event)
	for i := range evs.Items {
		e := &evs.Items[i]
		uid := string(e.InvolvedObject.UID)
		byUID[uid] = append(byUID[uid], e)
	}

	objs := []tlObject{{
		kind:   "Deployment",
		name:   dp.Name,
		uid:    string(dp.UID),
		birth:  dp.CreationTimestamp.Time,
		live:   sevNormal,
		events: byUID[string(dp.UID)],
	}}

	// ReplicaSets owned by this deployment, newest first, each followed by its pods.
	rss := make([]*appsRS, 0, len(rsList.Items))
	for i := range rsList.Items {
		rs := &rsList.Items[i]
		if !ownedBy(rs.OwnerReferences, string(dp.UID)) {
			continue
		}
		rss = append(rss, &appsRS{name: rs.Name, uid: string(rs.UID), birth: rs.CreationTimestamp.Time})
	}
	sort.Slice(rss, func(i, j int) bool { return rss[i].birth.After(rss[j].birth) })

	for _, rs := range rss {
		objs = append(objs, tlObject{
			kind:   "ReplicaSet",
			name:   rs.name,
			indent: 1,
			uid:    rs.uid,
			birth:  rs.birth,
			live:   sevNormal,
			events: byUID[rs.uid],
		})
		for i := range pods.Items {
			p := &pods.Items[i]
			if !ownedBy(p.OwnerReferences, rs.uid) {
				continue
			}
			objs = append(objs, tlObject{
				kind:   "Pod",
				name:   p.Name,
				indent: 2,
				uid:    string(p.UID),
				birth:  p.CreationTimestamp.Time,
				live:   podLiveSeverity(p),
				events: byUID[string(p.UID)],
			})
		}
	}

	return objs, nil
}

type appsRS struct {
	name, uid string
	birth     time.Time
}

// render rebuilds the axis, the band table and the detail of the current row.
func (t *Timeline) render() {
	now := time.Now()
	start := now.Add(-t.window())
	t.axis.SetText(t.axisLine())
	t.updateTitle()

	t.table.Clear()
	t.table.SetCell(0, 0, tview.NewTableCell("").SetSelectable(false))
	t.table.SetCell(0, 1, tview.NewTableCell("[gray::d]"+t.axisLine()).SetSelectable(false))

	for i := range t.objects {
		o := &t.objects[i]
		name := strings.Repeat("  ", o.indent) + o.name
		t.table.SetCell(i+1, 0, tview.NewTableCell(name).SetExpansion(0))
		t.table.SetCell(i+1, 1, tview.NewTableCell(t.band(o, start)).SetExpansion(1))
	}

	if t.table.GetRowCount() > 1 {
		t.table.Select(1, 0)
		t.selectionChanged(1, 0)
	}
}

// band builds the colored state band of an object over the look-back window.
func (t *Timeline) band(o *tlObject, start time.Time) string {
	bucket := t.window() / tlCols
	if bucket <= 0 {
		bucket = time.Second
	}
	birthIdx := int(o.birth.Sub(start) / bucket)

	sev := make([]int, tlCols)
	mark := make([]int, tlCols) // event glyph severity per bucket, 0 = none
	cur := sevNormal
	for i := range sev {
		if i < birthIdx {
			sev[i] = sevNone
		} else {
			sev[i] = sevNormal
		}
	}

	sorted := append([]*v1.Event(nil), o.events...)
	sort.Slice(sorted, func(i, j int) bool { return eventTime(sorted[i]).Before(eventTime(sorted[j])) })
	for _, e := range sorted {
		idx := int(eventTime(e).Sub(start) / bucket)
		if idx < 0 || idx >= tlCols {
			continue
		}
		cur = classifyEvent(e)
		for j := idx; j < tlCols; j++ { // carry forward; later events override the tail
			if j >= birthIdx {
				sev[j] = cur
			}
		}
		if s := classifyEvent(e); s > mark[idx] {
			mark[idx] = s
		}
	}
	// "Now" reflects the live status, regardless of carried-forward severity.
	if o.live != sevNone {
		sev[tlCols-1] = o.live
	}

	var b strings.Builder
	for i := range tlCols {
		switch {
		case i < birthIdx:
			b.WriteString("[gray::d]·")
		case mark[i] != sevNone:
			fmt.Fprintf(&b, "[%s::b]◆", sevColor(mark[i]))
		default:
			fmt.Fprintf(&b, "[%s]█", sevColor(sev[i]))
		}
	}

	return b.String()
}

func (t *Timeline) axisLine() string {
	w := t.window()
	left := "-" + humanizeDur(w)
	mid := "-" + humanizeDur(w/2)
	right := "now"

	line := []rune(strings.Repeat("─", tlCols))
	put := func(pos int, s string) {
		for i, r := range []rune(s) {
			if pos+i >= 0 && pos+i < len(line) {
				line[pos+i] = r
			}
		}
	}
	put(0, left)
	put(tlCols/2-len([]rune(mid))/2, mid)
	put(tlCols-len([]rune(right)), right)

	return string(line)
}

// selectionChanged refreshes the detail pane for the selected row.
func (t *Timeline) selectionChanged(row, _ int) {
	if row < 1 || row-1 >= len(t.objects) {
		return
	}
	o := &t.objects[row-1]
	t.detail.SetTitle(fmt.Sprintf(" Events · %s/%s ", strings.ToLower(o.kind), o.name))

	if len(o.events) == 0 {
		t.detail.SetText("[gray::d]  No events in the current window.")
		t.detail.ScrollToBeginning()
		return
	}

	sorted := append([]*v1.Event(nil), o.events...)
	sort.Slice(sorted, func(i, j int) bool { return eventTime(sorted[i]).Before(eventTime(sorted[j])) })

	now := time.Now()
	var b strings.Builder
	for _, e := range sorted {
		age := humanizeDur(now.Sub(eventTime(e)))
		count := ""
		if e.Count > 1 {
			count = fmt.Sprintf("  [gray::d](x%d)", e.Count)
		}
		col := sevColor(classifyEvent(e))
		fmt.Fprintf(&b, "[gray::d]%5s  [%s::b]%-8s[-:-:-]  [%s]%-22s[-:-:-]  %s%s\n",
			age, col, e.Type, col, e.Reason, e.Message, count)
	}
	t.detail.SetText(b.String())
	t.detail.ScrollToBeginning()
}

func (t *Timeline) keyboard(evt *tcell.EventKey) *tcell.EventKey {
	switch evt.Key() {
	case tcell.KeyEscape:
		return t.app.PrevCmd(evt)
	case tcell.KeyRune:
		switch evt.Rune() {
		case 'q':
			return t.app.PrevCmd(evt)
		case 'r':
			t.load()
			return nil
		case '+':
			if t.windowIx < len(tlWindows)-1 {
				t.windowIx++
				t.render()
			}
			return nil
		case '-':
			if t.windowIx > 0 {
				t.windowIx--
				t.render()
			}
			return nil
		}
	}

	return evt
}

func (t *Timeline) updateTitle() {
	frame := t.app.Styles.Frame()
	title := fmt.Sprintf(NSTitleFmt, timelineTitle, t.path+" · "+humanizeDur(t.window()))
	t.SetTitle(ui.SkinTitle(title, &frame))
}

// StylesChanged notifies the skin changed.
func (t *Timeline) StylesChanged(s *config.Styles) {
	t.SetBackgroundColor(s.BgColor())
	t.table.SetBackgroundColor(s.BgColor())
	t.detail.SetBackgroundColor(s.BgColor())
	t.detail.SetTextColor(s.FgColor())
	t.updateTitle()
}

// Start launches a gentle refresh ticker.
func (t *Timeline) Start() {
	t.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	t.cancelFn = cancel
	go func() {
		tick := time.NewTicker(tlRefreshDur)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				t.app.QueueUpdateDraw(func() { t.load() })
			}
		}
	}()
}

// Stop terminates the refresh ticker.
func (t *Timeline) Stop() {
	if t.cancelFn != nil {
		t.cancelFn()
		t.cancelFn = nil
	}
	t.app.Styles.RemoveListener(t)
}

// Name returns the component name.
func (*Timeline) Name() string { return timelineTitle }

// App returns the current app handle.
func (t *Timeline) App() *App { return t.app }

// Hints returns the view hints.
func (t *Timeline) Hints() model.MenuHints {
	return model.MenuHints{
		{Mnemonic: "r", Description: "Refresh", Visible: true},
		{Mnemonic: "+", Description: "Wider window", Visible: true},
		{Mnemonic: "-", Description: "Shorter window", Visible: true},
		{Mnemonic: "Esc", Description: "Back", Visible: true},
	}
}

// ExtraHints returns additional hints.
func (*Timeline) ExtraHints() map[string]string { return nil }

// InCmdMode checks if prompt is active.
func (*Timeline) InCmdMode() bool { return false }

func (*Timeline) SetCommand(*cmd.Interpreter)            {}
func (*Timeline) SetFilter(string, bool)                 {}
func (*Timeline) SetLabelSelector(labels.Selector, bool) {}

// GVR returns the resource descriptor.
func (t *Timeline) GVR() *client.GVR { return t.gvr }

// ----------------------------------------------------------------------------
// Helpers

func ownedBy(refs []metav1.OwnerReference, uid string) bool {
	for i := range refs {
		if string(refs[i].UID) == uid {
			return true
		}
	}
	return false
}

func eventTime(e *v1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.FirstTimestamp.Time
}

func classifyEvent(e *v1.Event) int {
	if e.Type != v1.EventTypeWarning {
		return sevNormal
	}
	switch {
	case containsAny(e.Reason, "BackOff", "Failed", "OOM", "Unhealthy", "Evicted", "Kill", "Error", "CrashLoop"):
		return sevError
	default:
		return sevWarning
	}
}

func podLiveSeverity(p *v1.Pod) int {
	switch p.Status.Phase {
	case v1.PodFailed:
		return sevError
	case v1.PodSucceeded:
		return sevNormal
	}
	for i := range p.Status.ContainerStatuses {
		cs := p.Status.ContainerStatuses[i]
		if cs.State.Waiting != nil && containsAny(cs.State.Waiting.Reason, "BackOff", "CrashLoop", "Err") {
			return sevError
		}
		if !cs.Ready {
			return sevWarning
		}
	}
	if p.Status.Phase == v1.PodPending {
		return sevWarning
	}
	return sevNormal
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func sevColor(sev int) string {
	switch sev {
	case sevError:
		return "red"
	case sevWarning:
		return "orange"
	case sevNormal:
		return "green"
	default:
		return "gray"
	}
}

// humanizeDur renders a duration compactly: 45m, 6h, 2d.
func humanizeDur(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// openTimeline pushes the timeline view onto the page stack.
func openTimeline(app *App, gvr *client.GVR, path string, sel labels.Selector) {
	v := NewTimeline(app, gvr, path, sel)
	ns, _ := client.Namespaced(path)
	if err := app.Config.SetActiveNamespace(ns); err != nil {
		slog.Error("Unable to set active namespace for timeline", slogs.Error, err)
	}
	if err := app.inject(v, false); err != nil {
		app.Flash().Err(err)
	}
}
