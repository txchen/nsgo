package gpu_test

import (
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"math"
	"testing"
	"time"

	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/gpu"
	"github.com/nsgo-dev/nsgo/nsgotest"
)

const color3DWGSL = `
struct Uniforms {
	mvp: mat4x4<f32>,
}

@group(0) @binding(0) var<uniform> u: Uniforms;

struct VertexIn {
	@location(0) pos: vec3<f32>,
	@location(1) color: vec4<f32>,
}

struct VertexOut {
	@builtin(position) pos: vec4<f32>,
	@location(0) color: vec4<f32>,
}

@vertex
fn vs_main(in: VertexIn) -> VertexOut {
	var out: VertexOut;
	out.pos = u.mvp * vec4<f32>(in.pos, 1.0);
	out.color = in.color;
	return out;
}

@fragment
fn fs_main(in: VertexOut) -> @location(0) vec4<f32> {
	return in.color;
}
`

const texturedWGSL = `
@group(0) @binding(0) var tex: texture_2d<f32>;
@group(0) @binding(1) var samp: sampler;

struct VertexIn {
	@location(0) pos: vec3<f32>,
	@location(1) uv: vec2<f32>,
}

struct VertexOut {
	@builtin(position) pos: vec4<f32>,
	@location(0) uv: vec2<f32>,
}

@vertex
fn vs_main(in: VertexIn) -> VertexOut {
	var out: VertexOut;
	out.pos = vec4<f32>(in.pos, 1.0);
	out.uv = in.uv;
	return out;
}

@fragment
fn fs_main(in: VertexOut) -> @location(0) vec4<f32> {
	return textureSample(tex, samp, in.uv);
}
`

func packFloat32s(vals ...float32) []byte {
	out := make([]byte, len(vals)*4)
	for i, v := range vals {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(v))
	}
	return out
}

func packUint16s(vals ...uint16) []byte {
	out := make([]byte, len(vals)*2)
	for i, v := range vals {
		binary.LittleEndian.PutUint16(out[i*2:], v)
	}
	return out
}

// perspectiveRH returns a WebGPU [0, 1] depth right-handed perspective matrix (column-major).
func perspectiveRH(fovYRad, aspect, near, far float32) [16]float32 {
	f := float32(1.0 / math.Tan(float64(fovYRad)/2.0))
	return [16]float32{
		f / aspect, 0, 0, 0,
		0, f, 0, 0,
		0, 0, far / (near - far), -1,
		0, 0, (far * near) / (near - far), 0,
	}
}

// translateMat4 returns a column-major translation matrix.
func translateMat4(tx, ty, tz float32) [16]float32 {
	return [16]float32{
		1, 0, 0, 0,
		0, 1, 0, 0,
		0, 0, 1, 0,
		tx, ty, tz, 1,
	}
}

func mulMat4(a, b [16]float32) [16]float32 {
	var out [16]float32
	for c := 0; c < 4; c++ {
		for r := 0; r < 4; r++ {
			var sum float32
			for k := 0; k < 4; k++ {
				sum += a[k*4+r] * b[c*4+k]
			}
			out[c*4+r] = sum
		}
	}
	return out
}

