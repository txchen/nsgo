//go:build darwin && !ios

package macos

import (
	"errors"
	"log"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"github.com/ebitengine/purego/objc"

	"github.com/nsgo-dev/nsgo/desktop/locales"
	"github.com/nsgo-dev/nsgo/internal/driver"
	"github.com/nsgo-dev/nsgo/internal/platform/apple"
	"github.com/nsgo-dev/nsgo/internal/platform/privacy"
)

// Taking a photo on macOS: an AVCaptureSession of the default video
// capture device (built-in, external or Continuity Camera iPhone) shown
// in an AVCaptureVideoPreviewLayer inside an AppKit sheet with Shutter
// and Cancel buttons, after checking camera availability and TCC camera
// permission through privacy.

const (
	avFoundationPath = "/System/Library/Frameworks/AVFoundation.framework/AVFoundation"

	cameraSheetWidth  = 560
	cameraSheetHeight = 440
	cameraBarHeight   = 56

	cameraWindowTitled = 1 << 0 // NSWindowStyleMaskTitled
)

var (
	avFoundationOnce     sync.Once
	avFoundationLoaded   bool
	avCaptureDeviceTypes []objc.ID

	cameraSheetOnce     sync.Once
	classCameraDelegate objc.Class
	cameraSheets        = map[objc.ID]*cameraSheet{}
)

// cameraSheet holds one active camera sheet on the main thread.
type cameraSheet struct {
	s          *surface
	id         uint32
	r          driver.PickRequest
	parent     objc.ID
	sheet      objc.ID
	captureBtn objc.ID
	session    objc.ID
	output     objc.ID
	capturing  bool
	quiet      bool
	// started closes once startRunning returned, so stopRunning never
	// runs before startRunning finishes and leaves the camera active.
	started chan struct{}
}

// cameraFlowDeps lets unit tests exercise the availability, permission
// and request flow without opening a real capture device or sheet.
type cameraFlowDeps struct {
	hasCamera         func() bool
	declaresUsage     func(string) bool
	permissionStatus  func(string) int
	requestPermission func(string, func(int))
	onMain            func(func())
	present           func(driver.PickRequest, uint32)
}

func loadAVFoundation() bool {
	avFoundationOnce.Do(func() {
		lib, err := purego.Dlopen(avFoundationPath, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return
		}
		for _, sym := range []string{
			"AVCaptureDeviceTypeBuiltInWideAngleCamera",
			"AVCaptureDeviceTypeExternal",
			"AVCaptureDeviceTypeContinuityCamera",
			"AVCaptureDeviceTypeExternalUnknown",
		} {
			if addr, err := purego.Dlsym(lib, sym); err == nil && addr != 0 {
				if v := **(**objc.ID)(unsafe.Pointer(&addr)); v != 0 {
					avCaptureDeviceTypes = append(avCaptureDeviceTypes, v)
				}
			}
		}
		avFoundationLoaded = true
	})
	return avFoundationLoaded
}

// defaultCameraDevice returns macOS's default video capture device,
// including built-in, USB and Continuity Camera devices, or 0 when none
// is attached.
func defaultCameraDevice() objc.ID {
	if !loadAVFoundation() {
		return 0
	}
	cls := apple.Class("AVCaptureDevice")
	if cls == 0 {
		return 0
	}
	if dev := objc.ID(cls).Send(sel("defaultDeviceWithMediaType:"), apple.NSString("vide")); dev != 0 {
		return dev
	}
	discoveryCls := apple.Class("AVCaptureDeviceDiscoverySession")
	if discoveryCls == 0 || len(avCaptureDeviceTypes) == 0 {
		return 0
	}
	types := objc.ID(apple.Class("NSMutableArray")).Send(sel("array"))
	for _, t := range avCaptureDeviceTypes {
		types.Send(sel("addObject:"), t)
	}
	const positionUnspecified = 0
	session := objc.ID(discoveryCls).Send(sel("discoverySessionWithDeviceTypes:mediaType:position:"),
		types, apple.NSString("vide"), positionUnspecified)
	if session == 0 {
		return 0
	}
	devices := session.Send(sel("devices"))
	if devices == 0 || objc.Send[uint](devices, sel("count")) == 0 {
		return 0
	}
	return devices.Send(sel("objectAtIndex:"), uint(0))
}

// cameraAvailable reports whether a video capture device is attached.
var cameraAvailable = func() bool {
	return defaultCameraDevice() != 0
}

