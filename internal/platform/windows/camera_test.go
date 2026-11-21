//go:build windows

package windows

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/nsgo-dev/nsgo/internal/driver"
)

func TestTakePhotoFlow(t *testing.T) {
	req := driver.PickRequest{Limit: 1, MaxDimension: 1280, Quality: 0.85}

	t.Run("no camera", func(t *testing.T) {
		var failed string
		started := false
		runTakePhotoFlow(req, 1, func(why string) { failed = why }, cameraFlowDeps{
			hasCamera:        func() bool { return false },
			permissionStatus: func(string) (int, bool) { return driver.PermissionGranted, false },
			startCapture:     func(driver.PickRequest, uint32) { started = true },
		})
		if failed != driver.NoCamera || started {
			t.Fatalf("failed=%q started=%v, want %q and false", failed, started, driver.NoCamera)
		}
	})

	t.Run("permission denied", func(t *testing.T) {
		var failed string
		runTakePhotoFlow(req, 2, func(why string) { failed = why }, cameraFlowDeps{
			hasCamera:        func() bool { return true },
			permissionStatus: func(string) (int, bool) { return driver.PermissionDenied, false },
			startCapture:     func(driver.PickRequest, uint32) { t.Fatal("should not start capture when denied") },
		})
		if failed != "camera access denied" {
			t.Fatalf("failed=%q, want camera access denied", failed)
		}
	})

	t.Run("not asked or granted starts capture", func(t *testing.T) {
		for _, status := range []int{driver.PermissionGranted, driver.PermissionNotAsked} {
			var gotReq driver.PickRequest
			var gotID uint32
			runTakePhotoFlow(req, 11, func(why string) { t.Fatalf("unexpected failure: %s", why) }, cameraFlowDeps{
				hasCamera:        func() bool { return true },
				permissionStatus: func(string) (int, bool) { return status, false },
				startCapture:     func(r driver.PickRequest, id uint32) { gotReq, gotID = r, id },
			})
			if gotID != 11 || gotReq != req {
				t.Fatalf("status=%d: id=%d req=%+v", status, gotID, gotReq)
			}
		}
	})
}

func TestCameraErrorText(t *testing.T) {
	cases := []struct {
		hr   uintptr
		want string
	}{
		{0x80004004, ""},
		{0x800704C7, ""},
		{0x80070005, "camera access denied"},
		{0x80070490, driver.NoCamera},
		{0xC00DABE0, driver.NoCamera},
		{0xC00D3704, driver.NoCamera},
		{0x80040154, driver.NoCamera},
	}
	for _, tc := range cases {
		if got := cameraErrorText(tc.hr); got != tc.want {
			t.Errorf("cameraErrorText(0x%x) = %q, want %q", tc.hr, got, tc.want)
		}
	}
	if got := cameraErrorText(0x80004005); !strings.Contains(got, "80004005") {
		t.Errorf("generic HRESULT error = %q, want HRESULT hex", got)
	}
}

func TestPhotoPickRequestOptions(t *testing.T) {
	got := photoPickRequest(driver.PickRequest{MaxDimension: 800})
	if got.Limit != 1 || got.Format != driver.PickCompatible || got.MaxDimension != 800 || got.Quality != driver.DefaultPickQuality {
		t.Fatalf("photoPickRequest default = %+v", got)
	}
	clamped := photoPickRequest(driver.PickRequest{Limit: 3, Format: driver.PickCurrent, Quality: 5})
	if clamped.Limit != 1 || clamped.Format != driver.PickCompatible || clamped.Quality != 1 {
		t.Fatalf("photoPickRequest clamped = %+v", clamped)
	}
}

func TestPrepareCapturedPhotoFile(t *testing.T) {
	dir := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 300, 150))
	for y := range 150 {
		for x := range 300 {
			img.SetRGBA(x, y, color.RGBA{R: 180, G: 90, B: 45, A: 255})
		}
	}
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}
	p := filepath.Join(dir, "capture.jpg")
	if err := os.WriteFile(p, raw.Bytes(), 0o600); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	got, err := prepareCapturedPhotoFile(driver.PickRequest{MaxDimension: 120, Quality: 0.75}, p)
	if err != nil {
		t.Fatalf("prepareCapturedPhotoFile: %v", err)
	}
	if got.Name != "photo.jpg" || got.MIME != "image/jpeg" {
		t.Fatalf("metadata = %q %q", got.Name, got.MIME)
	}
	if got.Size.Width != 120 || got.Size.Height != 60 {
		t.Fatalf("size = %+v, want 120x60", got.Size)
	}
	if driver.SniffImage(got.Data) != "image/jpeg" {
		t.Fatalf("sniff = %q, want image/jpeg", driver.SniffImage(got.Data))
	}

	empty := filepath.Join(dir, "empty.jpg")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatalf("write empty file: %v", err)
	}
	if _, err := prepareCapturedPhotoFile(driver.PickRequest{}, empty); err == nil {
		t.Fatal("expected error for empty file")
	}
	if _, err := prepareCapturedPhotoFile(driver.PickRequest{}, filepath.Join(dir, "missing.jpg")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestSPDeviceInterfaceDataSize(t *testing.T) {
	want := uintptr(32)
	if unsafe.Sizeof(uintptr(0)) == 4 {
		want = 28
	}
	if got := unsafe.Sizeof(spDeviceInterfaceData{}); got != want {
		t.Fatalf("sizeof(spDeviceInterfaceData) = %d, want %d", got, want)
	}
}

func TestCameraAsyncHandlerRefCount(t *testing.T) {
	var gotStatus uintptr
	h := newCameraAsyncHandler(func(_, status uintptr) { gotStatus = status })
	var out unsafe.Pointer
	if hr := cameraAsyncQueryInterface(h, &iidAsyncOpStorageFileCompleted, &out); hr != sOK || out != unsafe.Pointer(h) {
		t.Fatalf("QI completed handler: hr=0x%x out=%v", hr, out)
	}
	if hr := cameraAsyncQueryInterface(h, &iidAgileObject, &out); hr != sOK || out != unsafe.Pointer(h) {
		t.Fatalf("QI agile object: hr=0x%x out=%v", hr, out)
	}
	if hr := cameraAsyncQueryInterface(h, &iidCameraCaptureUI, &out); hr != eNoInterface || out != nil {
		t.Fatalf("QI unrelated: hr=0x%x out=%v", hr, out)
	}
	cameraAsyncInvoke(h, 0, asyncStatusCompleted)
	if gotStatus != asyncStatusCompleted {
		t.Fatalf("status = %d, want %d", gotStatus, asyncStatusCompleted)
	}
	cameraAsyncRelease(h)
	cameraAsyncRelease(h)
	if n := cameraAsyncRelease(h); n != 0 {
		t.Fatalf("final refcount = %d, want 0", n)
	}
}