func TestHeadless3DCubeDepthAndReadPixels(t *testing.T) {
	mod := gpu.MustCompileWGSL(color3DWGSL)

	var (
		readyCount int
		frameCount int
		readback   []byte
		readErr    error
	)

	// Two overlapping 3D quads inside a cube volume:
	// - Near quad at z = +0.5 (bright red, drawn FIRST)
	// - Far quad at z = -0.5 (bright blue, drawn SECOND — must be rejected by depth test!)
	// Both are wound CCW when viewed from +Z looking down -Z.
	verts := packFloat32s(
		// Near red quad (vertices 0..3) at z = +0.5
		-0.5, -0.5, 0.5, 1, 0, 0, 1,
		0.5, -0.5, 0.5, 1, 0, 0, 1,
		0.5, 0.5, 0.5, 1, 0, 0, 1,
		-0.5, 0.5, 0.5, 1, 0, 0, 1,
		// Far blue quad (vertices 4..7) at z = -0.5, larger so its outer rim is visible behind the red face!
		-1.0, -1.0, -0.5, 0, 0, 1, 1,
		1.0, -1.0, -0.5, 0, 0, 1, 1,
		1.0, 1.0, -0.5, 0, 0, 1, 1,
		-1.0, 1.0, -0.5, 0, 0, 1, 1,
	)
	indices := packUint16s(
		0, 1, 2, 0, 2, 3, // near red
		4, 5, 6, 4, 6, 7, // far blue
	)

	var (
		vbuf *gpu.Buffer
		ibuf *gpu.Buffer
		ubuf *gpu.Buffer
		bg   *gpu.BindGroup
		pipe *gpu.Pipeline
	)

	app := nsgotest.Start(t, func(cx *core.Scope) core.Node {
		if !cx.Env().SupportsGPU() {
			t.Fatalf("expected SupportsGPU() == true in headless nsgotest")
		}
		return gpu.View(cx, gpu.ViewProps{
			TestID:      "viewport",
			Style:       core.Style{Width: core.Pt(120), Height: core.Pt(120)},
			DepthFormat: gpu.FormatDepth24Plus,
			Children: []core.Node{
				core.Text{Content: "HUD"},
			},
			OnReady: func(dev *gpu.Device) {
				readyCount++
				var err error
				vbuf, err = dev.CreateBuffer(cx, gpu.BufferDesc{Label: "verts", Usage: gpu.UsageVertex, Data: verts})
				if err != nil {
					t.Fatalf("CreateBuffer verts: %v", err)
				}
				ibuf, err = dev.CreateBuffer(cx, gpu.BufferDesc{Label: "indices", Usage: gpu.UsageIndex, Data: indices})
				if err != nil {
					t.Fatalf("CreateBuffer indices: %v", err)
				}
				mvp := mulMat4(perspectiveRH(math.Pi/3, 1.0, 0.1, 20.0), translateMat4(0, 0, -3.0))
				ubuf, err = dev.CreateBuffer(cx, gpu.BufferDesc{Label: "uniforms", Usage: gpu.UsageUniform | gpu.UsageCopyDst, Data: packFloat32s(mvp[:]...)})
				if err != nil {
					t.Fatalf("CreateBuffer uniforms: %v", err)
				}
				bgl, err := dev.CreateBindGroupLayout(cx, []gpu.BindGroupLayoutEntry{
					{Binding: 0, Visibility: gpu.StageVertex, Type: gpu.BindingUniform},
				})
				if err != nil {
					t.Fatalf("CreateBindGroupLayout: %v", err)
				}
				bg, err = dev.CreateBindGroup(cx, bgl, []gpu.BindGroupEntry{
					{Binding: 0, Buffer: ubuf},
				})
				if err != nil {
					t.Fatalf("CreateBindGroup: %v", err)
				}
				pipe, err = dev.CreatePipeline(cx, gpu.PipelineDesc{
					Label:    "cube",
					Vertex:   gpu.ShaderStage{Module: mod, EntryPoint: "vs_main"},
					Fragment: gpu.ShaderStage{Module: mod, EntryPoint: "fs_main"},
					Layouts:  []*gpu.BindGroupLayout{bgl},
					VertexBuffers: []gpu.VertexBufferLayout{{
						Stride: 7 * 4,
						Attributes: []gpu.VertexAttribute{
							{Location: 0, Offset: 0, Format: gpu.VertexFloat32x3},
							{Location: 1, Offset: 3 * 4, Format: gpu.VertexFloat32x4},
						},
					}},
					Primitive: gpu.PrimitiveState{
						Topology:  gpu.TopologyTriangleList,
						FrontFace: gpu.FrontCCW,
						CullMode:  gpu.CullBack,
					},
					DepthStencil: &gpu.DepthStencilState{
						Format:       gpu.FormatDepth24Plus,
						DepthWrite:   true,
						DepthCompare: gpu.CompareLess,
					},
					Targets: []gpu.ColorTargetState{{Format: dev.SurfaceFormat()}},
				})
				if err != nil {
					t.Fatalf("CreatePipeline: %v", err)
				}
			},
			OnFrame: func(frame *gpu.Frame) {
				frameCount++
				pass := frame.BeginRenderPass(gpu.RenderPassDesc{})
				pass.ClearColor(core.RGB(0x10141c))
				pass.ClearDepth(1.0)
				pass.SetPipeline(pipe)
				pass.SetBindGroup(0, bg)
				pass.SetVertexBuffer(0, vbuf, 0)
				pass.SetIndexBuffer(ibuf, gpu.IndexUint16, 0)
				pass.DrawIndexed(12, 0, 0)
				pass.End()
				frame.Submit()
				if frameCount == 1 {
					frame.Device().ReadSurface(func(rgba []byte, err error) {
						readback, readErr = rgba, err
					})
				}
			},
		})
	}, nsgotest.Size(120, 120), nsgotest.ManualFrames())

	if readyCount != 1 {
		t.Fatalf("readyCount = %d, want 1", readyCount)
	}
	// HUD child still participates in the native view tree.
	app.Find("text:HUD")

	// Step one display frame so OnFrame executes off-commit.
	app.Advance(16 * time.Millisecond)

	if frameCount < 1 {
		t.Fatalf("expected OnFrame to run, got frameCount=%d", frameCount)
	}
	frames := app.GPUFrames()
	if len(frames) == 0 {
		t.Fatalf("expected recorded GPU frames")
	}
	var totalDraws int
	for _, fr := range frames {
		totalDraws += fr.DrawsCount
	}
	if totalDraws < 1 {
		t.Fatalf("expected at least 1 recorded draw call, got %+v", frames)
	}

	img, err := app.GPUReadPixels("id:viewport")
	if err != nil {
		t.Fatalf("GPUReadPixels: %v", err)
	}
	if img.Bounds().Dx() != 120 || img.Bounds().Dy() != 120 {
		t.Fatalf("image size = %v, want 120x120", img.Bounds())
	}

	// Corner pixel (2, 2) must equal the clear color (16, 20, 28, 255).
	corner := img.RGBAAt(2, 2)
	if corner != (color.RGBA{R: 16, G: 20, B: 28, A: 255}) {
		t.Errorf("corner pixel = %+v, want clear color {16 20 28 255}", corner)
	}

	// Center pixel (60, 60) must be bright red (255, 0, 0, 255): the near red quad
	// won the depth test against the far blue quad drawn after it!
	center := img.RGBAAt(60, 60)
	if center.R < 250 || center.G > 5 || center.B > 5 || center.A != 255 {
		t.Errorf("center pixel = %+v, want red (depth test should reject far blue face)", center)
	}

	// Rim pixel (36, 60) lies outside the near red quad but inside the larger far blue quad.
	rim := img.RGBAAt(36, 60)
	if rim.B < 250 || rim.R > 5 || rim.G > 5 || rim.A != 255 {
		t.Errorf("rim pixel (36,60) = %+v, want blue from larger far quad", rim)
	}

	// Async ReadSurface callback delivered matching pixels on the engine goroutine.
	if readErr != nil {
		t.Fatalf("ReadSurface error: %v", readErr)
	}
	if len(readback) != 120*120*4 {
		t.Fatalf("ReadSurface bytes = %d, want %d", len(readback), 120*120*4)
	}
	ci := (60*120 + 60) * 4
	if readback[ci] < 250 || readback[ci+2] > 5 {
		t.Errorf("ReadSurface center pixel = (%d,%d,%d,%d), want red", readback[ci], readback[ci+1], readback[ci+2], readback[ci+3])
	}
}

