package core

import (
	"math"
	"slices"
	"testing"

	"github.com/nsgo-dev/nsgo/internal/devtools"
	"github.com/nsgo-dev/nsgo/internal/driver"
)

// audit is DevTools' overlay showing every view's boxes, without outlining commits.
var audit = UpdateHighlights{NoUpdates: true, Layout: LayoutAudit{Bounds: true}}

// boxOf returns the box of the node with testID among boxes, failing when it is not there.
func (h *host) boxOf(boxes []auditBox, testID string) auditBox {
	h.t.Helper()
	b := h.tree.surfaceBounds(h.find(testID))
	for _, box := range boxes {
		if box.bounds == b {
			return box
		}
	}
	h.t.Fatalf("no box of %q at %v in %v", testID, b, boxes)
	return auditBox{}
}

func TestAuditBoxesPlaceEveryShownView(t *testing.T) {
	h := newHost(t, Size{Width: 200, Height: 300}, func(cx *Scope) Node {
		return Group{
			View{TestID: "app", Style: Style{FlexGrow: 1, Padding: All(10), BorderWidth: 2}, Children: []Node{
				View{TestID: "child", Style: Style{Height: Pt(40), Margin: Edges{Top: 5, Left: 6}}},
				View{TestID: "gone", Style: Style{Height: Pt(40), Display: DisplayNone}},
				Overlay{TestID: "over", Style: Style{Top: Pt(100), Left: Pt(20), Width: Pt(50), Height: Pt(30)},
					Children: []Node{View{TestID: "inside", Style: Style{Height: Pt(10)}}}},
			}},
			audit,
		}
	})
	h.commit()
	boxes := h.tree.auditBoxes(nil)
	if len(boxes) != 5 {
		t.Fatalf("got %d boxes, want the root, app, child, overlay and what it holds, and none of DevTools' own: %v", len(boxes), boxes)
	}
	surface := Bounds{Size: Size{Width: 200, Height: 300}}
	for i, id := range []string{"app", "child", "over", "inside"} {
		if got, want := boxes[i+1].bounds, h.tree.surfaceBounds(h.find(id)); got != want {
			t.Errorf("box %d = %v, want %s's %v", i+1, got, id, want)
		}
		if boxes[i+1].clip != surface {
			t.Errorf("%s's clip = %v, want the surface", id, boxes[i+1].clip)
		}
	}
	app := boxes[1].box
	if app.Border != [4]float32{2, 2, 2, 2} || app.Padding != [4]float32{10, 10, 10, 10} {
		t.Errorf("app's edges = %v and %v, want its style's", app.Border, app.Padding)
	}
	if m := h.boxOf(boxes, "child").box.Margin; m != [4]float32{5, 0, 0, 6} {
		t.Errorf("child's margin = %v", m)
	}
}

func TestAuditBoxesLeaveOutWhatHiddenViewsHold(t *testing.T) {
	var vis *Signal[Visibility]
	h := newHost(t, Size{Width: 200, Height: 300}, func(cx *Scope) Node {
		vis = NewSignal(cx, VisibilityVisible)
		return Group{
			View{Style: Style{FlexGrow: 1}, Children: []Node{
				View{TestID: "parent", StyleFunc: func() Style { return Style{Height: Pt(40), Visibility: vis.Get()} }, Children: []Node{
					View{TestID: "kid", Style: Style{Width: Pt(30), Height: Pt(20), Visibility: VisibilityVisible}},
					Overlay{TestID: "floating", Style: Style{Width: Pt(10), Height: Pt(10)}},
				}},
			}},
			audit,
		}
	})
	h.commit()
	if got := len(h.tree.auditBoxes(nil)); got != 5 {
		t.Fatalf("got %d boxes, want the root's, its view's, the parent's, the kid's and the overlay's", got)
	}
	vis.Set(VisibilityHidden)
	h.commit()
	boxes := h.tree.auditBoxes(nil)
	if len(boxes) != 3 {
		t.Fatalf("got %v, want the boxes of a hidden view and of what it holds left out, as the native view hides them", boxes)
	}
	// The overlay is the root view's native child, which the parent does not hide.
	h.boxOf(boxes, "floating")
}

