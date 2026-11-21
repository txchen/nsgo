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

const computeParticlesWGSL = `
struct StepParams {
	dt: f32,
	gain: f32,
}

struct Vertex {
	pos: vec4<f32>,
	color: vec4<f32>,
}

@group(0) @binding(0) var<uniform> params: StepParams;
@group(0) @binding(1) var<storage, read> vel: array<vec4<f32>>;
@group(0) @binding(2) var<storage, read_write> verts: array<Vertex>;

@compute @workgroup_size(4, 1, 1)
fn step_main(
	@builtin(global_invocation_id) gid: vec3<u32>,
	@builtin(local_invocation_id) lid: vec3<u32>,
	@builtin(workgroup_id) wid: vec3<u32>,
	@builtin(num_workgroups) nwg: vec3<u32>,
) {
	let idx = gid.x;
	let count = arrayLength(&verts);
	if (idx < count) {
		let v = verts[idx];
		var next: Vertex;
		next.pos = v.pos + vel[idx] * params.dt;
		// Encode a deterministic green channel scaled by params.gain and workgroup/num_workgroups sanity check.
		let wgCheck = f32(wid.x + nwg.x - nwg.x) * 0.0 + f32(lid.x) * 0.0;
		next.color = vec4<f32>(v.color.x, params.gain + wgCheck, v.color.z, 1.0);
		verts[idx] = next;
	}
}
`

const storageTexComputeWGSL = `
@group(0) @binding(0) var outTex: texture_storage_2d<rgba8unorm, write>;

@compute @workgroup_size(2, 2, 1)
fn fill_tex(@builtin(global_invocation_id) gid: vec3<u32>) {
	let r = f32(gid.x) / 3.0;
	let g = f32(gid.y) / 3.0;
	textureStore(outTex, vec2<i32>(i32(gid.x), i32(gid.y)), vec4<f32>(r, g, 1.0, 1.0));
}
`

const directVertexColorWGSL = `
struct VertexIn {
	@location(0) pos: vec4<f32>,
	@location(1) color: vec4<f32>,
}

struct VertexOut {
	@builtin(position) pos: vec4<f32>,
	@location(0) color: vec4<f32>,
}

@vertex
fn vs_main(in: VertexIn) -> VertexOut {
	var out: VertexOut;
	out.pos = in.pos;
	out.color = in.color;
	return out;
}

@fragment
fn fs_main(in: VertexOut) -> @location(0) vec4<f32> {
	return in.color;
}
`

func unpackFloat32s(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4 : i*4+4]))
	}
	return out
}

func packUint32s(vals ...uint32) []byte {
	out := make([]byte, len(vals)*4)
	for i, v := range vals {
		binary.LittleEndian.PutUint32(out[i*4:], v)
	}
	return out
}

