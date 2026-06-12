// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of K9s

package view

import (
	"context"
	"strings"

	"github.com/derailed/k9s/internal/client"
	"github.com/derailed/k9s/internal/render"
	"github.com/derailed/k9s/internal/ui"
	"github.com/derailed/tcell/v2"
)

// Bookmark represents a bookmarked resources view.
type Bookmark struct {
	ResourceViewer
}

// NewBookmark returns a new bookmark view.
func NewBookmark(gvr *client.GVR) ResourceViewer {
	b := Bookmark{
		ResourceViewer: NewBrowser(gvr),
	}
	b.GetTable().SetBorderFocusColor(tcell.ColorMediumSpringGreen)
	b.GetTable().SetSelectedStyle(tcell.StyleDefault.Foreground(tcell.ColorWhite).Background(tcell.ColorMediumSpringGreen).Attributes(tcell.AttrNone))
	b.AddBindKeysFn(b.bindKeys)

	return &b
}

// Init initializes the view.
func (b *Bookmark) Init(ctx context.Context) error {
	if err := b.ResourceViewer.Init(ctx); err != nil {
		return err
	}
	b.GetTable().GetModel().SetNamespace(client.NotNamespaced)

	return nil
}

func (b *Bookmark) bindKeys(aa *ui.KeyActions) {
	aa.Delete(ui.KeyShiftA, ui.KeyShiftN, ui.KeyShiftS, tcell.KeyCtrlS, tcell.KeyCtrlSpace, ui.KeySpace)
	aa.Delete(tcell.KeyCtrlW, tcell.KeyCtrlL, tcell.KeyCtrlB)
	aa.Bulk(ui.KeyMap{
		tcell.KeyEnter: ui.NewKeyAction("Goto", b.gotoCmd, true),
		ui.KeyShiftR:   ui.NewKeyAction("Sort Resource", b.GetTable().SortColCmd("RESOURCE", true), false),
		ui.KeyShiftN:   ui.NewKeyAction("Sort Name", b.GetTable().SortColCmd("NAME", true), false),
	})
}

func (b *Bookmark) gotoCmd(evt *tcell.EventKey) *tcell.EventKey {
	if b.GetTable().CmdBuff().IsActive() {
		return b.GetTable().activateCmd(evt)
	}

	sel := b.GetTable().GetSelectedItem()
	if sel == "" {
		return evt
	}
	gvr, fqn, ok := strings.Cut(sel, render.BookmarkSeparator)
	if !ok {
		return evt
	}
	b.App().gotoResource(gvr, fqn, false, true)

	return nil
}
