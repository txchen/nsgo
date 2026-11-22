//go:build windows

package windows

import (
	"testing"
	"time"
	"unsafe"

	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/internal/driver"
)

// NoShadow and its command must reach DWM for framed windows too, and restoring
// the shadow must retain the frame and the HWND.
func TestWindowShadowControlsDWM(t *testing.T) {
	if testing.Short() {
		t.Skip("opens a window")
	}
	for _, initial := range []bool{false, true} {
		name := "changed"
		if initial {
			name = "opened"
		}
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			hwnd := startAppWith(t, func(*core.Scope) core.Node {
				close(started)
				return core.View{}
			},
				Options{Title: "nsgo-shadow-" + name, Width: 180, Height: 100, NoShadow: initial})
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("the app did not start")
			}
			onMain(t, hwnd, func(w *window) {
				if !initial {
					w.windowCommand(&driver.WindowCommand{Action: driver.WindowSetShadow, On: false})
				}
				enabled := int32(-1)
				get := dwmapi.NewProc("DwmGetWindowAttribute")
				readEnabled := func() {
					t.Helper()
					const dwmwaNCRenderingEnabled = 1
					if hr, _, _ := get.Call(hwnd, dwmwaNCRenderingEnabled, uintptr(unsafe.Pointer(&enabled)), 4); failed(hr) {
						t.Errorf("DwmGetWindowAttribute: %#x", hr)
					}
				}
				readEnabled()
				if enabled != 0 {
					t.Errorf("shadowless non-client rendering = %d, want disabled", enabled)
				}
				w.windowCommand(&driver.WindowCommand{Action: driver.WindowSetShadow, On: true})
				readEnabled()
				if enabled == 0 {
					t.Error("restored frame has non-client rendering disabled")
				}
				style, _, _ := getWindowLongPtrW.Call(hwnd, gwlStyle)
				if w.hwnd != hwnd || style&wsCaption != wsCaption || w.transparent {
					t.Error("changing the shadow replaced the window or removed its frame")
				}
			})
		})
	}
}
