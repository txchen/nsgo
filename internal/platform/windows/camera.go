//go:build windows

package windows

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/nsgo-dev/nsgo/internal/driver"
	"github.com/nsgo-dev/nsgo/internal/platform/picked"
)

// Taking a photo on Windows: WinRT's Windows.Media.Capture.CameraCaptureUI,
// owned by the window's HWND through IInitializeWithWindow::Initialize.
// Availability checks that CameraCaptureUI resolves and a present video
// camera device interface (KSCATEGORY_VIDEO_CAMERA) is attached.

var (
	setupapi                    = syscall.NewLazyDLL("setupapi.dll")
	setupDiGetClassDevsW        = setupapi.NewProc("SetupDiGetClassDevsW")
	setupDiEnumDeviceInterfaces = setupapi.NewProc("SetupDiEnumDeviceInterfaces")
	setupDiDestroyDeviceInfo    = setupapi.NewProc("SetupDiDestroyDeviceInfoList")

	// KSCATEGORY_VIDEO_CAMERA (ksmedia.h).
	guidVideoCamera = parseGUID("e5323777-f976-4f5b-9b55-b94699c46e44")

	iidActivationFactory            = parseGUID("00000035-0000-0000-c000-000000000046")
	iidAsyncInfo                    = parseGUID("00000036-0000-0000-c000-000000000046")
	iidInitializeWithWindow         = parseGUID("3e68d4bd-7135-4d10-8018-9fb6d9f33fa1")
	iidCameraCaptureUI              = parseGUID("48587540-6f93-4bb4-b8f3-e89e48948c91")
	iidCameraCaptureUIPhotoSettings = parseGUID("b9f5be97-3472-46a8-8a9e-04ce42ccc97d")
	iidAsyncOpStorageFileCompleted  = parseGUID("e521c894-2c26-5946-9e61-2b5e188d01ed")
)

const (
	digcfPresent         = 0x00000002
	digcfDeviceInterface = 0x00000010
	invalidHandleValue   = ^uintptr(0)

	// IInitializeWithWindow method after IUnknown.
	initWithWindowInitialize = 3

	// ICameraCaptureUI methods after IInspectable.
	cameraUIGetPhotoSettings = 6
	cameraUICaptureFileAsync = 8

	// ICameraCaptureUIPhotoCaptureSettings methods after IInspectable.
	cameraPhotoPutFormat        = 7
	cameraPhotoPutMaxResolution = 9
	cameraPhotoPutAllowCropping = 15

	// CameraCaptureUIMode & photo settings enums.
	cameraCaptureModePhoto       = 1
	cameraPhotoFormatJPEG        = 0
	cameraPhotoResolutionHighest = 0

	// IAsyncOperation<StorageFile> methods after IInspectable.
	asyncOpPutCompleted = 6
	asyncOpGetResults   = 8

	// IAsyncInfo methods after IInspectable.
	asyncInfoGetErrorCode = 8
	asyncInfoCancel       = 9
	asyncInfoClose        = 10

	// AsyncStatus values (asyncStatusCompleted is defined in speak.go).
	asyncStatusCanceled = 2
	asyncStatusError    = 3

	// IStorageItem methods after IInspectable.
	storageItemGetPath = 12
)

// spDeviceInterfaceData matches SP_DEVICE_INTERFACE_DATA (setupapi.h)
// on 32-bit (28 bytes) and 64-bit (32 bytes) Windows.
type spDeviceInterfaceData struct {
	cbSize             uint32
	interfaceClassGuid guid
	flags              uint32
	reserved           uintptr
}

// hasVideoCameraDevice reports whether at least one present video camera
// device interface is registered with the system.
func hasVideoCameraDevice() bool {
	if setupDiGetClassDevsW.Find() != nil || setupDiEnumDeviceInterfaces.Find() != nil || setupDiDestroyDeviceInfo.Find() != nil {
		return false
	}
	h, _, _ := setupDiGetClassDevsW.Call(uintptr(unsafe.Pointer(&guidVideoCamera)), 0, 0, digcfPresent|digcfDeviceInterface)
	if h == 0 || h == invalidHandleValue {
		return false
	}
	defer setupDiDestroyDeviceInfo.Call(h)
	var data spDeviceInterfaceData
	data.cbSize = uint32(unsafe.Sizeof(data))
	r, _, _ := setupDiEnumDeviceInterfaces.Call(h, 0, uintptr(unsafe.Pointer(&guidVideoCamera)), 0, uintptr(unsafe.Pointer(&data)))
	return r != 0
}