func TestAuditBoxesFollowScrollsAndClip(t *testing.T) {
	h := newHost(t, Size{Width: 200, Height: 300}, func(cx *Scope) Node {
		return Group{
			View{Style: Style{FlexGrow: 1}, Children: []Node{
				Scroll{TestID: "scroll", Style: Style{Height: Pt(100)}, Children: []Node{
					View{TestID: "row", Style: Style{Height: Pt(400)}},
				}},
				View{TestID: "cut", Style: Style{Height: Pt(50), Width: Pt(80), Overflow: Hidden}, Children: []Node{
					View{TestID: "wide", Style: Style{Height: Pt(20), Width: Pt(300)}},
				}},
			}},
			audit,
		}
	})
	h.root.SetScrollbars(testBars{})
	h.commit()
	scroll := h.find("scroll")
	if scroll.scroll.barHost == nil {
		t.Fatal("no bar host")
	}
	h.tree.Dispatch(driver.Event{Kind: driver.EventScroll, ID: scroll.id, Point: Point{Y: 100}})
	h.commit()
	boxes := h.tree.auditBoxes(nil)
	for _, b := range boxes {
		if b.bounds.Size == scroll.frame.Size && b.bounds != h.tree.surfaceBounds(scroll) {
			t.Errorf("a box of the viewport's size at %v: the bar's host", b.bounds)
		}
	}
	row := h.boxOf(boxes, "row")
	if row.bounds.Origin.Y != -100 {
		t.Errorf("row at %v, want moved up by the scroll", row.bounds)
	}
	if row.clip != h.tree.surfaceBounds(scroll) {
		t.Errorf("row's clip = %v, want the scroll view's bounds", row.clip)
	}
	cut := h.tree.surfaceBounds(h.find("cut"))
	if wide := h.boxOf(boxes, "wide"); wide.clip != cut {
		t.Errorf("wide's clip = %v, want its parent's %v", wide.clip, cut)
	}
}

type testBars struct{}

func (testBars) Scrollbar(cx *Scope, target *Ref, horizontal bool) Node {
	return View{Style: Style{Position: Absolute, Right: Pt(0), Width: Pt(4), Height: Pt(20)}}
}

// drawings counts the drawings a batch sends for the node with testID.
func (h *host) drawings(b *driver.MountBatch, testID string) int {
	id := h.find(testID).id
	n := 0
	for _, it := range b.Items {
		if it.Kind == driver.ItemCanvas && it.ID == id {
			n++
		}
	}
	return n
}

func TestAuditRedrawsWithinTheCommit(t *testing.T) {
	var width, padding *Signal[Points]
	var hidden *Signal[bool]
	h := newHost(t, Size{Width: 200, Height: 300}, func(cx *Scope) Node {
		width, padding = NewSignal[Points](cx, 50), NewSignal[Points](cx, 0)
		hidden = NewSignal(cx, false)
		return Group{
			View{Style: Style{FlexGrow: 1}, Children: []Node{
				View{TestID: "sibling", StyleFunc: func() Style { return Style{Width: Pt(width.Get()), Height: Pt(20), Padding: All(padding.Get())} }},
				View{StyleFunc: func() Style {
					if hidden.Get() {
						return Style{Height: Pt(10), Visibility: VisibilityHidden}
					}
					return Style{Height: Pt(10)}
				}},
				Scroll{TestID: "scroll", Style: Style{Height: Pt(100)}, Children: []Node{View{Style: Style{Height: Pt(400)}}}},
			}},
			audit,
		}
	})
	h.commit()
	a := h.tree.highlights.audit
	width.Set(70)
	if b := h.commit(); h.drawings(b, "nsgo-layout-canvas") != 1 {
		t.Fatalf("a resize sent %v, want one drawing in the same batch", kinds(b))
	}
	if got := h.boxOf(a.boxes, "sibling").bounds.Size.Width; got != 70 {
		t.Errorf("the drawing read a width of %v, want the new one", got)
	}
	boxes := len(a.boxes)
	if h.tree.Commit() != nil {
		t.Fatal("a commit after it sent more")
	}
	h.tree.Dispatch(driver.Event{Kind: driver.EventScroll, ID: h.find("scroll").id, Point: Point{Y: 30}})
	if b := h.commit(); h.drawings(b, "nsgo-layout-canvas") != 1 {
		t.Fatalf("a scroll sent %v, want one drawing", kinds(b))
	}
	// Hiding a view and changing a padding move no frame, but change the boxes.
	hidden.Set(true)
	if b := h.commit(); h.drawings(b, "nsgo-layout-canvas") != 1 || len(a.boxes) != boxes-1 {
		t.Fatalf("hiding a view sent %v and read %d boxes, want one drawing of %d", kinds(b), len(a.boxes), boxes-1)
	}
	padding.Set(6)
	if b := h.commit(); h.drawings(b, "nsgo-layout-canvas") != 1 || h.boxOf(a.boxes, "sibling").box.Padding != [4]float32{6, 6, 6, 6} {
		t.Fatalf("a padding sent %v, want one drawing with it", kinds(b))
	}

	// With the boxes off, nothing reads them: a scroll commits nothing and a resize bumps nothing.
	a.bounds.Set(false)
	h.commit()
	if h.tree.auditRead() {
		t.Fatal("the boxes still have a reader")
	}
	h.tree.Dispatch(driver.Event{Kind: driver.EventScroll, ID: h.find("scroll").id, Point: Point{Y: 60}})
	if h.tree.framesMoved {
		t.Error("a scroll asked for a round with no reader of the boxes")
	}
	if h.tree.Commit() != nil {
		t.Error("a scroll committed with no reader of the boxes")
	}
	bumps := a.frames.GetUntracked()
	width.Set(90)
	h.commit()
	if a.frames.GetUntracked() != bumps || h.tree.framesMoved {
		t.Error("a resize bumped the boxes' signal with no reader")
	}
}

