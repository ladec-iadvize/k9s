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

	"github.com/derailed/k9s/internal"
	"github.com/derailed/k9s/internal/client"
	"github.com/derailed/k9s/internal/config"
	"github.com/derailed/k9s/internal/model"
	"github.com/derailed/k9s/internal/slogs"
	"github.com/derailed/k9s/internal/ui"
	"github.com/derailed/k9s/internal/view/cmd"
	"github.com/derailed/k9s/internal/watch"
	"github.com/derailed/tcell/v2"
	"github.com/derailed/tview"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	fillBarWidth   = 32
	fillRefresh    = 2 * time.Second
	fillFillGlyph  = '█'
	fillEmptyGlyph = '░'
	fillEmptyColor = "#4a4f6a"
)

// topoPalette gives each pod a distinguishable color in the topology view.
var topoPalette = []string{
	"#a3be8c", "#88c0d0", "#ebcb8b", "#b48ead",
	"#d08770", "#81a1c1", "#8fbcbb", "#bf616a",
}

// barFunc renders a single CPU (cpu==true) or MEM bar for a node.
type barFunc func(v *nodeFillView, n *nodeTopo, cpu bool) string

// podReq holds a single pod resource request contribution on a node.
type podReq struct {
	cpu, mem int64
}

// nodeTopo aggregates filling data for a single node.
type nodeTopo struct {
	name               string
	instanceType       string
	nodeClass          string
	allocCPU, allocMEM int64
	usageCPU, usageMEM int64
	reqs               []podReq
	podCount           int
}

func (n *nodeTopo) cpuPerc() int { return client.ToPercentage(n.usageCPU, n.allocCPU) }
func (n *nodeTopo) memPerc() int { return client.ToPercentage(n.usageMEM, n.allocMEM) }

// meta returns the parenthesized node descriptor: nodeclass · type · pods.
func (n *nodeTopo) meta() string {
	parts := make([]string, 0, 3)
	if n.nodeClass != "" {
		parts = append(parts, n.nodeClass)
	}
	if n.instanceType != "" {
		parts = append(parts, n.instanceType)
	}
	parts = append(parts, fmt.Sprintf("%d pods", n.podCount))
	return strings.Join(parts, " · ")
}

// nodeFillView is a per-node CPU/MEM bar view, shared by the monitoring
// (live usage) and topology (per-pod reservation) views.
type nodeFillView struct {
	*tview.TextView

	app       *App
	gvr       *client.GVR
	title     string
	barFn     barFunc
	lightSel  bool // brighten the selection background
	actions   *ui.KeyActions
	cancelFn  context.CancelFunc
	nodes     []nodeTopo
	selected  int
	sortKey   string // "name", "cpu" or "mem"
	filter    string
	filtering bool
}

func newNodeFillView(gvr *client.GVR, title string, fn barFunc, lightSel bool) *nodeFillView {
	return &nodeFillView{
		TextView: tview.NewTextView(),
		gvr:      gvr,
		title:    title,
		barFn:    fn,
		lightSel: lightSel,
		actions:  ui.NewKeyActions(),
		sortKey:  "name",
	}
}

// NewNodeMonitoring returns a node live-usage view (à la lazy-for-kubernetes).
func NewNodeMonitoring(gvr *client.GVR) ResourceViewer {
	return newNodeFillView(gvr, "Node Monitoring", usageBar, false)
}

// NewNodeTopology returns a node reservation view (per-pod requests).
func NewNodeTopology(gvr *client.GVR) ResourceViewer {
	return newNodeFillView(gvr, "Node Topology", reservationBar, true)
}

// Init initializes the view.
func (v *nodeFillView) Init(ctx context.Context) error {
	var err error
	if v.app, err = extractApp(ctx); err != nil {
		return err
	}

	v.SetBorder(true)
	v.SetDynamicColors(true)
	v.SetRegions(true)
	v.SetWrap(false)
	v.SetBorderPadding(0, 0, 1, 1)
	v.SetInputCapture(v.keyboard)
	v.updateTitle()

	v.bindKeys()
	v.app.Styles.AddListener(v)
	v.StylesChanged(v.app.Styles)

	return nil
}

