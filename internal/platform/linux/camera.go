//go:build linux && !android

package linux

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
	xdraw "golang.org/x/image/draw"

	"github.com/nsgo-dev/nsgo/desktop/locales"
	"github.com/nsgo-dev/nsgo/internal/driver"
)

// Taking a photo on Linux: XDG Desktop Portal's org.freedesktop.portal.Camera
// (IsCameraPresent, AccessCamera, OpenPipeWireRemote) paired with a
// GStreamer pipewiresrc pipeline shown in a modal GTK 4 camera sheet with
// Shutter and Cancel buttons. When no portal, camera, or PipeWire/GStreamer
// element is available, TakePhoto fails cleanly with driver.NoCamera.

const (
	cameraPortalIface = "org.freedesktop.portal.Camera"
	dbusPropsIface    = "org.freedesktop.DBus.Properties"

	cameraSheetWidth  = 560
	cameraSheetHeight = 440

	// gstStateNull and gstStatePlaying are defined in sound.go.
	gstFlowOK     = 0
	gdkMemoryRGBA = 5 // GDK_MEMORY_R8G8B8A8
)

var cameraPortal struct {
	once    sync.Once
	present atomic.Bool

	callWithFDList func(conn uintptr, name, path, iface, method *byte, params, replyType uintptr,
		flags uint32, timeout int32, fdList uintptr, outFDList *uintptr, cancellable uintptr, gerr *uintptr) uintptr
	unixFDListGet    func(list uintptr, index int32, gerr *uintptr) int32
	variantGetBool   func(v uintptr) int32
	variantGetHandle func(v uintptr) int32
	cbResponse       uintptr
}

var cameraUI struct {
	parseLaunch   func(desc *byte, gerr *uintptr) uintptr
	binGetByName  func(bin uintptr, name *byte) uintptr
	setState      func(elem uintptr, state int32) int32
	gstUnref      func(obj uintptr)
	miniUnref     func(obj uintptr)
	sampleCaps    func(sample uintptr) uintptr
	sampleBuffer  func(sample uintptr) uintptr
	capsStructure func(caps uintptr, idx uint32) uintptr
	structureInt  func(s uintptr, field *byte, out *int32) int32
	extractDup    func(buf, offset, size uintptr, dest, destSize *uintptr)
	pullSample    func(sink uintptr, timeout uint64) uintptr
	pullPreroll   func(sink uintptr, timeout uint64) uintptr
	videoMeta     func(buf uintptr) uintptr

	buttonNewWithLabel func(label *byte) uintptr
	windowSetTransient func(win, parent uintptr)
	windowSetModal     func(win uintptr, modal int32)
	setMarginTop       func(widget uintptr, margin int32)
	setMarginBottom    func(widget uintptr, margin int32)
	setMarginStart     func(widget uintptr, margin int32)
	setMarginEnd       func(widget uintptr, margin int32)
	pictureNew         func() uintptr
	pictureSetPaint    func(pic, paintable uintptr)
	pictureSetFit      func(pic uintptr, fit int32)
	pictureSetShrink   func(pic uintptr, shrink int32)
	memoryTextureNew   func(w, h, format int32, bytes uintptr, stride uintptr) uintptr
	takeBytes          func(data, size uintptr) uintptr

	cbSampleReady    uintptr
	cbDrawSample     uintptr
	cbCaptureClicked uintptr
	cbCancelClicked  uintptr
	cbCloseRequest   uintptr
}

func bindCameraPortal() {
	cameraPortal.once.Do(func() {
		bindGlobalHotKeys()
		purego.RegisterLibFunc(&cameraPortal.callWithFDList, gioLib, "g_dbus_connection_call_with_unix_fd_list_sync")
		purego.RegisterLibFunc(&cameraPortal.unixFDListGet, gioLib, "g_unix_fd_list_get")
		purego.RegisterLibFunc(&cameraPortal.variantGetBool, glibLib, "g_variant_get_boolean")
		purego.RegisterLibFunc(&cameraPortal.variantGetHandle, glibLib, "g_variant_get_handle")
		cameraPortal.cbResponse = newCallback(cameraPortalResponse)
	})
}