func TestHeadlessComputeStorageBufferAndRenderPass(t *testing.T) {
	computeMod := gpu.MustCompileWGSL(computeParticlesWGSL)
	renderMod := gpu.MustCompileWGSL(directVertexColorWGSL)
	texComputeMod := gpu.MustCompileWGSL(storageTexComputeWGSL)

	sr := gpu.NewSoftwareRenderer(32, 32, gpu.FormatUndefined)
	dev := sr.Device()

	if !dev.Capabilities().Compute || !dev.SupportsCompute() {
		t.Fatalf("expected SoftwareRenderer to report Compute capability true")
	}
	if !dev.Capabilities().StorageBuffers || !dev.Capabilities().StorageTextures {
		t.Fatalf("expected SoftwareRenderer to support storage buffers and storage textures")
	}

	// Start with 6 vertices (2 triangles) off-screen at x = [-2, 0] and red=0, green=0, blue=0.
	// The compute shader adds velocity (+1.0, 0, 0, 0) * dt (1.0) so the quad moves onto
	// [-1, +1] x [-1, +1] (covering the whole screen) and sets green = 1.0!
	initVerts := packFloat32s(
		// pos (vec4)                 color (vec4)
		-2.0, -1.0, 0.5, 1.0, 0.0, 0.0, 0.0, 1.0,
		0.0, -1.0, 0.5, 1.0, 0.0, 0.0, 0.0, 1.0,
		0.0, 1.0, 0.5, 1.0, 0.0, 0.0, 0.0, 1.0,
		-2.0, -1.0, 0.5, 1.0, 0.0, 0.0, 0.0, 1.0,
		0.0, 1.0, 0.5, 1.0, 0.0, 0.0, 0.0, 1.0,
		-2.0, 1.0, 0.5, 1.0, 0.0, 0.0, 0.0, 1.0,
	)
	velocities := packFloat32s(
		1.0, 0.0, 0.0, 0.0,
		1.0, 0.0, 0.0, 0.0,
		1.0, 0.0, 0.0, 0.0,
		1.0, 0.0, 0.0, 0.0,
		1.0, 0.0, 0.0, 0.0,
		1.0, 0.0, 0.0, 0.0,
	)

	paramsBuf, err := dev.CreateBuffer(nil, gpu.BufferDesc{
		Label: "params",
		Usage: gpu.UsageUniform,
		Data:  packFloat32s(1.0, 1.0), // dt=1.0, gain=1.0
	})
	if err != nil {
		t.Fatalf("CreateBuffer params: %v", err)
	}
	velBuf, err := dev.CreateBuffer(nil, gpu.BufferDesc{
		Label: "velocities",
		Usage: gpu.BufferUsageStorage,
		Data:  velocities,
	})
	if err != nil {
		t.Fatalf("CreateBuffer vel: %v", err)
	}
	// Shared storage + vertex + copy-src buffer: written by @compute, read back via ReadBuffer,
	// AND bound as a vertex buffer in the subsequent render pass!
	vertBuf, err := dev.CreateBuffer(nil, gpu.BufferDesc{
		Label: "particles",
		Usage: gpu.BufferUsageStorage | gpu.UsageVertex | gpu.UsageCopySrc | gpu.UsageCopyDst,
		Data:  initVerts,
	})
	if err != nil {
		t.Fatalf("CreateBuffer verts: %v", err)
	}
	indirectBuf, err := dev.CreateBuffer(nil, gpu.BufferDesc{
		Label: "dispatch_indirect",
		Usage: gpu.BufferUsageIndirect,
		Data:  packUint32s(2, 1, 1), // 2 workgroups * 4 threads = 8 invocations (bounds-checked to 6 verts by arrayLength)
	})
	if err != nil {
		t.Fatalf("CreateBuffer indirect: %v", err)
	}

	cBGL, err := dev.CreateBindGroupLayout(nil, []gpu.BindGroupLayoutEntry{
		{Binding: 0, Visibility: gpu.StageCompute, Type: gpu.BindingUniform},
		{Binding: 1, Visibility: gpu.StageCompute, Type: gpu.BindingTypeReadOnlyStorageBuffer},
		{Binding: 2, Visibility: gpu.StageCompute, Type: gpu.BindingTypeStorageBuffer},
	})
	if err != nil {
		t.Fatalf("CreateBindGroupLayout compute: %v", err)
	}
	cBG, err := dev.CreateBindGroup(nil, cBGL, []gpu.BindGroupEntry{
		{Binding: 0, Buffer: paramsBuf},
		{Binding: 1, Buffer: velBuf},
		{Binding: 2, Buffer: vertBuf},
	})
	if err != nil {
		t.Fatalf("CreateBindGroup compute: %v", err)
	}
	cPipe, err := dev.CreateComputePipeline(nil, gpu.ComputePipelineDesc{
		Label:   "step_particles",
		Compute: gpu.ShaderStage{Module: computeMod, EntryPoint: "step_main"},
		Layouts: []*gpu.BindGroupLayout{cBGL},
	})
	if err != nil {
		t.Fatalf("CreateComputePipeline: %v", err)
	}

	rPipe, err := dev.CreatePipeline(nil, gpu.PipelineDesc{
		Label:    "draw_particles",
		Vertex:   gpu.ShaderStage{Module: renderMod, EntryPoint: "vs_main"},
		Fragment: gpu.ShaderStage{Module: renderMod, EntryPoint: "fs_main"},
		VertexBuffers: []gpu.VertexBufferLayout{{
			Stride: 8 * 4,
			Attributes: []gpu.VertexAttribute{
				{Location: 0, Offset: 0, Format: gpu.VertexFloat32x4},
				{Location: 1, Offset: 4 * 4, Format: gpu.VertexFloat32x4},
			},
		}},
		Targets: []gpu.ColorTargetState{{Format: gpu.FormatRGBA8Unorm}},
	})
	if err != nil {
		t.Fatalf("CreatePipeline render: %v", err)
	}

	// Storage texture compute pass setup.
	storageTex, err := dev.CreateTexture(nil, gpu.TextureDesc{
		Label:  "compute_tex",
		Width:  4,
		Height: 4,
		Format: gpu.FormatRGBA8Unorm,
		Usage:  gpu.TextureStorageBinding | gpu.TextureCopySrc,
	})
	if err != nil {
		t.Fatalf("CreateTexture storage: %v", err)
	}
	tBGL, err := dev.CreateBindGroupLayout(nil, []gpu.BindGroupLayoutEntry{
		{Binding: 0, Visibility: gpu.StageCompute, Type: gpu.BindingTypeStorageTexture},
	})
	if err != nil {
		t.Fatalf("CreateBindGroupLayout storage tex: %v", err)
	}
	tBG, err := dev.CreateBindGroup(nil, tBGL, []gpu.BindGroupEntry{
		{Binding: 0, Texture: storageTex},
	})
	if err != nil {
		t.Fatalf("CreateBindGroup storage tex: %v", err)
	}
	tPipe, err := dev.CreateComputePipeline(nil, gpu.ComputePipelineDesc{
		Label:   "fill_tex",
		Compute: gpu.ShaderStage{Module: texComputeMod, EntryPoint: "fill_tex"},
		Layouts: []*gpu.BindGroupLayout{tBGL},
	})
	if err != nil {
		t.Fatalf("CreateComputePipeline tex: %v", err)
	}

	// Single command encoder: compute pass (indirect dispatch) -> storage texture compute pass -> render pass!
	enc := dev.BeginEncoder()

	cp := enc.BeginComputePass()
	cp.SetPipeline(cPipe)
	cp.SetBindGroup(0, cBG)
	if err := cp.DispatchIndirect(indirectBuf, 0); err != nil {
		t.Fatalf("DispatchIndirect: %v", err)
	}
	cp.SetPipeline(tPipe)
	cp.SetBindGroup(0, tBG)
	if err := cp.Dispatch(2, 2, 1); err != nil {
		t.Fatalf("Dispatch storage tex: %v", err)
	}
	cp.End()

	rp := enc.BeginRenderPass(gpu.RenderPassDesc{})
	rp.ClearColor(core.RGB(0x000000))
	rp.SetPipeline(rPipe)
	rp.SetVertexBuffer(0, vertBuf, 0)
	rp.Draw(6, 0)
	rp.End()

	dev.Submit(enc.Finish())
	if sr.LastError() != nil {
		t.Fatalf("SoftwareRenderer error: %v", sr.LastError())
	}

	// 1. Read back the compute-written storage buffer and verify exact float32 results.
	var gotBytes []byte
	var gotErr error
	dev.ReadBuffer(vertBuf, 0, 0, func(data []byte, err error) {
		gotBytes, gotErr = data, err
	})
	if gotErr != nil {
		t.Fatalf("ReadBuffer: %v", gotErr)
	}
	gotFloats := unpackFloat32s(gotBytes)
	wantFloats := []float32{
		-1.0, -1.0, 0.5, 1.0, 0.0, 1.0, 0.0, 1.0,
		1.0, -1.0, 0.5, 1.0, 0.0, 1.0, 0.0, 1.0,
		1.0, 1.0, 0.5, 1.0, 0.0, 1.0, 0.0, 1.0,
		-1.0, -1.0, 0.5, 1.0, 0.0, 1.0, 0.0, 1.0,
		1.0, 1.0, 0.5, 1.0, 0.0, 1.0, 0.0, 1.0,
		-1.0, 1.0, 0.5, 1.0, 0.0, 1.0, 0.0, 1.0,
	}
	if len(gotFloats) != len(wantFloats) {
		t.Fatalf("ReadBuffer float count = %d, want %d", len(gotFloats), len(wantFloats))
	}
	for i := range wantFloats {
		if math.Abs(float64(gotFloats[i]-wantFloats[i])) > 1e-6 {
			t.Errorf("ReadBuffer float[%d] = %v, want %v", i, gotFloats[i], wantFloats[i])
		}
	}

	// 2. Verify the render pass drew the compute-transformed fullscreen green quad!
	img := sr.Image()
	center := img.RGBAAt(16, 16)
	if center != (color.RGBA{R: 0, G: 255, B: 0, A: 255}) {
		t.Errorf("render pass center pixel = %+v, want green {0 255 0 255} from compute-written vertex buffer", center)
	}
	rightEdge := img.RGBAAt(28, 16)
	if rightEdge != (color.RGBA{R: 0, G: 255, B: 0, A: 255}) {
		t.Errorf("render pass right pixel = %+v, want green (proves x shifted from [-2,0] to [-1,1])", rightEdge)
	}

	// 3. Verify the storage texture written by textureStore!
	var texBytes []byte
	dev.ReadTexture(storageTex, gpu.TextureRegion{Width: 4, Height: 4}, func(rgba []byte, err error) {
		if err != nil {
			t.Fatalf("ReadTexture storageTex: %v", err)
		}
		texBytes = rgba
	})
	if len(texBytes) != 4*4*4 {
		t.Fatalf("storageTex bytes = %d, want 64", len(texBytes))
	}
	// Pixel (3, 3) has r = 3/3 = 255, g = 3/3 = 255, b = 255, a = 255.
	p33 := (3*4 + 3) * 4
	if texBytes[p33] != 255 || texBytes[p33+1] != 255 || texBytes[p33+2] != 255 || texBytes[p33+3] != 255 {
		t.Errorf("storageTex(3,3) = %v, want [255 255 255 255]", texBytes[p33:p33+4])
	}
	// Pixel (0, 0) has r = 0, g = 0, b = 255, a = 255.
	if texBytes[0] != 0 || texBytes[1] != 0 || texBytes[2] != 255 || texBytes[3] != 255 {
		t.Errorf("storageTex(0,0) = %v, want [0 0 255 255]", texBytes[0:4])
	}
}

