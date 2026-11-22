//go:build windows

package windows

import "unsafe"

// A transparent window is a popup without a frame, as NSH's is: a system frame around see-through content would draw a box nothing is in, and its system menu stays for the taskbar's menu. It draws through DirectComposition with alpha, as a window with a material does (material.go), clearing to nothing where the app draws nothing, and DWM draws it bare: no shadow and square corners. Only a window opened transparent has the composition, which WS_EX_NOREDIRECTIONBITMAP holds from the window's creation, so only such a window changes between transparent and framed later, clearing to white while framed, and only its environment reports CanMakeWindowsTransparent. NoShadow also disables DWM's non-client rendering on a framed window, keeping its frame style and default corner preference. A transparent panel (SurfaceConfig.Transparent) opens the same way, a popup whose content draws its own corners and shadow, and stays so; without DirectComposition it stays opaque, as its environment's WindowTransparent says.
//
// A window off the taskbar is a tool window, which neither the taskbar nor Alt+Tab lists; a framed one then shows a tool window's smaller caption. The taskbar adds or deletes the button of a window that shows as the style changes (ITaskbarList).

const (
	dwmwaNCRenderingPolicy      = 2
	dwmwaWindowCornerPreference = 33
	dwmNCRPUseWindowStyle       = 0
	dwmNCRPDisabled             = 1
	dwmCornerDefault            = 0
	dwmCornerDoNotRound         = 1

	taskbarAddTab    = 4
	taskbarDeleteTab = 5
)

// composeTransparent has a window opening transparent draw through DirectComposition, where the system has it; else it opens framed and opaque, and its environment says so (CanMakeWindowsTransparent).
func (w *window) composeTransparent() {
	if !compositionAvailable() {
		return
	}
	w.baseComposed, w.transparent, w.canTransparent = true, true, true
	if w.backdrop == 0 {
		w.backdrop = backdropNone
	}
}

// frameStyle is current with a transparent window's frame bits, the popup's, or a framed window's within its constraints.
func (w *window) frameStyle(current uintptr) uintptr {
	current &^= wsOverlappedWindow | wsPopup
	if !w.transparent {
		return constrainedStyle(current|wsOverlappedWindow, w.chrome.constraints)
	}
	style := current | wsPopup | wsSysMenu | wsMinimizeBox
	if w.chrome.constraints.NoMinimize {
		style &^= wsMinimizeBox
	}
	return style
}

// applyBare disables DWM's non-client rendering for transparent or shadowless
// windows, and also removes a transparent window's rounded corners.
func (w *window) applyBare() {
	policy, corners := int32(dwmNCRPUseWindowStyle), int32(dwmCornerDefault)
	if w.transparent || w.opts.NoShadow {
		policy = dwmNCRPDisabled
	}
	if w.transparent {
		corners = dwmCornerDoNotRound
	}
	// Each fails quietly where the system lacks it.
	dwmSetWindowAttribute.Call(w.hwnd, dwmwaNCRenderingPolicy, uintptr(unsafe.Pointer(&policy)), 4)
	dwmSetWindowAttribute.Call(w.hwnd, dwmwaWindowCornerPreference, uintptr(unsafe.Pointer(&corners)), 4)
}

// setShadow keeps the non-client shadow, or asks DWM to stop rendering it.
func (w *window) setShadow(on bool) {
	w.opts.NoShadow = !on
	w.applyBare()
	w.frameChanged()
}

// setTransparent changes a window opened transparent between transparent and framed, keeping its content's size and place; others stay as they are.
func (w *window) setTransparent(on bool) {
	if !w.canTransparent || on == w.transparent {
		return
	}
	w.transparent = on
	w.chrome.hover, w.chrome.pressed = 0, 0
	w.applyBare()
	if w.chrome.fullScreen {
		w.chrome.savedStyle = w.frameStyle(w.chrome.savedStyle)
		w.chromeChanged()
		return
	}
	var client rect
	getClientRect.Call(w.hwnd, uintptr(unsafe.Pointer(&client)))
	at := point{}
	clientToScreen.Call(w.hwnd, uintptr(unsafe.Pointer(&at)))
	current, _, _ := getWindowLongPtrW.Call(w.hwnd, gwlStyle)
	style := w.frameStyle(current)
	setWindowLongPtrW.Call(w.hwnd, gwlStyle, style)
	w.frameChanged()
	if !w.zoomed() {
		// What the new frame takes is measured, as placeNew does, rather than AdjustWindowRect's: that misses the hidden title bar's caption in the client area (ncCalcSize), a tool window's smaller caption and the monitor's DPI.
		var outer, inner rect
		getWindowRect.Call(w.hwnd, uintptr(unsafe.Pointer(&outer)))
		getClientRect.Call(w.hwnd, uintptr(unsafe.Pointer(&inner)))
		origin := point{}
		clientToScreen.Call(w.hwnd, uintptr(unsafe.Pointer(&origin)))
		cx := outer.Right - outer.Left - inner.Right + client.Right
		cy := outer.Bottom - outer.Top - inner.Bottom + client.Bottom
		x, y := at.X-(origin.X-outer.Left), at.Y-(origin.Y-outer.Top)
		setWindowPos.Call(w.hwnd, 0, uintptr(x), uintptr(y), uintptr(cx), uintptr(cy), swpNoZOrder|swpNoActivate)
	}
	w.chromeChanged()
}

// setSkipTaskbar makes the window a tool window, off the taskbar and Alt+Tab, or an app window again.
func (w *window) setSkipTaskbar(on bool) {
	ex, _, _ := getWindowLongPtrW.Call(w.hwnd, gwlExStyle)
	next := ex &^ wsExToolWindow
	if on {
		next |= wsExToolWindow
	}
	if next == ex {
		return
	}
	setWindowLongPtrW.Call(w.hwnd, gwlExStyle, next)
	w.frameChanged()
	if visible, _, _ := isWindowVisible.Call(w.hwnd); visible == 0 {
		return
	}
	if list := taskbar(); list != nil {
		if on {
			list.call(taskbarDeleteTab, w.hwnd)
		} else {
			list.call(taskbarAddTab, w.hwnd)
		}
	}
}