var bindCameraPreview = sync.OnceValue(func() bool {
	if !gstFeature("pipewiresrc") || !gstFeature("videoconvert") || !gstFeature("appsink") {
		return false
	}
	loadFirst := func(names []string, symbols map[string]any, optional bool) bool {
		for _, libName := range names {
			h, err := purego.Dlopen(libName, purego.RTLD_NOW|purego.RTLD_LOCAL)
			if err != nil {
				continue
			}
			for sym, fn := range symbols {
				p, err := purego.Dlsym(h, sym)
				if err != nil {
					if optional {
						return true
					}
					return false
				}
				purego.RegisterFunc(fn, p)
			}
			return true
		}
		return optional
	}
	if !loadFirst([]string{"libgstreamer-1.0.so.0", "libgstreamer-1.0.so"}, map[string]any{
		"gst_parse_launch":       &cameraUI.parseLaunch,
		"gst_bin_get_by_name":    &cameraUI.binGetByName,
		"gst_element_set_state":  &cameraUI.setState,
		"gst_object_unref":       &cameraUI.gstUnref,
		"gst_mini_object_unref":  &cameraUI.miniUnref,
		"gst_sample_get_caps":    &cameraUI.sampleCaps,
		"gst_sample_get_buffer":  &cameraUI.sampleBuffer,
		"gst_caps_get_structure": &cameraUI.capsStructure,
		"gst_structure_get_int":  &cameraUI.structureInt,
		"gst_buffer_extract_dup": &cameraUI.extractDup,
	}, false) {
		return false
	}
	if !loadFirst([]string{"libgstapp-1.0.so.0", "libgstapp-1.0.so"}, map[string]any{
		"gst_app_sink_try_pull_sample":  &cameraUI.pullSample,
		"gst_app_sink_try_pull_preroll": &cameraUI.pullPreroll,
	}, false) {
		return false
	}
	loadFirst([]string{"libgstvideo-1.0.so.0", "libgstvideo-1.0.so"}, map[string]any{
		"gst_buffer_get_video_meta": &cameraUI.videoMeta,
	}, true)

	for name, fn := range map[string]any{
		"gtk_button_new_with_label":    &cameraUI.buttonNewWithLabel,
		"gtk_window_set_transient_for": &cameraUI.windowSetTransient,
		"gtk_window_set_modal":         &cameraUI.windowSetModal,
		"gtk_widget_set_margin_top":    &cameraUI.setMarginTop,
		"gtk_widget_set_margin_bottom": &cameraUI.setMarginBottom,
		"gtk_widget_set_margin_start":  &cameraUI.setMarginStart,
		"gtk_widget_set_margin_end":    &cameraUI.setMarginEnd,
		"gtk_picture_new":              &cameraUI.pictureNew,
		"gtk_picture_set_paintable":    &cameraUI.pictureSetPaint,
		"gtk_picture_set_content_fit":  &cameraUI.pictureSetFit,
		"gtk_picture_set_can_shrink":   &cameraUI.pictureSetShrink,
		"gdk_memory_texture_new":       &cameraUI.memoryTextureNew,
	} {
		purego.RegisterLibFunc(fn, gtkLib, name)
	}
	purego.RegisterLibFunc(&cameraUI.takeBytes, glibLib, "g_bytes_new_take")

	cameraUI.cbSampleReady = newCallback(cameraSampleReady)
	cameraUI.cbDrawSample = newCallback(cameraDrawSample)
	cameraUI.cbCaptureClicked = newCallback(cameraCaptureClicked)
	cameraUI.cbCancelClicked = newCallback(cameraCancelClicked)
	cameraUI.cbCloseRequest = newCallback(cameraCloseRequest)
	return true
})

// cameraAvailable reports whether the desktop portal exposes a camera
// and GStreamer can stream from PipeWire.
var cameraAvailable = func() bool {
	return cameraPortal.present.Load()
}

