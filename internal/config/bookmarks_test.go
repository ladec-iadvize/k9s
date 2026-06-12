// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package config_test

import (
	"path/filepath"
	"testing"

	"github.com/derailed/k9s/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBookmarksToggle(t *testing.T) {
	b := config.NewBookmarks(filepath.Join(t.TempDir(), "bookmarks.yaml"))

	assert.True(t, b.Toggle("v1/pods", "fred/blee"))
	assert.True(t, b.IsBookmarked("v1/pods", "fred/blee"))
	assert.Len(t, b.List(), 1)

	assert.False(t, b.Toggle("v1/pods", "fred/blee"))
	assert.False(t, b.IsBookmarked("v1/pods", "fred/blee"))
	assert.Empty(t, b.List())
}

func TestBookmarksLoadSave(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bookmarks.yaml")

	b := config.NewBookmarks(p)
	require.NoError(t, b.Load())
	assert.Empty(t, b.List())

	b.Toggle("v1/pods", "fred/blee")
	b.Toggle("apps/v1/deployments", "fred/duh")
	require.NoError(t, b.Save())

	b2 := config.NewBookmarks(p)
	require.NoError(t, b2.Load())
	assert.Len(t, b2.List(), 2)
	assert.True(t, b2.IsBookmarked("v1/pods", "fred/blee"))
	assert.True(t, b2.IsBookmarked("apps/v1/deployments", "fred/duh"))

	b2.Delete("v1/pods", "fred/blee")
	assert.Len(t, b2.List(), 1)
	assert.False(t, b2.IsBookmarked("v1/pods", "fred/blee"))
}
