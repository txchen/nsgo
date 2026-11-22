//go:build linux && !android

package linux

import (
	"fmt"

	"github.com/nsgo-dev/nsgo/internal/driver"
	"github.com/nsgo-dev/nsgo/internal/platform/menutext"
)

// roleActionBase separates a role's action data from a menu item's ID.
const roleActionBase = 1 << 20

// setMenu replaces the application menu and its shortcuts; nil restores
// GTK's own menu bar.
func (w *window) setMenu(entries []driver.MenuEntry) {
	w.chrome.menu = entries
	w.chrome.keys = map[string]menuRef{}
	w.menuKeys(entries)
	w.showMenuBar()
}

// showMenuBar shows the application menu as a GTK menu bar over the surface,
// updating the existing GMenu model and GSimpleActionGroup in place when the
// bar is already shown so setting the menu again (such as checking an item)
// neither flashes the bar nor closes an open popover. No bar shows while the
// title bar is hidden, the window is full screen, the window is transparent
// or an auto-hiding bar is not revealed.
func (w *window) showMenuBar() {
	c := &w.chrome
	want := c.menu != nil && c.titleBar == driver.TitleBarDefault && !c.fullScreen && !w.transparent &&
		!(c.menuBar == driver.MenuBarAutoHide && !c.revealed)
	if !want {
		w.clearMenuBar()
		return
	}
	next := buildGTKMenuTree(c.menu, w.language, speakAvailable())
	if w.menuBar != 0 && w.menuModel != 0 && w.actionGroup != 0 {
		syncGTKMenuItems(gtkMenuBarOps{w}, w.menuModel, w.menuTree, next)
		w.menuTree = next
		return
	}
	w.clearMenuBar()
	group := gSimpleActionGroupNew()
	if group == 0 {
		return
	}
	model := gMenuNew()
	if model == 0 {
		gObjectUnref(group)
		return
	}
	w.actionGroup = group
	w.menuModel = model
	syncGTKMenuItems(gtkMenuBarOps{w}, model, nil, next)
	w.menuTree = next
	gtkWidgetInsertActionGroup(w.win, cstr("win"), group)
	bar := gtkPopoverMenuBarNewFromModel(model)
	if bar == 0 {
		w.clearMenuBar()
		return
	}
	gtkWidgetSetHalign(bar, 1) // start
	gtkBoxPrepend(w.box, bar)
	gtkWidgetSetVisible(bar, 1)
	w.menuBar = bar
}

// clearMenuBar removes the application menu bar widget and releases its owned
// GMenu models and GSimpleActionGroup.
func (w *window) clearMenuBar() {
	if w.menuBar != 0 {
		// Unparenting drops the bar's only reference.
		gtkWidgetSetVisible(w.menuBar, 0)
		if w.box != 0 {
			gtkBoxRemove(w.box, w.menuBar)
		}
		w.menuBar = 0
	}
	if len(w.menuTree) > 0 {
		releaseGTKMenuTree(gtkMenuBarOps{w}, w.menuTree)
		w.menuTree = nil
	}
	if w.menuModel != 0 {
		gObjectUnref(w.menuModel)
		w.menuModel = 0
	}
	if w.actionGroup != 0 {
		if w.win != 0 {
			gtkWidgetInsertActionGroup(w.win, cstr("win"), 0)
		}
		gObjectUnref(w.actionGroup)
		w.actionGroup = 0
	}
	clear(w.actionRefs)
}

type gtkMenuBarOps struct {
	w *window
}

func (gtkMenuBarOps) newMenu() uintptr { return gMenuNew() }

func (gtkMenuBarOps) unrefMenu(menu uintptr) {
	if menu != 0 {
		gObjectUnref(menu)
	}
}

func (gtkMenuBarOps) insertSubmenu(parent uintptr, pos int32, label string, submenu uintptr) {
	if parent != 0 && submenu != 0 {
		gMenuInsertSubmenu(parent, pos, cstr(label), submenu)
	}
}

func (gtkMenuBarOps) insertSection(parent uintptr, pos int32, section uintptr) {
	if parent != 0 && section != 0 {
		gMenuInsertSection(parent, pos, nil, section)
	}
}

func (gtkMenuBarOps) insertItem(parent uintptr, pos int32, label, actionName string, radio bool) {
	if parent == 0 {
		return
	}
	var item uintptr
	if actionName == "" {
		item = gMenuItemNew(cstr(label), nil)
	} else {
		item = gMenuItemNew(cstr(label), cstr("win."+actionName))
		if item != 0 && radio {
			// A target its action's state may equal makes GTK draw a radio, checked while it does.
			gMenuItemSetAttributeValue(item, cstr("target"), gVariantNewString(cstr(radioTarget)))
		}
	}
	if item != 0 {
		gMenuInsertItem(parent, pos, item)
		gObjectUnref(item)
	}
}

func (gtkMenuBarOps) removeAt(parent uintptr, pos int32) {
	if parent != 0 {
		gMenuRemove(parent, pos)
	}
}

func (o gtkMenuBarOps) addAction(n *gtkMenuNode) (uintptr, string) {
	return o.w.registerMenuAction(o.w.actionGroup, o.w.actionRefs, n.id, n.role, n.actionKind, n.disabled, n.checked)
}

