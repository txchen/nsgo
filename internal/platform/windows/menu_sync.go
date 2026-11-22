package windows

import (
	"strings"

	"github.com/nsgo-dev/nsgo/internal/driver"
	"github.com/nsgo-dev/nsgo/internal/platform/menutext"
)

const (
	// MENUITEMINFOW mask, type, state and position flags.
	miimState     = 0x00000001
	miimID        = 0x00000002
	miimSubMenu   = 0x00000004
	miimString    = 0x00000040
	miimFType     = 0x00000100
	mftString     = 0x00000000
	mftRadioCheck = 0x00000200
	mftSeparator  = 0x00000800
	mftRightOrder = 0x00002000
	mfsEnabled    = 0x00000000
	mfsGrayed     = 0x00000003
	mfsChecked    = 0x00000008
	mfByPosition  = 0x00000400
)

// menuRef is a menu item's ID and role.
type menuRef struct {
	id        uint32
	role      driver.MenuRole
	notInText bool
}

type winMenuKind uint8

const (
	winMenuCommand winMenuKind = iota
	winMenuSubmenu
	winMenuSeparator
)

// winMenuItem is one normalized item of a Win32 HMENU, kept alongside the
// live menu bar so later SetApplicationMenu calls can update items in place
// with SetMenuItemInfoW/InsertMenuItemW/DeleteMenu rather than rebuilding the
// whole HMENU.
type winMenuItem struct {
	kind      winMenuKind
	label     string
	disabled  bool
	checked   bool
	radio     bool
	rtl       bool
	commandID uint32
	children  []winMenuItem
	handle    uintptr // HMENU of a submenu, preserved across in-place syncs
}

func (it *winMenuItem) fType() uint32 {
	if it.kind == winMenuSeparator {
		return mftSeparator
	}
	ft := uint32(mftString)
	if it.rtl {
		ft |= mftRightOrder
	}
	if it.kind == winMenuCommand && it.radio {
		ft |= mftRadioCheck
	}
	return ft
}

func (it *winMenuItem) fState() uint32 {
	if it.kind == winMenuSeparator {
		return 0
	}
	st := uint32(mfsEnabled)
	if it.disabled {
		st |= mfsGrayed
	}
	if it.kind == winMenuCommand && it.checked {
		st |= mfsChecked
	}
	return st
}

// winMenuDiff records which MENUITEMINFOW fields differ between two items of
// the same kind.
type winMenuDiff struct {
	fTypeChanged  bool
	fStateChanged bool
	idChanged     bool
	labelChanged  bool
}

func (d winMenuDiff) any() bool {
	return d.fTypeChanged || d.fStateChanged || d.idChanged || d.labelChanged
}

func diffWinMenuItem(old, next *winMenuItem) winMenuDiff {
	if next.kind == winMenuSeparator {
		return winMenuDiff{}
	}
	return winMenuDiff{
		fTypeChanged:  old.fType() != next.fType(),
		fStateChanged: old.fState() != next.fState(),
		idChanged:     next.kind == winMenuCommand && old.commandID != next.commandID,
		labelChanged:  old.label != next.label,
	}
}

// winMenuOps abstracts Win32 HMENU mutations so menu diffing can be tested
// without a Windows display.
type winMenuOps interface {
	createPopup() uintptr
	insertItem(menu uintptr, pos uint32, it *winMenuItem)
	updateItem(menu uintptr, pos uint32, next *winMenuItem, diff winMenuDiff)
	deleteItem(menu uintptr, pos uint32)
}