// probeCamera queries org.freedesktop.portal.Camera's IsCameraPresent
// off the GTK thread and updates the window's environment once known.
func (w *window) probeCamera() {
	if !bindCameraPreview() {
		return
	}
	bindCameraPortal()
	appID := w.opts.ID
	portalQueue() <- func() {
		present := portalIsCameraPresent(portalBus(appID))
		if cameraPortal.present.Swap(present) != present {
			w.post(func() {
				if !w.closing {
					w.updateEnvironment()
				}
			})
		}
	}
}

func portalIsCameraPresent(bus uintptr) bool {
	if bus == 0 {
		return false
	}
	reply := portalCall(bus, globalShortcutPath, dbusPropsIface, "Get",
		gVariantNewString(cstr(cameraPortalIface)), gVariantNewString(cstr("IsCameraPresent")))
	if reply == 0 {
		return false
	}
	defer gVariantUnref(reply)
	if gVariantNChildren(reply) < 1 {
		return false
	}
	outer := gVariantGetChildValue(reply, 0)
	defer gVariantUnref(outer)
	val := outer
	for val != 0 && goString(gVariantGetTypeString(val)) == "v" {
		inner := gVariantGetVariant(val)
		if val != outer {
			gVariantUnref(val)
		}
		val = inner
	}
	if val == 0 {
		return false
	}
	defer func() {
		if val != outer {
			gVariantUnref(val)
		}
	}()
	if goString(gVariantGetTypeString(val)) != "b" {
		return false
	}
	return cameraPortal.variantGetBool(val) != 0
}

// cameraRequest tracks one in-flight TakePhoto request (portal access
// and optional GTK preview sheet).
type cameraRequest struct {
	key        uintptr
	w          *window
	id         uint32
	r          driver.PickRequest
	bus        uintptr
	request    string
	requestSub uint32

	win        uintptr
	picture    uintptr
	captureBtn uintptr
	pipeline   uintptr
	sink       uintptr
	pipewireFD int

	framePending atomic.Bool
	frameRGBA    []byte
	frameW       int
	frameH       int
	frameStride  int

	finished bool
	quiet    bool
}

var (
	cameraRequests   = map[uintptr]*cameraRequest{}
	cameraSamples    sync.Map // key -> *cameraRequest (accessed from GStreamer streaming thread)
	nextCameraReqKey atomic.Uintptr
)

// cameraFlowDeps lets unit tests exercise TakePhoto's availability and
// portal response handling without a live D-Bus or PipeWire daemon.
type cameraFlowDeps struct {
	hasPreview  func() bool
	startPortal func(driver.PickRequest, uint32)
}

func (w *window) takePhoto(r driver.PickRequest, id uint32) {
	if w.closing || w.engine == nil {
		return
	}
	fail := func(why string) {
		if !w.closing && w.engine != nil {
			w.engine.Dispatch(driver.Event{Kind: driver.EventPick, Count: id, Text: why})
		}
	}
	runTakePhotoFlow(r, id, fail, cameraFlowDeps{
		hasPreview:  bindCameraPreview,
		startPortal: func(req driver.PickRequest, reqID uint32) { w.startPortalCamera(req, reqID) },
	})
}

func runTakePhotoFlow(r driver.PickRequest, id uint32, fail func(why string), deps cameraFlowDeps) {
	if !deps.hasPreview() {
		fail(driver.NoCamera)
		return
	}
	deps.startPortal(r, id)
}

