//go:build darwin && !ios

package apple

import (
	"testing"
	"time"

	"github.com/ebitengine/purego/objc"

	"github.com/nsgo-dev/nsgo/internal/driver"
)

func TestLayoutGhostFadesAndLeavesItsSuperview(t *testing.T) {
	onMain(func() {
		makeView := func() objc.ID { return objc.ID(Class("NSView")).Send(Sel("new")) }
		parent, deleted, descendant := makeView(), makeView(), makeView()
		defer parent.Send(layoutRelease)
		defer deleted.Send(layoutRelease)
		defer descendant.Send(layoutRelease)
		parent.Send(Sel("addSubview:"), deleted)
		deleted.Send(Sel("addSubview:"), descendant)
		deleted.Send(Sel("setWantsLayer:"), true)
		layer := deleted.Send(layoutLayer)
		layer.Send(layoutSetOpacity, float32(0.6))

		deleted.Send(Sel("retain"))
		transition := &driver.LayoutTransition{Duration: 100 * time.Millisecond}
		ghosts := []*LayoutGhost{NewLayoutGhost(deleted, transition, 0)}
		if !InLayoutGhost(ghosts, descendant) {
			t.Error("the retained exit did not keep its descendants")
		}
		ghosts = StepLayoutMotions[int](50*time.Millisecond, nil, ghosts, nil, nil, nil)
		if got := objc.Send[float32](layer, layoutOpacity); len(ghosts) != 1 || got < 0.299 || got > 0.301 {
			t.Errorf("midpoint has %d exits and opacity %v, want one exit at 0.3", len(ghosts), got)
		}
		ghosts = StepLayoutMotions[int](100*time.Millisecond, nil, ghosts, nil, nil, nil)
		if len(ghosts) != 0 || deleted.Send(layoutSuperview) != 0 {
			t.Error("the completed exit is still attached")
		}
	})
}
