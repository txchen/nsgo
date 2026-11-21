//go:build linux && !android

package linux

import (
	"strings"
	"testing"

	"github.com/nsgo-dev/nsgo/internal/driver"
)

func TestTakePhotoFlow(t *testing.T) {
	req := driver.PickRequest{Limit: 1, MaxDimension: 1600, Quality: 0.8}

	t.Run("missing pipewire preview", func(t *testing.T) {
		var failed string
		started := false
		runTakePhotoFlow(req, 3, func(why string) { failed = why }, cameraFlowDeps{
			hasPreview:  func() bool { return false },
			startPortal: func(driver.PickRequest, uint32) { started = true },
		})
		if failed != driver.NoCamera || started {
			t.Fatalf("failed=%q started=%v, want %q and false", failed, started, driver.NoCamera)
		}
	})

	t.Run("starts portal access", func(t *testing.T) {
		var gotReq driver.PickRequest
		var gotID uint32
		runTakePhotoFlow(req, 19, func(why string) { t.Fatalf("unexpected failure: %s", why) }, cameraFlowDeps{
			hasPreview:  func() bool { return true },
			startPortal: func(r driver.PickRequest, id uint32) { gotReq, gotID = r, id },
		})
		if gotID != 19 || gotReq != req {
			t.Fatalf("id=%d req=%+v", gotID, gotReq)
		}
	})
}

func TestPortalCameraResponseError(t *testing.T) {
	if got := portalCameraResponseError(0); got != "" {
		t.Fatalf("code 0 = %q, want empty", got)
	}
	if got := portalCameraResponseError(1); got != "camera access denied" {
		t.Fatalf("code 1 = %q, want camera access denied", got)
	}
	if got := portalCameraResponseError(2); got != "the camera cannot be used" {
		t.Fatalf("code 2 = %q, want the camera cannot be used", got)
	}
}

func TestCameraPipelineDescription(t *testing.T) {
	got := cameraPipelineDescription(17)
	for _, want := range []string{"pipewiresrc fd=17", "videoconvert", "format=RGBA", "appsink name=sink"} {
		if !strings.Contains(got, want) {
			t.Fatalf("pipeline %q missing %q", got, want)
		}
	}
}

func TestPhotoPickRequestOptions(t *testing.T) {
	got := photoPickRequest(driver.PickRequest{MaxDimension: 640})
	if got.Limit != 1 || got.Format != driver.PickCompatible || got.MaxDimension != 640 || got.Quality != driver.DefaultPickQuality {
		t.Fatalf("photoPickRequest default = %+v", got)
	}
	clamped := photoPickRequest(driver.PickRequest{Limit: 4, Format: driver.PickCurrent, Quality: 3})
	if clamped.Limit != 1 || clamped.Format != driver.PickCompatible || clamped.Quality != 1 {
		t.Fatalf("photoPickRequest clamped = %+v", clamped)
	}
}

func TestEncodeCameraFrame(t *testing.T) {
	const (
		width  = 240
		height = 120
		// Include row padding beyond width*4 to test stride handling.
		stride = width*4 + 16
	)
	rgba := make([]byte, stride*height)
	for y := range height {
		row := rgba[y*stride : y*stride+width*4]
		for x := range width {
			i := x * 4
			row[i], row[i+1], row[i+2], row[i+3] = byte(x), byte(y), 180, 255
		}
	}

	scaled, err := encodeCameraFrame(driver.PickRequest{MaxDimension: 80, Quality: 0.75}, rgba, width, height, stride)
	if err != nil {
		t.Fatalf("encodeCameraFrame(scaled): %v", err)
	}
	if scaled.Name != "photo.jpg" || scaled.MIME != "image/jpeg" {
		t.Fatalf("metadata = %q %q", scaled.Name, scaled.MIME)
	}
	if scaled.Size.Width != 80 || scaled.Size.Height != 40 {
		t.Fatalf("size = %+v, want 80x40", scaled.Size)
	}
	if driver.SniffImage(scaled.Data) != "image/jpeg" {
		t.Fatalf("sniff = %q, want image/jpeg", driver.SniffImage(scaled.Data))
	}

	full, err := encodeCameraFrame(driver.PickRequest{}, rgba, width, height, stride)
	if err != nil {
		t.Fatalf("encodeCameraFrame(full): %v", err)
	}
	if full.Size.Width != width || full.Size.Height != height || full.MIME != "image/jpeg" || full.Name != "photo.jpg" {
		t.Fatalf("full = %+v", full)
	}

	if _, err := encodeCameraFrame(driver.PickRequest{}, rgba, 0, height, stride); err == nil {
		t.Fatal("expected error for zero width")
	}
	if _, err := encodeCameraFrame(driver.PickRequest{}, rgba, width, height, width*2); err == nil {
		t.Fatal("expected error for undersized stride")
	}
	if _, err := encodeCameraFrame(driver.PickRequest{}, rgba[:16], width, height, stride); err == nil {
		t.Fatal("expected error for truncated buffer")
	}
}