// cameraUIResolvable reports whether WinRT's CameraCaptureUI class can
// be activated on this Windows installation.
var cameraUIResolvable = sync.OnceValue(func() bool {
	comApartment()
	if roGetActivationFactory.Find() != nil || roActivateInstance.Find() != nil {
		return false
	}
	f := activationFactory("Windows.Media.Capture.CameraCaptureUI", &iidActivationFactory)
	if f == nil {
		return false
	}
	f.release()
	return true
})

// cameraAvailable is Environment.Camera on Windows.
var cameraAvailable = func() bool {
	return cameraUIResolvable() && hasVideoCameraDevice()
}

// cameraFlowDeps lets unit tests exercise TakePhoto's availability,
// permission and dispatch flow without activating WinRT UI.
type cameraFlowDeps struct {
	hasCamera        func() bool
	permissionStatus func(string) (int, bool)
	startCapture     func(driver.PickRequest, uint32)
}

func (w *window) takePhoto(r driver.PickRequest, id uint32) {
	fail := func(why string) {
		if !w.cameraClosed && w.engine != nil {
			w.engine.Dispatch(driver.Event{Kind: driver.EventPick, Count: id, Text: why})
		}
	}
	runTakePhotoFlow(r, id, fail, cameraFlowDeps{
		hasCamera:        cameraAvailable,
		permissionStatus: permissionStatus,
		startCapture: func(req driver.PickRequest, reqID uint32) {
			w.post(func() { w.startCameraCapture(req, reqID) })
		},
	})
}

func runTakePhotoFlow(r driver.PickRequest, id uint32, fail func(why string), deps cameraFlowDeps) {
	if !deps.hasCamera() {
		fail(driver.NoCamera)
		return
	}
	if status, _ := deps.permissionStatus(driver.PermissionCamera); status == driver.PermissionDenied {
		fail("camera access denied")
		return
	}
	deps.startCapture(r, id)
}

// cameraAsyncHandler implements IAsyncOperationCompletedHandler<StorageFile>
// with both Invoke parameters as uintptr so the integer AsyncStatus is
// never treated as a Go pointer.
type cameraAsyncHandler struct {
	vtbl   *[4]uintptr
	refs   atomic.Int32
	invoke func(asyncInfo, status uintptr)
}

var (
	cameraAsyncVtbl = [4]uintptr{
		syscall.NewCallback(cameraAsyncQueryInterface),
		syscall.NewCallback(cameraAsyncAddRef),
		syscall.NewCallback(cameraAsyncRelease),
		syscall.NewCallback(cameraAsyncInvoke),
	}
	cameraHandlersMu sync.Mutex
	cameraHandlers   = map[*cameraAsyncHandler]struct{}{}
)

func newCameraAsyncHandler(invoke func(asyncInfo, status uintptr)) *cameraAsyncHandler {
	h := &cameraAsyncHandler{vtbl: &cameraAsyncVtbl, invoke: invoke}
	h.refs.Store(1)
	cameraHandlersMu.Lock()
	cameraHandlers[h] = struct{}{}
	cameraHandlersMu.Unlock()
	return h
}

func (h *cameraAsyncHandler) ptr() uintptr { return uintptr(unsafe.Pointer(h)) }

func (h *cameraAsyncHandler) release() { cameraAsyncRelease(h) }

func cameraAsyncQueryInterface(h *cameraAsyncHandler, iid *guid, out *unsafe.Pointer) uintptr {
	if *iid == iidUnknown || *iid == iidAsyncOpStorageFileCompleted || *iid == iidAgileObject {
		*out = unsafe.Pointer(h)
		h.refs.Add(1)
		return sOK
	}
	*out = nil
	return eNoInterface
}

func cameraAsyncAddRef(h *cameraAsyncHandler) uintptr {
	return uintptr(h.refs.Add(1))
}