func TestComputeCapabilityQueryAndUnsupportedRejection(t *testing.T) {
	computeMod := gpu.MustCompileWGSL(computeParticlesWGSL)

	// 1. Standalone SoftwareRenderer simulating WebGL 2 / GLES 3.0 (Compute: false).
	sr := gpu.NewSoftwareRendererWithFeatures(16, 16, gpu.FormatUndefined, gpu.Features{Depth32Float: true})
	dev := sr.Device()

	if dev.Capabilities().Compute {
		t.Fatalf("expected Capabilities().Compute == false on WebGL2/GLES3-like device")
	}
	if dev.SupportsCompute() {
		t.Fatalf("expected SupportsCompute() == false on WebGL2/GLES3-like device")
	}
	if _, err := dev.CreateBuffer(nil, gpu.BufferDesc{Size: 16, Usage: gpu.BufferUsageStorage}); !errors.Is(err, gpu.ErrUnsupported) {
		t.Errorf("CreateBuffer(UsageStorage) = %v, want ErrUnsupported", err)
	}
	if _, err := dev.CreateTexture(nil, gpu.TextureDesc{Width: 4, Height: 4, Format: gpu.FormatRGBA8Unorm, Usage: gpu.TextureStorageBinding}); !errors.Is(err, gpu.ErrUnsupported) {
		t.Errorf("CreateTexture(TextureStorageBinding) = %v, want ErrUnsupported", err)
	}
	if _, err := dev.CreateBindGroupLayout(nil, []gpu.BindGroupLayoutEntry{
		{Binding: 0, Visibility: gpu.StageCompute, Type: gpu.BindingTypeStorageBuffer},
	}); !errors.Is(err, gpu.ErrUnsupported) {
		t.Errorf("CreateBindGroupLayout(StorageBuffer) = %v, want ErrUnsupported", err)
	}
	if _, err := dev.CreateComputePipeline(nil, gpu.ComputePipelineDesc{
		Compute: gpu.ShaderStage{Module: computeMod, EntryPoint: "step_main"},
	}); !errors.Is(err, gpu.ErrUnsupported) {
		t.Errorf("CreateComputePipeline = %v, want ErrUnsupported", err)
	}

	enc := dev.BeginEncoder()
	cp := enc.BeginComputePass()
	if err := cp.Dispatch(1, 1, 1); !errors.Is(err, gpu.ErrUnsupported) {
		t.Errorf("ComputePass.Dispatch on unsupported device = %v, want ErrUnsupported", err)
	}
	indBuf, err := dev.CreateBuffer(nil, gpu.BufferDesc{Size: 12, Usage: gpu.BufferUsageIndirect, Data: packUint32s(1, 1, 1)})
	if err != nil {
		t.Fatalf("CreateBuffer indirect: %v", err)
	}
	if err := cp.DispatchIndirect(indBuf, 0); !errors.Is(err, gpu.ErrUnsupported) {
		t.Errorf("ComputePass.DispatchIndirect on unsupported device = %v, want ErrUnsupported", err)
	}
	cp.End()

	// 2. Mounted gpu.View under nsgotest.NoGPUCompute() reports Compute: false cleanly.
	var viewCaps gpu.DeviceCapabilities
	var viewDispatchErr error
	nsgotest.Start(t, func(cx *core.Scope) core.Node {
		return gpu.View(cx, gpu.ViewProps{
			TestID: "nocompute",
			Style:  core.Style{Width: core.Pt(32), Height: core.Pt(32)},
			OnReady: func(d *gpu.Device) {
				viewCaps = d.Capabilities()
				e := d.BeginEncoder()
				p := e.BeginComputePass()
				viewDispatchErr = p.Dispatch(1, 1, 1)
				p.End()
			},
		})
	}, nsgotest.Size(32, 32), nsgotest.NoGPUCompute())

	if viewCaps.Compute {
		t.Errorf("NoGPUCompute view Capabilities().Compute = true, want false")
	}
	if !errors.Is(viewDispatchErr, gpu.ErrUnsupported) {
		t.Errorf("NoGPUCompute Dispatch error = %v, want ErrUnsupported", viewDispatchErr)
	}

	// 3. Default nsgotest gpu.View supports compute and records DispatchesCount in GPUFrames.
	var gotDispatched []float32
	simpleCompute := gpu.MustCompileWGSL(`
@group(0) @binding(0) var<storage, read_write> data: array<f32>;
@compute @workgroup_size(4)
fn double_it(@builtin(global_invocation_id) gid: vec3<u32>) {
	let i = gid.x;
	if (i < arrayLength(&data)) {
		data[i] = data[i] * 2.0 + 1.0;
	}
}
`)
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node {
		return gpu.View(cx, gpu.ViewProps{
			TestID: "computeview",
			Style:  core.Style{Width: core.Pt(32), Height: core.Pt(32)},
			OnReady: func(d *gpu.Device) {
				if !d.SupportsCompute() {
					t.Fatalf("expected default nsgotest GPU device to support compute")
				}
				buf, err := d.CreateBuffer(cx, gpu.BufferDesc{
					Usage: gpu.BufferUsageStorage | gpu.UsageCopySrc,
					Data:  packFloat32s(1, 2, 3, 4),
				})
				if err != nil {
					t.Fatalf("CreateBuffer: %v", err)
				}
				bgl, err := d.CreateBindGroupLayout(cx, []gpu.BindGroupLayoutEntry{
					{Binding: 0, Visibility: gpu.StageCompute, Type: gpu.BindingTypeStorageBuffer},
				})
				if err != nil {
					t.Fatalf("CreateBindGroupLayout: %v", err)
				}
				bg, err := d.CreateBindGroup(cx, bgl, []gpu.BindGroupEntry{{Binding: 0, Buffer: buf}})
				if err != nil {
					t.Fatalf("CreateBindGroup: %v", err)
				}
				pipe, err := d.CreateComputePipeline(cx, gpu.ComputePipelineDesc{
					Compute: gpu.ShaderStage{Module: simpleCompute, EntryPoint: "double_it"},
					Layouts: []*gpu.BindGroupLayout{bgl},
				})
				if err != nil {
					t.Fatalf("CreateComputePipeline: %v", err)
				}
				e := d.BeginEncoder()
				p := e.BeginComputePass()
				p.SetPipeline(pipe)
				p.SetBindGroup(0, bg)
				if err := p.Dispatch(1, 1, 1); err != nil {
					t.Fatalf("Dispatch: %v", err)
				}
				p.End()
				d.Submit(e.Finish())
				d.ReadBuffer(buf, 0, 0, func(b []byte, err error) {
					if err != nil {
						t.Errorf("ReadBuffer: %v", err)
						return
					}
					gotDispatched = unpackFloat32s(b)
				})
			},
		})
	}, nsgotest.Size(32, 32))

	want := []float32{3, 5, 7, 9}
	if len(gotDispatched) != len(want) {
		t.Fatalf("nsgotest ReadBuffer = %v, want %v", gotDispatched, want)
	}
	for i := range want {
		if gotDispatched[i] != want[i] {
			t.Errorf("gotDispatched[%d] = %v, want %v", i, gotDispatched[i], want[i])
		}
	}
	var totalDispatches int
	for _, fr := range app.GPUFrames() {
		totalDispatches += fr.DispatchesCount
	}
	if totalDispatches < 1 {
		t.Errorf("expected recorded DispatchesCount >= 1, got %+v", app.GPUFrames())
	}
}