func (w *window) startPortalCamera(r driver.PickRequest, id uint32) {
	bindCameraPortal()
	key := nextCameraReqKey.Add(1)
	req := &cameraRequest{
		key:        key,
		w:          w,
		id:         id,
		r:          r,
		pipewireFD: -1,
	}
	cameraRequests[key] = req
	appID := w.opts.ID
	portalQueue() <- func() {
		bus := portalBus(appID)
		if bus == 0 || !portalIsCameraPresent(bus) {
			cameraPortal.present.Store(false)
			w.post(func() { req.fail(driver.NoCamera) })
			return
		}
		cameraPortal.present.Store(true)
		w.post(func() {
			if req.finished || w.closing {
				return
			}
			req.bus = bus
			token := portalToken("nsgo_camera")
			req.request = portalRequestPath(bus, token)
			if req.request == "" {
				req.fail(driver.NoCamera)
				return
			}
			req.requestSub = gDBusConnectionSignalSubscribe(bus, cstr(globalShortcutName), cstr(globalRequestIface), cstr("Response"),
				cstr(req.request), nil, 0, cameraPortal.cbResponse, req.key, 0)
			if req.requestSub == 0 {
				req.fail(driver.NoCamera)
				return
			}
			awaited := req.request
			portalQueue() <- func() {
				reply := portalCall(bus, globalShortcutPath, cameraPortalIface, "AccessCamera", portalOptions(token))
				actual := ""
				if reply != 0 {
					actual = portalTupleText(reply, 0)
					gVariantUnref(reply)
				}
				if actual == awaited {
					return
				}
				w.post(func() {
					if req.finished || req.request != awaited {
						return
					}
					if req.requestSub != 0 {
						gDBusConnectionSignalUnsubscribe(req.bus, req.requestSub)
						req.requestSub = 0
					}
					if actual == "" {
						req.fail(driver.NoCamera)
						return
					}
					req.request = actual
					req.requestSub = gDBusConnectionSignalSubscribe(req.bus, cstr(globalShortcutName), cstr(globalRequestIface), cstr("Response"),
						cstr(req.request), nil, 0, cameraPortal.cbResponse, req.key, 0)
					if req.requestSub == 0 {
						req.fail(driver.NoCamera)
					}
				})
			}
		})
	}
}

// cameraPortalResponse handles org.freedesktop.portal.Request.Response
// for AccessCamera on the GTK main loop.
func cameraPortalResponse(_, _, path, _, _, params, data uintptr) {
	req := cameraRequests[data]
	if req == nil || req.finished || goString(path) != req.request {
		return
	}
	if req.requestSub != 0 {
		gDBusConnectionSignalUnsubscribe(req.bus, req.requestSub)
		req.requestSub = 0
	}
	if gVariantNChildren(params) < 1 {
		req.fail("the camera cannot be used")
		return
	}
	code := portalTupleUint32(params, 0)
	if errMsg := portalCameraResponseError(code); errMsg != "" {
		req.fail(errMsg)
		return
	}
	bus := req.bus
	portalQueue() <- func() {
		fd, errMsg := openPipeWireRemote(bus)
		req.w.post(func() {
			if req.finished || req.w.closing {
				if fd >= 0 {
					_ = syscall.Close(fd)
				}
				return
			}
			if errMsg != "" || fd < 0 {
				if errMsg == "" {
					errMsg = driver.NoCamera
				}
				req.fail(errMsg)
				return
			}
			req.pipewireFD = fd
			req.presentSheet()
		})
	}
}

// portalCameraResponseError maps XDG Desktop Portal Response codes for
// AccessCamera into a failure message ("" when granted).
func portalCameraResponseError(code uint32) string {
	switch code {
	case 0:
		return ""
	case 1:
		return "camera access denied"
	default:
		return "the camera cannot be used"
	}
}

// openPipeWireRemote calls org.freedesktop.portal.Camera.OpenPipeWireRemote
// and extracts the PipeWire socket file descriptor.
func openPipeWireRemote(bus uintptr) (int, string) {
	opts := portalVariant("a{sv}", "{}")
	if opts == 0 {
		return -1, "the camera cannot be used"
	}
	args := [1]uintptr{gVariantTakeRef(opts)}
	params := gVariantNewTuple(&args[0], 1)
	gVariantUnref(args[0])
	var outFDList, gerr uintptr
	reply := cameraPortal.callWithFDList(bus, cstr(globalShortcutName), cstr(globalShortcutPath), cstr(cameraPortalIface),
		cstr("OpenPipeWireRemote"), params, 0, 0, -1, 0, &outFDList, 0, &gerr)
	if gerr != 0 {
		gErrorFree(gerr)
		if outFDList != 0 {
			gObjectUnref(outFDList)
		}
		if reply != 0 {
			gVariantUnref(reply)
		}
		return -1, driver.NoCamera
	}
	if reply == 0 || outFDList == 0 {
		if outFDList != 0 {
			gObjectUnref(outFDList)
		}
		if reply != 0 {
			gVariantUnref(reply)
		}
		return -1, driver.NoCamera
	}
	defer gVariantUnref(reply)
	defer gObjectUnref(outFDList)
	if gVariantNChildren(reply) < 1 {
		return -1, driver.NoCamera
	}
	child := gVariantGetChildValue(reply, 0)
	defer gVariantUnref(child)
	handleIdx := cameraPortal.variantGetHandle(child)
	var fdErr uintptr
	fd := cameraPortal.unixFDListGet(outFDList, handleIdx, &fdErr)
	if fdErr != 0 {
		gErrorFree(fdErr)
		return -1, "the camera cannot be used"
	}
	if fd < 0 {
		return -1, driver.NoCamera
	}
	return int(fd), ""
}