func cameraAsyncRelease(h *cameraAsyncHandler) uintptr {
	n := h.refs.Add(-1)
	if n == 0 {
		cameraHandlersMu.Lock()
		delete(cameraHandlers, h)
		cameraHandlersMu.Unlock()
	}
	return uintptr(n)
}

func cameraAsyncInvoke(h *cameraAsyncHandler, asyncInfo, status uintptr) uintptr {
	if h.invoke != nil {
		h.invoke(asyncInfo, status)
	}
	return sOK
}

// startCameraCapture activates CameraCaptureUI on the STA UI thread,
// associates it with the window's owner HWND, and starts CaptureFileAsync.
func (w *window) startCameraCapture(r driver.PickRequest, id uint32) {
	if w.cameraClosed || w.engine == nil {
		return
	}
	comApartment()
	fail := func(why string) {
		if !w.cameraClosed && w.engine != nil {
			w.engine.Dispatch(driver.Event{Kind: driver.EventPick, Count: id, Text: why})
		}
	}
	class := hstring("Windows.Media.Capture.CameraCaptureUI")
	var inspectable *comObject
	hr, _, _ := roActivateInstance.Call(class, uintptr(unsafe.Pointer(&inspectable)))
	deleteHString(class)
	if failed(hr) || inspectable == nil {
		fail(cameraErrorText(hr))
		return
	}
	defer inspectable.release()

	if initWin := inspectable.query(&iidInitializeWithWindow); initWin != nil {
		hr = initWin.call(initWithWindowInitialize, w.owner())
		initWin.release()
		if failed(hr) {
			fail(cameraErrorText(hr))
			return
		}
	}

	ui := inspectable.query(&iidCameraCaptureUI)
	if ui == nil {
		fail(driver.NoCamera)
		return
	}
	defer ui.release()

	var settings *comObject
	if !failed(ui.call(cameraUIGetPhotoSettings, uintptr(unsafe.Pointer(&settings)))) && settings != nil {
		settings.call(cameraPhotoPutFormat, cameraPhotoFormatJPEG)
		settings.call(cameraPhotoPutMaxResolution, cameraPhotoResolutionHighest)
		settings.call(cameraPhotoPutAllowCropping, 0)
		settings.release()
	}

	var asyncOp *comObject
	if hr = ui.call(cameraUICaptureFileAsync, cameraCaptureModePhoto, uintptr(unsafe.Pointer(&asyncOp))); failed(hr) || asyncOp == nil {
		fail(cameraErrorText(hr))
		return
	}

	if w.cameraOps == nil {
		w.cameraOps = map[*comObject]struct{}{}
	}
	w.cameraOps[asyncOp] = struct{}{}

	var doneOnce sync.Once
	handler := newCameraAsyncHandler(func(_, status uintptr) {
		doneOnce.Do(func() {
			st := uint32(status)
			w.post(func() { w.finishCameraCapture(asyncOp, st, r, id) })
		})
	})
	hr = asyncOp.call(asyncOpPutCompleted, handler.ptr())
	handler.release()
	if failed(hr) {
		delete(w.cameraOps, asyncOp)
		closeAsyncOp(asyncOp)
		asyncOp.release()
		fail(cameraErrorText(hr))
	}
}

