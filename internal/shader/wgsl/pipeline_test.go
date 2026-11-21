package wgsl_test

import (
	"strings"
	"testing"

	"github.com/nsgo-dev/nsgo/internal/shader/wgsl"
	"github.com/nsgo-dev/nsgo/internal/shaderir"
	"github.com/nsgo-dev/nsgo/internal/shaderir/glsl"
	"github.com/nsgo-dev/nsgo/internal/shaderir/hlsl"
	"github.com/nsgo-dev/nsgo/internal/shaderir/msl"
	irwgsl "github.com/nsgo-dev/nsgo/internal/shaderir/wgsl"
)

const litCubeWGSL = `
struct Camera {
	mvp: mat4x4<f32>,
	tint: vec4<f32>,
}

@group(0) @binding(0) var<uniform> cam: Camera;
@group(0) @binding(1) var albedoTex: texture_2d<f32>;
@group(0) @binding(2) var albedoSamp: sampler;

struct VertexIn {
	@location(0) pos: vec3<f32>,
	@location(1) color: vec4<f32>,
	@location(2) uv: vec2<f32>,
}

struct VertexOut {
	@builtin(position) pos: vec4<f32>,
	@location(0) color: vec4<f32>,
	@location(1) uv: vec2<f32>,
}

@vertex
fn vs_main(in: VertexIn) -> VertexOut {
	var out: VertexOut;
	out.pos = cam.mvp * vec4<f32>(in.pos, 1.0);
	out.color = in.color * cam.tint;
	out.uv = in.uv;
	return out;
}

@fragment
fn fs_main(in: VertexOut) -> @location(0) vec4<f32> {
	let texel = textureSample(albedoTex, albedoSamp, in.uv);
	return in.color * texel;
}
`

func TestCompilePipelineAndEmitters(t *testing.T) {
	prog, err := wgsl.CompilePipeline(litCubeWGSL)
	if err != nil {
		t.Fatalf("CompilePipeline: %v", err)
	}
	ir := prog.IR
	if len(ir.Attributes) != 3 {
		t.Fatalf("attributes = %d, want 3", len(ir.Attributes))
	}
	if len(ir.Varyings) != 2 {
		t.Fatalf("varyings = %d, want 2", len(ir.Varyings))
	}
	if len(ir.Bindings) != 3 {
		t.Fatalf("bindings = %d, want 3", len(ir.Bindings))
	}
	if ir.Bindings[0].Kind != shaderir.BindingUniformBuffer || ir.Bindings[0].Group != 0 || ir.Bindings[0].Binding != 0 {
		t.Errorf("unexpected binding 0: %+v", ir.Bindings[0])
	}
	if ir.Bindings[0].Type.SizeBytes() != 80 {
		t.Errorf("Camera struct SizeBytes = %d, want 80", ir.Bindings[0].Type.SizeBytes())
	}

	// MSL emitter
	mslOut, err := msl.CompilePipeline(ir)
	if err != nil {
		t.Fatalf("msl.CompilePipeline: %v", err)
	}
	for _, want := range []string{"vertex StageOut vs_main", "fragment float4 fs_main", "[[attribute(0)]]", "[[position]]", "[[buffer(8)]]", "[[texture(0)]]", "[[sampler(0)]]", ".sample("} {
		if !strings.Contains(mslOut.Source, want) {
			t.Errorf("MSL missing %q:\n%s", want, mslOut.Source)
		}
	}

	// HLSL emitter
	hlslOut, err := hlsl.CompilePipeline(ir)
	if err != nil {
		t.Fatalf("hlsl.CompilePipeline: %v", err)
	}
	for _, want := range []string{"SV_POSITION", "TEXCOORD0", "register(b0)", "register(t0)", "register(s0)", "mul("} {
		if !strings.Contains(hlslOut.VertexSource, want) {
			t.Errorf("HLSL VS missing %q:\n%s", want, hlslOut.VertexSource)
		}
	}
	if !strings.Contains(hlslOut.PixelSource, ".Sample(") {
		t.Errorf("HLSL PS missing .Sample(:\n%s", hlslOut.PixelSource)
	}

	// GLSL ES 3.00 emitter (including WebGPU [0,1] -> OpenGL [-1,1] clip-space Z remap and combined sampler pair table)
	glslOut, err := glsl.CompilePipeline(ir, glsl.GLSLVersionES300)
	if err != nil {
		t.Fatalf("glsl.CompilePipeline: %v", err)
	}
	if !strings.Contains(glslOut.VertexSource, "gl_Position.z = gl_Position.z * 2.0 - gl_Position.w;") {
		t.Errorf("GLSL VS missing depth remap:\n%s", glslOut.VertexSource)
	}
	if !strings.Contains(glslOut.VertexSource, "layout(location = 0) in vec3 a0;") {
		t.Errorf("GLSL VS missing attribute layout:\n%s", glslOut.VertexSource)
	}
	if len(glslOut.SamplerPairs) != 1 || glslOut.SamplerPairs[0].TextureBinding != 1 || glslOut.SamplerPairs[0].SamplerBinding != 2 {
		t.Errorf("GLSL sampler pairs = %+v", glslOut.SamplerPairs)
	}

	// WGSL emitter
	wgslOut, err := irwgsl.CompilePipeline(ir)
	if err != nil {
		t.Fatalf("irwgsl.CompilePipeline: %v", err)
	}
	for _, want := range []string{"@group(0) @binding(0) var<uniform>", "@vertex fn vs_main", "@fragment fn fs_main", "textureSample("} {
		if !strings.Contains(wgslOut, want) {
			t.Errorf("WGSL missing %q:\n%s", want, wgslOut)
		}
	}
}

