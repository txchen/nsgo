package windows

import (
	"slices"
	"testing"

	"github.com/nsgo-dev/nsgo/internal/driver"
)

type fakeWinMenuItemState struct {
	kind      winMenuKind
	label     string
	fType     uint32
	fState    uint32
	commandID uint32
	subMenu   uintptr
}

type fakeWinMenuOps struct {
	nextHandle uintptr
	menus      map[uintptr][]fakeWinMenuItemState
	popups     int
	inserts    int
	updates    int
	deletes    int
	lastDiff   winMenuDiff
}

func newFakeWinMenuOps() *fakeWinMenuOps {
	return &fakeWinMenuOps{
		nextHandle: 100,
		menus:      map[uintptr][]fakeWinMenuItemState{},
	}
}

func (f *fakeWinMenuOps) newBar() uintptr {
	f.nextHandle++
	h := f.nextHandle
	f.menus[h] = nil
	return h
}

func (f *fakeWinMenuOps) createPopup() uintptr {
	f.popups++
	f.nextHandle++
	h := f.nextHandle
	f.menus[h] = nil
	return h
}

func (f *fakeWinMenuOps) insertItem(menu uintptr, pos uint32, it *winMenuItem) {
	f.inserts++
	item := fakeWinMenuItemState{
		kind:      it.kind,
		label:     it.label,
		fType:     it.fType(),
		fState:    it.fState(),
		commandID: it.commandID,
		subMenu:   it.handle,
	}
	items := f.menus[menu]
	idx := int(pos)
	items = slices.Insert(items, idx, item)
	f.menus[menu] = items
}

func (f *fakeWinMenuOps) updateItem(menu uintptr, pos uint32, next *winMenuItem, diff winMenuDiff) {
	f.updates++
	f.lastDiff = diff
	item := &f.menus[menu][int(pos)]
	if diff.fTypeChanged {
		item.fType = next.fType()
	}
	if diff.fStateChanged {
		item.fState = next.fState()
	}
	if diff.idChanged {
		item.commandID = next.commandID
	}
	if diff.labelChanged {
		item.label = next.label
	}
}

func (f *fakeWinMenuOps) deleteItem(menu uintptr, pos uint32) {
	f.deletes++
	items := f.menus[menu]
	removed := items[int(pos)]
	f.menus[menu] = slices.Delete(items, int(pos), int(pos)+1)
	if removed.subMenu != 0 {
		f.destroySubtree(removed.subMenu)
	}
}

func (f *fakeWinMenuOps) destroySubtree(menu uintptr) {
	for _, it := range f.menus[menu] {
		if it.subMenu != 0 {
			f.destroySubtree(it.subMenu)
		}
	}
	delete(f.menus, menu)
}

func TestWinMenuSyncPreservesHandlesOnStateToggle(t *testing.T) {
	ops := newFakeWinMenuOps()
	bar := ops.newBar()

	makeMenu := func(checked, disabled bool, firstID uint32) ([]winMenuItem, []menuRef) {
		entries := []driver.MenuEntry{
			{Label: "File", Items: []driver.MenuEntry{
				{Label: "New", Shortcut: "mod+n", ID: firstID},
			}},
			{Label: "View", Items: []driver.MenuEntry{
				{Label: "Sidebar", Checkable: true, Checked: checked, Disabled: disabled, ID: firstID + 1},
				{Separator: true},
				{Label: "Movies", Checkable: true, Radio: true, Checked: !checked, ID: firstID + 2},
			}},
		}
		var refs []menuRef
		return buildWinMenuTree(entries, "en", false, false, &refs), refs
	}

	tree1, refs1 := makeMenu(true, false, 10)
	if !syncWinMenu(ops, bar, nil, tree1) {
		t.Fatal("initial sync reported no top-level change")
	}
	if len(refs1) != 3 {
		t.Fatalf("len(refs1) = %d, want 3", len(refs1))
	}
	fileHandle := tree1[0].handle
	viewHandle := tree1[1].handle
	if fileHandle == 0 || viewHandle == 0 {
		t.Fatal("submenu handles not populated")
	}

	// Reset counters and toggle Sidebar checked + Movies radio checked.
	ops.popups, ops.inserts, ops.updates, ops.deletes = 0, 0, 0, 0
	tree2, refs2 := makeMenu(false, true, 20)
	topChanged := syncWinMenu(ops, bar, tree1, tree2)
	if topChanged {
		t.Error("toggling submenu item states reported top-level bar change")
	}
	if ops.popups != 0 || ops.inserts != 0 || ops.deletes != 0 {
		t.Errorf("expected zero structural mutations, got popups=%d inserts=%d deletes=%d", ops.popups, ops.inserts, ops.deletes)
	}
	if ops.updates != 2 {
		t.Errorf("updates = %d, want 2 (Sidebar and Movies)", ops.updates)
	}
	if tree2[0].handle != fileHandle || tree2[1].handle != viewHandle {
		t.Error("submenu handles were not preserved across in-place sync")
	}
	if got := ops.menus[viewHandle][0].fState; got != mfsGrayed {
		t.Errorf("Sidebar fState = %#x, want %#x (unchecked+grayed)", got, mfsGrayed)
	}
	if got := ops.menus[viewHandle][2].fState; got != mfsChecked {
		t.Errorf("Movies fState = %#x, want %#x (checked)", got, mfsChecked)
	}
	if got := ops.menus[viewHandle][2].fType; got != mftString|mftRadioCheck {
		t.Errorf("Movies fType = %#x, want %#x", got, mftString|mftRadioCheck)
	}
	if refs2[1].id != 21 || refs2[2].id != 22 {
		t.Errorf("refs2 IDs = %+v, want 21 and 22", refs2)
	}
}