func TestSoftwareTexturedBlendAndOffscreenTarget(t *testing.T) {
	texMod := gpu.MustCompileWGSL(texturedWGSL)
	colorMod := gpu.MustCompileWGSL(color3DWGSL)

	sr := gpu.NewSoftwareRenderer(32, 32, gpu.FormatDepth24Plus)
	dev := sr.Device()

	// Validate synchronous descriptor errors.
	if _, err := dev.CreateBuffer(nil, gpu.BufferDesc{Size: 16}); !errors.Is(err, gpu.ErrInvalidDescriptor) {
		t.Fatalf("CreateBuffer without Usage got %v, want ErrInvalidDescriptor", err)
	}
	if _, err := dev.CreateTexture(nil, gpu.TextureDesc{Width: 0, Height: 4, Format: gpu.FormatRGBA8Unorm, Usage: gpu.TextureSampled}); !errors.Is(err, gpu.ErrInvalidDescriptor) {
		t.Fatalf("CreateTexture width=0 got %v, want ErrInvalidDescriptor", err)
	}

	// Pass 1: Render a solid green triangle into an offscreen RenderTarget.
	rt, err := dev.CreateRenderTarget(nil, gpu.RenderTargetDesc{
		Label:       "offscreen",
		Width:       16,
		Height:      16,
		ColorFormat: gpu.FormatRGBA8Unorm,
	})
	if err != nil {
		t.Fatalf("CreateRenderTarget: %v", err)
	}

	// Pass 2: Sample a 2x2 RGBA texture (left=green, right=magenta) on a fullscreen quad
	// AND blend a semi-transparent white overlay on top.
	texImg := image.NewRGBA(image.Rect(0, 0, 2, 2))
	texImg.SetRGBA(0, 0, color.RGBA{0, 255, 0, 255})
	texImg.SetRGBA(1, 0, color.RGBA{255, 0, 255, 255})
	texImg.SetRGBA(0, 1, color.RGBA{0, 255, 0, 255})
	texImg.SetRGBA(1, 1, color.RGBA{255, 0, 255, 255})

	tex, err := dev.CreateTextureFromImage(nil, core.ImageSource{Pixels: texImg}, gpu.TextureDesc{
		Label:  "checker",
		Format: gpu.FormatRGBA8Unorm,
		Usage:  gpu.TextureSampled | gpu.TextureCopyDst,
	})
	if err != nil {
		t.Fatalf("CreateTextureFromImage: %v", err)
	}
	samp, err := dev.CreateSampler(nil, gpu.SamplerDesc{
		MinFilter: gpu.FilterNearest,
		MagFilter: gpu.FilterNearest,
		AddressU:  gpu.AddressClampToEdge,
		AddressV:  gpu.AddressClampToEdge,
	})
	if err != nil {
		t.Fatalf("CreateSampler: %v", err)
	}
	bgl, err := dev.CreateBindGroupLayout(nil, []gpu.BindGroupLayoutEntry{
		{Binding: 0, Visibility: gpu.StageFragment, Type: gpu.BindingTexture},
		{Binding: 1, Visibility: gpu.StageFragment, Type: gpu.BindingSampler},
	})
	if err != nil {
		t.Fatalf("CreateBindGroupLayout: %v", err)
	}
	bg, err := dev.CreateBindGroup(nil, bgl, []gpu.BindGroupEntry{
		{Binding: 0, Texture: tex},
		{Binding: 1, Sampler: samp},
	})
	if err != nil {
		t.Fatalf("CreateBindGroup: %v", err)
	}
	quadVerts := packFloat32s(
		-1, -1, 0.5, 0, 1,
		1, -1, 0.5, 1, 1,
		1, 1, 0.5, 1, 0,
		-1, -1, 0.5, 0, 1,
		1, 1, 0.5, 1, 0,
		-1, 1, 0.5, 0, 0,
	)
	vb, err := dev.CreateBuffer(nil, gpu.BufferDesc{Usage: gpu.UsageVertex, Data: quadVerts})
	if err != nil {
		t.Fatalf("CreateBuffer quad: %v", err)
	}
	texPipe, err := dev.CreatePipeline(nil, gpu.PipelineDesc{
		Vertex:   gpu.ShaderStage{Module: texMod, EntryPoint: "vs_main"},
		Fragment: gpu.ShaderStage{Module: texMod, EntryPoint: "fs_main"},
		Layouts:  []*gpu.BindGroupLayout{bgl},
		VertexBuffers: []gpu.VertexBufferLayout{{
			Stride: 5 * 4,
			Attributes: []gpu.VertexAttribute{
				{Location: 0, Offset: 0, Format: gpu.VertexFloat32x3},
				{Location: 1, Offset: 3 * 4, Format: gpu.VertexFloat32x2},
			},
		}},
		Targets: []gpu.ColorTargetState{{Format: gpu.FormatRGBA8Unorm}},
	})
	if err != nil {
		t.Fatalf("CreatePipeline tex: %v", err)
	}

	// Blended overlay pipeline (50% red over the textured background).
	ident := [16]float32{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	uBuf, _ := dev.CreateBuffer(nil, gpu.BufferDesc{Usage: gpu.UsageUniform, Data: packFloat32s(ident[:]...)})
	cBGL, _ := dev.CreateBindGroupLayout(nil, []gpu.BindGroupLayoutEntry{{Binding: 0, Visibility: gpu.StageVertex, Type: gpu.BindingUniform}})
	cBG, _ := dev.CreateBindGroup(nil, cBGL, []gpu.BindGroupEntry{{Binding: 0, Buffer: uBuf}})
	blendVerts := packFloat32s(
		-1, -1, 0.2, 1, 0, 0, 0.5,
		1, -1, 0.2, 1, 0, 0, 0.5,
		1, 1, 0.2, 1, 0, 0, 0.5,
		-1, -1, 0.2, 1, 0, 0, 0.5,
		1, 1, 0.2, 1, 0, 0, 0.5,
		-1, 1, 0.2, 1, 0, 0, 0.5,
	)
	blendVB, _ := dev.CreateBuffer(nil, gpu.BufferDesc{Usage: gpu.UsageVertex, Data: blendVerts})
	blendPipe, err := dev.CreatePipeline(nil, gpu.PipelineDesc{
		Vertex:   gpu.ShaderStage{Module: colorMod, EntryPoint: "vs_main"},
		Fragment: gpu.ShaderStage{Module: colorMod, EntryPoint: "fs_main"},
		Layouts:  []*gpu.BindGroupLayout{cBGL},
		VertexBuffers: []gpu.VertexBufferLayout{{
			Stride: 7 * 4,
			Attributes: []gpu.VertexAttribute{
				{Location: 0, Offset: 0, Format: gpu.VertexFloat32x3},
				{Location: 1, Offset: 3 * 4, Format: gpu.VertexFloat32x4},
			},
		}},
		Targets: []gpu.ColorTargetState{{Format: gpu.FormatRGBA8Unorm, Blend: gpu.BlendAlpha()}},
	})
	if err != nil {
		t.Fatalf("CreatePipeline blend: %v", err)
	}

	enc := dev.BeginEncoder()
	// Offscreen clear pass on rt.Color.
	p0 := enc.BeginRenderPass(gpu.RenderPassDesc{
		ColorAttachments: []gpu.ColorAttachment{{
			Texture:    rt.Color,
			LoadOp:     gpu.LoadClear,
			StoreOp:    gpu.StoreStore,
			ClearColor: core.RGB(0x0a141e),
		}},
	})
	p0.End()

	// Main pass: textured quad + 50% red alpha-blended overlay.
	p1 := enc.BeginRenderPass(gpu.RenderPassDesc{})
	p1.ClearColor(core.RGB(0x000000))
	p1.SetPipeline(texPipe)
	p1.SetBindGroup(0, bg)
	p1.SetVertexBuffer(0, vb, 0)
	p1.Draw(6, 0)
	p1.SetPipeline(blendPipe)
	p1.SetBindGroup(0, cBG)
	p1.SetVertexBuffer(0, blendVB, 0)
	p1.Draw(6, 0)
	p1.End()

	dev.Submit(enc.Finish())
	if sr.LastError() != nil {
		t.Fatalf("SoftwareRenderer error: %v", sr.LastError())
	}

	img := sr.Image()
	// Left half: green (0,255,0) blended with 50% red (255,0,0) -> (~128, ~128, 0, 255)
	left := img.RGBAAt(4, 16)
	if left.R < 120 || left.R > 136 || left.G < 120 || left.G > 136 || left.B > 5 {
		t.Errorf("left blended pixel = %+v, want ~{128 128 0 255}", left)
	}
	// Right half: magenta (255,0,255) blended with 50% red (255,0,0) -> (255, 0, ~128, 255)
	right := img.RGBAAt(28, 16)
	if right.R < 250 || right.G > 5 || right.B < 120 || right.B > 136 {
		t.Errorf("right blended pixel = %+v, want ~{255 0 128 255}", right)
	}

	// Verify offscreen RenderTarget readback works too.
	var rtBytes []byte
	dev.ReadTexture(rt.Color, gpu.TextureRegion{Width: 16, Height: 16}, func(rgba []byte, err error) {
		if err != nil {
			t.Errorf("ReadTexture rt: %v", err)
		}
		rtBytes = rgba
	})
	if len(rtBytes) != 16*16*4 || rtBytes[0] != 10 || rtBytes[1] != 20 || rtBytes[2] != 30 || rtBytes[3] != 255 {
		t.Errorf("offscreen ReadTexture first pixel = %v, want [10 20 30 255]", rtBytes[:min(4, len(rtBytes))])
	}
}

func TestGPUDeviceLossAndNoGPUFallback(t *testing.T) {
	var (
		gens []uint32
		lost []gpu.DeviceLostEvent
	)
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node {
		return gpu.View(cx, gpu.ViewProps{
			TestID: "gpu",
			Style:  core.Style{Width: core.Pt(64), Height: core.Pt(64)},
			OnReady: func(dev *gpu.Device) {
				gens = append(gens, dev.Generation())
			},
			OnDeviceLost: func(ev gpu.DeviceLostEvent) {
				lost = append(lost, ev)
			},
		})
	}, nsgotest.Size(64, 64))

	if len(gens) != 1 || gens[0] != 1 {
		t.Fatalf("initial generations = %v, want [1]", gens)
	}
	app.TriggerGPUDeviceLost("id:gpu", nsgotest.GPULostReset, "driver reset")
	if len(lost) != 1 || lost[0].Reason != gpu.LostReset || lost[0].Message != "driver reset" {
		t.Fatalf("lost events = %+v", lost)
	}
	app.RestoreGPUDevice("id:gpu")
	if len(gens) != 2 || gens[1] != 2 {
		t.Fatalf("restored generations = %v, want [1 2]", gens)
	}

	// NoGPU fallback path renders Children and reports ErrUnsupported via OnError.
	var gotErr gpu.Error
	fallbackApp := nsgotest.Start(t, func(cx *core.Scope) core.Node {
		if cx.Env().SupportsGPU() {
			t.Fatalf("expected SupportsGPU() == false under NoGPU()")
		}
		return gpu.View(cx, gpu.ViewProps{
			TestID: "fallback",
			Children: []core.Node{
				core.Text{Content: "No GPU"},
			},
			OnReady: func(*gpu.Device) {
				t.Errorf("OnReady must not run when SupportsGPU() is false")
			},
			OnError: func(err gpu.Error) {
				gotErr = err
			},
		})
	}, nsgotest.NoGPU())

	fallbackApp.Find("text:No GPU")
	if !errors.Is(gotErr, gpu.ErrUnsupported) {
		t.Errorf("NoGPU OnError = %v, want ErrUnsupported", gotErr)
	}
}

