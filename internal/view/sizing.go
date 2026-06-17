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
	"github.com/derailed/tcell/v2"
	"github.com/derailed/tview"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// sizingRefresh is deliberately slow: a 24h average barely moves, so there
	// is no point hammering Prometheus at the usual k9s cadence.
	sizingRefresh = 3 * time.Minute
	// sizingWindow is the Prometheus look-back used for the average.
	sizingWindow = "24h"
	// sizingLowPct: at or above this usage/requests ratio we leave the workload
	// alone. Below it, we recommend resizing.
	sizingLowPct = 30
	// sizingTargetPct is the utilization we aim for when recommending.
	sizingTargetPct = 50
)

// Column indices for the sizing table. colSep is a vertical rule separating the
// CPU block from the MEM block.
const (
	colNS = iota
	colDP
	colPods
	colCPUReq
	colCPUUse
	colCPUPerc
	colCPUReco
	colCPUWaste
	colSep
	colMEMReq
	colMEMUse
	colMEMPerc
	colMEMReco
	colMEMWaste
	sizingColCount
)

var sizingHeaders = [sizingColCount]string{
	colNS: "NAMESPACE", colDP: "DEPLOYMENT", colPods: "PODS",
	colCPUReq: "CPU/REQ", colCPUUse: "CPU/USE", colCPUPerc: "CPU%",
	colCPUReco: "CPU→RECO", colCPUWaste: "CPU/WASTE",
	colSep:    "│",
	colMEMReq: "MEM/REQ", colMEMUse: "MEM/USE", colMEMPerc: "MEM%",
	colMEMReco: "MEM→RECO", colMEMWaste: "MEM/WASTE",
}

// sizingRow holds the per-pod requests/usage of one deployment plus the
// derived recommendation. CPU is in millicores, MEM in bytes.
type sizingRow struct {
	namespace, name string
	pods            int
	cpuReq, cpuUse  int64
	memReq, memUse  int64
	rawSelector     *metav1.LabelSelector
}

func (r *sizingRow) hasData() bool { return r.pods > 0 && (r.cpuUse > 0 || r.memUse > 0) }

func (r *sizingRow) cpuPerc() int { return client.ToPercentage(r.cpuUse, r.cpuReq) }
func (r *sizingRow) memPerc() int { return client.ToPercentage(r.memUse, r.memReq) }

// reco returns the recommended per-pod request and true when the workload uses
// less than sizingLowPct of what it reserves (target ~sizingTargetPct).
func reco(use, req int64) (int64, bool) {
	if req <= 0 || use <= 0 {
		return 0, false
	}
	if client.ToPercentage(use, req) >= sizingLowPct {
		return 0, false
	}
	return use * 100 / sizingTargetPct, true
}

// waste returns the total reserved-but-unused resource across the deployment's
// pods (per-pod gap times pod count).
func waste(use, req int64, pods int) int64 {
	if use <= 0 || req <= use {
		return 0
	}
	if pods < 1 {
		pods = 1
	}
	return (req - use) * int64(pods)
}

// sizingView recommends pod resizing per deployment from 24h Prometheus usage.
type sizingView struct {
	*tview.Table

	app      *App
	gvr      *client.GVR
	prom     *client.Prometheus
	actions  *ui.KeyActions
	cmdBuff  *model.FishBuff
	cancelFn context.CancelFunc
	rows     []sizingRow
	sortKey  string // "cpu", "mem" or "name"
	filter   string
}

// NewDeploymentSizing returns a new sizing view.
func NewDeploymentSizing(gvr *client.GVR) ResourceViewer {
	return &sizingView{
		Table:   tview.NewTable(),
		gvr:     gvr,
		actions: ui.NewKeyActions(),
		cmdBuff: model.NewFishBuff('/', model.FilterBuffer),
		sortKey: "cpu",
	}
}

// Init initializes the view.
func (v *sizingView) Init(ctx context.Context) error {
	var err error
	if v.app, err = extractApp(ctx); err != nil {
		return err
	}
	v.prom = client.DialPrometheus(v.app.Conn())

	v.SetBorder(true)
	v.SetSelectable(true, false)
	v.SetFixed(1, 0)
	v.SetBorderPadding(0, 0, 1, 1)
	v.SetInputCapture(v.keyboard)
	v.updateTitle()

	v.bindKeys()
	v.app.Styles.AddListener(v)
	v.StylesChanged(v.app.Styles)

	v.app.Prompt().SetModel(v.cmdBuff)
	v.cmdBuff.AddListener(v)

	return nil
}

