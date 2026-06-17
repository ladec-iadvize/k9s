// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package view

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/derailed/k9s/internal"
	"github.com/derailed/k9s/internal/client"
	"github.com/derailed/k9s/internal/config"
	"github.com/derailed/k9s/internal/dao"
	"github.com/derailed/k9s/internal/model"
	"github.com/derailed/k9s/internal/model1"
	"github.com/derailed/k9s/internal/slogs"
	"github.com/derailed/k9s/internal/ui"
	"github.com/derailed/tview"
	appsv1 "k8s.io/api/apps/v1"
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

	sizingDefaultSortCol = "CPU/WASTE"
)

// sizingHeader defines the table columns. CPU values are rendered in
// millicores with a uniform "m" suffix so MX (natural) sort stays numeric; MEM
// values are rendered in Mi/Gi and tagged Capacity so they sort as quantities.
func sizingHeader() model1.Header {
	mx := model1.Attrs{Align: tview.AlignRight, MX: true}
	cap := model1.Attrs{Align: tview.AlignRight, Capacity: true}
	return model1.Header{
		model1.HeaderColumn{Name: "NAMESPACE"},
		model1.HeaderColumn{Name: "DEPLOYMENT"},
		model1.HeaderColumn{Name: "PODS", Attrs: mx},
		model1.HeaderColumn{Name: "CPU/REQ", Attrs: mx},
		model1.HeaderColumn{Name: "CPU/USE", Attrs: mx},
		model1.HeaderColumn{Name: "CPU%", Attrs: mx},
		model1.HeaderColumn{Name: "CPU→RECO", Attrs: mx},
		model1.HeaderColumn{Name: "CPU/WASTE", Attrs: mx},
		model1.HeaderColumn{Name: "MEM/REQ", Attrs: cap},
		model1.HeaderColumn{Name: "MEM/USE", Attrs: cap},
		model1.HeaderColumn{Name: "MEM%", Attrs: mx},
		model1.HeaderColumn{Name: "MEM→RECO", Attrs: cap},
		model1.HeaderColumn{Name: "MEM/WASTE", Attrs: cap},
	}
}

// sizingRow holds the per-pod requests/usage of one deployment plus the
// derived recommendation. CPU is in millicores, MEM in bytes.
type sizingRow struct {
	namespace, name string
	pods            int
	cpuReq, cpuUse  int64
	memReq, memUse  int64
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

func fmtCPU(milli int64) string {
	if milli <= 0 {
		return "0"
	}
	return fmt.Sprintf("%dm", milli)
}

// fmtMem renders bytes in a human-friendly binary unit: Mi below 1 Gi, Gi
// above (one decimal, trailing .0 trimmed).
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

func trimZeroDec(f float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", f), ".0")
}

// sizingFields renders a row in the column order of sizingHeader().
func sizingFields(r *sizingRow) model1.Fields {
	cpuReq, memReq := fmtCPU(r.cpuReq), fmtMem(r.memReq)
	pods, cpuUse, cpuPct, cpuReco, cpuWaste := "-", "-", "-", "-", "-"
	memUse, memPct, memReco, memWaste := "-", "-", "-", "-"
	if r.hasData() {
		pods = strconv.Itoa(r.pods)
		cpuUse = fmtCPU(r.cpuUse)
		cpuPct = fmt.Sprintf("%d%%", r.cpuPerc())
		if rec, yes := reco(r.cpuUse, r.cpuReq); yes {
			cpuReco = fmtCPU(rec)
		} else {
			cpuReco = "ok"
		}
		cpuWaste = fmtCPU(waste(r.cpuUse, r.cpuReq, r.pods))
		memUse = fmtMem(r.memUse)
		memPct = fmt.Sprintf("%d%%", r.memPerc())
		if rec, yes := reco(r.memUse, r.memReq); yes {
			memReco = fmtMem(rec)
		} else {
			memReco = "ok"
		}
		memWaste = fmtMem(waste(r.memUse, r.memReq, r.pods))
	}
	return model1.Fields{
		r.namespace, r.name, pods,
		cpuReq, cpuUse, cpuPct, cpuReco, cpuWaste,
		memReq, memUse, memPct, memReco, memWaste,
	}
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
// Model

// sizingModel feeds the table widget with deployment sizing data computed from
// Prometheus. It implements ui.Tabular so it plugs straight into view.Table and
// inherits its sort/filter/selection behavior.
type sizingModel struct {
	gvr         *client.GVR
	app         *App
	prom        *client.Prometheus
	data        *model1.TableData
	listeners   []model.TableListener
	refreshRate time.Duration
	labelSel    labels.Selector
	mx          sync.RWMutex
}

func newSizingModel(gvr *client.GVR, app *App) *sizingModel {
	return &sizingModel{
		gvr:         gvr,
		app:         app,
		prom:        client.DialPrometheus(app.Conn()),
		data:        model1.NewTableData(gvr),
		refreshRate: sizingRefresh,
	}
}

func (m *sizingModel) Watch(ctx context.Context) error {
	if err := m.refresh(ctx); err != nil {
		return err
	}
	go func() {
		tick := time.NewTicker(m.refreshRate)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				_ = m.refresh(ctx)
			}
		}
	}()
	return nil
}