func solidRGBA(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

const cubemapSampleWGSL = `
struct Params {
	lod: f32,
	useLevel: f32,
	pad0: f32,
	pad1: f32,
}

@group(0) @binding(0) var<uniform> params: Params;
@group(0) @binding(1) var envCube: texture_cube<f32>;
@group(0) @binding(2) var envSamp: sampler;

struct VertexIn {
	@location(0) pos: vec3<f32>,
	@location(1) dir: vec3<f32>,
}

struct VertexOut {
	@builtin(position) pos: vec4<f32>,
	@location(0) dir: vec3<f32>,
}

@vertex
fn vs_main(in: VertexIn) -> VertexOut {
	var out: VertexOut;
	out.pos = vec4<f32>(in.pos, 1.0);
	out.dir = in.dir;
	return out;
}

@fragment
fn fs_main(in: VertexOut) -> @location(0) vec4<f32> {
	if (params.useLevel > 0.5) {
		return textureSampleLevel(envCube, envSamp, in.dir, params.lod);
	}
	return textureSample(envCube, envSamp, in.dir);
}
`

func TestCubemapSixFacesAndMipSampling(t *testing.T) {
	mod := gpu.MustCompileWGSL(cubemapSampleWGSL)
	sr := gpu.NewSoftwareRenderer(60, 10, gpu.FormatUndefined)
	dev := sr.Device()

	// Create a 4x4 6-face cubemap with 3 mip levels.
	cube, err := dev.CreateTexture(nil, gpu.TextureDesc{
		Label:     "skybox",
		Dimension: gpu.DimCube,
		Width:     4,
		Height:    4,
		MipLevels: 3,
		Format:    gpu.FormatRGBA8Unorm,
		Usage:     gpu.TextureSampled | gpu.TextureCopyDst | gpu.TextureCopySrc,
	})
	if err != nil {
		t.Fatalf("CreateTexture DimCube: %v", err)
	}
	if cube.Desc().Layers != 6 {
		t.Fatalf("cubemap Layers = %d, want 6", cube.Desc().Layers)
	}

	faceColors := [6]color.RGBA{
		gpu.CubeFacePosX: {R: 255, G: 0, B: 0, A: 255},   // +X Red
		gpu.CubeFaceNegX: {R: 0, G: 255, B: 0, A: 255},   // -X Green
		gpu.CubeFacePosY: {R: 0, G: 0, B: 255, A: 255},   // +Y Blue
		gpu.CubeFaceNegY: {R: 255, G: 255, B: 0, A: 255}, // -Y Yellow
		gpu.CubeFacePosZ: {R: 0, G: 255, B: 255, A: 255}, // +Z Cyan
		gpu.CubeFaceNegZ: {R: 255, G: 0, B: 255, A: 255}, // -Z Magenta
	}
	for face, c := range faceColors {
		dev.WriteCubeFaceRGBA(cube, uint32(face), 0, solidRGBA(4, 4, c))
	}
	// Upload a distinct white color on +X face mip level 1 to verify explicit cubemap LOD sampling.
	dev.WriteCubeFaceRGBA(cube, gpu.CubeFacePosX, 1, solidRGBA(2, 2, color.RGBA{255, 255, 255, 255}))

	samp, err := dev.CreateSampler(nil, gpu.SamplerDesc{
		MinFilter:    gpu.FilterNearest,
		MagFilter:    gpu.FilterNearest,
		MipmapFilter: gpu.FilterNearest,
	})
	if err != nil {
		t.Fatalf("CreateSampler: %v", err)
	}
	uBuf, err := dev.CreateBuffer(nil, gpu.BufferDesc{
		Usage: gpu.UsageUniform | gpu.UsageCopyDst,
		Data:  packFloat32s(0, 0, 0, 0),
	})
	if err != nil {
		t.Fatalf("CreateBuffer uniform: %v", err)
	}
	bgl, err := dev.CreateBindGroupLayout(nil, []gpu.BindGroupLayoutEntry{
		{Binding: 0, Visibility: gpu.StageFragment, Type: gpu.BindingUniform},
		{Binding: 1, Visibility: gpu.StageFragment, Type: gpu.BindingTexture},
		{Binding: 2, Visibility: gpu.StageFragment, Type: gpu.BindingSampler},
	})
	if err != nil {
		t.Fatalf("CreateBindGroupLayout: %v", err)
	}
	bg, err := dev.CreateBindGroup(nil, bgl, []gpu.BindGroupEntry{
		{Binding: 0, Buffer: uBuf},
		{Binding: 1, Texture: cube},
		{Binding: 2, Sampler: samp},
	})
	if err != nil {
		t.Fatalf("CreateBindGroup: %v", err)
	}
	pipe, err := dev.CreatePipeline(nil, gpu.PipelineDesc{
		Vertex:   gpu.ShaderStage{Module: mod, EntryPoint: "vs_main"},
		Fragment: gpu.ShaderStage{Module: mod, EntryPoint: "fs_main"},
		Layouts:  []*gpu.BindGroupLayout{bgl},
		VertexBuffers: []gpu.VertexBufferLayout{{
			Stride: 6 * 4,
			Attributes: []gpu.VertexAttribute{
				{Location: 0, Offset: 0, Format: gpu.VertexFloat32x3},
				{Location: 1, Offset: 3 * 4, Format: gpu.VertexFloat32x3},
			},
		}},
		Targets: []gpu.ColorTargetState{{Format: gpu.FormatRGBA8Unorm}},
	})
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}

	// Build 6 adjacent vertical strips in NDC [-1, +1] covering 10px each in the 60x10 viewport,
	// each sampling one of the 6 cardinal directions (+X, -X, +Y, -Y, +Z, -Z).
	dirs := [6][3]float32{
		{1, 0, 0},
		{-1, 0, 0},
		{0, 1, 0},
		{0, -1, 0},
		{0, 0, 1},
		{0, 0, -1},
	}
	var quadData []float32
	for i, d := range dirs {
		x0 := -1.0 + float32(i)*(2.0/6.0)
		x1 := -1.0 + float32(i+1)*(2.0/6.0)
		quadData = append(quadData,
			x0, -1, 0.5, d[0], d[1], d[2],
			x1, -1, 0.5, d[0], d[1], d[2],
			x1, 1, 0.5, d[0], d[1], d[2],
			x0, -1, 0.5, d[0], d[1], d[2],
			x1, 1, 0.5, d[0], d[1], d[2],
			x0, 1, 0.5, d[0], d[1], d[2],
		)
	}
	vb, err := dev.CreateBuffer(nil, gpu.BufferDesc{Usage: gpu.UsageVertex, Data: packFloat32s(quadData...)})
	if err != nil {
		t.Fatalf("CreateBuffer verts: %v", err)
	}

	enc := dev.BeginEncoder()
	pass := enc.BeginRenderPass(gpu.RenderPassDesc{})
	pass.ClearColor(core.RGB(0x000000))
	pass.SetPipeline(pipe)
	pass.SetBindGroup(0, bg)
	pass.SetVertexBuffer(0, vb, 0)
	pass.Draw(uint32(len(dirs)*6), 0)
	pass.End()
	dev.Submit(enc.Finish())
	if sr.LastError() != nil {
		t.Fatalf("SoftwareRenderer error: %v", sr.LastError())
	}

	img := sr.Image()
	for i, want := range faceColors {
		px := img.RGBAAt(i*10+5, 5)
		if px != want {
			t.Errorf("cubemap face %d pixel = %+v, want %+v", i, px, want)
		}
	}

	// Verify textureSampleLevel on cubemap (+X face mip 1 is white).
	dev.WriteBuffer(uBuf, 0, packFloat32s(1.0, 1.0, 0, 0))
	enc2 := dev.BeginEncoder()
	pass2 := enc2.BeginRenderPass(gpu.RenderPassDesc{})
	pass2.ClearColor(core.RGB(0x000000))
	pass2.SetPipeline(pipe)
	pass2.SetBindGroup(0, bg)
	pass2.SetVertexBuffer(0, vb, 0)
	pass2.Draw(6, 0) // only +X strip
	pass2.End()
	dev.Submit(enc2.Finish())
	if sr.LastError() != nil {
		t.Fatalf("SoftwareRenderer mip error: %v", sr.LastError())
	}
	gotMip := sr.Image().RGBAAt(5, 5)
	if gotMip != (color.RGBA{255, 255, 255, 255}) {
		t.Errorf("cubemap +X mip 1 pixel = %+v, want white", gotMip)
	}

	// Verify UV projection orientation on +Z face (top-left=Red, top-right=Green, bottom-left=Blue, bottom-right=Yellow).
	posZImg := image.NewRGBA(image.Rect(0, 0, 2, 2))
	posZImg.SetRGBA(0, 0, color.RGBA{255, 0, 0, 255})   // top-left: -X, +Y
	posZImg.SetRGBA(1, 0, color.RGBA{0, 255, 0, 255})   // top-right: +X, +Y
	posZImg.SetRGBA(0, 1, color.RGBA{0, 0, 255, 255})   // bottom-left: -X, -Y
	posZImg.SetRGBA(1, 1, color.RGBA{255, 255, 0, 255}) // bottom-right: +X, -Y
	dev.WriteCubeFaceRGBA(cube, gpu.CubeFacePosZ, 1, posZImg)
	dev.WriteBuffer(uBuf, 0, packFloat32s(1.0, 1.0, 0, 0))

	quadUVDirs := [4][3]float32{
		{-0.5, 0.5, 1.0},  // top-left -> Red
		{0.5, 0.5, 1.0},   // top-right -> Green
		{-0.5, -0.5, 1.0}, // bottom-left -> Blue
		{0.5, -0.5, 1.0},  // bottom-right -> Yellow
	}
	wantUVColors := [4]color.RGBA{
		{255, 0, 0, 255},
		{0, 255, 0, 255},
		{0, 0, 255, 255},
		{255, 255, 0, 255},
	}
	var uvQuadData []float32
	for i, d := range quadUVDirs {
		x0 := -1.0 + float32(i)*0.5
		x1 := -1.0 + float32(i+1)*0.5
		uvQuadData = append(uvQuadData,
			x0, -1, 0.5, d[0], d[1], d[2],
			x1, -1, 0.5, d[0], d[1], d[2],
			x1, 1, 0.5, d[0], d[1], d[2],
			x0, -1, 0.5, d[0], d[1], d[2],
			x1, 1, 0.5, d[0], d[1], d[2],
			x0, 1, 0.5, d[0], d[1], d[2],
		)
	}
	vb2, _ := dev.CreateBuffer(nil, gpu.BufferDesc{Usage: gpu.UsageVertex, Data: packFloat32s(uvQuadData...)})
	enc3 := dev.BeginEncoder()
	pass3 := enc3.BeginRenderPass(gpu.RenderPassDesc{})
	pass3.ClearColor(core.RGB(0x000000))
	pass3.SetPipeline(pipe)
	pass3.SetBindGroup(0, bg)
	pass3.SetVertexBuffer(0, vb2, 0)
	pass3.Draw(24, 0)
	pass3.End()
	dev.Submit(enc3.Finish())
	img3 := sr.Image()
	for i, want := range wantUVColors {
		px := img3.RGBAAt(i*15+7, 5)
		if px != want {
			t.Errorf("cubemap +Z quadrant %d pixel = %+v, want %+v", i, px, want)
		}
	}
}

