// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package render

import (
	"fmt"

	"github.com/derailed/k9s/internal/client"
	"github.com/derailed/k9s/internal/model1"
	"github.com/derailed/tcell/v2"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// BookmarkSeparator separates the gvr from the resource path in a bookmark id.
const BookmarkSeparator = "|"

// Bookmark renders bookmarked resources to screen.
type Bookmark struct {
	Base
}

// ColorerFunc colors a resource row.
func (Bookmark) ColorerFunc() model1.ColorerFunc {
	return func(string, model1.Header, *model1.RowEvent) tcell.Color {
		return tcell.ColorMediumSpringGreen
	}
}

// Header returns a header row.
func (Bookmark) Header(string) model1.Header {
	return model1.Header{
		model1.HeaderColumn{Name: "RESOURCE"},
		model1.HeaderColumn{Name: "NAMESPACE"},
		model1.HeaderColumn{Name: "NAME"},
	}
}

// Render renders a K8s resource to screen.
func (Bookmark) Render(o any, _ string, r *model1.Row) error {
	b, ok := o.(BookmarkRes)
	if !ok {
		return fmt.Errorf("expecting BookmarkRes, but got %T", o)
	}

	ns, n := client.Namespaced(b.Path)
	r.ID = b.GVR + BookmarkSeparator + b.Path
	r.Fields = model1.Fields{
		b.GVR,
		ns,
		n,
	}

	return nil
}

// ----------------------------------------------------------------------------

// BookmarkRes represents a bookmarked resource.
type BookmarkRes struct {
	GVR  string
	Path string
}

// GetObjectKind returns a schema object.
func (BookmarkRes) GetObjectKind() schema.ObjectKind {
	return nil
}

// DeepCopyObject returns a container copy.
func (b BookmarkRes) DeepCopyObject() runtime.Object {
	return b
}
