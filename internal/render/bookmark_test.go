// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package render_test

import (
	"testing"

	"github.com/derailed/k9s/internal/model1"
	"github.com/derailed/k9s/internal/render"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBookmarkRender(t *testing.T) {
	uu := map[string]struct {
		res render.BookmarkRes
		eID string
		eF  model1.Fields
	}{
		"namespaced": {
			res: render.BookmarkRes{GVR: "v1/pods", Path: "fred/blee"},
			eID: "v1/pods|fred/blee",
			eF:  model1.Fields{"v1/pods", "fred", "blee"},
		},
		"cluster_scoped": {
			res: render.BookmarkRes{GVR: "v1/nodes", Path: "node1"},
			eID: "v1/nodes|node1",
			eF:  model1.Fields{"v1/nodes", "", "node1"},
		},
	}

	var re render.Bookmark
	for k := range uu {
		u := uu[k]
		t.Run(k, func(t *testing.T) {
			var r model1.Row
			require.NoError(t, re.Render(u.res, "", &r))
			assert.Equal(t, u.eID, r.ID)
			assert.Equal(t, u.eF, r.Fields)
		})
	}
}
