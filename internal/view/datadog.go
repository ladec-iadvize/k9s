// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package view

import (
	"errors"
	"net/url"
	"runtime"
	"strings"

	"github.com/derailed/k9s/internal/client"
	"github.com/derailed/k9s/internal/model1"
)

const (
	// datadogSite is the iAdvize Datadog EU host.
	datadogSite = "app.datadoghq.eu"
	// datadogMaxValues guards against pathologically long URLs when no
	// filter is active on a large cluster.
	datadogMaxValues = 200
)

// openDatadogLogs opens the Datadog log explorer for every resource currently
// visible in the table (i.e. after any active "/" filter), grouping them under
// the given Datadog tag (e.g. "kube_deployment", "pod_name"). When all visible
// resources live in a single namespace, the query is scoped to it.
func openDatadogLogs(app *App, t *Table, tag string) {
	if t == nil {
		app.Flash().Err(errors.New("no table to read from"))
		return
	}
	data := t.GetFilteredData()
	if data == nil {
		app.Flash().Err(errors.New("no data to open in Datadog"))
		return
	}

	var names []string
	var singleNS string
	var multiNS bool
	data.RowsRange(func(_ int, re model1.RowEvent) bool {
		ns, name := client.Namespaced(re.Row.ID)
		if name == "" {
			return true
		}
		names = append(names, name)
		if ns != "" && ns != client.ClusterScope {
			switch {
			case singleNS == "":
				singleNS = ns
			case singleNS != ns:
				multiNS = true
			}
		}
		return true
	})

	if len(names) == 0 {
		app.Flash().Warn("no resources visible to open in Datadog")
		return
	}
	if len(names) > datadogMaxValues {
		app.Flash().Warnf("too many resources (%d) — narrow your filter first", len(names))
		return
	}

	query := tag + ":(" + strings.Join(names, " OR ") + ")"
	// Scope to the namespace only when every visible resource shares one.
	if singleNS != "" && !multiNS {
		query += " kube_namespace:" + singleNS
	}
	site := "https://" + datadogSite + "/logs?query=" + url.QueryEscape(query)

	bin := browseLinux
	if runtime.GOOS == "darwin" {
		bin = browseOSX
	}
	ok, errChan, _ := run(app, &shellOpts{
		background: true,
		binary:     bin,
		args:       []string{site},
	})
	if !ok {
		app.Flash().Err(errors.New("unable to run browser command"))
		return
	}
	var errs error
	for e := range errChan {
		errs = errors.Join(errs, e)
	}
	if errs != nil {
		app.Flash().Err(errs)
		return
	}
	app.Flash().Infof("Opening Datadog logs for %d resource(s)", len(names))
}