// cameraPipelineDescription builds the GStreamer pipeline reading a
// PipeWire remote fd into an RGBA appsink.
func cameraPipelineDescription(fd int) string {
	return fmt.Sprintf("pipewiresrc fd=%d ! videoconvert ! video/x-raw,format=RGBA ! appsink name=sink emit-signals=true max-buffers=1 drop=true", fd)
}

// presentSheet opens a modal GTK 4 window displaying the PipeWire camera
// stream with Shutter and Cancel buttons.
func (req *cameraRequest) presentSheet() {
	var gerr uintptr
	pipeline := cameraUI.parseLaunch(cstr(cameraPipelineDescription(req.pipewireFD)), &gerr)
	if gerr != 0 {
		gErrorFree(gerr)
	}
	if pipeline == 0 {
		req.fail("the camera cannot be used")
		return
	}
	sink := cameraUI.binGetByName(pipeline, cstr("sink"))
	if sink == 0 {
		cameraUI.gstUnref(pipeline)
		req.fail("the camera cannot be used")
		return
	}
	req.pipeline = pipeline
	req.sink = sink
	cameraSamples.Store(req.key, req)
	gSignalConnect(sink, "new-sample", cameraUI.cbSampleReady, req.key)
	gSignalConnect(sink, "new-preroll", cameraUI.cbSampleReady, req.key)

	msgs := locales.In(req.w.language)
	win := gtkWindowNew()
	gtkWindowSetTitle(win, cstr(msgs.TakePhoto()))
	gtkWindowSetDefaultSize(win, cameraSheetWidth, cameraSheetHeight)
	if top := req.w.toplevel(); top != 0 {
		cameraUI.windowSetTransient(win, top)
		cameraUI.windowSetModal(win, 1)
	}

	const (
		gtkOrientHorizontal  = 0
		gtkOrientVertical    = 1
		gtkAlignEnd          = 2
		gtkContentFitContain = 1
	)
	rootBox := gtkBoxNew(gtkOrientVertical, 0)
	picture := cameraUI.pictureNew()
	cameraUI.pictureSetShrink(picture, 1)
	cameraUI.pictureSetFit(picture, gtkContentFitContain)
	gtkWidgetSetHexpand(picture, 1)
	gtkWidgetSetVexpand(picture, 1)
	gtkBoxAppend(rootBox, picture)

	bar := gtkBoxNew(gtkOrientHorizontal, 8)
	gtkWidgetSetHalign(bar, gtkAlignEnd)
	cameraUI.setMarginTop(bar, 12)
	cameraUI.setMarginBottom(bar, 12)
	cameraUI.setMarginStart(bar, 16)
	cameraUI.setMarginEnd(bar, 16)

	cancelBtn := cameraUI.buttonNewWithLabel(cstr(msgs.Cancel()))
	captureBtn := cameraUI.buttonNewWithLabel(cstr(msgs.TakePhoto()))
	// Enable the shutter once the first camera frame arrives.
	gtkWidgetSetSensitive(captureBtn, 0)

	gSignalConnect(cancelBtn, "clicked", cameraUI.cbCancelClicked, req.key)
	gSignalConnect(captureBtn, "clicked", cameraUI.cbCaptureClicked, req.key)
	gSignalConnect(win, "close-request", cameraUI.cbCloseRequest, req.key)

	gtkBoxAppend(bar, cancelBtn)
	gtkBoxAppend(bar, captureBtn)
	gtkBoxAppend(rootBox, bar)
	gtkWindowSetChild(win, rootBox)

	req.win = win
	req.picture = picture
	req.captureBtn = captureBtn

	cameraUI.setState(pipeline, gstStatePlaying)
	gtkWindowPresent(win)
}

