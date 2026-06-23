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
	tlDefaultCols = 60  // band width before the first layout is known
	tlMinCols     = 20  // don't shrink the band below this
	tlMaxCols     = 500 // sanity cap on very wide terminals
	tlWidthPct    = 40  // band uses this % of the available pane width
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
	list     *tview.TextView
	detail   *tview.TextView
	actions  *ui.KeyActions
	objects  []tlObject
	selIndex int
	windowIx int
	cols     int // band width in buckets, derived from the pane width
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
		list:     tview.NewTextView(),
		detail:   tview.NewTextView(),
		actions:  ui.NewKeyActions(),
		windowIx: 2, // default 1h
		cols:     tlDefaultCols,
	}
}

// Init initializes the view.
func (t *Timeline) Init(_ context.Context) error {
	t.SetBorder(true)
	t.SetBorderPadding(0, 0, 1, 1)
	t.updateTitle()

	t.axis.SetDynamicColors(true).SetWrap(false)
	t.list.SetDynamicColors(true).SetWrap(false).SetScrollable(true)
	t.list.SetInputCapture(t.keyboard)

	t.detail.SetDynamicColors(true).SetScrollable(true).SetWrap(true)
	t.detail.SetBorder(true)
	t.detail.SetTitle(" Events ")

	t.AddItem(t.axis, 1, 0, false)
	t.AddItem(t.list, 0, 3, true)
	t.AddItem(t.detail, 0, 2, false)

	t.app.Styles.AddListener(t)
	t.StylesChanged(t.app.Styles)

	t.load()
	t.app.SetFocus(t.list)

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
	if t.selIndex >= len(objs) {
		t.selIndex = 0
	}
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

	start := time.Now().Add(-t.window())
	for _, rs := range rss {
		podRows := make([]tlObject, 0)
		for i := range pods.Items {
			p := &pods.Items[i]
			if !ownedBy(p.OwnerReferences, rs.uid) {
				continue
			}
			podRows = append(podRows, tlObject{
				kind:   "Pod",
				name:   p.Name,
				indent: 2,
				uid:    string(p.UID),
				birth:  p.CreationTimestamp.Time,
				live:   podLiveSeverity(p),
				events: byUID[string(p.UID)],
			})
		}
		// Skip idle leftover ReplicaSets: no pods and no recent activity.
		if len(podRows) == 0 && !hasEventInWindow(byUID[rs.uid], start) {
			continue
		}
		objs = append(objs, tlObject{
			kind:   "ReplicaSet",
			name:   rs.name,
			indent: 1,
			uid:    rs.uid,
			birth:  rs.birth,
			live:   sevNormal,
			events: byUID[rs.uid],
		})
		objs = append(objs, podRows...)
	}

	return objs, nil
}

type appsRS struct {
	name, uid string
	birth     time.Time
}

// nameWidth returns the display width of the widest (indented) object name.
func (t *Timeline) nameWidth() int {
	w := 0
	for i := range t.objects {
		if n := t.objects[i].indent*2 + len(t.objects[i].name); n > w {
			w = n
		}
	}
	return w
}

// render rebuilds the axis, the band list and the detail of the current row.
func (t *Timeline) render() {
	t.updateTitle()
	t.paint()
	t.selectionChanged()
}

// paint rebuilds the width-dependent content (axis + bands) at the current
// band width.
func (t *Timeline) paint() {
	nw := t.nameWidth()
	t.axis.SetText(strings.Repeat(" ", nw+1) + "[gray::d]" + t.axisLine())
	t.renderList(nw)
}

// Draw recomputes the band width from the available pane width before drawing,
// so the timeline fills the screen and reflows on resize.
func (t *Timeline) Draw(screen tcell.Screen) {
	if _, _, w, _ := t.GetInnerRect(); w > 0 {
		c := min((w-t.nameWidth()-1)*tlWidthPct/100, tlMaxCols)
		if c >= tlMinCols && c != t.cols {
			t.cols = c
			t.paint()
		}
	}
	t.Flex.Draw(screen)
}

// renderList draws every object's band, highlighting the selected row.
func (t *Timeline) renderList(nw int) {
	start := time.Now().Add(-t.window())
	var b strings.Builder
	for i := range t.objects {
		o := &t.objects[i]
		name := strings.Repeat("  ", o.indent) + o.name
		name += strings.Repeat(" ", nw-len([]rune(name)))
		if i == t.selIndex {
			fmt.Fprintf(&b, "[black:aqua:b]%s[-:-:-] %s\n", name, t.band(o, start))
		} else {
			// Reset first: tview dynamic colors carry across newlines, so without
			// this the trailing color of the previous band bleeds into the name.
			fmt.Fprintf(&b, "[-:-:-]%s %s\n", name, t.band(o, start))
		}
	}
	t.list.SetText(b.String())
}