func (v *nodeFillView) bindKeys() {
	v.actions.Merge(ui.NewKeyActionsFromMap(ui.KeyMap{
		tcell.KeyEnter:  ui.NewKeyAction("Pods", v.enterCmd, true),
		tcell.KeyDown:   ui.NewKeyAction("Down", v.moveCmd(1), false),
		tcell.KeyUp:     ui.NewKeyAction("Up", v.moveCmd(-1), false),
		ui.KeyJ:         ui.NewKeyAction("Down", v.moveCmd(1), false),
		ui.KeyK:         ui.NewKeyAction("Up", v.moveCmd(-1), false),
		ui.KeyC:         ui.NewKeyAction("Sort CPU", v.sortCmd("cpu"), true),
		ui.KeyM:         ui.NewKeyAction("Sort MEM", v.sortCmd("mem"), true),
		ui.KeyN:         ui.NewKeyAction("Sort Name", v.sortCmd("name"), true),
		ui.KeySlash:     ui.NewKeyAction("Filter", v.activateFilterCmd, false),
		tcell.KeyEscape: ui.NewKeyAction("Clear", v.clearFilterCmd, false),
	}))
}

// StylesChanged notifies the skin changed.
func (v *nodeFillView) StylesChanged(s *config.Styles) {
	v.SetBackgroundColor(s.BgColor())
	v.SetTextColor(s.FgColor())
	v.SetBorderColor(s.Frame().Border.FgColor.Color())
	if v.app != nil {
		v.render()
	}
}

// Start initializes the refresh loop.
func (v *nodeFillView) Start() {
	v.Stop()

	ctx := context.WithValue(context.Background(), internal.KeyFactory, v.app.factory)
	ctx, v.cancelFn = context.WithCancel(ctx)

	v.refresh()
	go func() {
		tick := time.NewTicker(fillRefresh)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				v.refresh()
			}
		}
	}()
}

// Stop terminates the refresh loop.
func (v *nodeFillView) Stop() {
	if v.cancelFn == nil {
		return
	}
	v.cancelFn()
	v.cancelFn = nil
	if v.app != nil {
		v.app.Styles.RemoveListener(v)
	}
}

func (v *nodeFillView) refresh() {
	nn, err := v.gather()
	if err != nil {
		v.app.QueueUpdateDraw(func() { v.app.Flash().Err(err) })
		return
	}
	v.app.QueueUpdateDraw(func() {
		v.nodes = nn
		v.applySort()
		v.render()
	})
}

// gather collects node allocatable/usage and per-pod requests.
func (v *nodeFillView) gather() ([]nodeTopo, error) {
	f := v.app.factory

	nodeObjs, err := f.List(client.NodeGVR, client.BlankNamespace, true, labels.Everything())
	if err != nil {
		return nil, err
	}

	var mxMap client.NodesMetricsMap
	if v.app.Conn().HasMetrics() {
		mxMap, _ = client.DialMetrics(v.app.Conn()).FetchNodesMetricsMap(context.Background())
	}

	reqByNode, countByNode := v.podRequestsByNode(f)

	nn := make([]nodeTopo, 0, len(nodeObjs))
	for _, o := range nodeObjs {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		var no v1.Node
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &no); err != nil {
			continue
		}
		nt := nodeTopo{
			name:         no.Name,
			instanceType: no.Labels["node.kubernetes.io/instance-type"],
			nodeClass:    no.Labels["karpenter.k8s.aws/ec2nodeclass"],
			allocCPU:     no.Status.Allocatable.Cpu().MilliValue(),
			allocMEM:     no.Status.Allocatable.Memory().Value(),
			reqs:         reqByNode[no.Name],
			podCount:     countByNode[no.Name],
		}
		if mx, ok := mxMap[no.Name]; ok && mx != nil {
			nt.usageCPU = mx.Usage.Cpu().MilliValue()
			nt.usageMEM = mx.Usage.Memory().Value()
		}
		nn = append(nn, nt)
	}

	return nn, nil
}