func TestWinMenuSyncUpdatesTopologyLabelsRTLAndCommandIDs(t *testing.T) {
	ops := newFakeWinMenuOps()
	bar := ops.newBar()

	var refs1 []menuRef
	tree1 := buildWinMenuTree([]driver.MenuEntry{
		{Label: "File", Items: []driver.MenuEntry{
			{Label: "Open", ID: 1},
		}},
		{Label: "View", Items: []driver.MenuEntry{
			{Label: "Zoom", ID: 2},
		}},
	}, "en", false, false, &refs1)
	syncWinMenu(ops, bar, nil, tree1)
	fileHandle, viewHandle := tree1[0].handle, tree1[1].handle

	// Insert an item into File (shifting View's commandID from 2 to 3), rename
	// View -> Display, add a Help menu, and switch to RTL.
	ops.popups, ops.inserts, ops.updates, ops.deletes = 0, 0, 0, 0
	var refs2 []menuRef
	tree2 := buildWinMenuTree([]driver.MenuEntry{
		{Label: "File", Items: []driver.MenuEntry{
			{Label: "New", Shortcut: "mod+n", ID: 10},
			{Label: "Open", ID: 11},
		}},
		{Label: "Display", Items: []driver.MenuEntry{
			{Label: "Zoom", ID: 12},
		}},
		{Label: "Help", Items: []driver.MenuEntry{
			{Label: "Docs", ID: 13},
		}},
	}, "en", true, false, &refs2)
	topChanged := syncWinMenu(ops, bar, tree1, tree2)
	if !topChanged {
		t.Error("adding top-level menu and renaming View should report top-level change")
	}
	if tree2[0].handle != fileHandle || tree2[1].handle != viewHandle {
		t.Error("existing File and View submenu handles were not preserved")
	}
	if tree2[2].handle == 0 {
		t.Error("added Help submenu got no handle")
	}
	if got := ops.menus[bar][1].label; got != "Display" {
		t.Errorf("renamed top-level menu = %q, want Display", got)
	}
	if got := ops.menus[bar][0].fType; got&mftRightOrder == 0 {
		t.Errorf("RTL top-level item missing MFT_RIGHTORDER: %#x", got)
	}
	if got := ops.menus[fileHandle][0].label; got != "New\tCtrl+N" {
		t.Errorf("File[0] label = %q, want New\\tCtrl+N", got)
	}
	if got := ops.menus[viewHandle][0].commandID; got != 3 {
		t.Errorf("shifted View[0] commandID = %d, want 3", got)
	}
	if len(refs2) != 4 || refs2[2].id != 12 || refs2[3].id != 13 {
		t.Errorf("unexpected refs2: %+v", refs2)
	}

	// Replace top-level submenu with a leaf action and drop Help.
	var refs3 []menuRef
	tree3 := buildWinMenuTree([]driver.MenuEntry{
		{Label: "Sync & Run", ID: 30},
	}, "en", false, false, &refs3)
	if !syncWinMenu(ops, bar, tree2, tree3) {
		t.Error("replacing top-level submenus should report top-level change")
	}
	if len(ops.menus[bar]) != 1 || ops.menus[bar][0].kind != winMenuCommand || ops.menus[bar][0].label != "Sync && Run" {
		t.Errorf("bar after replace = %+v", ops.menus[bar])
	}
	if len(ops.menus) != 1 {
		t.Errorf("expected old submenus to be destroyed, still alive: %d", len(ops.menus))
	}
}