func cameraSampleReady(_, key uintptr) int32 {
	v, ok := cameraSamples.Load(key)
	if !ok {
		return gstFlowOK
	}
	req := v.(*cameraRequest)
	if req.framePending.CompareAndSwap(false, true) {
		gIdleAdd(cameraUI.cbDrawSample, key)
	}
	return gstFlowOK
}

func cameraDrawSample(key uintptr) int32 {
	req := cameraRequests[key]
	if req == nil || req.finished || req.sink == 0 {
		return 0
	}
	req.framePending.Store(false)
	sample := cameraUI.pullSample(req.sink, 0)
	if sample == 0 {
		sample = cameraUI.pullPreroll(req.sink, 0)
	}
	if sample == 0 {
		return 0
	}
	defer cameraUI.miniUnref(sample)
	req.applySample(sample)
	return 0
}

func (req *cameraRequest) applySample(sample uintptr) {
	caps, buf := cameraUI.sampleCaps(sample), cameraUI.sampleBuffer(sample)
	if caps == 0 || buf == 0 {
		return
	}
	s := cameraUI.capsStructure(caps, 0)
	var width, height int32
	if cameraUI.structureInt(s, cstr("width"), &width) == 0 || cameraUI.structureInt(s, cstr("height"), &height) == 0 ||
		width <= 0 || height <= 0 {
		return
	}
	offset, stride := uintptr(0), uintptr(width)*4
	if cameraUI.videoMeta != nil {
		if meta := cameraUI.videoMeta(buf); meta != 0 {
			plane := (*gstVideoPlane)(cPointer(meta))
			if int64(plane.stride[0]) < int64(width)*4 {
				return
			}
			offset, stride = plane.offset[0], uintptr(plane.stride[0])
		}
	}
	size := stride*uintptr(height-1) + uintptr(width)*4
	var pixels, copied uintptr
	cameraUI.extractDup(buf, offset, size, &pixels, &copied)
	if copied != size || pixels == 0 {
		if pixels != 0 {
			gFree(pixels)
		}
		return
	}
	raw := make([]byte, copied)
	copy(raw, unsafe.Slice((*byte)(cPointer(pixels)), copied))

	gbytes := cameraUI.takeBytes(pixels, copied)
	texture := cameraUI.memoryTextureNew(width, height, gdkMemoryRGBA, gbytes, stride)
	gBytesUnref(gbytes)
	if texture != 0 {
		cameraUI.pictureSetPaint(req.picture, texture)
		gObjectUnref(texture)
	}

	firstFrame := len(req.frameRGBA) == 0
	req.frameRGBA, req.frameW, req.frameH, req.frameStride = raw, int(width), int(height), int(stride)
	if firstFrame && req.captureBtn != 0 {
		gtkWidgetSetSensitive(req.captureBtn, 1)
	}
}

func cameraCaptureClicked(_, key uintptr) {
	req := cameraRequests[key]
	if req == nil || req.finished || len(req.frameRGBA) == 0 {
		return
	}
	rgba, w, h, stride := req.frameRGBA, req.frameW, req.frameH, req.frameStride
	pickReq, id, eng := req.r, req.id, req.w.engine
	req.cleanup()
	if eng == nil {
		return
	}
	go func() {
		f, err := encodeCameraFrame(pickReq, rgba, w, h, stride)
		if err != nil {
			eng.Dispatch(driver.Event{Kind: driver.EventPick, Count: id, Text: err.Error()})
			return
		}
		eng.Dispatch(driver.Event{Kind: driver.EventPick, Count: id, Data: []driver.PickedFile{f}})
	}()
}

func cameraCancelClicked(_, key uintptr) {
	if req := cameraRequests[key]; req != nil {
		req.cancel()
	}
}