func (v *nodeFillView) podRequestsByNode(f *watch.Factory) (map[string][]podReq, map[string]int) {
	reqByNode := make(map[string][]podReq)
	countByNode := make(map[string]int)

	podObjs, err := f.List(client.PodGVR, client.BlankNamespace, false, labels.Everything())
	if err != nil {
		slog.Warn("NodeFill: unable to list pods", slogs.Error, err)
		return reqByNode, countByNode
	}

	for _, o := range podObjs {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		var po v1.Pod
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &po); err != nil {
			continue
		}
		node := po.Spec.NodeName
		if node == "" || po.Status.Phase == v1.PodSucceeded || po.Status.Phase == v1.PodFailed {
			continue
		}
		var r podReq
		for i := range po.Spec.Containers {
			rl := po.Spec.Containers[i].Resources.Requests
			r.cpu += quantityMilli(rl.Cpu())
			r.mem += quantityValue(rl.Memory())
		}
		reqByNode[node] = append(reqByNode[node], r)
		countByNode[node]++
	}

	return reqByNode, countByNode
}

func quantityMilli(q *resource.Quantity) int64 {
	if q == nil {
		return 0
	}
	return q.MilliValue()
}

func quantityValue(q *resource.Quantity) int64 {
	if q == nil {
		return 0
	}
	return q.Value()
}

// render draws all node blocks into the text view.
func (v *nodeFillView) render() {
	v.Clear()
	v.updateTitle()

	nodes := v.visibleNodes()
	if len(nodes) == 0 {
		fmt.Fprint(v, "\n  [gray::]No nodes to display.[-:-:-]\n")
		return
	}
	if v.selected >= len(nodes) {
		v.selected = len(nodes) - 1
	}
	if v.selected < 0 {
		v.selected = 0
	}

	// k9s draws highlighted regions inverted (fg/bg swapped), so pre-swap the
	// selected line with cursorBg/cursorFg to land on the standard cursor colors.
	selFg := v.app.Styles.Table().CursorFgColor.String()
	selBg := v.app.Styles.Table().CursorBgColor.String()
	if v.lightSel {
		selBg = lighten(v.app.Styles.Table().CursorBgColor.Color(), 0.5)
	}

	var b strings.Builder
	for i := range nodes {
		n := &nodes[i]
		// Region spans the name line only, so the highlight does not wash
		// out the colored bars below it.
		if i == v.selected {
			fmt.Fprintf(&b, `["n%d"][%s:%s:b] ● %s (%s)[""][-:-:-]`+"\n", i, selBg, selFg, n.name, n.meta())
		} else {
			fmt.Fprintf(&b, `["n%d"] [green::]●[-:-:-] [white::b]%s[-:-:-] [gray::](%s)[""]`+"\n", i, n.name, n.meta())
		}
		fmt.Fprintf(&b, "     [::b]CPU[::-] %s    [::b]MEM[::-] %s\n", v.barFn(v, n, true), v.barFn(v, n, false))
	}
	fmt.Fprint(v, b.String())

	v.Highlight(fmt.Sprintf("n%d", v.selected)).ScrollToHighlight()
}

// lighten blends a color toward white by factor f (0..1) and returns a hex tag.
func lighten(c tcell.Color, f float64) string {
	if !c.Valid() {
		return "white"
	}
	r, g, b := c.RGB()
	lr := r + int32(float64(255-r)*f)
	lg := g + int32(float64(255-g)*f)
	lb := b + int32(float64(255-b)*f)
	return fmt.Sprintf("#%02x%02x%02x", lr, lg, lb)
}

// severity returns the threshold color for a metric percentage (capped at 100
// so over-committed nodes still show as critical).
func (v *nodeFillView) severity(metric string, perc int) string {
	if perc > 100 {
		perc = 100
	}
	return v.app.Config.K9s.Thresholds.SeverityColor(metric, perc)
}

func (v *nodeFillView) visibleNodes() []nodeTopo {
	if v.filter == "" {
		return v.nodes
	}
	out := make([]nodeTopo, 0, len(v.nodes))
	q := strings.ToLower(v.filter)
	for _, n := range v.nodes {
		if strings.Contains(strings.ToLower(n.name), q) {
			out = append(out, n)
		}
	}
	return out
}

