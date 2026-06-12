// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package dao

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/derailed/k9s/internal/config"
	"github.com/derailed/k9s/internal/render"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

var (
	_ Accessor = (*Bookmark)(nil)
	_ Nuker    = (*Bookmark)(nil)
)

// Bookmark tracks bookmarked resources.
type Bookmark struct {
	NonResource
}

// BookmarksFile resolves the bookmarks file for the active context.
func (b *Bookmark) BookmarksFile() (string, error) {
	cfg := b.Client().Config()
	cl, err := cfg.CurrentClusterName()
	if err != nil {
		return "", err
	}
	ct, err := cfg.CurrentContextName()
	if err != nil {
		return "", err
	}

	return config.AppContextBookmarksFile(cl, ct), nil
}

// List returns a collection of bookmarks.
func (b *Bookmark) List(context.Context, string) ([]runtime.Object, error) {
	file, err := b.BookmarksFile()
	if err != nil {
		return nil, err
	}
	bm := config.NewBookmarks(file)
	if err := bm.Load(); err != nil {
		return nil, err
	}

	bb := bm.List()
	oo := make([]runtime.Object, 0, len(bb))
	for _, bk := range bb {
		oo = append(oo, render.BookmarkRes{GVR: bk.GVR, Path: bk.Path})
	}

	return oo, nil
}

// Get fetch a resource.
func (*Bookmark) Get(context.Context, string) (runtime.Object, error) {
	return nil, errors.New("nyi")
}

// Delete removes a bookmark.
func (b *Bookmark) Delete(_ context.Context, path string, _ *metav1.DeletionPropagation, _ Grace) error {
	gvr, fqn, ok := strings.Cut(path, render.BookmarkSeparator)
	if !ok {
		return fmt.Errorf("invalid bookmark id %q", path)
	}
	file, err := b.BookmarksFile()
	if err != nil {
		return err
	}
	bm := config.NewBookmarks(file)
	if err := bm.Load(); err != nil {
		return err
	}
	bm.Delete(gvr, fqn)

	return bm.Save()
}