func (m *sizingModel) Refresh(ctx context.Context) error { return m.refresh(ctx) }

func (m *sizingModel) refresh(ctx context.Context) error {
	td, err := m.buildData(ctx)
	if err != nil {
		m.fire(func(l model.TableListener) { l.TableLoadFailed(err) })
		return err
	}
	m.mx.Lock()
	m.data = td
	m.mx.Unlock()
	snap := td.Clone()
	if td.Empty() {
		m.fire(func(l model.TableListener) { l.TableNoData(snap) })
		return nil
	}
	m.fire(func(l model.TableListener) { l.TableDataChanged(snap) })
	return nil
}

// buildData lists deployments from the informer cache and joins them with 24h
// per-pod usage pulled from Prometheus in just two aggregated queries.
func (m *sizingModel) buildData(ctx context.Context) (*model1.TableData, error) {
	f := m.app.factory
	ns := m.app.Config.ActiveNamespace()

	listNS := ns
	if sizingIsAllNS(ns) {
		listNS = client.BlankNamespace
	}
	dpObjs, err := f.List(client.DpGVR, listNS, true, labels.Everything())
	if err != nil {
		return nil, err
	}

	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cpuUse, err := m.prom.QueryVector(qctx, sizingUsageQuery("container_cpu_usage_seconds_total", ns, true))
	if err != nil {
		return nil, err
	}
	memUse, err := m.prom.QueryVector(qctx, sizingUsageQuery("container_memory_working_set_bytes", ns, false))
	if err != nil {
		return nil, err
	}

	td := model1.NewTableData(m.gvr)
	hdrNS := ns
	if sizingIsAllNS(ns) {
		hdrNS = client.NamespaceAll
	}
	td.SetHeader(hdrNS, sizingHeader())

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

		r := sizingRow{namespace: dp.Namespace, name: dp.Name}
		for i := range dp.Spec.Template.Spec.Containers {
			rl := dp.Spec.Template.Spec.Containers[i].Resources.Requests
			r.cpuReq += quantityMilli(rl.Cpu())
			r.memReq += quantityValue(rl.Memory())
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
			r.pods++
		}
		if r.pods > 0 {
			r.cpuUse = int64(sumCPU / float64(r.pods))
			r.memUse = int64(sumMEM / float64(r.pods))
		}

		td.AddRow(model1.NewRowEvent(model1.EventAdd, model1.Row{
			ID:     client.FQN(dp.Namespace, dp.Name),
			Fields: sizingFields(&r),
		}))
	}

	return td, nil
}

func (m *sizingModel) fire(f func(model.TableListener)) {
	m.mx.RLock()
	ll := make([]model.TableListener, len(m.listeners))
	copy(ll, m.listeners)
	m.mx.RUnlock()
	for _, l := range ll {
		f(l)
	}
}

func (m *sizingModel) Peek() *model1.TableData {
	m.mx.RLock()
	defer m.mx.RUnlock()
	if m.data == nil {
		return model1.NewTableData(m.gvr)
	}
	return m.data.Clone()
}

func (m *sizingModel) AddListener(l model.TableListener) {
	m.mx.Lock()
	defer m.mx.Unlock()
	m.listeners = append(m.listeners, l)
}

func (m *sizingModel) RemoveListener(l model.TableListener) {
	m.mx.Lock()
	defer m.mx.Unlock()
	for i, lis := range m.listeners {
		if lis == l {
			m.listeners = append(m.listeners[:i], m.listeners[i+1:]...)
			break
		}
	}
}