const texArraySampleWGSL = `
struct Params {
	lod: f32,
	useLevel: f32,
	pad0: f32,
	pad1: f32,
}

@group(0) @binding(0) var<uniform> params: Params;
@group(0) @binding(1) var arrTex: texture_2d_array<f32>;
@group(0) @binding(2) var arrSamp: sampler;

struct VertexIn {
	@location(0) pos: vec3<f32>,
	@location(1) uv: vec2<f32>,
	@location(2) layer: i32,
}

struct VertexOut {
	@builtin(position) pos: vec4<f32>,
	@location(0) uv: vec2<f32>,
	@location(1) @interpolate(flat) layer: i32,
}

@vertex
fn vs_main(in: VertexIn) -> VertexOut {
	var out: VertexOut;
	out.pos = vec4<f32>(in.pos, 1.0);
	out.uv = in.uv;
	out.layer = in.layer;
	return out;
}

@fragment
fn fs_main(in: VertexOut) -> @location(0) vec4<f32> {
	if (params.useLevel > 0.5) {
		return textureSampleLevel(arrTex, arrSamp, in.uv, in.layer, params.lod);
	}
	return textureSample(arrTex, arrSamp, in.uv, in.layer);
}
`

func TestTexture2DArraySampling(t *testing.T) {
	mod := gpu.MustCompileWGSL(texArraySampleWGSL)
	sr := gpu.NewSoftwareRenderer(40, 10, gpu.FormatUndefined)
	dev := sr.Device()

	arrTex, err := dev.CreateTexture(nil, gpu.TextureDesc{
		Label:     "tiles",
		Dimension: gpu.Dim2DArray,
		Width:     4,
		Height:    4,
		Layers:    4,
		MipLevels: 2,
		Format:    gpu.FormatRGBA8Unorm,
		Usage:     gpu.TextureSampled | gpu.TextureCopyDst | gpu.TextureCopySrc,
	})
	if err != nil {
		t.Fatalf("CreateTexture Dim2DArray: %v", err)
	}

	layerMip0 := [4]color.RGBA{
		{255, 0, 0, 255},
		{0, 255, 0, 255},
		{0, 0, 255, 255},
		{255, 255, 0, 255},
	}
	layerMip1 := [4]color.RGBA{
		{128, 0, 0, 255},
		{0, 128, 0, 255},
		{0, 0, 128, 255},
		{128, 128, 0, 255},
	}
	for l := uint32(0); l < 4; l++ {
		dev.WriteTextureRGBA(arrTex, 0, l, solidRGBA(4, 4, layerMip0[l]))
		dev.WriteTextureMip(arrTex, 1, l, solidRGBA(2, 2, layerMip1[l]).Pix, 0)
	}

	samp, err := dev.CreateSampler(nil, gpu.SamplerDesc{
		MinFilter:    gpu.FilterNearest,
		MagFilter:    gpu.FilterNearest,
		MipmapFilter: gpu.FilterNearest,
	})
	if err != nil {
		t.Fatalf("CreateSampler: %v", err)
	}
	uBuf, _ := dev.CreateBuffer(nil, gpu.BufferDesc{Usage: gpu.UsageUniform | gpu.UsageCopyDst, Data: packFloat32s(0, 0, 0, 0)})
	bgl, _ := dev.CreateBindGroupLayout(nil, []gpu.BindGroupLayoutEntry{
		{Binding: 0, Visibility: gpu.StageFragment, Type: gpu.BindingUniform},
		{Binding: 1, Visibility: gpu.StageFragment, Type: gpu.BindingTexture},
		{Binding: 2, Visibility: gpu.StageFragment, Type: gpu.BindingSampler},
	})
	bg, _ := dev.CreateBindGroup(nil, bgl, []gpu.BindGroupEntry{
		{Binding: 0, Buffer: uBuf},
		{Binding: 1, Texture: arrTex},
		{Binding: 2, Sampler: samp},
	})
	pipe, err := dev.CreatePipeline(nil, gpu.PipelineDesc{
		Vertex:   gpu.ShaderStage{Module: mod, EntryPoint: "vs_main"},
		Fragment: gpu.ShaderStage{Module: mod, EntryPoint: "fs_main"},
		Layouts:  []*gpu.BindGroupLayout{bgl},
		VertexBuffers: []gpu.VertexBufferLayout{{
			Stride: 6 * 4,
			Attributes: []gpu.VertexAttribute{
				{Location: 0, Offset: 0, Format: gpu.VertexFloat32x3},
				{Location: 1, Offset: 3 * 4, Format: gpu.VertexFloat32x2},
				{Location: 2, Offset: 5 * 4, Format: gpu.VertexSint32},
			},
		}},
		Targets: []gpu.ColorTargetState{{Format: gpu.FormatRGBA8Unorm}},
	})
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}

	// Build 4 strips sampling layers 0..3.
	packVert := func(x, y, z, u, v float32, layer int32) []byte {
		b := make([]byte, 24)
		binary.LittleEndian.PutUint32(b[0:], math.Float32bits(x))
		binary.LittleEndian.PutUint32(b[4:], math.Float32bits(y))
		binary.LittleEndian.PutUint32(b[8:], math.Float32bits(z))
		binary.LittleEndian.PutUint32(b[12:], math.Float32bits(u))
		binary.LittleEndian.PutUint32(b[16:], math.Float32bits(v))
		binary.LittleEndian.PutUint32(b[20:], uint32(layer))
		return b
	}
	var vbytes []byte
	for l := int32(0); l < 4; l++ {
		x0 := -1.0 + float32(l)*0.5
		x1 := -1.0 + float32(l+1)*0.5
		vbytes = append(vbytes, packVert(x0, -1, 0.5, 0.5, 0.5, l)...)
		vbytes = append(vbytes, packVert(x1, -1, 0.5, 0.5, 0.5, l)...)
		vbytes = append(vbytes, packVert(x1, 1, 0.5, 0.5, 0.5, l)...)
		vbytes = append(vbytes, packVert(x0, -1, 0.5, 0.5, 0.5, l)...)
		vbytes = append(vbytes, packVert(x1, 1, 0.5, 0.5, 0.5, l)...)
		vbytes = append(vbytes, packVert(x0, 1, 0.5, 0.5, 0.5, l)...)
	}
	vb, _ := dev.CreateBuffer(nil, gpu.BufferDesc{Usage: gpu.UsageVertex, Data: vbytes})

	// Pass 1: textureSample (mip 0).
	enc := dev.BeginEncoder()
	p := enc.BeginRenderPass(gpu.RenderPassDesc{})
	p.ClearColor(core.RGB(0x000000))
	p.SetPipeline(pipe)
	p.SetBindGroup(0, bg)
	p.SetVertexBuffer(0, vb, 0)
	p.Draw(24, 0)
	p.End()
	dev.Submit(enc.Finish())
	if sr.LastError() != nil {
		t.Fatalf("SoftwareRenderer error: %v", sr.LastError())
	}
	img := sr.Image()
	for l, want := range layerMip0 {
		px := img.RGBAAt(l*10+5, 5)
		if px != want {
			t.Errorf("layer %d mip 0 pixel = %+v, want %+v", l, px, want)
		}
	}

	// Pass 2: textureSampleLevel at LOD 1.0 (mip 1).
	dev.WriteBuffer(uBuf, 0, packFloat32s(1.0, 1.0, 0, 0))
	enc2 := dev.BeginEncoder()
	p2 := enc2.BeginRenderPass(gpu.RenderPassDesc{})
	p2.ClearColor(core.RGB(0x000000))
	p2.SetPipeline(pipe)
	p2.SetBindGroup(0, bg)
	p2.SetVertexBuffer(0, vb, 0)
	p2.Draw(24, 0)
	p2.End()
	dev.Submit(enc2.Finish())
	img2 := sr.Image()
	for l, want := range layerMip1 {
		px := img2.RGBAAt(l*10+5, 5)
		if px != want {
			t.Errorf("layer %d mip 1 pixel = %+v, want %+v", l, px, want)
		}
	}

	// Verify per-layer per-mip ReadTexture.
	var got []byte
	dev.ReadTexture(arrTex, gpu.TextureRegion{MipLevel: 1, ArrayLayer: 2, Width: 2, Height: 2}, func(rgba []byte, err error) {
		if err != nil {
			t.Fatalf("ReadTexture array layer 2 mip 1: %v", err)
		}
		got = rgba
	})
	if len(got) != 2*2*4 || got[0] != 0 || got[1] != 0 || got[2] != 128 || got[3] != 255 {
		t.Errorf("ReadTexture layer 2 mip 1 pixel 0 = %v, want [0 0 128 255]", got[:min(4, len(got))])
	}
}