// takePhoto checks camera availability and TCC permission, then presents
// the camera sheet or answers with why it cannot.
func (s *surface) takePhoto(r driver.PickRequest, id uint32) {
	fail := func(why string) {
		if !s.closed && s.engine != nil {
			s.engine.Dispatch(driver.Event{Kind: driver.EventPick, Count: id, Text: why})
		}
	}
	runTakePhotoFlow(r, id, fail, cameraFlowDeps{
		hasCamera:         cameraAvailable,
		declaresUsage:     privacy.DeclaresUsage,
		permissionStatus:  privacy.Status,
		requestPermission: privacy.Request,
		onMain:            apple.OnMain,
		present: func(req driver.PickRequest, reqID uint32) {
			if s.closed {
				return
			}
			if why := s.presentCamera(req, reqID); why != "" {
				fail(why)
			}
		},
	})
}

// runTakePhotoFlow validates camera availability and TCC camera access
// before presenting the camera UI.
func runTakePhotoFlow(r driver.PickRequest, id uint32, fail func(why string), deps cameraFlowDeps) {
	if !deps.hasCamera() {
		fail(driver.NoCamera)
		return
	}
	if !deps.declaresUsage("NSCameraUsageDescription") {
		log.Printf("nsgo: taking a photo needs NSCameraUsageDescription in the app's Info.plist (nsgo.yaml's apple.info)")
		fail("the app declares no NSCameraUsageDescription")
		return
	}
	switch deps.permissionStatus(driver.PermissionCamera) {
	case driver.PermissionGranted:
		deps.present(r, id)
	case driver.PermissionNotAsked:
		deps.requestPermission(driver.PermissionCamera, func(status int) {
			deps.onMain(func() {
				switch status {
				case driver.PermissionGranted:
					deps.present(r, id)
				case driver.PermissionUnavailable:
					fail("AVFoundation is missing")
				default:
					fail("camera access denied")
				}
			})
		})
	case driver.PermissionUnavailable:
		fail("AVFoundation is missing")
	default:
		fail("camera access denied")
	}
}