// finishCameraCapture handles completion of CaptureFileAsync on the STA
// UI thread, reads the StorageFile path, and prepares the JPEG off-thread.
func (w *window) finishCameraCapture(asyncOp *comObject, status uint32, r driver.PickRequest, id uint32) {
	delete(w.cameraOps, asyncOp)
	defer func() {
		closeAsyncOp(asyncOp)
		asyncOp.release()
	}()
	if w.cameraClosed || w.engine == nil {
		return
	}
	switch status {
	case asyncStatusCanceled:
		w.engine.Dispatch(driver.Event{Kind: driver.EventPick, Count: id})
		return
	case asyncStatusError:
		var errCode uintptr
		if info := asyncOp.query(&iidAsyncInfo); info != nil {
			info.call(asyncInfoGetErrorCode, uintptr(unsafe.Pointer(&errCode)))
			info.release()
		}
		text := cameraErrorText(errCode)
		w.engine.Dispatch(driver.Event{Kind: driver.EventPick, Count: id, Text: text})
		return
	case asyncStatusCompleted:
		var storageFile *comObject
		hr := asyncOp.call(asyncOpGetResults, uintptr(unsafe.Pointer(&storageFile)))
		if failed(hr) {
			w.engine.Dispatch(driver.Event{Kind: driver.EventPick, Count: id, Text: cameraErrorText(hr)})
			return
		}
		if storageFile == nil {
			// CameraCaptureUI returns a nil StorageFile when the user cancels.
			w.engine.Dispatch(driver.Event{Kind: driver.EventPick, Count: id})
			return
		}
		path := storageFilePath(storageFile)
		storageFile.release()
		if path == "" {
			w.engine.Dispatch(driver.Event{Kind: driver.EventPick, Count: id, Text: "captured photo had no file path"})
			return
		}
		eng := w.engine
		go func() {
			defer os.Remove(path)
			f, err := prepareCapturedPhotoFile(r, path)
			if err != nil {
				eng.Dispatch(driver.Event{Kind: driver.EventPick, Count: id, Text: err.Error()})
				return
			}
			eng.Dispatch(driver.Event{Kind: driver.EventPick, Count: id, Data: []driver.PickedFile{f}})
		}()
	default:
		w.engine.Dispatch(driver.Event{Kind: driver.EventPick, Count: id})
	}
}

func storageFilePath(storageFile *comObject) string {
	item := storageFile.query(&iidStorageItem)
	if item == nil {
		return ""
	}
	defer item.release()
	var pathHStr uintptr
	if failed(item.call(storageItemGetPath, uintptr(unsafe.Pointer(&pathHStr)))) {
		return ""
	}
	defer deleteHString(pathHStr)
	return hstringText(pathHStr)
}

func closeAsyncOp(asyncOp *comObject) {
	if info := asyncOp.query(&iidAsyncInfo); info != nil {
		info.call(asyncInfoClose)
		info.release()
	}
}

// closeCameras cancels any in-flight CameraCaptureUI operation when the
// window is destroyed.
func (w *window) closeCameras() {
	w.cameraClosed = true
	for op := range w.cameraOps {
		if info := op.query(&iidAsyncInfo); info != nil {
			info.call(asyncInfoCancel)
			info.release()
		}
	}
}

// cameraErrorText maps a WinRT/COM HRESULT from CameraCaptureUI into
// the EventPick Text ("" for cancellation, driver.NoCamera when no
// camera hardware is available, or a descriptive error).
func cameraErrorText(hr uintptr) string {
	switch uint32(hr) {
	case 0:
		return "Windows CameraCaptureUI failed"
	case 0x80004004, 0x800704C7: // E_ABORT, ERROR_CANCELLED
		return ""
	case 0x80070005: // E_ACCESSDENIED
		return "camera access denied"
	case 0x80070490, 0xC00DABE0, 0xC00D3704, 0x80040154: // ERROR_NOT_FOUND, MF_E_NO_CAPTURE_DEVICES_AVAILABLE, MF_E_HW_MFT_FAILED_START_STREAMING, REGDB_E_CLASSNOTREG
		return driver.NoCamera
	default:
		return hresultError("Windows CameraCaptureUI", hr).Error()
	}
}

// photoPickRequest normalizes r for TakePhoto: a single compatible
// photo always re-encoded upright at the requested JPEG quality.
func photoPickRequest(r driver.PickRequest) driver.PickRequest {
	r.Limit = 1
	r.Format = driver.PickCompatible
	r.Quality = r.JPEGQuality()
	return r
}

// prepareCapturedPhotoFile reads the temporary photo written by
// CameraCaptureUI and prepares it as an upright JPEG matching r.
func prepareCapturedPhotoFile(r driver.PickRequest, path string) (driver.PickedFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return driver.PickedFile{}, err
	}
	if len(data) == 0 {
		return driver.PickedFile{}, errors.New("captured photo had no image data")
	}
	req := photoPickRequest(r)
	files := picked.Prepare(req, []driver.PickedFile{{Name: "photo.jpg", Data: data}})
	if len(files) == 0 {
		return driver.PickedFile{}, errors.New("captured photo could not be prepared")
	}
	return files[0], nil
}