// band builds the colored state band of an object over the look-back window.
func (t *Timeline) band(o *tlObject, start time.Time) string {
	n := t.cols
	bucket := t.window() / time.Duration(n)
	if bucket <= 0 {
		bucket = time.Second
	}
	birthIdx := int(o.birth.Sub(start) / bucket)

	sev := make([]int, n)
	mark := make([]int, n) // event glyph severity per bucket, 0 = none
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
		if idx < 0 || idx >= n {
			continue
		}
		cur := classifyEvent(e)
		for j := idx; j < n; j++ { // carry forward; later events override the tail
			if j >= birthIdx {
				sev[j] = cur
			}
		}
		if cur > mark[idx] {
			mark[idx] = cur
		}
	}
	// "Now" reflects the live status, regardless of carried-forward severity.
	if o.live != sevNone {
		sev[n-1] = o.live
	}

	var b strings.Builder
	for i := range n {
		switch {
		case i < birthIdx:
			b.WriteString(" ") // object does not exist yet
		case mark[i] != sevNone:
			// A big dot marks a bucket where an event occurred, colored by its
			// severity so it pops out of the thin state track.
			fmt.Fprintf(&b, "[%s::b]●", sevColor(mark[i]))
		default:
			// A thin colored track of small dots conveys the carried-forward
			// state without the heavy solid band.
			fmt.Fprintf(&b, "[%s::-]·", sevColor(sev[i]))
		}
	}

	return b.String()
}

func (t *Timeline) axisLine() string {
	w := t.window()
	left := "-" + humanizeDur(w)
	mid := "-" + humanizeDur(w/2)
	right := "now"

	line := []rune(strings.Repeat("─", t.cols))
	put := func(pos int, s string) {
		for i, r := range []rune(s) {
			if pos+i >= 0 && pos+i < len(line) {
				line[pos+i] = r
			}
		}
	}
	put(0, left)
	put(t.cols/2-len([]rune(mid))/2, mid)
	put(t.cols-len([]rune(right)), right)

	return string(line)
}

// selectionChanged refreshes the detail pane for the selected row.
func (t *Timeline) selectionChanged() {
	if t.selIndex < 0 || t.selIndex >= len(t.objects) {
		return
	}
	o := &t.objects[t.selIndex]
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
		fmt.Fprintf(&b, "[gray::d]%5s  [%s::b]%-8s[-:-:-]  [%s::b]%-22s[-:-:-]  %s%s\n",
			age, col, e.Type, col, e.Reason, e.Message, count)
	}
	t.detail.SetText(b.String())
	t.detail.ScrollToBeginning()
}

// move shifts the selection cursor and keeps it visible.
func (t *Timeline) move(delta int) {
	if len(t.objects) == 0 {
		return
	}
	t.selIndex += delta
	if t.selIndex < 0 {
		t.selIndex = 0
	}
	if t.selIndex >= len(t.objects) {
		t.selIndex = len(t.objects) - 1
	}
	t.renderList(t.nameWidth())
	t.list.ScrollTo(max(0, t.selIndex-1), 0)
	t.selectionChanged()
}

func (t *Timeline) keyboard(evt *tcell.EventKey) *tcell.EventKey {
	switch evt.Key() {
	case tcell.KeyEscape:
		return t.app.PrevCmd(evt)
	case tcell.KeyUp:
		t.move(-1)
		return nil
	case tcell.KeyDown:
		t.move(1)
		return nil
	case tcell.KeyRune:
		switch evt.Rune() {
		case 'k':
			t.move(-1)
			return nil
		case 'j':
			t.move(1)
			return nil
		case 'q':
			return t.app.PrevCmd(evt)
		case 'r':
			t.load()
			return nil
		case '>', '+', '=':
			if t.windowIx < len(tlWindows)-1 {
				t.windowIx++
				t.load()
			}
			return nil
		case '<':
			if t.windowIx > 0 {
				t.windowIx--
				t.load()
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
	t.axis.SetBackgroundColor(s.BgColor())
	t.list.SetBackgroundColor(s.BgColor())
	t.list.SetTextColor(s.FgColor())
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
		{Mnemonic: "j/k", Description: "Up/Down", Visible: true},
		{Mnemonic: "r", Description: "Refresh", Visible: true},
		{Mnemonic: ">", Description: "Wider window", Visible: true},
		{Mnemonic: "<", Description: "Shorter window", Visible: true},
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
	// Some unhealthy events are emitted as Normal by the kubelet (notably
	// Killing after a failed probe), so judge by reason/message too, not just
	// the event Type.
	if containsAny(e.Reason, "BackOff", "Failed", "OOM", "CrashLoop", "Evicted", "Error") ||
		containsAny(e.Message, "liveness probe", "readiness probe", "startup probe", "will be restarted") {
		return sevError
	}
	if e.Type == v1.EventTypeWarning || containsAny(e.Reason, "Unhealthy", "Preempt", "NodeNotReady") {
		return sevWarning
	}
	return sevNormal
}

// hasEventInWindow reports whether any event falls within the look-back window.
func hasEventInWindow(evs []*v1.Event, start time.Time) bool {
	for _, e := range evs {
		if !eventTime(e).Before(start) {
			return true
		}
	}
	return false
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
