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
	// datadogNamespaceAttr scopes the query to a single namespace.
	// iAdvize ships logs through Vector, so cluster metadata lands as log
	// attributes (@...), not as the standard Datadog kube_* tags.
	datadogNamespaceAttr = "@namespace"
	// datadogProdIndexes / datadogDevIndex select which Datadog log indexes
	// to search depending on the active k8s context (prod vs dev).
	datadogProdIndexes = "main,staging"
	datadogDevIndex    = "dev"
)

// openDatadogLogs opens the Datadog log explorer for every resource currently
// visible in the table (i.e. after any active "/" filter), grouping them under
// the given Datadog log attribute (e.g. "@job" for workloads, "@container_name"
// for pods). When all visible resources share a namespace, the query is scoped
// to it, and the log index is picked from the active context (prod vs dev).
func openDatadogLogs(app *App, t *Table, attr string) {
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

	query := attr + ":(" + strings.Join(names, " OR ") + ")"
	// Scope to the namespace only when every visible resource shares one.
	if singleNS != "" && !multiNS {
		query += " " + datadogNamespaceAttr + ":" + singleNS
	}

	site := "https://" + datadogSite + "/logs?query=" + url.QueryEscape(query) +
		"&index=" + url.QueryEscape(datadogIndexes(app.Config.ActiveContextName()))

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

// datadogIndexes maps the active k8s context to the Datadog log index(es) to
// search: prod contexts hit the main+staging indexes, everything else dev.
func datadogIndexes(context string) string {
	if strings.Contains(strings.ToLower(context), "prod") {
		return datadogProdIndexes
	}
	return datadogDevIndex
}