const mipSampleWGSL = `
struct Params {
	lod: f32,
	pad0: f32,
	pad1: f32,
	pad2: f32,
}

@group(0) @binding(0) var<uniform> params: Params;
@group(0) @binding(1) var tex: texture_2d<f32>;
@group(0) @binding(2) var samp: sampler;

struct VertexIn {
	@location(0) pos: vec3<f32>,
	@location(1) uv: vec2<f32>,
}

struct VertexOut {
	@builtin(position) pos: vec4<f32>,
	@location(0) uv: vec2<f32>,
}

@vertex
fn vs_main(in: VertexIn) -> VertexOut {
	var out: VertexOut;
	out.pos = vec4<f32>(in.pos, 1.0);
	out.uv = in.uv;
	return out;
}

@fragment
fn fs_main(in: VertexOut) -> @location(0) vec4<f32> {
	return textureSampleLevel(tex, samp, in.uv, params.lod);
}
`

func TestMipmapsExplicitAndGeneratedAndLODClamping(t *testing.T) {
	mod := gpu.MustCompileWGSL(mipSampleWGSL)
	sr := gpu.NewSoftwareRenderer(8, 8, gpu.FormatUndefined)
	dev := sr.Device()

	// Descriptor validation checks for cubemaps, arrays, mip levels, and sampler LODs.
	if _, err := dev.CreateTexture(nil, gpu.TextureDesc{Dimension: gpu.DimCube, Width: 8, Height: 4, Format: gpu.FormatRGBA8Unorm, Usage: gpu.TextureSampled}); !errors.Is(err, gpu.ErrInvalidDescriptor) {
		t.Errorf("non-square cubemap err = %v, want ErrInvalidDescriptor", err)
	}
	if _, err := dev.CreateTexture(nil, gpu.TextureDesc{Dimension: gpu.DimCube, Width: 4, Height: 4, Layers: 4, Format: gpu.FormatRGBA8Unorm, Usage: gpu.TextureSampled}); !errors.Is(err, gpu.ErrInvalidDescriptor) {
		t.Errorf("cubemap layers=4 err = %v, want ErrInvalidDescriptor", err)
	}
	if _, err := dev.CreateTexture(nil, gpu.TextureDesc{Dimension: gpu.Dim2D, Width: 4, Height: 4, Layers: 2, Format: gpu.FormatRGBA8Unorm, Usage: gpu.TextureSampled}); !errors.Is(err, gpu.ErrInvalidDescriptor) {
		t.Errorf("2D texture layers=2 err = %v, want ErrInvalidDescriptor", err)
	}
	if _, err := dev.CreateTexture(nil, gpu.TextureDesc{Dimension: gpu.Dim2D, Width: 4, Height: 4, MipLevels: 5, Format: gpu.FormatRGBA8Unorm, Usage: gpu.TextureSampled}); !errors.Is(err, gpu.ErrInvalidDescriptor) {
		t.Errorf("excessive MipLevels err = %v, want ErrInvalidDescriptor", err)
	}
	if _, err := dev.CreateSampler(nil, gpu.SamplerDesc{MinLOD: -1}); !errors.Is(err, gpu.ErrInvalidDescriptor) {
		t.Errorf("negative MinLOD err = %v, want ErrInvalidDescriptor", err)
	}
	if _, err := dev.CreateSampler(nil, gpu.SamplerDesc{MinLOD: 3, MaxLOD: 1}); !errors.Is(err, gpu.ErrInvalidDescriptor) {
		t.Errorf("MaxLOD < MinLOD err = %v, want ErrInvalidDescriptor", err)
	}

	if got := gpu.MaxMipLevels(8, 8); got != 4 {
		t.Fatalf("MaxMipLevels(8,8) = %d, want 4", got)
	}

	// 1. Explicit mip level uploads via WriteTextureMip + textureSampleLevel + trilinear + MinLOD/MaxLOD.
	mipTex, err := dev.CreateTexture(nil, gpu.TextureDesc{
		Label:     "explicit-mips",
		Dimension: gpu.Dim2D,
		Width:     8,
		Height:    8,
		MipLevels: 4,
		Format:    gpu.FormatRGBA8Unorm,
		Usage:     gpu.TextureSampled | gpu.TextureCopyDst | gpu.TextureCopySrc,
	})
	if err != nil {
		t.Fatalf("CreateTexture mipTex: %v", err)
	}
	mipColors := [4]color.RGBA{
		{255, 0, 0, 255},     // mip 0 (8x8): Red
		{0, 255, 0, 255},     // mip 1 (4x4): Green
		{0, 0, 255, 255},     // mip 2 (2x2): Blue
		{255, 255, 255, 255}, // mip 3 (1x1): White
	}
	for lvl := uint32(0); lvl < 4; lvl++ {
		mw, mh := gpu.MipSize(8, 8, lvl)
		dev.WriteTextureMip(mipTex, lvl, 0, solidRGBA(int(mw), int(mh), mipColors[lvl]).Pix, 0)
	}

	uBuf, _ := dev.CreateBuffer(nil, gpu.BufferDesc{Usage: gpu.UsageUniform | gpu.UsageCopyDst, Data: packFloat32s(0, 0, 0, 0)})
	bgl, _ := dev.CreateBindGroupLayout(nil, []gpu.BindGroupLayoutEntry{
		{Binding: 0, Visibility: gpu.StageFragment, Type: gpu.BindingUniform},
		{Binding: 1, Visibility: gpu.StageFragment, Type: gpu.BindingTexture},
		{Binding: 2, Visibility: gpu.StageFragment, Type: gpu.BindingSampler},
	})
	pipe, err := dev.CreatePipeline(nil, gpu.PipelineDesc{
		Vertex:   gpu.ShaderStage{Module: mod, EntryPoint: "vs_main"},
		Fragment: gpu.ShaderStage{Module: mod, EntryPoint: "fs_main"},
		Layouts:  []*gpu.BindGroupLayout{bgl},
		VertexBuffers: []gpu.VertexBufferLayout{{
			Stride: 5 * 4,
			Attributes: []gpu.VertexAttribute{
				{Location: 0, Offset: 0, Format: gpu.VertexFloat32x3},
				{Location: 1, Offset: 3 * 4, Format: gpu.VertexFloat32x2},
			},
		}},
		Targets: []gpu.ColorTargetState{{Format: gpu.FormatRGBA8Unorm}},
	})
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	quadVerts := packFloat32s(
		-1, -1, 0.5, 0.5, 0.5,
		1, -1, 0.5, 0.5, 0.5,
		1, 1, 0.5, 0.5, 0.5,
		-1, -1, 0.5, 0.5, 0.5,
		1, 1, 0.5, 0.5, 0.5,
		-1, 1, 0.5, 0.5, 0.5,
	)
	vb, _ := dev.CreateBuffer(nil, gpu.BufferDesc{Usage: gpu.UsageVertex, Data: quadVerts})

	sampleAt := func(tex *gpu.Texture, sdesc gpu.SamplerDesc, lod float32) color.RGBA {
		s, err := dev.CreateSampler(nil, sdesc)
		if err != nil {
			t.Fatalf("CreateSampler: %v", err)
		}
		defer s.Close()
		bg, err := dev.CreateBindGroup(nil, bgl, []gpu.BindGroupEntry{
			{Binding: 0, Buffer: uBuf},
			{Binding: 1, Texture: tex},
			{Binding: 2, Sampler: s},
		})
		if err != nil {
			t.Fatalf("CreateBindGroup: %v", err)
		}
		defer bg.Close()
		dev.WriteBuffer(uBuf, 0, packFloat32s(lod, 0, 0, 0))
		enc := dev.BeginEncoder()
		p := enc.BeginRenderPass(gpu.RenderPassDesc{})
		p.ClearColor(core.RGB(0x000000))
		p.SetPipeline(pipe)
		p.SetBindGroup(0, bg)
		p.SetVertexBuffer(0, vb, 0)
		p.Draw(6, 0)
		p.End()
		dev.Submit(enc.Finish())
		if sr.LastError() != nil {
			t.Fatalf("SoftwareRenderer error: %v", sr.LastError())
		}
		return sr.Image().RGBAAt(4, 4)
	}

	nearestDesc := gpu.SamplerDesc{MinFilter: gpu.FilterNearest, MagFilter: gpu.FilterNearest, MipmapFilter: gpu.FilterNearest}
	for lvl, want := range mipColors {
		if got := sampleAt(mipTex, nearestDesc, float32(lvl)); got != want {
			t.Errorf("LOD %d pixel = %+v, want %+v", lvl, got, want)
		}
	}

	// Trilinear interpolation at LOD 0.5 blends mip 0 (Red) and mip 1 (Green) -> (~128, ~128, 0, 255).
	trilinearDesc := gpu.SamplerDesc{MinFilter: gpu.FilterLinear, MagFilter: gpu.FilterLinear, MipmapFilter: gpu.FilterLinear}
	gotBlend := sampleAt(mipTex, trilinearDesc, 0.5)
	if gotBlend.R < 124 || gotBlend.R > 132 || gotBlend.G < 124 || gotBlend.G > 132 || gotBlend.B > 2 {
		t.Errorf("trilinear LOD 0.5 pixel = %+v, want ~{128 128 0 255}", gotBlend)
	}

	// MinLOD=1, MaxLOD=2 clamps LOD 0 -> mip 1 (Green) and LOD 3 -> mip 2 (Blue).
	clampedDesc := gpu.SamplerDesc{MinFilter: gpu.FilterNearest, MagFilter: gpu.FilterNearest, MipmapFilter: gpu.FilterNearest, MinLOD: 1.0, MaxLOD: 2.0}
	if got := sampleAt(mipTex, clampedDesc, 0.0); got != mipColors[1] {
		t.Errorf("clamped LOD 0.0 pixel = %+v, want mip 1 Green %+v", got, mipColors[1])
	}
	if got := sampleAt(mipTex, clampedDesc, 3.0); got != mipColors[2] {
		t.Errorf("clamped LOD 3.0 pixel = %+v, want mip 2 Blue %+v", got, mipColors[2])
	}

	// 2. GenerateMipmaps (box filter) on a 4x4 4-quadrant texture + CPU GenerateMipChainRGBA parity.
	baseImg := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			var c color.RGBA
			switch {
			case x < 2 && y < 2:
				c = color.RGBA{255, 0, 0, 255}
			case x >= 2 && y < 2:
				c = color.RGBA{0, 255, 0, 255}
			case x < 2 && y >= 2:
				c = color.RGBA{0, 0, 255, 255}
			default:
				c = color.RGBA{255, 255, 255, 255}
			}
			baseImg.SetRGBA(x, y, c)
		}
	}

	genTex, err := dev.CreateTexture(nil, gpu.TextureDesc{
		Label:     "generated-mips",
		Dimension: gpu.Dim2D,
		Width:     4,
		Height:    4,
		MipLevels: 3,
		Format:    gpu.FormatRGBA8Unorm,
		Usage:     gpu.TextureSampled | gpu.TextureCopyDst | gpu.TextureCopySrc,
	})
	if err != nil {
		t.Fatalf("CreateTexture genTex: %v", err)
	}
	dev.WriteTextureRGBA(genTex, 0, 0, baseImg)
	dev.GenerateMipmaps(genTex)

	// Mip 2 (1x1) must average Red, Green, Blue, White -> (128, 128, 128, 255).
	gotGen := sampleAt(genTex, nearestDesc, 2.0)
	if gotGen != (color.RGBA{128, 128, 128, 255}) {
		t.Errorf("GenerateMipmaps LOD 2 pixel = %+v, want {128 128 128 255}", gotGen)
	}

	var mip1Bytes, mip2Bytes []byte
	dev.ReadTexture(genTex, gpu.TextureRegion{MipLevel: 1, Width: 2, Height: 2}, func(rgba []byte, err error) {
		if err != nil {
			t.Fatalf("ReadTexture mip 1: %v", err)
		}
		mip1Bytes = rgba
	})
	dev.ReadTexture(genTex, gpu.TextureRegion{MipLevel: 2, Width: 1, Height: 1}, func(rgba []byte, err error) {
		if err != nil {
			t.Fatalf("ReadTexture mip 2: %v", err)
		}
		mip2Bytes = rgba
	})
	// Check CPU GenerateMipChainRGBA matches GPU GenerateMipmaps byte-for-byte.
	chain := gpu.GenerateMipChainRGBA(baseImg, 0)
	if len(chain) != 3 {
		t.Fatalf("GenerateMipChainRGBA len = %d, want 3", len(chain))
	}
	if string(mip1Bytes) != string(chain[1].Pix) {
		t.Errorf("GPU mip 1 %v != CPU mip 1 %v", mip1Bytes, chain[1].Pix)
	}
	if string(mip2Bytes) != string(chain[2].Pix) {
		t.Errorf("GPU mip 2 %v != CPU mip 2 %v", mip2Bytes, chain[2].Pix)
	}
}