func TestAuditMountsNothingUntilItShows(t *testing.T) {
	h := newHost(t, Size{Width: 200, Height: 300}, func(cx *Scope) Node {
		return Group{View{Style: Style{FlexGrow: 1}}, UpdateHighlights{NoUpdates: true}}
	})
	h.commit()
	for _, n := range h.tree.nodes {
		if n != nil && !n.deleted && n.props.TestID == "nsgo-layout-audit" {
			t.Fatal("an audit that starts hidden mounts its layer")
		}
	}
	if h.tree.auditRead() {
		t.Error("a hidden audit reads the boxes")
	}
}

func TestPickRules(t *testing.T) {
	view := Size{Width: 200, Height: 100}
	var m measurement
	for _, p := range []Point{{X: Points(math.NaN()), Y: 1}, {X: Points(math.Inf(1)), Y: 1}, {X: -1, Y: 5}, {X: 5, Y: 101}} {
		if m = m.pick(p, view); m.points != 0 {
			t.Fatalf("picked %v", p)
		}
	}
	m = m.pick(Point{X: 150, Y: 80}, view)
	m = m.pick(Point{X: 50, Y: 20}, view)
	if m.points != 2 || m.horizontal() != 100 || m.vertical() != 60 {
		t.Fatalf("measured %+v: %v by %v, want 100 by 60 from reversed points", m, m.horizontal(), m.vertical())
	}
	if m = m.pick(Point{X: 10, Y: 10}, view); m.points != 1 || m.start != (Point{X: 10, Y: 10}) {
		t.Errorf("a third point gave %+v, want it to start anew", m)
	}
}

func TestAuditNumbersDropAWholeDecimal(t *testing.T) {
	for v, want := range map[Points]string{170: "170", 12.34: "12.3", 0.5: "0.5", 844: "844"} {
		if got := auditNumber(v); got != want {
			t.Errorf("auditNumber(%v) = %q, want %q", v, got, want)
		}
	}
}

func TestAuditGridStartsAtItsSmallestStep(t *testing.T) {
	cx := testRoot(RootOptions{})
	for grid, want := range map[Points]Points{0: 0, 2: minAuditGrid, 12: 12, Points(math.NaN()): 0, -8: 0} {
		if got := newLayoutAudit(cx, LayoutAudit{Grid: grid}, false).grid.GetUntracked(); got != want {
			t.Errorf("a grid of %v starts at %v, want %v", grid, got, want)
		}
	}
}