func (v *nodeFillView) applySort() {
	switch v.sortKey {
	case "cpu":
		sort.SliceStable(v.nodes, func(i, j int) bool {
			return v.nodes[i].cpuPerc() > v.nodes[j].cpuPerc()
		})
	case "mem":
		sort.SliceStable(v.nodes, func(i, j int) bool {
			return v.nodes[i].memPerc() > v.nodes[j].memPerc()
		})
	default:
		sort.SliceStable(v.nodes, func(i, j int) bool {
			return v.nodes[i].name < v.nodes[j].name
		})
	}
}

func (v *nodeFillView) updateTitle() {
	base := fmt.Sprintf(" %s [%d] ", v.title, len(v.visibleNodes()))
	switch {
	case v.filtering:
		base = fmt.Sprintf(" %s </%s> ", v.title, v.filter)
	case v.filter != "":
		base = fmt.Sprintf(" %s [%d] </%s> ", v.title, len(v.visibleNodes()), v.filter)
	}
	frame := v.app.Styles.Frame()
	v.SetTitle(ui.SkinTitle(base, &frame))
}

// ----------------------------------------------------------------------------
// Bars

// renderBar frames a width-long bar, coalescing same-color runs. cell returns
// the glyph and color for each position.
func renderBar(cell func(i int) (rune, string)) string {
	var b strings.Builder
	b.WriteString("[gray::][") // literal opening bracket
	prev := ""
	for i := range fillBarWidth {
		glyph, color := cell(i)
		if color != prev {
			fmt.Fprintf(&b, "[%s::]", color)
			prev = color
		}
		b.WriteRune(glyph)
	}
	b.WriteString("[gray::]]") // literal closing bracket
	return b.String()
}

func percSuffix(color string, perc int) string {
	return fmt.Sprintf(" [%s::]%3d%%[-:-:-]", color, perc)
}

// usageBar fills by live usage (monitoring).
func usageBar(v *nodeFillView, n *nodeTopo, cpu bool) string {
	metric, usage, alloc := metricFor(n, cpu)
	perc := client.ToPercentage(usage, alloc)
	fill := v.severity(metric, perc)
	cells := scaleCells(usage, alloc)

	bar := renderBar(func(i int) (rune, string) {
		if i < cells {
			return fillFillGlyph, fill
		}
		return fillEmptyGlyph, fillEmptyColor
	})

	return bar + percSuffix(fill, perc)
}

// reservationBar fills by per-pod requests, one color block per pod (topology).
func reservationBar(v *nodeFillView, n *nodeTopo, cpu bool) string {
	metric, _, alloc := metricFor(n, cpu)
	segs := sortedReqs(n, cpu)

	var total int64
	for _, s := range segs {
		total += s
	}
	perc := client.ToPercentage(total, alloc)

	owner := make([]int, fillBarWidth)
	for i := range owner {
		owner[i] = -1
	}
	if alloc > 0 {
		var cum int64
		for idx, s := range segs {
			start := int((cum * int64(fillBarWidth)) / alloc)
			cum += s
			end := int((cum * int64(fillBarWidth)) / alloc)
			if start < 0 {
				start = 0
			}
			if end > fillBarWidth {
				end = fillBarWidth
			}
			for c := start; c < end; c++ {
				owner[c] = idx
			}
		}
	}

	bar := renderBar(func(i int) (rune, string) {
		if owner[i] >= 0 {
			return fillFillGlyph, topoPalette[owner[i]%len(topoPalette)]
		}
		return fillEmptyGlyph, fillEmptyColor
	})

	return bar + percSuffix(v.severity(metric, perc), perc)
}

func metricFor(n *nodeTopo, cpu bool) (metric string, usage, alloc int64) {
	if cpu {
		return "cpu", n.usageCPU, n.allocCPU
	}
	return "memory", n.usageMEM, n.allocMEM
}

func sortedReqs(n *nodeTopo, cpu bool) []int64 {
	segs := make([]int64, 0, len(n.reqs))
	for _, r := range n.reqs {
		if cpu {
			segs = append(segs, r.cpu)
		} else {
			segs = append(segs, r.mem)
		}
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i] > segs[j] })
	return segs
}