func cameraCloseRequest(_, key uintptr) int32 {
	if req := cameraRequests[key]; req != nil {
		req.win = 0 // GTK destroys the window when close-request returns 0
		req.cancel()
	}
	return 0
}

func (req *cameraRequest) fail(why string) {
	if req.finished {
		return
	}
	eng, id, quiet := req.w.engine, req.id, req.quiet
	closing := req.w.closing
	req.cleanup()
	if !quiet && !closing && eng != nil {
		eng.Dispatch(driver.Event{Kind: driver.EventPick, Count: id, Text: why})
	}
}

func (req *cameraRequest) cancel() {
	if req.finished {
		return
	}
	eng, id, quiet := req.w.engine, req.id, req.quiet
	closing := req.w.closing
	req.cleanup()
	if !quiet && !closing && eng != nil {
		eng.Dispatch(driver.Event{Kind: driver.EventPick, Count: id})
	}
}

func (req *cameraRequest) cleanup() {
	if req.finished {
		return
	}
	req.finished = true
	delete(cameraRequests, req.key)
	cameraSamples.Delete(req.key)
	if req.requestSub != 0 {
		gDBusConnectionSignalUnsubscribe(req.bus, req.requestSub)
		req.requestSub = 0
	}
	if req.pipeline != 0 {
		cameraUI.setState(req.pipeline, gstStateNull)
		if req.sink != 0 {
			cameraUI.gstUnref(req.sink)
			req.sink = 0
		}
		cameraUI.gstUnref(req.pipeline)
		req.pipeline = 0
	}
	if req.pipewireFD >= 0 {
		_ = syscall.Close(req.pipewireFD)
		req.pipewireFD = -1
	}
	if req.win != 0 {
		win := req.win
		req.win = 0
		gtkWindowDestroy(win)
	}
}

// closeCameras silently closes any open camera sheet belonging to w.
func (w *window) closeCameras() {
	for _, req := range cameraRequests {
		if req.w == w {
			req.quiet = true
			req.cleanup()
		}
	}
}

// photoPickRequest normalizes r for TakePhoto: single image, compatible
// format, explicit JPEG quality.
func photoPickRequest(r driver.PickRequest) driver.PickRequest {
	r.Limit = 1
	r.Format = driver.PickCompatible
	r.Quality = r.JPEGQuality()
	return r
}

// encodeCameraFrame converts a captured RGBA frame (with optional row
// stride padding) into a scaled JPEG PickedFile honoring r.
func encodeCameraFrame(r driver.PickRequest, rgba []byte, width, height, stride int) (driver.PickedFile, error) {
	if width <= 0 || height <= 0 || stride < width*4 {
		return driver.PickedFile{}, errors.New("captured photo had invalid dimensions")
	}
	minBytes := stride*(height-1) + width*4
	if len(rgba) < minBytes {
		return driver.PickedFile{}, errors.New("captured photo had truncated pixel data")
	}
	src := image.NewRGBA(image.Rect(0, 0, width, height))
	rowBytes := width * 4
	for y := range height {
		copy(src.Pix[y*src.Stride:y*src.Stride+rowBytes], rgba[y*stride:y*stride+rowBytes])
	}
	req := photoPickRequest(r)
	var outImg image.Image = src
	target := req.ScaledSize(driver.Size{Width: driver.Points(width), Height: driver.Points(height)})
	if tw, th := int(target.Width), int(target.Height); tw != width || th != height {
		dst := image.NewRGBA(image.Rect(0, 0, tw, th))
		xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Src, nil)
		outImg = dst
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, outImg, &jpeg.Options{Quality: int(req.JPEGQuality()*100 + 0.5)}); err != nil {
		return driver.PickedFile{}, err
	}
	files := driver.FinishPick(req, []driver.PickedFile{{
		Name: "photo.jpg",
		MIME: "image/jpeg",
		Data: buf.Bytes(),
		Size: target,
	}}, nil)
	if len(files) == 0 {
		return driver.PickedFile{}, errors.New("captured photo could not be prepared")
	}
	return files[0], nil
}
