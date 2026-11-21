package gpu

import (
	"image"

	"github.com/nsgo-dev/nsgo/internal/driver"
)

// SoftwareRenderer hosts a standalone, deterministic pure-Go software 3D GPU
// device for headless unit tests and offscreen rendering without a UI window.
type SoftwareRenderer struct {
	soft *driver.GPUSoftwareDevice
	dev  *Device
	err  error
}

// NewSoftwareRenderer creates a pure-Go software 3D GPU renderer with a swapchain
// of width x height pixels and an optional depthFormat (e.g. FormatDepth24Plus).
func NewSoftwareRenderer(width, height int, depthFormat TextureFormat) *SoftwareRenderer {
	return NewSoftwareRendererWithConfig(width, height, FormatRGBA8Unorm, depthFormat, 1)
}

// NewSoftwareRendererWithConfig creates a pure-Go software 3D GPU renderer with
// explicit swapchain color format, depth/stencil format, and MSAA sample count (1 or 4).
func NewSoftwareRendererWithConfig(width, height int, colorFormat, depthFormat TextureFormat, sampleCount int) *SoftwareRenderer {
	if colorFormat == FormatUndefined {
		colorFormat = FormatRGBA8Unorm
	}
	if sampleCount <= 0 {
		sampleCount = 1
	}
	cfg := driver.GPUViewConfig{
		ColorFormat: driver.GPUTextureFormat(colorFormat),
		DepthFormat: driver.GPUTextureFormat(depthFormat),
		SampleCount: sampleCount,
	}
	soft := driver.NewGPUSoftwareDevice(width, height, cfg)
	r := &SoftwareRenderer{soft: soft}
	dev := newDevice(
		nil,
		nil,
		1,
		width,
		height,
		colorFormat,
		limitsFromDriver(driver.DefaultGPULimits()),
		Features{
			MSAA4x:           true,
			Depth32Float:     true,
			Float16Filtering: true,
			Float16Blending:  true,
			Float32Filtering: true,
		},
	)
	dev.submitFn = func(frame driver.GPUFrame) {
		rbs, err := soft.Submit(frame)
		if err != nil {
			r.err = err
		}
		for _, rb := range rbs {
			dev.completeReadback(rb.ID, rb.Pixels, rb.Err)
		}
	}
	r.dev = dev
	return r
}

// Device returns the gpu.Device backed by this software rasterizer.
func (r *SoftwareRenderer) Device() *Device { return r.dev }

// Resize updates the software swapchain size in pixels.
func (r *SoftwareRenderer) Resize(width, height int) {
	r.soft.Resize(width, height)
	r.dev.width, r.dev.height = r.soft.Size()
}

// Image returns an independent RGBA snapshot of the rendered swapchain surface.
func (r *SoftwareRenderer) Image() *image.RGBA {
	return r.soft.SurfaceImage()
}

// FrameCount returns how many GPU frames have been submitted.
func (r *SoftwareRenderer) FrameCount() int {
	return len(r.soft.Frames())
}

// LastError returns the last execution error reported by the software device.
func (r *SoftwareRenderer) LastError() error {
	return r.err
}
