//go:build darwin && !ios

package macos

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/nsgo-dev/nsgo/internal/driver"
)

func TestTakePhotoFlow(t *testing.T) {
	req := driver.PickRequest{Limit: 1, MaxDimension: 1024, Quality: 0.8}

	t.Run("no camera", func(t *testing.T) {
		var failed string
		presented := false
		runTakePhotoFlow(req, 7, func(why string) { failed = why }, cameraFlowDeps{
			hasCamera:        func() bool { return false },
			declaresUsage:    func(string) bool { return true },
			permissionStatus: func(string) int { return driver.PermissionGranted },
			present:          func(driver.PickRequest, uint32) { presented = true },
		})
		if failed != driver.NoCamera || presented {
			t.Fatalf("failed=%q presented=%v, want %q and false", failed, presented, driver.NoCamera)
		}
	})

	t.Run("missing usage description", func(t *testing.T) {
		var failed, gotKey string
		runTakePhotoFlow(req, 7, func(why string) { failed = why }, cameraFlowDeps{
			hasCamera:        func() bool { return true },
			declaresUsage:    func(k string) bool { gotKey = k; return false },
			permissionStatus: func(string) int { return driver.PermissionGranted },
			present:          func(driver.PickRequest, uint32) { t.Fatal("should not present without usage key") },
		})
		if gotKey != "NSCameraUsageDescription" || failed == "" {
			t.Fatalf("key=%q failed=%q", gotKey, failed)
		}
	})

	t.Run("permission denied", func(t *testing.T) {
		var failed string
		runTakePhotoFlow(req, 7, func(why string) { failed = why }, cameraFlowDeps{
			hasCamera:        func() bool { return true },
			declaresUsage:    func(string) bool { return true },
			permissionStatus: func(string) int { return driver.PermissionDenied },
			present:          func(driver.PickRequest, uint32) { t.Fatal("should not present when denied") },
		})
		if failed != "camera access denied" {
			t.Fatalf("failed=%q, want camera access denied", failed)
		}
	})

	t.Run("permission unavailable", func(t *testing.T) {
		var failed string
		runTakePhotoFlow(req, 7, func(why string) { failed = why }, cameraFlowDeps{
			hasCamera:        func() bool { return true },
			declaresUsage:    func(string) bool { return true },
			permissionStatus: func(string) int { return driver.PermissionUnavailable },
			present:          func(driver.PickRequest, uint32) { t.Fatal("should not present when unavailable") },
		})
		if failed != "AVFoundation is missing" {
			t.Fatalf("failed=%q, want AVFoundation is missing", failed)
		}
	})

	t.Run("not asked then granted", func(t *testing.T) {
		var gotReq driver.PickRequest
		var gotID uint32
		var requested string
		runTakePhotoFlow(req, 42, func(why string) { t.Fatalf("unexpected failure: %s", why) }, cameraFlowDeps{
			hasCamera:        func() bool { return true },
			declaresUsage:    func(string) bool { return true },
			permissionStatus: func(string) int { return driver.PermissionNotAsked },
			requestPermission: func(name string, answer func(int)) {
				requested = name
				answer(driver.PermissionGranted)
			},
			onMain: func(fn func()) { fn() },
			present: func(r driver.PickRequest, id uint32) {
				gotReq, gotID = r, id
			},
		})
		if requested != driver.PermissionCamera || gotID != 42 || gotReq != req {
			t.Fatalf("requested=%q id=%d req=%+v", requested, gotID, gotReq)
		}
	})

	t.Run("not asked then denied", func(t *testing.T) {
		var failed string
		runTakePhotoFlow(req, 42, func(why string) { failed = why }, cameraFlowDeps{
			hasCamera:        func() bool { return true },
			declaresUsage:    func(string) bool { return true },
			permissionStatus: func(string) int { return driver.PermissionNotAsked },
			requestPermission: func(_ string, answer func(int)) {
				answer(driver.PermissionDenied)
			},
			onMain:  func(fn func()) { fn() },
			present: func(driver.PickRequest, uint32) { t.Fatal("should not present when denied") },
		})
		if failed != "camera access denied" {
			t.Fatalf("failed=%q, want camera access denied", failed)
		}
	})

	t.Run("already granted", func(t *testing.T) {
		presented := false
		runTakePhotoFlow(req, 9, func(why string) { t.Fatalf("unexpected failure: %s", why) }, cameraFlowDeps{
			hasCamera:         func() bool { return true },
			declaresUsage:     func(string) bool { return true },
			permissionStatus:  func(string) int { return driver.PermissionGranted },
			requestPermission: func(string, func(int)) { t.Fatal("should not request when already granted") },
			present:           func(driver.PickRequest, uint32) { presented = true },
		})
		if !presented {
			t.Fatal("expected camera to be presented")
		}
	})
}

func TestPhotoPickRequestOptions(t *testing.T) {
	got := photoPickRequest(driver.PickRequest{MaxDimension: 1200})
	if got.Limit != 1 || got.Format != driver.PickCompatible || got.MaxDimension != 1200 || got.Quality != driver.DefaultPickQuality {
		t.Fatalf("default photoPickRequest = %+v", got)
	}
	clamped := photoPickRequest(driver.PickRequest{Limit: 5, Format: driver.PickCurrent, Quality: 2.5})
	if clamped.Limit != 1 || clamped.Format != driver.PickCompatible || clamped.Quality != 1 {
		t.Fatalf("clamped photoPickRequest = %+v", clamped)
	}
}

func TestPrepareCapturedPhoto(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for y := range 200 {
		for x := range 400 {
			img.SetRGBA(x, y, color.RGBA{R: 40, G: 120, B: 200, A: 255})
		}
	}
	var raw bytes.Buffer
	if err := jpeg.Encode(&raw, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode test jpeg: %v", err)
	}

	scaled, err := prepareCapturedPhoto(driver.PickRequest{MaxDimension: 100, Quality: 0.7}, raw.Bytes())
	if err != nil {
		t.Fatalf("prepareCapturedPhoto(scaled): %v", err)
	}
	if scaled.Name != "photo.jpg" || scaled.MIME != "image/jpeg" {
		t.Fatalf("scaled metadata = %q %q", scaled.Name, scaled.MIME)
	}
	if scaled.Size.Width != 100 || scaled.Size.Height != 50 {
		t.Fatalf("scaled size = %+v, want 100x50", scaled.Size)
	}
	if driver.SniffImage(scaled.Data) != "image/jpeg" {
		t.Fatalf("scaled bytes sniffed as %q, want image/jpeg", driver.SniffImage(scaled.Data))
	}

	full, err := prepareCapturedPhoto(driver.PickRequest{}, raw.Bytes())
	if err != nil {
		t.Fatalf("prepareCapturedPhoto(full): %v", err)
	}
	if full.Size.Width != 400 || full.Size.Height != 200 || full.MIME != "image/jpeg" || full.Name != "photo.jpg" {
		t.Fatalf("full photo = %+v", full)
	}

	if _, err := prepareCapturedPhoto(driver.PickRequest{}, nil); err == nil {
		t.Fatal("expected error for empty photo bytes")
	}
}

func TestCameraPhotoClassRegistration(t *testing.T) {
	if !loadAVFoundation() {
		t.Skip("AVFoundation not available")
	}
	// Probing availability must never prompt or crash.
	_ = cameraAvailable()
	onMain(func() {
		registerCameraSheet()
		if classCameraDelegate == 0 {
			t.Fatal("NsgoCameraSheetDelegate was not registered")
		}
	})
}