func TestWinMenuBuildNormalizesRolesAndSeparators(t *testing.T) {
	entries := []driver.MenuEntry{
		{Separator: true},
		{Label: "App", Items: []driver.MenuEntry{
			{Role: driver.MenuAbout},
			{Separator: true},
			{Role: driver.MenuServices, Items: []driver.MenuEntry{{Label: "Mine", ID: 1}}},
			{Separator: true},
			{Separator: true},
			{Role: driver.MenuQuit, Label: "Quit"},
			{Separator: true},
		}},
		{Label: "Edit", Items: []driver.MenuEntry{
			{Role: driver.MenuDelete, Label: "Delete"},
			{Separator: true},
			{Label: "Speech", Items: []driver.MenuEntry{
				{Role: driver.MenuStartSpeaking},
				{Role: driver.MenuStopSpeaking},
			}},
		}},
		{Label: "Help", Role: driver.MenuHelpMenu},
	}

	var refsNoSpeech []menuRef
	treeNoSpeech := buildWinMenuTree(entries, "en", false, false, &refsNoSpeech)
	if len(treeNoSpeech) != 2 {
		t.Fatalf("without speech: got %d top-level menus, want 2", len(treeNoSpeech))
	}
	if len(treeNoSpeech[0].children) != 1 || treeNoSpeech[0].children[0].label != "Quit\tAlt+F4" {
		t.Errorf("App children = %+v, want only Quit\\tAlt+F4", treeNoSpeech[0].children)
	}
	if len(treeNoSpeech[1].children) != 1 || treeNoSpeech[1].children[0].label != "Delete" {
		t.Errorf("Edit children without speech = %+v, want only Delete", treeNoSpeech[1].children)
	}

	var refsSpeech []menuRef
	treeSpeech := buildWinMenuTree(entries, "en", false, true, &refsSpeech)
	if len(treeSpeech[1].children) != 3 || treeSpeech[1].children[1].kind != winMenuSeparator || treeSpeech[1].children[2].label != "Speech" {
		t.Errorf("Edit children with speech = %+v", treeSpeech[1].children)
	}
}

func TestWinFirstShowStateTransitions(t *testing.T) {
	var fs firstShow
	if fs.consume(false) {
		t.Fatal("unarmed firstShow consumed true")
	}

	// Non-hidden window waits for its first batch, surviving non-show commands.
	fs.arm(false)
	for _, action := range []driver.WindowAction{
		driver.WindowSetTitleBar, driver.WindowSetMenuBar, driver.WindowSetFrame,
		driver.WindowSetConstraints, driver.WindowSetLevel, driver.WindowFitSize,
		driver.WindowSetAnchor, driver.WindowToggleFullScreen, driver.WindowSetTransparent,
	} {
		fs.command(action)
	}
	if !fs.consume(false) {
		t.Fatal("expected first batch to consume pending show")
	}
	if fs.consume(false) {
		t.Fatal("second batch should not consume pending show again")
	}

	// Hidden window never shows on its first batch.
	fs.arm(true)
	if fs.consume(false) {
		t.Fatal("hidden window consumed show")
	}

	// Explicit hide/show/close/destroy cancels deferred first show.
	for _, action := range []driver.WindowAction{
		driver.WindowHide, driver.WindowShow, driver.WindowOrderFront,
		driver.WindowClose, driver.WindowDestroy,
	} {
		fs.arm(false)
		fs.command(action)
		if fs.consume(false) {
			t.Errorf("command %v did not cancel deferred first show", action)
		}
	}

	// Destroying window does not present even if still armed.
	fs.arm(false)
	if fs.consume(true) {
		t.Fatal("destroying window consumed show")
	}
	if fs.consume(false) {
		t.Fatal("consumed flag was not cleared when closed")
	}
}