// presentCamera starts an AVCaptureSession with AVCapturePhotoOutput and
// presents a preview sheet on s.window, returning a failure message if
// the session cannot be set up.
func (s *surface) presentCamera(r driver.PickRequest, id uint32) string {
	registerCameraSheet()
	if classCameraDelegate == 0 {
		return "AVFoundation is missing"
	}
	device := defaultCameraDevice()
	if device == 0 {
		return driver.NoCamera
	}
	var errObj objc.ID
	input := objc.ID(apple.Class("AVCaptureDeviceInput")).Send(sel("deviceInputWithDevice:error:"), device, unsafe.Pointer(&errObj))
	session := objc.ID(apple.Class("AVCaptureSession")).Send(selNew)
	if session == 0 {
		return "AVFoundation is missing"
	}
	if input == 0 || !objc.Send[bool](session, sel("canAddInput:"), input) {
		session.Send(selRelease)
		if errObj != 0 {
			if desc := apple.GoString(errObj.Send(sel("localizedDescription"))); desc != "" {
				return desc
			}
		}
		return "the camera cannot be used"
	}
	photoPreset := apple.NSString("AVCaptureSessionPresetPhoto")
	if objc.Send[bool](session, sel("canSetSessionPreset:"), photoPreset) {
		session.Send(sel("setSessionPreset:"), photoPreset)
	}
	session.Send(sel("addInput:"), input)

	output := objc.ID(apple.Class("AVCapturePhotoOutput")).Send(selNew)
	if output == 0 || !objc.Send[bool](session, sel("canAddOutput:"), output) {
		if output != 0 {
			output.Send(selRelease)
		}
		session.Send(selRelease)
		return "the camera cannot take photos"
	}
	session.Send(sel("addOutput:"), output)

	msgs := locales.In(s.language)
	delegate := objc.ID(classCameraDelegate).Send(selNew)

	sheet := objc.ID(apple.Class("NSWindow")).Send(selAlloc).Send(
		sel("initWithContentRect:styleMask:backing:defer:"),
		apple.Rect(0, 0, cameraSheetWidth, cameraSheetHeight),
		uint(cameraWindowTitled),
		uint(nsBackingStoreBuffered),
		false,
	)
	sheet.Send(sel("setReleasedWhenClosed:"), false)
	sheet.Send(sel("setTitle:"), apple.NSString(msgs.TakePhoto()))
	sheet.Send(selSetDelegate, delegate)

	content := sheet.Send(sel("contentView"))
	previewH := float64(cameraSheetHeight - cameraBarHeight)
	previewView := objc.ID(apple.Class("NSView")).Send(selAlloc).Send(
		sel("initWithFrame:"),
		apple.Rect(0, cameraBarHeight, cameraSheetWidth, previewH),
	)
	previewView.Send(sel("setWantsLayer:"), true)
	black := objc.ID(apple.Class("NSColor")).Send(sel("blackColor")).Send(sel("CGColor"))
	previewLayer := previewView.Send(sel("layer"))
	previewLayer.Send(sel("setBackgroundColor:"), black)

	videoLayer := objc.ID(apple.Class("AVCaptureVideoPreviewLayer")).Send(sel("layerWithSession:"), session)
	videoLayer.Send(sel("setVideoGravity:"), apple.NSString("AVLayerVideoGravityResizeAspectFill"))
	videoLayer.Send(sel("setFrame:"), apple.Rect(0, 0, cameraSheetWidth, previewH))
	previewLayer.Send(sel("addSublayer:"), videoLayer)
	content.Send(sel("addSubview:"), previewView)
	previewView.Send(selRelease)

	captureBtn := makeSheetButton(msgs.TakePhoto(), delegate, sel("capture:"), "\r")
	cancelBtn := makeSheetButton(msgs.Cancel(), delegate, sel("cancel:"), "\x1b")
	// Disable the shutter until startRunning completes so capturePhoto
	// never races with an inactive video connection.
	captureBtn.Send(sel("setEnabled:"), false)

	captureW := max(objc.Send[apple.CGRect](captureBtn, sel("frame")).Size.Width, 96)
	cancelW := max(objc.Send[apple.CGRect](cancelBtn, sel("frame")).Size.Width, 80)
	const btnH = 32.0
	btnY := (cameraBarHeight - btnH) / 2
	captureX := cameraSheetWidth - 16 - captureW
	cancelX := captureX - 10 - cancelW
	captureBtn.Send(sel("setFrame:"), apple.Rect(captureX, btnY, captureW, btnH))
	cancelBtn.Send(sel("setFrame:"), apple.Rect(cancelX, btnY, cancelW, btnH))
	content.Send(sel("addSubview:"), cancelBtn)
	content.Send(sel("addSubview:"), captureBtn)

	parent := s.window
	if parent == 0 && s.native != 0 {
		parent = s.native.Send(selWindow)
	}
	started := make(chan struct{})
	cameraSheets[delegate] = &cameraSheet{
		s:          s,
		id:         id,
		r:          r,
		parent:     parent,
		sheet:      sheet,
		captureBtn: captureBtn,
		session:    session,
		output:     output,
		started:    started,
	}

	if parent != 0 {
		parent.Send(sel("beginSheet:completionHandler:"), sheet, objc.ID(0))
	} else {
		sheet.Send(sel("center"))
		sheet.Send(sel("makeKeyAndOrderFront:"), objc.ID(0))
	}

	// startRunning blocks until the camera runs; keep it off the main thread.
	go func() {
		session.Send(sel("startRunning"))
		close(started)
		apple.OnMain(func() {
			if c := cameraSheets[delegate]; c != nil && !c.capturing {
				c.captureBtn.Send(sel("setEnabled:"), true)
			}
		})
	}()
	return ""
}

func makeSheetButton(title string, target objc.ID, action objc.SEL, keyEquiv string) objc.ID {
	btn := objc.ID(apple.Class("NSButton")).Send(sel("buttonWithTitle:target:action:"), apple.NSString(title), target, action)
	btn.Send(sel("setKeyEquivalent:"), apple.NSString(keyEquiv))
	btn.Send(sel("sizeToFit"))
	return btn
}