func (m *sizingModel) ClusterWide() bool      { return client.IsClusterWide(m.Peek().GetNamespace()) }
func (m *sizingModel) GetNamespace() string   { return m.Peek().GetNamespace() }
func (m *sizingModel) SetNamespace(ns string) { m.mx.Lock(); m.data.Reset(ns); m.mx.Unlock() }
func (m *sizingModel) InNamespace(ns string) bool {
	return m.GetNamespace() == ns
}
func (m *sizingModel) Empty() bool                                         { return m.Peek().Empty() }
func (m *sizingModel) RowCount() int                                       { return m.Peek().RowCount() }
func (m *sizingModel) SetInstance(string)                                  {}
func (m *sizingModel) SetLabelSelector(s labels.Selector)                  { m.labelSel = s }
func (m *sizingModel) GetLabelSelector() labels.Selector                   { return m.labelSel }
func (m *sizingModel) SetRefreshRate(d time.Duration)                      { m.refreshRate = d }
func (m *sizingModel) SetViewSetting(context.Context, *config.ViewSetting) {}

func (*sizingModel) Get(context.Context, string) (runtime.Object, error) {
	return nil, errors.New("not supported on the sizing view")
}

func (*sizingModel) Delete(context.Context, string, *metav1.DeletionPropagation, dao.Grace) error {
	return errors.New("not supported on the sizing view")
}

// ----------------------------------------------------------------------------
// View

// sizingView is a thin resource viewer that drives the standard k9s table
// widget from a Prometheus-backed model, inheriting column sort (Shift+O),
// Shift+←/→ column selection, the "/" filter, selection and colors for free.
type sizingView struct {
	*Table

	model    *sizingModel
	cancelFn context.CancelFunc
}

// NewDeploymentSizing returns a new sizing view.
func NewDeploymentSizing(gvr *client.GVR) ResourceViewer {
	return &sizingView{Table: NewTable(gvr)}
}

func (v *sizingView) Init(ctx context.Context) error {
	if err := v.Table.Init(ctx); err != nil {
		return err
	}
	v.model = newSizingModel(v.GVR(), v.App())
	v.SetModel(v.model)
	v.SetSortCol(sizingDefaultSortCol, false)
	v.SetEnterFn(v.gotoPods)

	return nil
}

func (v *sizingView) Start() {
	v.Stop()
	v.GetModel().AddListener(v)
	v.Table.Start()
	v.CmdBuff().AddListener(v)

	ctx := context.WithValue(context.Background(), internal.KeyFactory, v.App().factory)
	ctx, v.cancelFn = context.WithCancel(ctx)
	if err := v.GetModel().Watch(ctx); err != nil {
		v.App().Flash().Err(err)
	}
}

func (v *sizingView) Stop() {
	if v.cancelFn != nil {
		v.cancelFn()
		v.cancelFn = nil
	}
	if v.model != nil {
		v.GetModel().RemoveListener(v)
	}
	v.CmdBuff().RemoveListener(v)
	v.Table.Stop()
}

// gotoPods jumps to the selected deployment's pods.
func (v *sizingView) gotoPods(app *App, _ ui.Tabular, _ *client.GVR, path string) {
	o, err := app.factory.Get(client.DpGVR, path, true, labels.Everything())
	if err != nil {
		app.Flash().Err(err)
		return
	}
	u, ok := o.(*unstructured.Unstructured)
	if !ok {
		app.Flash().Err(errors.New("unexpected deployment object"))
		return
	}
	var dp appsv1.Deployment
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &dp); err != nil {
		app.Flash().Err(err)
		return
	}
	showPodsFromSelector(app, path, dp.Spec.Selector)
}

// TableDataChanged renders fresh model data into the widget.
func (v *sizingView) TableDataChanged(data *model1.TableData) {
	if !v.App().IsRunning() {
		return
	}
	cdata := v.Update(data, true)
	v.App().QueueUpdateDraw(func() {
		v.UpdateUI(cdata, data)
	})
}

func (v *sizingView) TableNoData(data *model1.TableData) {
	if !v.App().IsRunning() {
		return
	}
	cdata := v.Update(data, true)
	v.App().QueueUpdateDraw(func() {
		v.UpdateUI(cdata, data)
	})
}

func (v *sizingView) TableLoadFailed(err error) {
	v.App().QueueUpdateDraw(func() { v.App().Flash().Err(err) })
}

// GetTable returns the underlying table widget.
func (v *sizingView) GetTable() *Table { return v.Table }

// Name returns the component name.
func (*sizingView) Name() string { return "sizing" }

// InCmdMode reports whether the filter prompt is active.
func (v *sizingView) InCmdMode() bool { return v.CmdBuff().InCmdMode() }

func (*sizingView) SetContextFn(ContextFunc)               {}
func (*sizingView) SetInstance(string)                     {}
func (*sizingView) SetFilter(string, bool)                 {}
func (*sizingView) SetLabelSelector(labels.Selector, bool) {}