func scaleCells(val, alloc int64) int {
	if alloc <= 0 {
		return 0
	}
	c := int((val*int64(fillBarWidth) + alloc/2) / alloc)
	if c > fillBarWidth {
		c = fillBarWidth
	}
	if c < 0 {
		c = 0
	}
	return c
}

// ----------------------------------------------------------------------------
// Commands

func (v *nodeFillView) keyboard(evt *tcell.EventKey) *tcell.EventKey {
	if v.filtering {
		return v.filterKey(evt)
	}
	key := evt.Key()
	if key == tcell.KeyRune {
		key = tcell.Key(evt.Rune())
	}
	if a, ok := v.actions.Get(key); ok {
		return a.Action(evt)
	}
	return evt
}

func (v *nodeFillView) filterKey(evt *tcell.EventKey) *tcell.EventKey {
	switch evt.Key() {
	case tcell.KeyEscape:
		v.filtering, v.filter = false, ""
		v.render()
	case tcell.KeyEnter:
		v.filtering = false
		v.render()
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if v.filter != "" {
			v.filter = v.filter[:len(v.filter)-1]
		}
		v.render()
	case tcell.KeyRune:
		v.filter += string(evt.Rune())
		v.selected = 0
		v.render()
	}
	return nil
}

func (v *nodeFillView) moveCmd(delta int) func(*tcell.EventKey) *tcell.EventKey {
	return func(*tcell.EventKey) *tcell.EventKey {
		n := len(v.visibleNodes())
		if n == 0 {
			return nil
		}
		v.selected = (v.selected + delta + n) % n
		v.render()
		return nil
	}
}

func (v *nodeFillView) sortCmd(key string) func(*tcell.EventKey) *tcell.EventKey {
	return func(*tcell.EventKey) *tcell.EventKey {
		v.sortKey = key
		v.selected = 0
		v.applySort()
		v.render()
		return nil
	}
}

func (v *nodeFillView) activateFilterCmd(*tcell.EventKey) *tcell.EventKey {
	v.filtering, v.filter = true, ""
	v.render()
	return nil
}

func (v *nodeFillView) clearFilterCmd(evt *tcell.EventKey) *tcell.EventKey {
	if v.filter == "" && !v.filtering {
		return evt
	}
	v.filtering, v.filter = false, ""
	v.render()
	return nil
}

func (v *nodeFillView) enterCmd(*tcell.EventKey) *tcell.EventKey {
	nodes := v.visibleNodes()
	if v.selected < 0 || v.selected >= len(nodes) {
		return nil
	}
	name := nodes[v.selected].name
	v.Stop()
	v.app.SetFocus(v.app.Main)
	showPods(v.app, "", nil, "spec.nodeName="+name)
	return nil
}

// ----------------------------------------------------------------------------
// Component plumbing

// InCmdMode checks if prompt is active.
func (v *nodeFillView) InCmdMode() bool { return v.filtering }

func (*nodeFillView) SetCommand(*cmd.Interpreter)            {}
func (*nodeFillView) SetFilter(string, bool)                 {}
func (*nodeFillView) SetLabelSelector(labels.Selector, bool) {}
func (*nodeFillView) SetInstance(string)                     {}
func (*nodeFillView) SetEnvFn(EnvFunc)                       {}
func (*nodeFillView) AddBindKeysFn(BindKeysFunc)             {}
func (*nodeFillView) SetContextFn(ContextFunc)               {}
func (*nodeFillView) GetContextFn() ContextFunc              { return nil }
func (*nodeFillView) GetTable() *Table                       { return nil }
func (*nodeFillView) Refresh()                               {}
func (*nodeFillView) Restart()                               {}
func (*nodeFillView) ExtraHints() map[string]string          { return nil }

// GVR returns a resource descriptor.
func (v *nodeFillView) GVR() *client.GVR { return v.gvr }

// Name returns the component name.
func (v *nodeFillView) Name() string { return v.title }

// App returns the current app handle.
func (v *nodeFillView) App() *App { return v.app }

// Actions returns active menu bindings.
func (v *nodeFillView) Actions() *ui.KeyActions { return v.actions }

// Hints returns the view hints.
func (v *nodeFillView) Hints() model.MenuHints { return v.actions.Hints() }