func registerCameraSheet() {
	cameraSheetOnce.Do(func() {
		if !loadAVFoundation() || apple.Class("AVCapturePhotoOutput") == 0 {
			return
		}
		var protocols []*objc.Protocol
		for _, name := range []string{"AVCapturePhotoCaptureDelegate", "NSWindowDelegate"} {
			if p := objc.GetProtocol(name); p != nil {
				protocols = append(protocols, p)
			}
		}
		classCameraDelegate = register("NsgoCameraSheetDelegate", apple.Class("NSObject"), protocols, []objc.MethodDef{
			{
				Cmd: sel("capture:"),
				Fn: func(self objc.ID, _ objc.SEL, _ objc.ID) {
					c := cameraSheets[self]
					if c == nil || c.capturing {
						return
					}
					c.capturing = true
					c.captureBtn.Send(sel("setEnabled:"), false)
					settings := objc.ID(apple.Class("AVCapturePhotoSettings")).Send(sel("photoSettings"))
					if settings == 0 {
						finishCameraSheet(self, nil, "could not configure photo capture")
						return
					}
					c.output.Send(sel("capturePhotoWithSettings:delegate:"), settings, self)
				},
			},
			{
				Cmd: sel("cancel:"),
				Fn: func(self objc.ID, _ objc.SEL, _ objc.ID) {
					finishCameraSheet(self, nil, "")
				},
			},
			{
				Cmd: sel("windowWillClose:"),
				Fn: func(self objc.ID, _ objc.SEL, _ objc.ID) {
					finishCameraSheet(self, nil, "")
				},
			},
			{
				Cmd: sel("captureOutput:didFinishProcessingPhoto:error:"),
				Fn: func(self objc.ID, _ objc.SEL, _, photo, errObj objc.ID) {
					var data []byte
					var errMsg string
					if errObj != 0 {
						errMsg = apple.GoString(errObj.Send(sel("localizedDescription")))
						if errMsg == "" {
							errMsg = "could not capture photo"
						}
					} else if photo != 0 {
						data = apple.Bytes(photo.Send(sel("fileDataRepresentation")))
						if len(data) == 0 {
							errMsg = "captured photo had no image data"
						}
					} else {
						errMsg = "could not capture photo"
					}
					apple.OnMain(func() {
						finishCameraSheet(self, data, errMsg)
					})
				},
			},
		})
	})
}

// finishCameraSheet tears down the camera sheet for delegate on the main
// thread and dispatches EventPick (unless quiet or the surface closed).
func finishCameraSheet(delegate objc.ID, data []byte, errMsg string) {
	c := cameraSheets[delegate]
	if c == nil {
		return
	}
	delete(cameraSheets, delegate)
	c.sheet.Send(selSetDelegate, objc.ID(0))
	if c.parent != 0 {
		c.parent.Send(sel("endSheet:"), c.sheet)
	}
	c.sheet.Send(sel("orderOut:"), objc.ID(0))
	c.sheet.Send(selRelease)

	session, output, started := c.session, c.output, c.started
	go func() {
		<-started
		session.Send(sel("stopRunning"))
		output.Send(selRelease)
		session.Send(selRelease)
	}()
	delegate.Send(selRelease)

	if c.quiet || c.s.closed || c.s.engine == nil {
		return
	}
	if errMsg != "" {
		c.s.engine.Dispatch(driver.Event{Kind: driver.EventPick, Count: c.id, Text: errMsg})
		return
	}
	if len(data) == 0 {
		c.s.engine.Dispatch(driver.Event{Kind: driver.EventPick, Count: c.id})
		return
	}
	req, id, eng := c.r, c.id, c.s.engine
	go func() {
		f, err := prepareCapturedPhoto(req, data)
		if err != nil {
			eng.Dispatch(driver.Event{Kind: driver.EventPick, Count: id, Text: err.Error()})
			return
		}
		eng.Dispatch(driver.Event{Kind: driver.EventPick, Count: id, Data: []driver.PickedFile{f}})
	}()
}

// endSurfaceCameras silently closes any open camera sheet belonging to s
// when s closes.
func endSurfaceCameras(s *surface) {
	for delegate, c := range cameraSheets {
		if c.s == s {
			c.quiet = true
			finishCameraSheet(delegate, nil, "")
		}
	}
}

// prepareCapturedPhoto normalizes raw captured photo bytes into an
// upright JPEG prepared according to r (MaxDimension and Quality).
func prepareCapturedPhoto(r driver.PickRequest, data []byte) (driver.PickedFile, error) {
	if len(data) == 0 {
		return driver.PickedFile{}, errors.New("captured photo had no image data")
	}
	req := photoPickRequest(r)
	f := apple.PreparePicked(req, driver.PickedFile{Name: "photo.jpg", Data: data})
	files := driver.FinishPick(req, []driver.PickedFile{f}, nil)
	if len(files) == 0 {
		return driver.PickedFile{}, errors.New("captured photo could not be prepared")
	}
	return files[0], nil
}

// photoPickRequest returns r normalized for TakePhoto: single image,
// compatible format, and an explicit JPEG quality so the image is always
// re-encoded upright at the requested quality.
func photoPickRequest(r driver.PickRequest) driver.PickRequest {
	r.Limit = 1
	r.Format = driver.PickCompatible
	r.Quality = r.JPEGQuality()
	return r
}
