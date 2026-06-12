// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package config

import (
	"errors"
	"io/fs"
	"os"
	"sync"

	"github.com/derailed/k9s/internal/config/data"
	"gopkg.in/yaml.v3"
)

// Bookmark tracks a bookmarked resource.
type Bookmark struct {
	GVR  string `yaml:"gvr"`
	Path string `yaml:"path"`
}

// Bookmarks tracks a collection of per-context resource bookmarks.
type Bookmarks struct {
	Bookmarks []Bookmark `yaml:"bookmarks"`

	path string
	mx   sync.RWMutex
}

// NewBookmarks returns a new bookmarks configuration for the given file path.
func NewBookmarks(path string) *Bookmarks {
	return &Bookmarks{path: path}
}

// Load reads bookmarks from disk. A missing file is not an error.
func (b *Bookmarks) Load() error {
	b.mx.Lock()
	defer b.mx.Unlock()

	bb, err := os.ReadFile(b.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}

	return yaml.Unmarshal(bb, b)
}

// Save writes bookmarks to disk.
func (b *Bookmarks) Save() error {
	b.mx.RLock()
	defer b.mx.RUnlock()

	if err := data.EnsureDirPath(b.path, data.DefaultDirMod); err != nil {
		return err
	}

	return data.SaveYAML(b.path, b)
}

// List returns a copy of the current bookmarks.
func (b *Bookmarks) List() []Bookmark {
	b.mx.RLock()
	defer b.mx.RUnlock()

	out := make([]Bookmark, len(b.Bookmarks))
	copy(out, b.Bookmarks)

	return out
}

// IsBookmarked checks whether the given resource is bookmarked.
func (b *Bookmarks) IsBookmarked(gvr, path string) bool {
	b.mx.RLock()
	defer b.mx.RUnlock()

	return b.indexOf(gvr, path) >= 0
}

// Toggle adds the bookmark if absent, removes it otherwise.
// Returns true when the bookmark was added.
func (b *Bookmarks) Toggle(gvr, path string) bool {
	b.mx.Lock()
	defer b.mx.Unlock()

	if i := b.indexOf(gvr, path); i >= 0 {
		b.Bookmarks = append(b.Bookmarks[:i], b.Bookmarks[i+1:]...)
		return false
	}
	b.Bookmarks = append(b.Bookmarks, Bookmark{GVR: gvr, Path: path})

	return true
}

// Delete removes a bookmark if present.
func (b *Bookmarks) Delete(gvr, path string) {
	b.mx.Lock()
	defer b.mx.Unlock()

	if i := b.indexOf(gvr, path); i >= 0 {
		b.Bookmarks = append(b.Bookmarks[:i], b.Bookmarks[i+1:]...)
	}
}

func (b *Bookmarks) indexOf(gvr, path string) int {
	for i := range b.Bookmarks {
		if b.Bookmarks[i].GVR == gvr && b.Bookmarks[i].Path == path {
			return i
		}
	}

	return -1
}