const cubemapWGSL = `
struct Params {
	lod: f32,
	mixAmt: f32,
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
	let base = textureSample(envCube, envSamp, in.dir);
	let mip = textureSampleLevel(envCube, envSamp, in.dir, params.lod);
	return mix(base, mip, params.mixAmt);
}
`

func TestCompileCubemapAndEmitters(t *testing.T) {
	prog, err := wgsl.CompilePipeline(cubemapWGSL)
	if err != nil {
		t.Fatalf("CompilePipeline cubemap: %v", err)
	}
	ir := prog.IR
	if len(ir.Bindings) != 3 || ir.Bindings[1].Type.Main != shaderir.TextureCube {
		t.Fatalf("unexpected bindings: %+v", ir.Bindings)
	}

	mslOut, err := msl.CompilePipeline(ir)
	if err != nil {
		t.Fatalf("msl.CompilePipeline: %v", err)
	}
	for _, want := range []string{"texturecube<float> B1 [[texture(0)]]", "B1.sample(B2,", "level("} {
		if !strings.Contains(mslOut.Source, want) {
			t.Errorf("MSL missing %q:\n%s", want, mslOut.Source)
		}
	}

	hlslOut, err := hlsl.CompilePipeline(ir)
	if err != nil {
		t.Fatalf("hlsl.CompilePipeline: %v", err)
	}
	for _, want := range []string{"TextureCube B1 : register(t0);", "B1.Sample(B2,", "B1.SampleLevel(B2,"} {
		if !strings.Contains(hlslOut.PixelSource, want) {
			t.Errorf("HLSL PS missing %q:\n%s", want, hlslOut.PixelSource)
		}
	}

	for _, ver := range []glsl.GLSLVersion{glsl.GLSLVersionDefault, glsl.GLSLVersionES300} {
		glslOut, err := glsl.CompilePipeline(ir, ver)
		if err != nil {
			t.Fatalf("glsl.CompilePipeline(%v): %v", ver, err)
		}
		for _, want := range []string{"uniform samplerCube S_t1_s2;", "texture(S_t1_s2,", "textureLod(S_t1_s2,"} {
			if !strings.Contains(glslOut.FragmentSource, want) {
				t.Errorf("GLSL(%v) FS missing %q:\n%s", ver, want, glslOut.FragmentSource)
			}
		}
		if len(glslOut.SamplerPairs) != 1 || glslOut.SamplerPairs[0].TextureType != shaderir.TextureCube {
			t.Errorf("GLSL(%v) SamplerPairs = %+v, want TextureCube", ver, glslOut.SamplerPairs)
		}
	}

	wgslOut, err := irwgsl.CompilePipeline(ir)
	if err != nil {
		t.Fatalf("irwgsl.CompilePipeline: %v", err)
	}
	for _, want := range []string{"texture_cube<f32>", "textureSample(B1, B2,", "textureSampleLevel(B1, B2,"} {
		if !strings.Contains(wgslOut, want) {
			t.Errorf("WGSL missing %q:\n%s", want, wgslOut)
		}
	}
	if _, err := wgsl.CompilePipeline(wgslOut); err != nil {
		t.Errorf("emitted cubemap WGSL did not round-trip: %v\n%s", err, wgslOut)
	}
}

const texArrayMipWGSL = `
struct Params {
	layer: i32,
	lod: f32,
	pad0: f32,
	pad1: f32,
}

@group(0) @binding(0) var<uniform> params: Params;
@group(0) @binding(1) var arrTex: texture_2d_array<f32>;
@group(0) @binding(2) var arrSamp: sampler;

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
	let a = textureSample(arrTex, arrSamp, in.uv, params.layer);
	let b = textureSampleLevel(arrTex, arrSamp, in.uv, params.layer, params.lod);
	let c = textureLoad(arrTex, vec2<i32>(0, 0), params.layer, 0);
	let dims = textureDimensions(arrTex, 1);
	let scale = select(0.5, 1.0, dims.x > 0u);
	return (a + b + c) * scale;
}
`

