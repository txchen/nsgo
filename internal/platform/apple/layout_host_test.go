//go:build darwin

package apple

import (
	"testing"
	"time"

	"github.com/nsgo-dev/nsgo/internal/driver"
)

func TestLayoutHostStartsOnlyInsertedSubtreeRootsAndRetargetsCurrentFrames(t *testing.T) {
	frame := driver.Bounds{Origin: driver.Point{X: 4}, Size: driver.Size{Width: 20}}
	motions := map[int]*LayoutMotion{}
	ticks, placements := 0, 0
	host := LayoutHost[int]{
		Motions: motions,
		ID:      func(v int) driver.NodeID { return driver.NodeID(v) },
		Framed:  func(int) bool { return true },
		Frame:   func(int) driver.Bounds { return frame },
		Place:   func(_ int, next driver.Bounds) { frame = next; placements++ },
		Now:     func() time.Duration { return 10 * time.Millisecond },
		Start:   func(*driver.LayoutTransition) time.Duration { return 5 * time.Millisecond },
		Tick:    func() { ticks++ },
	}
	create := &driver.LayoutTransition{Duration: time.Second}
	update := &driver.LayoutTransition{Duration: time.Second}
	batch := &LayoutBatch{Animation: &driver.LayoutAnimation{Create: create, Update: update}, Created: map[driver.NodeID]bool{1: true, 2: true}}
	host.Appear(2, batch, 1)
	if len(motions) != 0 || ticks != 0 {
		t.Fatal("a child started a second appearance inside its appearing parent")
	}
	host.Appear(1, batch, 0)
	if m := motions[1]; m == nil || m.Appear != create || m.Start != 10*time.Millisecond {
		t.Fatal("the inserted root did not start its appearance on the surface clock")
	}
	delete(batch.Created, 1)
	to := frame
	to.Origin.X = 30
	host.SetFrame(1, &driver.MountItem{Frame: to}, batch)
	m := motions[1]
	if m.Appear != create || m.Frame != update || m.From != frame || m.To != to || m.Start != 5*time.Millisecond || placements != 0 {
		t.Fatal("frame motion lost its appearance or the host's transition clock")
	}
	host.SetFrame(1, &driver.MountItem{Frame: to}, batch)
	if ticks != 2 {
		t.Fatal("an unchanged target restarted its running transition")
	}
	frame.Origin.X = 12 // The last frame drawn, partway to the previous target.
	to.Origin.X = 60
	host.SetFrame(1, &driver.MountItem{Frame: to}, batch)
	if m.From != frame || m.To != to {
		t.Fatal("a retargeted transition jumped back to its previous start")
	}
	host.SetFrame(1, &driver.MountItem{Frame: frame}, nil)
	if m.Frame != nil || m.Appear != create || placements != 1 {
		t.Fatal("immediate placement did not cancel only the frame transition")
	}
}