func (v *sizingView) bindKeys() {
	v.actions.Merge(ui.NewKeyActionsFromMap(ui.KeyMap{
		tcell.KeyEnter:  ui.NewSharedKeyAction("Pods", v.enterCmd, true),
		ui.KeyJ:         ui.NewKeyAction("Down", v.moveCmd(1), false),
		ui.KeyK:         ui.NewKeyAction("Up", v.moveCmd(-1), false),
		ui.KeyC:         ui.NewKeyAction("Sort CPU Waste", v.sortCmd("cpu"), true),
		ui.KeyM:         ui.NewKeyAction("Sort MEM Waste", v.sortCmd("mem"), true),
		ui.KeyN:         ui.NewKeyAction("Sort Name", v.sortCmd("name"), true),
		ui.KeySlash:     ui.NewSharedKeyAction("Filter Mode", v.activateCmd, false),
		tcell.KeyEscape: ui.NewSharedKeyAction("Clear", v.resetCmd, false),
		tcell.KeyDelete: ui.NewSharedKeyAction("Erase", v.eraseCmd, false),
	}))
}

// StylesChanged notifies the skin changed.
func (v *sizingView) StylesChanged(s *config.Styles) {
	v.SetBackgroundColor(s.BgColor())
	v.SetBorderColor(s.Frame().Border.FgColor.Color())
	if v.app != nil {
		v.render()
	}
}