// A platform-presented iOS sheet (ModalPageSheet / ModalFormSheet sized by EventModalSize) mounts its own Topmost DevTools overlay inside the sheet so the layout audit, update highlights and Pick work inside it while staying out of Elements and cleaning up on close.
func TestDevToolsOverlayInsideIOSSheetModal(t *testing.T) {
	devtools.Enabled = true
	t.Cleanup(func() {
		devtools.Enabled = false
		for _, s := range devtools.Surfaces() {
			devtools.Unregister(s)
		}
	})
	var sheetOpen *Signal[bool]
	var sheetCount *Signal[int]
	pressed := 0
	h := newHost(t, Size{Width: 390, Height: 844}, func(cx *Scope) Node {
		sheetOpen = NewSignal(cx, true)
		sheetCount = NewSignal(cx, 0)
		return Group{
			View{TestID: "app", Style: Style{FlexGrow: 1}, Children: []Node{
				Show{When: sheetOpen.Get, Then: func(cx *Scope) Node {
					return Modal{TestID: "sheet", Presentation: ModalPageSheet, Children: []Node{
						View{TestID: "sheet-btn", Style: Style{Width: Pt(100), Height: Pt(40)},
							OnPress: func(PressEvent) { pressed++ },
							Children: []Node{
								Text{TestID: "sheet-label", ContentFunc: func() string {
									if sheetCount.Get() == 0 {
										return "Sheet 0"
									}
									return "Sheet 1"
								}},
							}},
					}}
				}},
			}},
			UpdateHighlights{Layout: LayoutAudit{Bounds: true}},
		}
	})
	h.commit()
	h.drain()

	sheet := h.find("sheet")
	h.tree.Dispatch(driver.Event{Kind: driver.EventModalSize, ID: sheet.id, Size: Size{Width: 300, Height: 540}, Insets: Edges{Bottom: 20}})
	h.commit()
	h.drain()

	if len(h.tree.highlights.sheets) != 1 {
		t.Fatalf("sheet overlays = %d, want 1", len(h.tree.highlights.sheets))
	}
	sh := h.tree.highlights.sheets[0]
	if sh.root.n == nil || sh.root.n.hoist == nil || sh.root.n.hoist.host != sheet {
		t.Fatalf("sheet overlay host = %v, want sheet modal", sh.root.n)
	}
	// Root audit leaves out the reported sheet; sheet audit covers sheet, sheet-btn and sheet-label.
	for _, b := range h.tree.auditBoxes(nil) {
		if b.bounds == h.tree.surfaceBounds(h.find("sheet-btn")) {
			t.Error("root auditBoxes included a view inside a reported sheet")
		}
	}
	if got := len(h.tree.auditBoxesFrom(sheet, nil)); got != 3 {
		t.Fatalf("sheet auditBoxesFrom = %d, want 3 (sheet, sheet-btn, sheet-label)", got)
	}

	// Changing a text inside the sheet highlights it in the sheet's own overlay.
	sheetCount.Set(1)
	h.commit()
	h.drain()
	if len(sh.boxes.GetUntracked()) == 0 {
		t.Fatal("updating a view inside the sheet produced no highlight box in the sheet overlay")
	}

	// Pick inside the sheet hits sheet-btn without pressing it.
	s := &devSurface{h.tree}
	var picked uint32
	s.Pick(true, nil, func(id uint32) { picked = id })
	h.commit()
	var sheetPick *node
	for _, n := range h.tree.nodes {
		if n != nil && !n.deleted && n.props.TestID == "nsgo-devtools-pick" && h.tree.highlights.sheetFor(n) == sh {
			sheetPick = n
		}
	}
	if sheetPick == nil {
		t.Fatal("no pick catcher mounted inside the sheet overlay")
	}
	h.tree.Dispatch(driver.Event{Kind: driver.EventPressDown, ID: sheetPick.id, Point: Point{X: 10, Y: 10}})
	h.commit()
	if pressed != 0 {
		t.Fatalf("sheet-btn pressed %d times during Pick, want 0", pressed)
	}
	if picked != uint32(h.find("sheet-label").id) {
		t.Fatalf("picked = %d, want sheet-label %d", picked, h.find("sheet-label").id)
	}

	// Elements excludes the sheet's DevTools overlay.
	var hasDevView func(devtools.Node) bool
	hasDevView = func(n devtools.Node) bool {
		if n.TestID == devtools.OverlayTestID || n.TestID == "nsgo-layout-audit" {
			return true
		}
		return slices.ContainsFunc(n.Children, hasDevView)
	}
	if hasDevView(s.Elements()) {
		t.Error("Elements lists DevTools' sheet overlay views")
	}

	// Closing the sheet cleanly removes its sheetHighlights entry.
	sheetOpen.Set(false)
	h.commit()
	h.drain()
	if len(h.tree.highlights.sheets) != 0 {
		t.Fatalf("sheet overlays after close = %d, want 0", len(h.tree.highlights.sheets))
	}
}