func TestCompileTexture2DArrayAndMipEmitters(t *testing.T) {
	prog, err := wgsl.CompilePipeline(texArrayMipWGSL)
	if err != nil {
		t.Fatalf("CompilePipeline tex2darray: %v", err)
	}
	ir := prog.IR
	if len(ir.Bindings) != 3 || ir.Bindings[1].Type.Main != shaderir.Texture2DArray {
		t.Fatalf("unexpected bindings: %+v", ir.Bindings)
	}

	mslOut, err := msl.CompilePipeline(ir)
	if err != nil {
		t.Fatalf("msl.CompilePipeline: %v", err)
	}
	for _, want := range []string{
		"texture2d_array<float> B1 [[texture(0)]]",
		"B1.sample(B2, (l2).M1, uint((B0).M0))",
		"B1.sample(B2, (l2).M1, uint((B0).M0), level((B0).M1))",
		"B1.read(uint2(",
		"B1.get_width(uint(1))",
	} {
		if !strings.Contains(mslOut.Source, want) {
			t.Errorf("MSL missing %q:\n%s", want, mslOut.Source)
		}
	}

	hlslOut, err := hlsl.CompilePipeline(ir)
	if err != nil {
		t.Fatalf("hlsl.CompilePipeline: %v", err)
	}
	for _, want := range []string{
		"Texture2DArray B1 : register(t0);",
		"B1.Sample(B2, float3((l2).M1, float((B0).M0)))",
		"B1.SampleLevel(B2, float3((l2).M1, float((B0).M0)), (B0).M1)",
		"B1.Load(int4(",
	} {
		if !strings.Contains(hlslOut.PixelSource, want) {
			t.Errorf("HLSL PS missing %q:\n%s", want, hlslOut.PixelSource)
		}
	}

	for _, ver := range []glsl.GLSLVersion{glsl.GLSLVersionDefault, glsl.GLSLVersionES300} {
		glslOut, err := glsl.CompilePipeline(ir, ver)
		if err != nil {
			t.Fatalf("glsl.CompilePipeline(%v): %v", ver, err)
		}
		wants := []string{
			"uniform sampler2DArray S_t1_s2;",
			"texture(S_t1_s2, vec3((l2).M1, float((B0).M0)))",
			"textureLod(S_t1_s2, vec3((l2).M1, float((B0).M0)), (B0).M1)",
			"texelFetch(S_t1, ivec3(",
			"uvec2(textureSize(S_t1, int(1)).xy)",
		}
		if ver == glsl.GLSLVersionES300 {
			wants = append(wants, "precision highp sampler2DArray;")
		}
		for _, want := range wants {
			if !strings.Contains(glslOut.FragmentSource, want) {
				t.Errorf("GLSL(%v) FS missing %q:\n%s", ver, want, glslOut.FragmentSource)
			}
		}
		if len(glslOut.SamplerPairs) < 1 || glslOut.SamplerPairs[0].TextureType != shaderir.Texture2DArray {
			t.Errorf("GLSL(%v) SamplerPairs = %+v, want Texture2DArray", ver, glslOut.SamplerPairs)
		}
	}

	wgslOut, err := irwgsl.CompilePipeline(ir)
	if err != nil {
		t.Fatalf("irwgsl.CompilePipeline: %v", err)
	}
	for _, want := range []string{
		"texture_2d_array<f32>",
		"textureSample(B1, B2,",
		"textureSampleLevel(B1, B2,",
		"textureLoad(B1,",
		"textureDimensions(B1, 1i)",
	} {
		if !strings.Contains(wgslOut, want) {
			t.Errorf("WGSL missing %q:\n%s", want, wgslOut)
		}
	}
	if _, err := wgsl.CompilePipeline(wgslOut); err != nil {
		t.Errorf("emitted 2D array WGSL did not round-trip: %v\n%s", err, wgslOut)
	}
}

func TestPipelineTextureOverloadErrors(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{
			name: "cube_with_vec2",
			src: `
@group(0) @binding(0) var c: texture_cube<f32>;
@group(0) @binding(1) var s: sampler;
@fragment fn fs_main(@location(0) uv: vec2<f32>) -> @location(0) vec4<f32> {
	return textureSample(c, s, uv);
}`,
		},
		{
			name: "array_missing_layer",
			src: `
@group(0) @binding(0) var a: texture_2d_array<f32>;
@group(0) @binding(1) var s: sampler;
@fragment fn fs_main(@location(0) uv: vec2<f32>) -> @location(0) vec4<f32> {
	return textureSample(a, s, uv);
}`,
		},
		{
			name: "array_float_layer",
			src: `
@group(0) @binding(0) var a: texture_2d_array<f32>;
@group(0) @binding(1) var s: sampler;
@fragment fn fs_main(@location(0) uv: vec2<f32>) -> @location(0) vec4<f32> {
	return textureSample(a, s, uv, 1.5);
}`,
		},
		{
			name: "array_sample_level_missing_lod",
			src: `
@group(0) @binding(0) var a: texture_2d_array<f32>;
@group(0) @binding(1) var s: sampler;
@fragment fn fs_main(@location(0) uv: vec2<f32>) -> @location(0) vec4<f32> {
	return textureSampleLevel(a, s, uv, 1);
}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := wgsl.CompilePipeline(tc.src); err == nil {
				t.Fatalf("expected compile error for %s, got nil", tc.name)
			}
		})
	}
}