// buildWinMenuTree normalizes entries for a Windows HMENU, appending each leaf
// command's menuRef to refs and numbering commands by their 1-based index in
// refs. Unsupported macOS-only roles, empty menu-role holders, empty submenus,
// and leading/trailing/consecutive separators are omitted.
func buildWinMenuTree(entries []driver.MenuEntry, language string, rtl, speak bool, refs *[]menuRef) []winMenuItem {
	var out []winMenuItem
	separate := false
	for i := range entries {
		e := &entries[i]
		switch {
		case e.Separator:
			separate = len(out) > 0
			continue
		case driver.MenuRoleUnsupported(e.Role, driver.OSWindows, speak), e.Role.HoldsMenu() && len(e.Items) == 0:
			continue
		}
		label := strings.ReplaceAll(menutext.Label(language, e, ""), "&", "&&")
		if len(e.Items) > 0 {
			children := buildWinMenuTree(e.Items, language, rtl, speak, refs)
			if len(children) == 0 {
				continue
			}
			if separate {
				out = append(out, winMenuItem{kind: winMenuSeparator})
				separate = false
			}
			out = append(out, winMenuItem{
				kind:     winMenuSubmenu,
				label:    label,
				disabled: e.Disabled,
				rtl:      rtl,
				children: children,
			})
			continue
		}
		if separate {
			out = append(out, winMenuItem{kind: winMenuSeparator})
			separate = false
		}
		if k := driver.MenuShortcutKey(*e, false); k != "" {
			label += "\t" + shortcutLabel(k)
		}
		*refs = append(*refs, menuRef{e.ID, e.Role, e.NotInText})
		out = append(out, winMenuItem{
			kind:      winMenuCommand,
			label:     label,
			disabled:  e.Disabled,
			checked:   e.Checked,
			radio:     e.Radio,
			rtl:       rtl,
			commandID: uint32(len(*refs)),
		})
	}
	return out
}

// syncWinMenu updates menu in place from old to next, keeping items and
// submenu handles of the same kind and mutating only changed fields. It
// reports whether any direct item of menu changed (which tells the caller
// whether DrawMenuBar is needed for the top-level menu bar).
func syncWinMenu(ops winMenuOps, menu uintptr, old, next []winMenuItem) bool {
	changed := false
	common := min(len(old), len(next))
	pos := uint32(0)
	for i := range common {
		o, n := &old[i], &next[i]
		if o.kind == n.kind {
			if d := diffWinMenuItem(o, n); d.any() {
				ops.updateItem(menu, pos, n, d)
				changed = true
			}
			if n.kind == winMenuSubmenu {
				n.handle = o.handle
				syncWinMenu(ops, n.handle, o.children, n.children)
			}
			pos++
			continue
		}
		ops.deleteItem(menu, pos)
		changed = true
		if insertWinMenuItem(ops, menu, pos, n) {
			pos++
		}
	}
	for i := len(old) - 1; i >= len(next); i-- {
		ops.deleteItem(menu, pos+uint32(i-len(next)))
		changed = true
	}
	for i := len(old); i < len(next); i++ {
		if insertWinMenuItem(ops, menu, pos, &next[i]) {
			pos++
			changed = true
		}
	}
	return changed
}

func insertWinMenuItem(ops winMenuOps, menu uintptr, pos uint32, it *winMenuItem) bool {
	if it.kind == winMenuSubmenu {
		sub := ops.createPopup()
		if sub == 0 {
			return false
		}
		it.handle = sub
		syncWinMenu(ops, sub, nil, it.children)
	}
	ops.insertItem(menu, pos, it)
	return true
}

// shortcutLabel shows a normalized shortcut as Windows' menus do, such
// as "Ctrl+Shift+N".
func shortcutLabel(key string) string {
	parts := strings.Split(key, "+")
	for i, part := range parts {
		switch part {
		case "mod", "ctrl":
			parts[i] = "Ctrl"
		case "alt":
			parts[i] = "Alt"
		case "shift":
			parts[i] = "Shift"
		case "meta":
			parts[i] = "Win"
		case "pageup":
			parts[i] = "PgUp"
		case "pagedown":
			parts[i] = "PgDn"
		case "delete":
			parts[i] = "Del"
		case "escape":
			parts[i] = "Esc"
		default:
			parts[i] = strings.ToUpper(part[:1]) + part[1:]
		}
	}
	return strings.Join(parts, "+")
}

// firstShow tracks a newly created window waiting for its first mount batch
// before presenting itself on screen, matching macOS's surface.showFirst.
type firstShow struct {
	pending bool
}

func (f *firstShow) arm(hidden bool) { f.pending = !hidden }

func (f *firstShow) cancel() { f.pending = false }

func (f *firstShow) command(action driver.WindowAction) {
	switch action {
	case driver.WindowShow, driver.WindowOrderFront, driver.WindowHide,
		driver.WindowClose, driver.WindowDestroy:
		f.pending = false
	}
}

func (f *firstShow) consume(closed bool) bool {
	if !f.pending {
		return false
	}
	f.pending = false
	return !closed
}