// Start initializes the refresh loop.
func (v *sizingView) Start() {
	v.Stop()

	ctx := context.WithValue(context.Background(), internal.KeyFactory, v.app.factory)
	ctx, v.cancelFn = context.WithCancel(ctx)

	go v.refresh()
	go func() {
		tick := time.NewTicker(sizingRefresh)
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
func (v *sizingView) Stop() {
	if v.cancelFn == nil {
		return
	}
	v.cancelFn()
	v.cancelFn = nil
	if v.app != nil {
		v.app.Styles.RemoveListener(v)
	}
}

func (v *sizingView) refresh() {
	rr, err := v.gather()
	if err != nil {
		v.app.QueueUpdateDraw(func() { v.app.Flash().Err(err) })
		return
	}
	v.app.QueueUpdateDraw(func() {
		v.rows = rr
		v.applySort()
		v.render()
	})
}

// gather lists deployments from the informer cache and joins them with 24h
// per-pod usage pulled from Prometheus in just two aggregated queries.
func (v *sizingView) gather() ([]sizingRow, error) {
	f := v.app.factory
	ns := v.app.Config.ActiveNamespace()

	listNS := ns
	if sizingIsAllNS(ns) {
		listNS = client.BlankNamespace
	}
	dpObjs, err := f.List(client.DpGVR, listNS, true, labels.Everything())
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cpuUse, err := v.prom.QueryVector(ctx, sizingUsageQuery("container_cpu_usage_seconds_total", ns, true))
	if err != nil {
		return nil, err
	}
	memUse, err := v.prom.QueryVector(ctx, sizingUsageQuery("container_memory_working_set_bytes", ns, false))
	if err != nil {
		return nil, err
	}

	rows := make([]sizingRow, 0, len(dpObjs))
	for _, o := range dpObjs {
		u, ok := o.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		var dp appsv1.Deployment
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &dp); err != nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(dp.Spec.Selector)
		if err != nil {
			continue
		}

		var cpuReq, memReq int64
		for i := range dp.Spec.Template.Spec.Containers {
			rl := dp.Spec.Template.Spec.Containers[i].Resources.Requests
			cpuReq += quantityMilli(rl.Cpu())
			memReq += quantityValue(rl.Memory())
		}

		row := sizingRow{
			namespace:   dp.Namespace,
			name:        dp.Name,
			cpuReq:      cpuReq,
			memReq:      memReq,
			rawSelector: dp.Spec.Selector,
		}

		podObjs, err := f.List(client.PodGVR, dp.Namespace, false, sel)
		if err != nil {
			slog.Warn("Sizing: unable to list pods", slogs.Error, err)
		}
		var sumCPU, sumMEM float64
		for _, po := range podObjs {
			pu, ok := po.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			key := client.FQN(dp.Namespace, pu.GetName())
			cu, okc := cpuUse[key]
			mu, okm := memUse[key]
			if !okc && !okm {
				continue
			}
			sumCPU += cu * 1000 // cores -> millicores
			sumMEM += mu
			row.pods++
		}
		if row.pods > 0 {
			row.cpuUse = int64(sumCPU / float64(row.pods))
			row.memUse = int64(sumMEM / float64(row.pods))
		}
		rows = append(rows, row)
	}

	return rows, nil
}

// sizingUsageQuery builds the aggregated PromQL. CPU is a counter (rate), MEM a
// gauge (avg_over_time); both averaged over the window, grouped by ns+pod.
func sizingUsageQuery(metric, ns string, isCounter bool) string {
	sel := `container!=""`
	if !sizingIsAllNS(ns) {
		sel += fmt.Sprintf(`,namespace=%q`, ns)
	}
	inner := fmt.Sprintf("avg_over_time(%s{%s}[%s])", metric, sel, sizingWindow)
	if isCounter {
		inner = fmt.Sprintf("rate(%s{%s}[%s])", metric, sel, sizingWindow)
	}
	return fmt.Sprintf("sum by (namespace, pod) (%s)", inner)
}

func sizingIsAllNS(ns string) bool {
	return ns == "" || ns == client.NamespaceAll || ns == client.NotNamespaced || client.IsClusterWide(ns)
}

// ----------------------------------------------------------------------------
// Render

func (v *sizingView) render() {
	v.Clear()
	v.updateTitle()

	styles := v.app.Styles
	hdrColor := styles.Table().Header.FgColor.Color()
	for c, h := range sizingHeaders {
		if c == colSep {
			v.SetCell(0, c, sizingSepCell(false))
			continue
		}
		cell := tview.NewTableCell(" " + h + " ").
			SetTextColor(hdrColor).
			SetAttributes(tcell.AttrBold).
			SetSelectable(false)
		if c >= colPods {
			cell.SetAlign(tview.AlignRight)
		}
		v.SetCell(0, c, cell)
	}

	rows := v.visibleRows()
	fg := styles.Table().FgColor.Color()
	dim := tcell.ColorGray
	ok := tcell.ColorGreen

	for i := range rows {
		r := &rows[i]
		set := func(col int, text string, color tcell.Color, right bool) {
			cell := tview.NewTableCell(text).SetTextColor(color)
			if right {
				cell.SetAlign(tview.AlignRight)
			}
			v.SetCell(i+1, col, cell)
		}

		set(colNS, r.namespace, fg, false)
		set(colDP, r.name, fg, false)
		v.SetCell(i+1, colSep, sizingSepCell(true))
		if !r.hasData() {
			for _, c := range []int{
				colPods, colCPUReq, colCPUUse, colCPUPerc, colCPUReco, colCPUWaste,
				colMEMReq, colMEMUse, colMEMPerc, colMEMReco, colMEMWaste,
			} {
				set(c, "-", dim, true)
			}
			continue
		}

		set(colPods, fmt.Sprintf("%d", r.pods), fg, true)
		// CPU
		set(colCPUReq, fmtCPU(r.cpuReq), fg, true)
		set(colCPUUse, fmtCPU(r.cpuUse), fg, true)
		set(colCPUPerc, fmt.Sprintf("%d%%", r.cpuPerc()), v.severity("cpu", r.cpuPerc()), true)
		if rec, yes := reco(r.cpuUse, r.cpuReq); yes {
			set(colCPUReco, "→ "+fmtCPU(rec), tcell.ColorOrange, true)
		} else {
			set(colCPUReco, "ok", ok, true)
		}
		set(colCPUWaste, fmtCPU(waste(r.cpuUse, r.cpuReq, r.pods)), dim, true)
		// MEM
		set(colMEMReq, fmtMem(r.memReq), fg, true)
		set(colMEMUse, fmtMem(r.memUse), fg, true)
		set(colMEMPerc, fmt.Sprintf("%d%%", r.memPerc()), v.severity("memory", r.memPerc()), true)
		if rec, yes := reco(r.memUse, r.memReq); yes {
			set(colMEMReco, "→ "+fmtMem(rec), tcell.ColorOrange, true)
		} else {
			set(colMEMReco, "ok", ok, true)
		}
		set(colMEMWaste, fmtMem(waste(r.memUse, r.memReq, r.pods)), dim, true)
	}

	if v.GetRowCount() > 1 {
		row, _ := v.GetSelection()
		if row < 1 {
			row = 1
		}
		if row >= v.GetRowCount() {
			row = v.GetRowCount() - 1
		}
		v.Select(row, 0)
	}
}

// severity returns the threshold color for a metric percentage.
func (v *sizingView) severity(metric string, perc int) tcell.Color {
	if perc > 100 {
		perc = 100
	}
	return tcell.GetColor(v.app.Config.K9s.Thresholds.SeverityColor(metric, perc))
}

// sizingSepCell builds the vertical rule between the CPU and MEM blocks. Data
// rows keep it selectable so the row highlight stays continuous; the header
// rule is not selectable.
func sizingSepCell(selectable bool) *tview.TableCell {
	return tview.NewTableCell(" │ ").
		SetTextColor(tcell.ColorGray).
		SetAlign(tview.AlignCenter).
		SetSelectable(selectable)
}

func fmtCPU(milli int64) string {
	if milli <= 0 {
		return "0"
	}
	return resource.NewMilliQuantity(milli, resource.DecimalSI).String()
}

// fmtMem renders bytes in a human-friendly binary unit: Mi below 1 Gi, Gi
// above (with one decimal, trailing .0 trimmed). resource.Quantity is avoided
// here because it dumps raw bytes for values that aren't a round power of two.
func fmtMem(bytes int64) string {
	if bytes <= 0 {
		return "0"
	}
	const (
		mi = 1024 * 1024
		gi = 1024 * mi
	)
	if bytes >= gi {
		return trimZeroDec(float64(bytes)/float64(gi)) + "Gi"
	}
	return fmt.Sprintf("%dMi", (bytes+mi/2)/mi)
}

// trimZeroDec formats with one decimal but drops a trailing ".0".
func trimZeroDec(f float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", f), ".0")
}

func (v *sizingView) visibleRows() []sizingRow {
	if v.filter == "" {
		return v.rows
	}
	q := strings.ToLower(v.filter)
	out := make([]sizingRow, 0, len(v.rows))
	for _, r := range v.rows {
		if strings.Contains(strings.ToLower(r.name), q) ||
			strings.Contains(strings.ToLower(r.namespace), q) {
			out = append(out, r)
		}
	}
	return out
}

func (v *sizingView) applySort() {
	switch v.sortKey {
	case "mem":
		sort.SliceStable(v.rows, func(i, j int) bool {
			return waste(v.rows[i].memUse, v.rows[i].memReq, v.rows[i].pods) >
				waste(v.rows[j].memUse, v.rows[j].memReq, v.rows[j].pods)
		})
	case "name":
		sort.SliceStable(v.rows, func(i, j int) bool {
			if v.rows[i].namespace != v.rows[j].namespace {
				return v.rows[i].namespace < v.rows[j].namespace
			}
			return v.rows[i].name < v.rows[j].name
		})
	default: // cpu waste
		sort.SliceStable(v.rows, func(i, j int) bool {
			return waste(v.rows[i].cpuUse, v.rows[i].cpuReq, v.rows[i].pods) >
				waste(v.rows[j].cpuUse, v.rows[j].cpuReq, v.rows[j].pods)
		})
	}
}

func (v *sizingView) updateTitle() {
	frame := v.app.Styles.Frame()
	count := fmt.Sprintf("%d", len(v.visibleRows()))
	ns := v.app.Config.ActiveNamespace()
	if sizingIsAllNS(ns) {
		ns = client.NamespaceAll
	}
	title := ui.SkinTitle(fmt.Sprintf(ui.NSTitleFmt, "sizing", ns, count), &frame)
	if v.filter != "" {
		title += ui.SkinTitle(fmt.Sprintf(ui.SearchFmt, v.filter), &frame)
	}
	v.SetTitle(title)
}

// ----------------------------------------------------------------------------
// Commands

func (v *sizingView) keyboard(evt *tcell.EventKey) *tcell.EventKey {
	if a, ok := v.actions.Get(ui.AsKey(evt)); ok {
		return a.Action(evt)
	}
	return evt
}

func (v *sizingView) moveCmd(delta int) func(*tcell.EventKey) *tcell.EventKey {
	return func(*tcell.EventKey) *tcell.EventKey {
		if v.GetRowCount() <= 1 {
			return nil
		}
		row, _ := v.GetSelection()
		row += delta
		if row < 1 {
			row = 1
		}
		if row >= v.GetRowCount() {
			row = v.GetRowCount() - 1
		}
		v.Select(row, 0)
		return nil
	}
}

func (v *sizingView) sortCmd(key string) func(*tcell.EventKey) *tcell.EventKey {
	return func(*tcell.EventKey) *tcell.EventKey {
		v.sortKey = key
		v.applySort()
		v.render()
		return nil
	}
}

func (v *sizingView) activateCmd(evt *tcell.EventKey) *tcell.EventKey {
	if v.app.InCmdMode() {
		return evt
	}
	v.app.ResetPrompt(v.cmdBuff)
	return nil
}

func (v *sizingView) eraseCmd(*tcell.EventKey) *tcell.EventKey {
	if !v.cmdBuff.IsActive() {
		return nil
	}
	v.cmdBuff.Delete()
	return nil
}

func (v *sizingView) resetCmd(evt *tcell.EventKey) *tcell.EventKey {
	if !v.cmdBuff.InCmdMode() {
		v.cmdBuff.Reset()
		return v.app.PrevCmd(evt)
	}
	if v.cmdBuff.GetText() != "" {
		v.filter = ""
	}
	v.cmdBuff.SetActive(false)
	v.cmdBuff.Reset()
	v.render()
	return nil
}

func (v *sizingView) enterCmd(evt *tcell.EventKey) *tcell.EventKey {
	if v.cmdBuff.IsActive() {
		v.filter = v.cmdBuff.GetText()
		v.cmdBuff.SetActive(false)
		v.render()
		return nil
	}

	rows := v.visibleRows()
	row, _ := v.GetSelection()
	idx := row - 1
	if idx < 0 || idx >= len(rows) {
		return evt
	}
	r := rows[idx]
	v.Stop()
	v.app.SetFocus(v.app.Main)
	showPodsFromSelector(v.app, client.FQN(r.namespace, r.name), r.rawSelector)
	return nil
}

// BufferChanged live-filters as the user types.
func (v *sizingView) BufferChanged(text, _ string) {
	v.filter = text
	v.render()
}

// BufferCompleted applies the accepted filter text.
func (v *sizingView) BufferCompleted(text, _ string) {
	v.filter = text
	v.render()
}

// BufferActive notifies the app the prompt activity changed.
func (v *sizingView) BufferActive(state bool, k model.BufferKind) {
	v.app.BufferActive(state, k)
}

// ----------------------------------------------------------------------------
// Component plumbing

// InCmdMode checks if prompt is active.
func (v *sizingView) InCmdMode() bool { return v.cmdBuff.InCmdMode() }

func (*sizingView) SetCommand(*cmd.Interpreter)            {}
func (*sizingView) SetFilter(string, bool)                 {}
func (*sizingView) SetLabelSelector(labels.Selector, bool) {}
func (*sizingView) SetInstance(string)                     {}
func (*sizingView) SetEnvFn(EnvFunc)                       {}
func (*sizingView) AddBindKeysFn(BindKeysFunc)             {}
func (*sizingView) SetContextFn(ContextFunc)               {}
func (*sizingView) GetContextFn() ContextFunc              { return nil }
func (*sizingView) GetTable() *Table                       { return nil }
func (*sizingView) Refresh()                               {}
func (*sizingView) Restart()                               {}
func (*sizingView) ExtraHints() map[string]string          { return nil }

// GVR returns a resource descriptor.
func (v *sizingView) GVR() *client.GVR { return v.gvr }

// Name returns the component name.
func (*sizingView) Name() string { return "sizing" }

// App returns the current app handle.
func (v *sizingView) App() *App { return v.app }

// Actions returns active menu bindings.
func (v *sizingView) Actions() *ui.KeyActions { return v.actions }

// Hints returns the view hints.
func (v *sizingView) Hints() model.MenuHints { return v.actions.Hints() }