func (o gtkMenuBarOps) updateAction(action uintptr, old, next *gtkMenuNode) {
	if old.disabled != next.disabled {
		gSimpleActionSetEnabled(action, boolInt(!next.disabled))
	}
	if old.checked != next.checked {
		switch next.actionKind {
		case gtkMenuActionCheck:
			// The action sinks the floating variant and owns it.
			gSimpleActionSetState(action, gVariantNewBoolean(boolInt(next.checked)))
		case gtkMenuActionRadio:
			state := ""
			if next.checked {
				state = radioTarget
			}
			gSimpleActionSetState(action, gVariantNewString(cstr(state)))
		}
	}
	if old.id != next.id || old.role != next.role {
		o.w.actionRefs[action] = menuRef{id: next.id, role: next.role}
	}
}

func (o gtkMenuBarOps) removeAction(action uintptr, name string) {
	delete(o.w.actionRefs, action)
	if o.w.actionGroup != 0 && name != "" {
		gActionMapRemoveAction(o.w.actionGroup, cstr(name))
	}
}

// buildSubmenu returns a GMenuModel of a submenu's items, separated into
// sections at the Separator entries as GTK menus show groups. Used by
// context popups (popupMenu).
func (w *window) buildSubmenu(entries []driver.MenuEntry) uintptr {
	sub := gMenuNew()
	section := gMenuNew()
	flush := func() {
		if section == 0 {
			return
		}
		gMenuAppendSection(sub, nil, section)
		gObjectUnref(section)
		section = 0
	}
	for i := range entries {
		e := &entries[i]
		if e.Separator {
			flush()
			section = gMenuNew()
			continue
		}
		if !shown(e) {
			continue
		}
		if len(e.Items) > 0 {
			child := w.buildSubmenu(e.Items)
			if child != 0 {
				gMenuAppendSubmenu(section, cstr(menutext.Label(w.language, e, "")), child)
				gObjectUnref(child)
			}
			continue
		}
		if item := w.buildItem(e); item != 0 {
			gMenuAppendItem(section, item)
			gObjectUnref(item)
		}
	}
	flush()
	return sub
}

// shown reports whether the menu shows e: not a role GTK has no use for, nor a submenu none of whose items it shows, such as a Help holder with no items or a Speech submenu of macOS's roles.
func shown(e *driver.MenuEntry) bool {
	return linuxMenuShown(e, speakAvailable())
}

// buildItem returns a GMenuItem of e with an action when it has one.
func (w *window) buildItem(e *driver.MenuEntry) uintptr {
	label := menutext.Label(w.language, e, "")
	action := ""
	if e.ID != 0 {
		action = w.addAction(e.ID, e.Role, e.Disabled, e.Checkable, e.Checked, e.Radio)
	} else if e.Role != driver.MenuRoleNone {
		action = w.addAction(0, e.Role, e.Disabled, false, false, false)
	}
	if action == "" {
		return gMenuItemNew(cstr(label), nil)
	}
	prefix := "win."
	if w.building.prefix != "" {
		prefix = w.building.prefix
	}
	item := gMenuItemNew(cstr(label), cstr(prefix+action))
	if item != 0 && e.Radio && e.ID != 0 {
		// A target its action's state may equal makes GTK draw a radio, checked while it does.
		gMenuItemSetAttributeValue(item, cstr("target"), gVariantNewString(cstr(radioTarget)))
	}
	return item
}

// radioTarget is the target of a radio item, and its action's state while it is checked.
const radioTarget = "selected"

// addAction registers a menu item's action on the window (or popup group) and
// returns its name; "" when the item does nothing.
func (w *window) addAction(id uint32, role driver.MenuRole, disabled, checkable, checked, radio bool) string {
	kind := gtkMenuActionPlain
	switch {
	case radio:
		kind = gtkMenuActionRadio
	case checkable:
		kind = gtkMenuActionCheck
	}
	group, refs := w.actionGroup, w.actionRefs
	if w.building.group != 0 {
		group, refs = w.building.group, w.building.refs
	}
	_, name := w.registerMenuAction(group, refs, id, role, kind, disabled, checked)
	return name
}

// registerMenuAction creates a GSimpleAction for one menu item, registers it
// in group and refs, and returns its handle and name.
func (w *window) registerMenuAction(group uintptr, refs map[uintptr]menuRef, id uint32, role driver.MenuRole, kind gtkMenuActionKind, disabled, checked bool) (uintptr, string) {
	w.menuCounter++
	name := fmt.Sprintf("nsgo.item-%d", w.menuCounter)
	var action uintptr
	switch kind {
	case gtkMenuActionRadio:
		// A radio's action takes its target as a string parameter, which GTK requires to activate it, and its string state says whether the item is checked. The action sinks the floating variant and owns it.
		state := ""
		if checked {
			state = radioTarget
		}
		action = gSimpleActionNewStateful(cstr(name), cstr("s"), gVariantNewString(cstr(state)))
	case gtkMenuActionCheck:
		// The action sinks the floating variant and owns it.
		state := gVariantNewBoolean(boolInt(checked))
		action = gSimpleActionNewStateful(cstr(name), nil, state)
	default:
		action = gSimpleActionNew(cstr(name), 0)
	}
	if action == 0 {
		return 0, ""
	}
	if disabled {
		gSimpleActionSetEnabled(action, 0)
	}
	refs[action] = menuRef{id: id, role: role}
	gSignalConnect(action, "activate", cbMenuActivate, w.handle)
	if group != 0 {
		gActionMapAddAction(group, action)
	}
	gObjectUnref(action)
	return action, name
}

// menuActivateCallback runs the item of a chosen menu action.
func menuActivateCallback(action, parameter, data uintptr) {
	w := windowOf(data)
	if w == nil || w.closing {
		return
	}
	if ref, ok := w.actionRefs[action]; ok {
		w.menuChosen(ref)
	} else if ref, ok := w.popupRefs[action]; ok {
		w.menuChosen(ref)
	}
}
