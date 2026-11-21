package driver

import (
	"encoding/binary"
	"fmt"
	"go/constant"
	"image"
	"math"
	"slices"

	"github.com/nsgo-dev/nsgo/internal/shaderir"
)

// GPUSoftReadback holds the completed CPU pixels of a GPUOpReadTexture op.
type GPUSoftReadback struct {
	ID     uint64
	Pixels []byte
	Err    string
}

type softBuffer struct {
	desc GPUBufferDesc
	data []byte
}

type softTexture struct {
	desc    GPUTextureDesc
	samples int
	color   []float32 // w * h * samples * 4 linear RGBA float32
	depth   []float32 // w * h * samples when Format.IsDepth()
	stencil []uint8   // w * h * samples when Format.HasStencil()
}

type softSampler struct {
	desc GPUSamplerDesc
}

type softBindGroupLayout struct {
	desc GPUBindGroupLayoutDesc
}

type softBindGroup struct {
	desc    GPUBindGroupDesc
	entries map[uint32]GPUBindGroupEntry
}

type softPipeline struct {
	desc     GPUPipelineDesc
	combined *shaderir.Program
}

// GPUSoftwareDevice is a deterministic pure-Go software 3D GPU rasterizer
// and shaderir interpreter used by headless nsgotest and unit tests.
type GPUSoftwareDevice struct {
	width, height int
	cfg           GPUViewConfig
	swapSamples   int
	colorBuf      []byte
	swapColor     []float32
	depthBuf      []float32
	stencilBuf    []uint8

	buffers    map[GPUID]*softBuffer
	textures   map[GPUID]*softTexture
	samplers   map[GPUID]*softSampler
	layouts    map[GPUID]*softBindGroupLayout
	bindGroups map[GPUID]*softBindGroup
	pipelines  map[GPUID]*softPipeline

	frames []GPUFrame
}

// NewGPUSoftwareDevice creates a software GPU device for a surface of width x height pixels.
func NewGPUSoftwareDevice(width, height int, cfg GPUViewConfig) *GPUSoftwareDevice {
	if width <= 0 {
		width = 1
	}
	if height <= 0 {
		height = 1
	}
	samples := 1
	if cfg.SampleCount == 4 {
		samples = 4
	}
	if cfg.ColorFormat == GPUFormatUndefined {
		cfg.ColorFormat = GPUFormatRGBA8Unorm
	}
	d := &GPUSoftwareDevice{
		cfg:         cfg,
		swapSamples: samples,
		buffers:     map[GPUID]*softBuffer{},
		textures:    map[GPUID]*softTexture{},
		samplers:    map[GPUID]*softSampler{},
		layouts:     map[GPUID]*softBindGroupLayout{},
		bindGroups:  map[GPUID]*softBindGroup{},
		pipelines:   map[GPUID]*softPipeline{},
	}
	d.Resize(width, height)
	return d
}

// Resize reallocates the swapchain color, depth, and stencil buffers for a new pixel size.
func (d *GPUSoftwareDevice) Resize(width, height int) {
	if width <= 0 {
		width = 1
	}
	if height <= 0 {
		height = 1
	}
	if d.width == width && d.height == height && len(d.colorBuf) == width*height*4 {
		return
	}
	d.width, d.height = width, height
	d.colorBuf = make([]byte, width*height*4)
	nSamp := width * height * max(d.swapSamples, 1)
	d.swapColor = make([]float32, nSamp*4)
	d.depthBuf = make([]float32, nSamp)
	for i := range d.depthBuf {
		d.depthBuf[i] = 1.0
	}
	d.stencilBuf = make([]uint8, nSamp)
}

// Size returns the current swapchain dimensions in pixels.
func (d *GPUSoftwareDevice) Size() (int, int) { return d.width, d.height }

// Frames returns all GPUFrames submitted to this device, oldest first.
func (d *GPUSoftwareDevice) Frames() []GPUFrame { return d.frames }

// ReadBuffer returns a copy of buffer id's contents, if present.
func (d *GPUSoftwareDevice) ReadBuffer(id GPUID) ([]byte, bool) {
	b, ok := d.buffers[id]
	if !ok {
		return nil, false
	}
	return slices.Clone(b.data), true
}

// SurfaceImage returns an independent RGBA snapshot of the swapchain color buffer.
func (d *GPUSoftwareDevice) SurfaceImage() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, d.width, d.height))
	copy(img.Pix, d.colorBuf)
	return img
}

// Submit executes all resource ops, copies, and render passes in frame.
func (d *GPUSoftwareDevice) Submit(frame GPUFrame) ([]GPUSoftReadback, error) {
	d.frames = append(d.frames, frame)
	var readbacks []GPUSoftReadback
	for _, op := range frame.Ops {
		rb, err := d.execOp(op)
		if err != nil {
			return readbacks, err
		}
		if rb != nil {
			readbacks = append(readbacks, *rb)
		}
	}
	for _, cp := range frame.Copies {
		if cp.AfterPass <= 0 {
			if err := d.execCopy(cp); err != nil {
				return readbacks, err
			}
		}
	}
	for pi, pass := range frame.Passes {
		if err := d.execPass(pass); err != nil {
			return readbacks, err
		}
		for _, cp := range frame.Copies {
			if cp.AfterPass == pi+1 {
				if err := d.execCopy(cp); err != nil {
					return readbacks, err
				}
			}
		}
	}
	for _, cp := range frame.Copies {
		if cp.AfterPass > len(frame.Passes) {
			if err := d.execCopy(cp); err != nil {
				return readbacks, err
			}
		}
	}
	return readbacks, nil
}

func (d *GPUSoftwareDevice) execOp(op GPUResourceOp) (*GPUSoftReadback, error) {
	switch op.Kind {
	case GPUOpCreateBuffer:
		if op.Buffer == nil {
			return nil, fmt.Errorf("gpu: missing buffer descriptor")
		}
		size := op.Buffer.Size
		if size == 0 {
			size = uint64(len(op.Buffer.Data))
		}
		buf := make([]byte, size)
		copy(buf, op.Buffer.Data)
		d.buffers[op.ID] = &softBuffer{desc: *op.Buffer, data: buf}
	case GPUOpWriteBuffer:
		b, ok := d.buffers[op.ID]
		if !ok {
			return nil, fmt.Errorf("gpu: write to unknown buffer %d", op.ID)
		}
		if op.Offset+uint64(len(op.Data)) > uint64(len(b.data)) {
			return nil, fmt.Errorf("gpu: buffer %d write out of bounds (%d+%d > %d)", op.ID, op.Offset, len(op.Data), len(b.data))
		}
		copy(b.data[op.Offset:], op.Data)
	case GPUOpDestroyBuffer:
		delete(d.buffers, op.ID)
	case GPUOpCreateTexture:
		if op.Texture == nil {
			return nil, fmt.Errorf("gpu: missing texture descriptor")
		}
		w, h := int(op.Texture.Width), int(op.Texture.Height)
		if w <= 0 || h <= 0 {
			return nil, fmt.Errorf("gpu: invalid texture size %dx%d", w, h)
		}
		samples := max(op.Texture.SampleCount, 1)
		if samples != 1 && samples != 4 {
			return nil, fmt.Errorf("gpu: unsupported sample count %d", samples)
		}
		st := &softTexture{desc: *op.Texture, samples: samples}
		nSamp := w * h * samples
		if op.Texture.Format.IsDepth() {
			st.depth = make([]float32, nSamp)
			for i := range st.depth {
				st.depth[i] = 1.0
			}
		}
		if op.Texture.Format.HasStencil() {
			st.stencil = make([]uint8, nSamp)
		}
		if !op.Texture.Format.IsDepthStencil() {
			st.color = make([]float32, nSamp*4)
		}
		d.textures[op.ID] = st
	case GPUOpWriteTexture:
		tex, ok := d.textures[op.TextureDst.Texture]
		if !ok {
			return nil, fmt.Errorf("gpu: write to unknown texture %d", op.TextureDst.Texture)
		}
		if tex.color == nil {
			return nil, nil
		}
		w, h := int(tex.desc.Width), int(tex.desc.Height)
		src := op.Data
		if op.DataLayout.Offset < uint64(len(src)) {
			src = src[op.DataLayout.Offset:]
		}
		bpp := int(tex.desc.Format.BytesPerPixel())
		if bpp == 0 {
			bpp = 4
		}
		srcStride := int(op.DataLayout.BytesPerRow)
		if srcStride == 0 {
			srcStride = w * bpp
		}
		ox, oy := int(op.TextureDst.OriginX), int(op.TextureDst.OriginY)
		for y := 0; y+oy < h; y++ {
			rowStart := y * srcStride
			if rowStart >= len(src) {
				break
			}
			for x := 0; x+ox < w; x++ {
				si := rowStart + x*bpp
				if si+bpp > len(src) {
					break
				}
				c := quantizeColorForFormat(tex.desc.Format, decodePixelBytes(src[si:si+bpp], tex.desc.Format))
				pixIdx := (y+oy)*w + (x + ox)
				for s := 0; s < tex.samples; s++ {
					di := (pixIdx*tex.samples + s) * 4
					tex.color[di+0] = c[0]
					tex.color[di+1] = c[1]
					tex.color[di+2] = c[2]
					tex.color[di+3] = c[3]
				}
			}
		}
	case GPUOpDestroyTexture:
		delete(d.textures, op.ID)
	case GPUOpCreateSampler:
		if op.Sampler == nil {
			return nil, fmt.Errorf("gpu: missing sampler descriptor")
		}
		d.samplers[op.ID] = &softSampler{desc: *op.Sampler}
	case GPUOpDestroySampler:
		delete(d.samplers, op.ID)
	case GPUOpCreateBindGroupLayout:
		if op.BindGroupLayout == nil {
			return nil, fmt.Errorf("gpu: missing bind group layout descriptor")
		}
		d.layouts[op.ID] = &softBindGroupLayout{desc: *op.BindGroupLayout}
	case GPUOpDestroyBindGroupLayout:
		delete(d.layouts, op.ID)
	case GPUOpCreateBindGroup:
		if op.BindGroup == nil {
			return nil, fmt.Errorf("gpu: missing bind group descriptor")
		}
		m := make(map[uint32]GPUBindGroupEntry, len(op.BindGroup.Entries))
		for _, e := range op.BindGroup.Entries {
			m[e.Binding] = e
		}
		d.bindGroups[op.ID] = &softBindGroup{desc: *op.BindGroup, entries: m}
	case GPUOpDestroyBindGroup:
		delete(d.bindGroups, op.ID)
	case GPUOpCreatePipeline:
		if op.Pipeline == nil {
			return nil, fmt.Errorf("gpu: missing pipeline descriptor")
		}
		comb := mergeShaderPrograms(op.Pipeline.Vertex.IR, op.Pipeline.Fragment.IR)
		d.pipelines[op.ID] = &softPipeline{desc: *op.Pipeline, combined: comb}
	case GPUOpDestroyPipeline:
		delete(d.pipelines, op.ID)
	case GPUOpReadTexture:
		if op.ReadbackSurface || op.ID == 0 {
			tw, th := d.width, d.height
			rx, ry := int(op.ReadbackRegion.X), int(op.ReadbackRegion.Y)
			rw, rh := int(op.ReadbackRegion.Width), int(op.ReadbackRegion.Height)
			if rw == 0 {
				rw = tw - rx
			}
			if rh == 0 {
				rh = th - ry
			}
			if rx < 0 || ry < 0 || rw <= 0 || rh <= 0 || rx+rw > tw || ry+rh > th {
				return &GPUSoftReadback{ID: op.ReadbackID, Err: "readback region out of bounds"}, nil
			}
			out := make([]byte, rw*rh*4)
			for y := 0; y < rh; y++ {
				copy(out[y*rw*4:(y+1)*rw*4], d.colorBuf[((ry+y)*tw+rx)*4:((ry+y)*tw+rx+rw)*4])
			}
			return &GPUSoftReadback{ID: op.ReadbackID, Pixels: out}, nil
		}
		tex, ok := d.textures[op.ID]
		if !ok {
			return &GPUSoftReadback{ID: op.ReadbackID, Err: fmt.Sprintf("unknown texture %d", op.ID)}, nil
		}
		if tex.color == nil {
			return &GPUSoftReadback{ID: op.ReadbackID, Err: "cannot read back non-color texture"}, nil
		}
		tw, th := int(tex.desc.Width), int(tex.desc.Height)
		rx, ry := int(op.ReadbackRegion.X), int(op.ReadbackRegion.Y)
		rw, rh := int(op.ReadbackRegion.Width), int(op.ReadbackRegion.Height)
		if rw == 0 {
			rw = tw - rx
		}
		if rh == 0 {
			rh = th - ry
		}
		if rx < 0 || ry < 0 || rw <= 0 || rh <= 0 || rx+rw > tw || ry+rh > th {
			return &GPUSoftReadback{ID: op.ReadbackID, Err: "readback region out of bounds"}, nil
		}
		bpp := readbackBytesPerPixel(tex.desc.Format)
		out := make([]byte, rw*rh*bpp)
		invSamp := float32(1.0) / float32(max(tex.samples, 1))
		for y := 0; y < rh; y++ {
			for x := 0; x < rw; x++ {
				pixIdx := (ry+y)*tw + (rx + x)
				var avg [4]float32
				for s := 0; s < tex.samples; s++ {
					si := (pixIdx*tex.samples + s) * 4
					avg[0] += tex.color[si+0]
					avg[1] += tex.color[si+1]
					avg[2] += tex.color[si+2]
					avg[3] += tex.color[si+3]
				}
				avg[0] *= invSamp
				avg[1] *= invSamp
				avg[2] *= invSamp
				avg[3] *= invSamp
				di := (y*rw + x) * bpp
				encodePixelBytes(out[di:di+bpp], tex.desc.Format, avg)
			}
		}
		return &GPUSoftReadback{ID: op.ReadbackID, Pixels: out}, nil
	}
	return nil, nil
}

func (d *GPUSoftwareDevice) execCopy(cp GPUCopyOp) error {
	switch cp.Kind {
	case GPUCopyBufferToBuffer:
		s, ok1 := d.buffers[cp.SrcID]
		t, ok2 := d.buffers[cp.DstID]
		if !ok1 || !ok2 {
			return fmt.Errorf("gpu: CopyBufferToBuffer references unknown buffer")
		}
		if cp.SrcOffset+cp.Size > uint64(len(s.data)) || cp.DstOffset+cp.Size > uint64(len(t.data)) {
			return fmt.Errorf("gpu: CopyBufferToBuffer out of bounds")
		}
		copy(t.data[cp.DstOffset:cp.DstOffset+cp.Size], s.data[cp.SrcOffset:cp.SrcOffset+cp.Size])
	case GPUCopyTextureToTexture:
		s, ok1 := d.textures[cp.SrcTex.Texture]
		t, ok2 := d.textures[cp.DstTex.Texture]
		if !ok1 || !ok2 || s.color == nil || t.color == nil {
			return fmt.Errorf("gpu: CopyTextureToTexture references unknown texture")
		}
		sw, dw := int(s.desc.Width), int(t.desc.Width)
		for y := 0; y < int(cp.Height); y++ {
			for x := 0; x < int(cp.Width); x++ {
				sp := (int(cp.SrcTex.OriginY)+y)*sw + int(cp.SrcTex.OriginX) + x
				dp := (int(cp.DstTex.OriginY)+y)*dw + int(cp.DstTex.OriginX) + x
				si := sp * s.samples * 4
				c := quantizeColorForFormat(t.desc.Format, [4]float32{s.color[si], s.color[si+1], s.color[si+2], s.color[si+3]})
				for k := 0; k < t.samples; k++ {
					di := (dp*t.samples + k) * 4
					t.color[di+0] = c[0]
					t.color[di+1] = c[1]
					t.color[di+2] = c[2]
					t.color[di+3] = c[3]
				}
			}
		}
	case GPUCopyResolveTexture:
		s, ok1 := d.textures[cp.SrcTex.Texture]
		t, ok2 := d.textures[cp.DstTex.Texture]
		if !ok1 || !ok2 || s.color == nil || t.color == nil {
			return fmt.Errorf("gpu: ResolveTexture references unknown texture")
		}
		resolveColorBuffers(s.color, s.samples, int(s.desc.Width), int(s.desc.Height), t.color, t.desc.Format, int(t.desc.Width), int(t.desc.Height))
	}
	return nil
}

func mergeShaderPrograms(vs, fs *shaderir.Program) *shaderir.Program {
	if vs == fs && vs != nil {
		return vs
	}
	if vs == nil {
		return fs
	}
	if fs == nil {
		return vs
	}
	// Separate VS and FS modules: keep VS's attributes/bindings/VertexFunc and FS's FragmentFunc.
	out := *vs
	out.FragmentFunc = fs.FragmentFunc
	out.FragmentOutputs = fs.FragmentOutputs
	out.FragmentOutputMeta = fs.FragmentOutputMeta
	if len(fs.Varyings) > len(out.Varyings) {
		out.Varyings = fs.Varyings
		out.VaryingMeta = fs.VaryingMeta
	}
	return &out
}

type softColorTarget struct {
	color      []float32
	format     GPUTextureFormat
	samples    int
	width      int
	height     int
	isSwap     bool
	resolveTex *softTexture
}

var (
	msaa1Offsets = [1][2]float32{{0.5, 0.5}}
	msaa4Offsets = [4][2]float32{
		{0.375, 0.125},
		{0.875, 0.375},
		{0.125, 0.625},
		{0.625, 0.875},
	}
)

func (d *GPUSoftwareDevice) execPass(pass GPUPass) error {
	var colorTargets []softColorTarget
	var depthPix []float32
	var stencilPix []uint8
	var dsFormat GPUTextureFormat
	var targetW, targetH int
	passSamples := 1

	attachSwap := func(ca GPUColorAttachment) {
		ct := softColorTarget{
			color:   d.swapColor,
			format:  d.cfg.ColorFormat,
			samples: max(d.swapSamples, 1),
			width:   d.width,
			height:  d.height,
			isSwap:  true,
		}
		if ct.format == GPUFormatUndefined {
			ct.format = GPUFormatRGBA8Unorm
		}
		if ca.LoadOp == GPULoadClear {
			r, g, b, a := ca.ClearColor.Components()
			clearColorBuffer(ct.color, ct.format, [4]float32{float32(r), float32(g), float32(b), float32(a)})
		}
		colorTargets = append(colorTargets, ct)
		if targetW == 0 {
			targetW, targetH, passSamples = ct.width, ct.height, ct.samples
		}
	}

	if len(pass.ColorAttachments) > 0 {
		for _, ca := range pass.ColorAttachments {
			if ca.Texture == 0 {
				attachSwap(ca)
				continue
			}
			tex, ok := d.textures[ca.Texture]
			if !ok {
				return fmt.Errorf("gpu: render pass color attachment %d not found", ca.Texture)
			}
			if tex.color == nil {
				return fmt.Errorf("gpu: render pass color attachment %d is not a color texture", ca.Texture)
			}
			var resolveTex *softTexture
			if ca.ResolveTarget != 0 {
				rt, ok := d.textures[ca.ResolveTarget]
				if !ok {
					return fmt.Errorf("gpu: render pass resolve target %d not found", ca.ResolveTarget)
				}
				if rt.color == nil {
					return fmt.Errorf("gpu: render pass resolve target %d is not a color texture", ca.ResolveTarget)
				}
				resolveTex = rt
			}
			ct := softColorTarget{
				color:      tex.color,
				format:     tex.desc.Format,
				samples:    max(tex.samples, 1),
				width:      int(tex.desc.Width),
				height:     int(tex.desc.Height),
				resolveTex: resolveTex,
			}
			if ca.LoadOp == GPULoadClear {
				r, g, b, a := ca.ClearColor.Components()
				clearColorBuffer(ct.color, ct.format, [4]float32{float32(r), float32(g), float32(b), float32(a)})
			}
			colorTargets = append(colorTargets, ct)
			if targetW == 0 {
				targetW, targetH, passSamples = ct.width, ct.height, ct.samples
			}
		}
	} else if pass.DepthStencil == nil || pass.DepthStencil.Texture == 0 {
		attachSwap(GPUColorAttachment{LoadOp: GPULoadLoad, StoreOp: GPUStoreStore})
	}

	if pass.DepthStencil != nil {
		ds := pass.DepthStencil
		if ds.Texture == 0 {
			dsFormat = d.cfg.DepthFormat
			if dsFormat.IsDepth() {
				depthPix = d.depthBuf
			}
			if dsFormat.HasStencil() {
				stencilPix = d.stencilBuf
			}
			if targetW == 0 {
				targetW, targetH, passSamples = d.width, d.height, max(d.swapSamples, 1)
			}
		} else if tex, ok := d.textures[ds.Texture]; ok {
			dsFormat = tex.desc.Format
			depthPix = tex.depth
			stencilPix = tex.stencil
			if targetW == 0 {
				targetW, targetH, passSamples = int(tex.desc.Width), int(tex.desc.Height), max(tex.samples, 1)
			}
		} else {
			return fmt.Errorf("gpu: render pass depth-stencil attachment %d not found", ds.Texture)
		}
		if depthPix != nil && ds.DepthLoadOp == GPULoadClear {
			for i := range depthPix {
				depthPix[i] = ds.ClearDepth
			}
		}
		if stencilPix != nil && ds.StencilLoadOp == GPULoadClear {
			v := uint8(ds.ClearStencil & 0xFF)
			for i := range stencilPix {
				stencilPix[i] = v
			}
		}
	} else if d.cfg.DepthFormat.IsDepthStencil() && len(colorTargets) > 0 && colorTargets[0].isSwap {
		dsFormat = d.cfg.DepthFormat
		if dsFormat.IsDepth() {
			depthPix = d.depthBuf
		}
		if dsFormat.HasStencil() {
			stencilPix = d.stencilBuf
		}
	}

	// Render pass state
	vpX, vpY, vpW, vpH := float32(0), float32(0), float32(targetW), float32(targetH)
	vpMinD, vpMaxD := float32(0), float32(1)
	scX, scY, scW, scH := int32(0), int32(0), int32(targetW), int32(targetH)
	var pipe *softPipeline
	bindGroups := map[uint32]*softBindGroup{}
	type vbSlot struct {
		buf    *softBuffer
		offset uint64
	}
	vbufs := map[uint32]vbSlot{}
	var idxBuf *softBuffer
	var idxFmt GPUIndexFormat
	var idxOff uint64
	blendConst := [4]float32{0, 0, 0, 0}
	var stencilRef uint8

	draw := func(indexed bool, count, instances, first, firstInst uint32, baseVertex int32) error {
		if pipe == nil || pipe.combined == nil || pipe.combined.VertexFunc.Block == nil {
			return fmt.Errorf("gpu: draw without bound render pipeline")
		}
		if count == 0 || instances == 0 {
			return nil
		}
		prog := pipe.combined
		ctx := &softDrawCtx{
			dev:          d,
			prog:         prog,
			pipe:         pipe,
			bindGroups:   bindGroups,
			colorTargets: colorTargets,
			depthPix:     depthPix,
			stencilPix:   stencilPix,
			dsFormat:     dsFormat,
			sampleCount:  passSamples,
			targetW:      targetW,
			targetH:      targetH,
			vpX:          vpX, vpY: vpY, vpW: vpW, vpH: vpH,
			vpMinD: vpMinD, vpMaxD: vpMaxD,
			scX: int(scX), scY: int(scY), scW: int(scW), scH: int(scH),
			blendConst: blendConst,
			stencilRef: stencilRef,
		}
		fetchIndex := func(i uint32) (uint32, error) {
			if !indexed {
				return first + i, nil
			}
			if idxBuf == nil {
				return 0, fmt.Errorf("gpu: DrawIndexed without bound index buffer")
			}
			if idxFmt == GPUIndexUint16 {
				off := idxOff + uint64(first+i)*2
				if off+2 > uint64(len(idxBuf.data)) {
					return 0, fmt.Errorf("gpu: index buffer out of bounds")
				}
				return uint32(int32(binary.LittleEndian.Uint16(idxBuf.data[off:off+2])) + baseVertex), nil
			}
			off := idxOff + uint64(first+i)*4
			if off+4 > uint64(len(idxBuf.data)) {
				return 0, fmt.Errorf("gpu: index buffer out of bounds")
			}
			return uint32(int32(binary.LittleEndian.Uint32(idxBuf.data[off:off+4])) + baseVertex), nil
		}

		fetchAttrs := func(vertIdx, instIdx uint32) []irVal {
			attrs := make([]irVal, len(prog.Attributes))
			for i, at := range prog.Attributes {
				attrs[i] = zeroIRVal(at)
			}
			for slotIdx, layout := range pipe.desc.VertexBuffers {
				vb, ok := vbufs[uint32(slotIdx)]
				if !ok || vb.buf == nil {
					continue
				}
				elemIdx := vertIdx
				if layout.StepMode == GPUStepInstance {
					elemIdx = instIdx
				}
				base := vb.offset + uint64(elemIdx)*layout.Stride
				for _, attr := range layout.Attributes {
					// Match attribute location to IR attribute index.
					irIdx := -1
					for ai := range prog.Attributes {
						loc := uint32(ai)
						if ai < len(prog.AttributeMeta) {
							loc = prog.AttributeMeta[ai].Location
						}
						if loc == attr.Location {
							irIdx = ai
							break
						}
					}
					if irIdx < 0 {
						continue
					}
					off := base + attr.Offset
					if off+attr.Format.ByteSize() > uint64(len(vb.buf.data)) {
						continue
					}
					attrs[irIdx] = decodeVertexAttr(vb.buf.data[off:], attr.Format)
				}
			}
			return attrs
		}

		for inst := uint32(0); inst < instances; inst++ {
			instID := firstInst + inst
			switch pipe.desc.Primitive.Topology {
			case GPUTopologyTriangleStrip:
				if count < 3 {
					continue
				}
				for t := uint32(0); t+2 < count; t++ {
					i0, err := fetchIndex(t)
					if err != nil {
						return err
					}
					i1, err := fetchIndex(t + 1)
					if err != nil {
						return err
					}
					i2, err := fetchIndex(t + 2)
					if err != nil {
						return err
					}
					if t%2 == 1 {
						i1, i2 = i2, i1
					}
					v0 := ctx.runVertex(fetchAttrs(i0, instID), i0, instID)
					v1 := ctx.runVertex(fetchAttrs(i1, instID), i1, instID)
					v2 := ctx.runVertex(fetchAttrs(i2, instID), i2, instID)
					ctx.rasterTriangle(v0, v1, v2)
				}
			default: // TriangleList
				for t := uint32(0); t+2 < count; t += 3 {
					i0, err := fetchIndex(t)
					if err != nil {
						return err
					}
					i1, err := fetchIndex(t + 1)
					if err != nil {
						return err
					}
					i2, err := fetchIndex(t + 2)
					if err != nil {
						return err
					}
					v0 := ctx.runVertex(fetchAttrs(i0, instID), i0, instID)
					v1 := ctx.runVertex(fetchAttrs(i1, instID), i1, instID)
					v2 := ctx.runVertex(fetchAttrs(i2, instID), i2, instID)
					ctx.rasterTriangle(v0, v1, v2)
				}
			}
		}
		return nil
	}

	for _, cmd := range pass.Commands {
		switch cmd.Kind {
		case GPUCmdSetViewport:
			vpX, vpY, vpW, vpH, vpMinD, vpMaxD = cmd.F0, cmd.F1, cmd.F2, cmd.F3, cmd.F4, cmd.F5
		case GPUCmdSetScissor:
			scX, scY, scW, scH = cmd.I0, cmd.I1, cmd.I2, cmd.I3
		case GPUCmdSetPipeline:
			p, ok := d.pipelines[cmd.ID]
			if !ok {
				return fmt.Errorf("gpu: unknown pipeline %d", cmd.ID)
			}
			pipe = p
		case GPUCmdSetBindGroup:
			bg, ok := d.bindGroups[cmd.ID]
			if !ok {
				return fmt.Errorf("gpu: unknown bind group %d", cmd.ID)
			}
			bindGroups[cmd.Slot] = bg
		case GPUCmdSetVertexBuffer:
			b, ok := d.buffers[cmd.ID]
			if !ok {
				return fmt.Errorf("gpu: unknown vertex buffer %d", cmd.ID)
			}
			vbufs[cmd.Slot] = vbSlot{buf: b, offset: cmd.Offset}
		case GPUCmdSetIndexBuffer:
			b, ok := d.buffers[cmd.ID]
			if !ok {
				return fmt.Errorf("gpu: unknown index buffer %d", cmd.ID)
			}
			idxBuf, idxFmt, idxOff = b, cmd.IndexFormat, cmd.Offset
		case GPUCmdSetStencilReference:
			stencilRef = uint8(cmd.U0 & 0xFF)
		case GPUCmdSetBlendConstant:
			blendConst = [4]float32{cmd.F0, cmd.F1, cmd.F2, cmd.F3}
		case GPUCmdDraw:
			if err := draw(false, cmd.U0, 1, cmd.U1, 0, 0); err != nil {
				return err
			}
		case GPUCmdDrawIndexed:
			if err := draw(true, cmd.U0, 1, cmd.U1, 0, cmd.I0); err != nil {
				return err
			}
		case GPUCmdDrawInstanced:
			if err := draw(false, cmd.U0, cmd.U1, cmd.U2, cmd.U3, 0); err != nil {
				return err
			}
		case GPUCmdDrawIndexedInstanced:
			if err := draw(true, cmd.U0, cmd.U1, cmd.U2, cmd.U3, cmd.I0); err != nil {
				return err
			}
		}
	}

	// End-of-pass MSAA resolve and swapchain snapshot flush.
	for _, ct := range colorTargets {
		if ct.isSwap {
			d.flushSwapchain()
		} else if ct.resolveTex != nil {
			resolveColorBuffers(ct.color, ct.samples, ct.width, ct.height, ct.resolveTex.color, ct.resolveTex.desc.Format, int(ct.resolveTex.desc.Width), int(ct.resolveTex.desc.Height))
		}
	}
	return nil
}

func (d *GPUSoftwareDevice) flushSwapchain() {
	n := d.width * d.height
	samples := max(d.swapSamples, 1)
	invSamp := float32(1.0) / float32(samples)
	fmtVal := d.cfg.ColorFormat
	if fmtVal == GPUFormatUndefined {
		fmtVal = GPUFormatRGBA8Unorm
	}
	for p := 0; p < n; p++ {
		var avg [4]float32
		for s := 0; s < samples; s++ {
			si := (p*samples + s) * 4
			avg[0] += d.swapColor[si+0]
			avg[1] += d.swapColor[si+1]
			avg[2] += d.swapColor[si+2]
			avg[3] += d.swapColor[si+3]
		}
		avg[0] *= invSamp
		avg[1] *= invSamp
		avg[2] *= invSamp
		avg[3] *= invSamp
		di := p * 4
		if fmtVal.IsSRGB() {
			d.colorBuf[di+0] = floatToByte(linearToSRGB(avg[0]))
			d.colorBuf[di+1] = floatToByte(linearToSRGB(avg[1]))
			d.colorBuf[di+2] = floatToByte(linearToSRGB(avg[2]))
			d.colorBuf[di+3] = floatToByte(avg[3])
		} else {
			d.colorBuf[di+0] = floatToByte(avg[0])
			d.colorBuf[di+1] = floatToByte(avg[1])
			d.colorBuf[di+2] = floatToByte(avg[2])
			d.colorBuf[di+3] = floatToByte(avg[3])
		}
	}
}

func resolveColorBuffers(src []float32, srcSamples, srcW, srcH int, dst []float32, dstFmt GPUTextureFormat, dstW, dstH int) {
	w := min(srcW, dstW)
	h := min(srcH, dstH)
	samples := max(srcSamples, 1)
	invSamp := float32(1.0) / float32(samples)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sp := y*srcW + x
			var avg [4]float32
			for s := 0; s < samples; s++ {
				si := (sp*samples + s) * 4
				avg[0] += src[si+0]
				avg[1] += src[si+1]
				avg[2] += src[si+2]
				avg[3] += src[si+3]
			}
			avg[0] *= invSamp
			avg[1] *= invSamp
			avg[2] *= invSamp
			avg[3] *= invSamp
			q := quantizeColorForFormat(dstFmt, avg)
			di := (y*dstW + x) * 4
			dst[di+0] = q[0]
			dst[di+1] = q[1]
			dst[di+2] = q[2]
			dst[di+3] = q[3]
		}
	}
}

func clearColorBuffer(buf []float32, format GPUTextureFormat, clearRGBA [4]float32) {
	q := quantizeColorForFormat(format, clearRGBA)
	for i := 0; i+3 < len(buf); i += 4 {
		buf[i+0] = q[0]
		buf[i+1] = q[1]
		buf[i+2] = q[2]
		buf[i+3] = q[3]
	}
}

// --- Software rasterizer & shaderir VM ---

type softDrawCtx struct {
	dev          *GPUSoftwareDevice
	prog         *shaderir.Program
	pipe         *softPipeline
	bindGroups   map[uint32]*softBindGroup
	colorTargets []softColorTarget
	depthPix     []float32
	stencilPix   []uint8
	dsFormat     GPUTextureFormat
	sampleCount  int
	targetW      int
	targetH      int
	vpX, vpY     float32
	vpW, vpH     float32
	vpMinD       float32
	vpMaxD       float32
	scX, scY     int
	scW, scH     int
	blendConst   [4]float32
	stencilRef   uint8
}

type shadedVertex struct {
	pos      [4]float32
	varyings []irVal
}

func (c *softDrawCtx) runVertex(attrs []irVal, vertID, instID uint32) shadedVertex {
	p := c.prog
	na, nv := len(p.Attributes), len(p.Varyings)
	pseudo := na + nv + 1
	locals := make([]irVal, pseudo+len(p.VertexFunc.Block.LocalVars))
	copy(locals, attrs)
	locals[na] = irVal{typ: shaderir.Type{Main: shaderir.Vec4}, f: [16]float32{0, 0, 0, 1}}
	for i, vt := range p.Varyings {
		locals[na+1+i] = zeroIRVal(vt)
	}
	for i, lt := range p.VertexFunc.Block.LocalVars {
		locals[pseudo+i] = zeroIRVal(lt)
	}
	vm := &irVM{
		ctx:    c,
		locals: locals,
		vertID: vertID,
		instID: instID,
	}
	vm.execBlock(p.VertexFunc.Block)
	out := shadedVertex{
		pos:      [4]float32{locals[na].f[0], locals[na].f[1], locals[na].f[2], locals[na].f[3]},
		varyings: make([]irVal, nv),
	}
	for i := range nv {
		out.varyings[i] = locals[na+1+i]
	}
	return out
}

const maxSoftColorAttachments = 4

func (c *softDrawCtx) runFragment(fragCoord [4]float32, varyings []irVal, frontFacing bool) ([maxSoftColorAttachments][4]float32, bool) {
	var outs [maxSoftColorAttachments][4]float32
	p := c.prog
	if p.FragmentFunc.Block == nil {
		outs[0] = [4]float32{1, 1, 1, 1}
		return outs, false
	}
	nv := len(p.Varyings)
	no := len(p.FragmentOutputs)
	pseudo := nv + 1 + no
	locals := make([]irVal, pseudo+len(p.FragmentFunc.Block.LocalVars))
	locals[0] = irVal{typ: shaderir.Type{Main: shaderir.Vec4}, f: [16]float32{fragCoord[0], fragCoord[1], fragCoord[2], fragCoord[3]}}
	for i := 0; i < nv && i < len(varyings); i++ {
		locals[1+i] = varyings[i]
	}
	for i, ot := range p.FragmentOutputs {
		locals[nv+1+i] = zeroIRVal(ot)
	}
	for i, lt := range p.FragmentFunc.Block.LocalVars {
		locals[pseudo+i] = zeroIRVal(lt)
	}
	vm := &irVM{
		ctx:         c,
		locals:      locals,
		frontFacing: frontFacing,
		fragCoord:   fragCoord,
	}
	vm.execBlock(p.FragmentFunc.Block)
	if vm.discarded {
		return outs, true
	}
	if no == 0 {
		outs[0] = [4]float32{vm.retVal.f[0], vm.retVal.f[1], vm.retVal.f[2], vm.retVal.f[3]}
		return outs, false
	}
	for i := 0; i < no; i++ {
		loc := uint32(i)
		if i < len(p.FragmentOutputMeta) {
			loc = p.FragmentOutputMeta[i].Location
		}
		if loc < maxSoftColorAttachments {
			v := locals[nv+1+i]
			outs[loc] = [4]float32{v.f[0], v.f[1], v.f[2], v.f[3]}
			if p.FragmentOutputs[i].Main == shaderir.Float {
				outs[loc][3] = 1.0
			}
		}
	}
	return outs, false
}

func (c *softDrawCtx) rasterTriangle(v0, v1, v2 shadedVertex) {
	// Skip triangles with non-positive W (behind camera).
	if v0.pos[3] <= 1e-6 || v1.pos[3] <= 1e-6 || v2.pos[3] <= 1e-6 {
		return
	}
	invW0, invW1, invW2 := 1.0/v0.pos[3], 1.0/v1.pos[3], 1.0/v2.pos[3]

	// Canonical WebGPU NDC: X in [-1, 1], Y in [-1, 1] (+Y up), Z in [0, 1].
	// Screen coordinates: X right, Y down.
	toScreen := func(p [4]float32, invW float32) (sx, sy, sz float32) {
		nx, ny, nz := p[0]*invW, p[1]*invW, p[2]*invW
		sx = c.vpX + (nx*0.5+0.5)*c.vpW
		sy = c.vpY + (0.5-ny*0.5)*c.vpH
		sz = c.vpMinD + nz*(c.vpMaxD-c.vpMinD)
		return
	}
	x0, y0, z0 := toScreen(v0.pos, invW0)
	x1, y1, z1 := toScreen(v1.pos, invW1)
	x2, y2, z2 := toScreen(v2.pos, invW2)

	// Signed screen-space area. Because screen Y goes DOWN while NDC Y goes UP,
	// a CCW triangle in NDC has NEGATIVE signed area in screen coordinates.
	area := (x1-x0)*(y2-y0) - (y1-y0)*(x2-x0)
	if math.Abs(float64(area)) < 1e-6 {
		return
	}
	isCCW := area < 0
	frontFacing := isCCW
	if c.pipe.desc.Primitive.FrontFace == GPUFrontCW {
		frontFacing = !isCCW
	}
	switch c.pipe.desc.Primitive.CullMode {
	case GPUCullBack:
		if !frontFacing {
			return
		}
	case GPUCullFront:
		if frontFacing {
			return
		}
	}

	minX := int(math.Floor(float64(min(x0, min(x1, x2)))))
	maxX := int(math.Ceil(float64(max(x0, max(x1, x2)))))
	minY := int(math.Floor(float64(min(y0, min(y1, y2)))))
	maxY := int(math.Ceil(float64(max(y0, max(y1, y2)))))

	// Clip to viewport, scissor, and target bounds.
	clipMinX := max(0, max(int(math.Floor(float64(c.vpX))), c.scX))
	clipMinY := max(0, max(int(math.Floor(float64(c.vpY))), c.scY))
	clipMaxX := min(c.targetW, min(int(math.Ceil(float64(c.vpX+c.vpW))), c.scX+c.scW))
	clipMaxY := min(c.targetH, min(int(math.Ceil(float64(c.vpY+c.vpH))), c.scY+c.scH))
	minX = max(minX, clipMinX)
	minY = max(minY, clipMinY)
	maxX = min(maxX, clipMaxX-1)
	maxY = min(maxY, clipMaxY-1)
	if minX > maxX || minY > maxY {
		return
	}

	invArea := 1.0 / area
	nv := len(c.prog.Varyings)
	interpVaryings := make([]irVal, nv)
	ds := c.pipe.desc.DepthStencil

	// Per-triangle depth bias (constant + slope-scaled, clamped).
	var depthBias float32
	hasDepthBias := ds != nil && (ds.DepthBias != 0 || ds.DepthBiasSlopeScale != 0)
	if hasDepthBias {
		dzdx := ((y1-y2)*z0 + (y2-y0)*z1 + (y0-y1)*z2) * invArea
		dzdy := ((x2-x1)*z0 + (x0-x2)*z1 + (x1-x0)*z2) * invArea
		maxSlope := max(float32(math.Abs(float64(dzdx))), float32(math.Abs(float64(dzdy))))
		r := float32(1.0) / float32((1<<24)-1)
		if c.dsFormat == GPUFormatDepth32Float {
			maxZ := max(z0, max(z1, z2))
			r = max(float32(1.0)/float32(1<<23), math.Nextafter32(maxZ, 2.0)-maxZ)
		}
		depthBias = float32(ds.DepthBias)*r + ds.DepthBiasSlopeScale*maxSlope
		if ds.DepthBiasClamp > 0 && depthBias > ds.DepthBiasClamp {
			depthBias = ds.DepthBiasClamp
		} else if ds.DepthBiasClamp < 0 && depthBias < ds.DepthBiasClamp {
			depthBias = ds.DepthBiasClamp
		}
	}

	// Active stencil face state & masks.
	var stencilFace GPUStencilFaceState
	var stencilReadMask, stencilWriteMask uint8 = 0xFF, 0xFF
	if ds != nil {
		if frontFacing {
			stencilFace = ds.StencilFront
		} else {
			stencilFace = ds.StencilBack
		}
		if ds.StencilReadMask != 0 {
			stencilReadMask = uint8(ds.StencilReadMask & 0xFF)
		}
		if ds.StencilWriteMask != 0 {
			stencilWriteMask = uint8(ds.StencilWriteMask & 0xFF)
		}
	}

	sampleCount := max(c.sampleCount, 1)
	var sampleOffsets [][2]float32
	if sampleCount == 4 {
		sampleOffsets = msaa4Offsets[:]
	} else {
		sampleCount = 1
		sampleOffsets = msaa1Offsets[:]
	}
	hasDepthTest := ds != nil && c.depthPix != nil && c.dsFormat.IsDepth()
	hasStencilTest := ds != nil && c.stencilPix != nil && c.dsFormat.HasStencil()

	baryAt := func(fx, fy float32) (w0, w1, w2 float32) {
		w0 = ((x1-fx)*(y2-fy) - (y1-fy)*(x2-fx)) * invArea
		w1 = ((x2-fx)*(y0-fy) - (y2-fy)*(x0-fx)) * invArea
		w2 = 1.0 - w0 - w1
		return
	}

	var sampleCovered [4]bool
	var sampleDepth [4]float32

	for py := minY; py <= maxY; py++ {
		for px := minX; px <= maxX; px++ {
			coveredCount := 0
			var centX, centY float32
			for s := 0; s < sampleCount; s++ {
				sx := float32(px) + sampleOffsets[s][0]
				sy := float32(py) + sampleOffsets[s][1]
				w0, w1, w2 := baryAt(sx, sy)
				if w0 < -1e-5 || w1 < -1e-5 || w2 < -1e-5 {
					sampleCovered[s] = false
					continue
				}
				z := w0*z0 + w1*z1 + w2*z2
				if hasDepthBias {
					z = clamp01(z + depthBias)
				}
				if z < 0 || z > 1 {
					sampleCovered[s] = false
					continue
				}
				sampleCovered[s] = true
				sampleDepth[s] = z
				coveredCount++
				centX += sx
				centY += sy
			}
			if coveredCount == 0 {
				continue
			}

			// Evaluate fragment shader at pixel center (or centroid of covered samples if center is outside).
			fx := float32(px) + 0.5
			fy := float32(py) + 0.5
			w0, w1, w2 := baryAt(fx, fy)
			if w0 < -1e-5 || w1 < -1e-5 || w2 < -1e-5 {
				fx = centX / float32(coveredCount)
				fy = centY / float32(coveredCount)
				w0, w1, w2 = baryAt(fx, fy)
			}
			centerDepth := w0*z0 + w1*z1 + w2*z2
			if hasDepthBias {
				centerDepth = clamp01(centerDepth + depthBias)
			}

			// Perspective-correct interpolation weights.
			pw0, pw1, pw2 := w0*invW0, w1*invW1, w2*invW2
			invSum := float32(1.0) / (pw0 + pw1 + pw2)
			for vi := range nv {
				flat := vi < len(c.prog.VaryingMeta) && c.prog.VaryingMeta[vi].Interpolation == shaderir.InterpFlat
				if flat {
					interpVaryings[vi] = v0.varyings[vi]
				} else {
					interpVaryings[vi] = lerpIRVal(v0.varyings[vi], v1.varyings[vi], v2.varyings[vi], pw0*invSum, pw1*invSum, pw2*invSum)
				}
			}

			fragOuts, discarded := c.runFragment([4]float32{fx, fy, centerDepth, invSum}, interpVaryings, frontFacing)
			if discarded {
				continue
			}

			pixIdx := py*c.targetW + px
			for s := 0; s < sampleCount; s++ {
				if !sampleCovered[s] {
					continue
				}
				sampIdx := pixIdx*sampleCount + s
				z := sampleDepth[s]

				// Stencil test (per WebGPU spec: compare(ref & readMask, buf & readMask)).
				if hasStencilTest {
					cur := c.stencilPix[sampIdx]
					if !compareStencil(stencilFace.Compare, c.stencilRef&stencilReadMask, cur&stencilReadMask) {
						next := applyStencilOp(stencilFace.FailOp, cur, c.stencilRef)
						c.stencilPix[sampIdx] = (cur &^ stencilWriteMask) | (next & stencilWriteMask)
						continue
					}
				}

				// Depth test.
				if hasDepthTest {
					if !compareDepth(ds.DepthCompare, z, c.depthPix[sampIdx]) {
						if hasStencilTest {
							cur := c.stencilPix[sampIdx]
							next := applyStencilOp(stencilFace.DepthFailOp, cur, c.stencilRef)
							c.stencilPix[sampIdx] = (cur &^ stencilWriteMask) | (next & stencilWriteMask)
						}
						continue
					}
				}

				// Both stencil and depth passed.
				if hasStencilTest {
					cur := c.stencilPix[sampIdx]
					next := applyStencilOp(stencilFace.PassOp, cur, c.stencilRef)
					c.stencilPix[sampIdx] = (cur &^ stencilWriteMask) | (next & stencilWriteMask)
				}
				if hasDepthTest && ds.DepthWrite {
					c.depthPix[sampIdx] = z
				}

				// Write all MRT color attachments for this sample.
				for ti := range c.colorTargets {
					ct := &c.colorTargets[ti]
					if ct.color == nil || ti >= maxSoftColorAttachments {
						continue
					}
					var writeMask GPUColorWriteMask = GPUColorWriteAll
					var blend *GPUBlendState
					if ti < len(c.pipe.desc.Targets) {
						ts := c.pipe.desc.Targets[ti]
						if ts.WriteMask != 0 {
							writeMask = ts.WriteMask
						}
						blend = ts.Blend
					}
					if writeMask == GPUColorWriteNone {
						continue
					}
					src := fragOuts[ti]
					isFloat := ct.format.IsFloat()
					if !isFloat {
						src = [4]float32{clamp01(src[0]), clamp01(src[1]), clamp01(src[2]), clamp01(src[3])}
					}
					off := sampIdx * 4
					if blend != nil {
						dst := [4]float32{ct.color[off+0], ct.color[off+1], ct.color[off+2], ct.color[off+3]}
						r, g, b, a := applyBlend(blend, src, dst, c.blendConst, !isFloat)
						src = [4]float32{r, g, b, a}
					}
					q := quantizeColorForFormat(ct.format, src)
					if writeMask&GPUColorWriteRed != 0 {
						ct.color[off+0] = q[0]
					}
					if writeMask&GPUColorWriteGreen != 0 {
						ct.color[off+1] = q[1]
					}
					if writeMask&GPUColorWriteBlue != 0 {
						ct.color[off+2] = q[2]
					}
					if writeMask&GPUColorWriteAlpha != 0 {
						ct.color[off+3] = q[3]
					}
				}
			}
		}
	}
}

func compareDepth(fn GPUCompareFunc, src, dst float32) bool {
	switch fn {
	case GPUCompareNever:
		return false
	case GPUCompareLess, GPUCompareUndefined:
		return src < dst
	case GPUCompareEqual:
		return src == dst
	case GPUCompareLessEqual:
		return src <= dst
	case GPUCompareGreater:
		return src > dst
	case GPUCompareNotEqual:
		return src != dst
	case GPUCompareGreaterEqual:
		return src >= dst
	case GPUCompareAlways:
		return true
	default:
		return src < dst
	}
}

func compareStencil(fn GPUCompareFunc, ref, buf uint8) bool {
	switch fn {
	case GPUCompareNever:
		return false
	case GPUCompareLess:
		return ref < buf
	case GPUCompareEqual:
		return ref == buf
	case GPUCompareLessEqual:
		return ref <= buf
	case GPUCompareGreater:
		return ref > buf
	case GPUCompareNotEqual:
		return ref != buf
	case GPUCompareGreaterEqual:
		return ref >= buf
	case GPUCompareAlways, GPUCompareUndefined:
		return true
	default:
		return true
	}
}

func applyStencilOp(op GPUStencilOperation, cur, ref uint8) uint8 {
	switch op {
	case GPUStencilZero:
		return 0
	case GPUStencilReplace:
		return ref
	case GPUStencilInvert:
		return ^cur
	case GPUStencilIncrementClamp:
		if cur < 255 {
			return cur + 1
		}
		return 255
	case GPUStencilDecrementClamp:
		if cur > 0 {
			return cur - 1
		}
		return 0
	case GPUStencilIncrementWrap:
		return cur + 1
	case GPUStencilDecrementWrap:
		return cur - 1
	default: // GPUStencilKeep
		return cur
	}
}

func blendFactorVal(f GPUBlendFactor, src, dst, c [4]float32) [4]float32 {
	switch f {
	case GPUBlendZero:
		return [4]float32{0, 0, 0, 0}
	case GPUBlendOne:
		return [4]float32{1, 1, 1, 1}
	case GPUBlendSrc:
		return src
	case GPUBlendOneMinusSrc:
		return [4]float32{1 - src[0], 1 - src[1], 1 - src[2], 1 - src[3]}
	case GPUBlendSrcAlpha:
		return [4]float32{src[3], src[3], src[3], src[3]}
	case GPUBlendOneMinusSrcAlpha:
		a := 1 - src[3]
		return [4]float32{a, a, a, a}
	case GPUBlendDst:
		return dst
	case GPUBlendOneMinusDst:
		return [4]float32{1 - dst[0], 1 - dst[1], 1 - dst[2], 1 - dst[3]}
	case GPUBlendDstAlpha:
		return [4]float32{dst[3], dst[3], dst[3], dst[3]}
	case GPUBlendOneMinusDstAlpha:
		a := 1 - dst[3]
		return [4]float32{a, a, a, a}
	case GPUBlendConstant:
		return c
	case GPUBlendOneMinusConstant:
		return [4]float32{1 - c[0], 1 - c[1], 1 - c[2], 1 - c[3]}
	default:
		return [4]float32{1, 1, 1, 1}
	}
}

func blendCombine(op GPUBlendOperation, s, d float32, clamp bool) float32 {
	var out float32
	switch op {
	case GPUBlendSubtract:
		out = s - d
	case GPUBlendReverseSubtract:
		out = d - s
	case GPUBlendMin:
		out = min(s, d)
	case GPUBlendMax:
		out = max(s, d)
	default:
		out = s + d
	}
	if clamp {
		return clamp01(out)
	}
	return out
}

func applyBlend(bs *GPUBlendState, src, dst, c [4]float32, clamp bool) (float32, float32, float32, float32) {
	if bs.Color.Operation == GPUBlendMin || bs.Color.Operation == GPUBlendMax {
		r := blendCombine(bs.Color.Operation, src[0], dst[0], clamp)
		g := blendCombine(bs.Color.Operation, src[1], dst[1], clamp)
		b := blendCombine(bs.Color.Operation, src[2], dst[2], clamp)
		var a float32
		if bs.Alpha.Operation == GPUBlendMin || bs.Alpha.Operation == GPUBlendMax {
			a = blendCombine(bs.Alpha.Operation, src[3], dst[3], clamp)
		} else {
			as := blendFactorVal(bs.Alpha.SrcFactor, src, dst, c)
			ad := blendFactorVal(bs.Alpha.DstFactor, src, dst, c)
			a = blendCombine(bs.Alpha.Operation, src[3]*as[3], dst[3]*ad[3], clamp)
		}
		return r, g, b, a
	}
	cs := blendFactorVal(bs.Color.SrcFactor, src, dst, c)
	cd := blendFactorVal(bs.Color.DstFactor, src, dst, c)
	r := blendCombine(bs.Color.Operation, src[0]*cs[0], dst[0]*cd[0], clamp)
	g := blendCombine(bs.Color.Operation, src[1]*cs[1], dst[1]*cd[1], clamp)
	b := blendCombine(bs.Color.Operation, src[2]*cs[2], dst[2]*cd[2], clamp)
	var a float32
	if bs.Alpha.Operation == GPUBlendMin || bs.Alpha.Operation == GPUBlendMax {
		a = blendCombine(bs.Alpha.Operation, src[3], dst[3], clamp)
	} else {
		as := blendFactorVal(bs.Alpha.SrcFactor, src, dst, c)
		ad := blendFactorVal(bs.Alpha.DstFactor, src, dst, c)
		a = blendCombine(bs.Alpha.Operation, src[3]*as[3], dst[3]*ad[3], clamp)
	}
	return r, g, b, a
}

// --- Format quantization, sRGB, float16, and RG11B10F conversion ---

func srgbToLinear(c float32) float32 {
	c = clamp01(c)
	if c <= 0.04045 {
		return c / 12.92
	}
	return float32(math.Pow(float64((c+0.055)/1.055), 2.4))
}

func linearToSRGB(c float32) float32 {
	c = clamp01(c)
	if c <= 0.0031308 {
		return c * 12.92
	}
	return float32(1.055*math.Pow(float64(c), 1.0/2.4) - 0.055)
}

func float32ToFloat16(f float32) uint16 {
	bits := math.Float32bits(f)
	sign := uint16((bits >> 16) & 0x8000)
	exp := int((bits>>23)&0xFF) - 127 + 15
	mant := bits & 0x007FFFFF

	if exp <= 0 {
		if exp < -10 {
			return sign
		}
		mant |= 0x00800000
		shift := uint(14 - exp)
		half := uint16(mant >> shift)
		// Round to nearest even
		roundBit := uint32(1) << (shift - 1)
		if (mant&roundBit) != 0 && ((mant&(roundBit-1)) != 0 || (half&1) != 0) {
			half++
		}
		return sign | half
	}
	if exp >= 31 {
		if ((bits>>23)&0xFF) == 0xFF && mant != 0 {
			return sign | 0x7E00 // NaN
		}
		return sign | 0x7C00 // Inf
	}
	half := uint16((exp << 10) | int(mant>>13))
	if (mant&0x1000) != 0 && ((mant&0x0FFF) != 0 || (half&1) != 0) {
		half++
	}
	return sign | half
}

func float16ToFloat32(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32((h >> 10) & 0x1F)
	mant := uint32(h & 0x03FF)
	switch exp {
	case 0:
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		// Subnormal
		e := uint32(127 - 14)
		for (mant & 0x0400) == 0 {
			mant <<= 1
			e--
		}
		mant &= 0x03FF
		return math.Float32frombits(sign | (e << 23) | (mant << 13))
	case 31:
		if mant == 0 {
			return math.Float32frombits(sign | 0x7F800000)
		}
		return math.Float32frombits(sign | 0x7FC00000 | (mant << 13))
	default:
		return math.Float32frombits(sign | ((exp + (127 - 15)) << 23) | (mant << 13))
	}
}

func encodeUnsignedFloat(f float32, mantBits uint) uint32 {
	if math.IsNaN(float64(f)) || f <= 0 {
		return 0
	}
	bits := math.Float32bits(f)
	exp := int((bits>>23)&0xFF) - 127 + 15
	mant := bits & 0x007FFFFF
	maxVal := uint32((30 << mantBits) | ((1 << mantBits) - 1))
	if exp <= 0 {
		if exp < -int(mantBits) {
			return 0
		}
		mant |= 0x00800000
		shift := uint(24-mantBits) - uint(exp)
		return mant >> shift
	}
	if exp >= 31 {
		return maxVal
	}
	shift := uint(23 - mantBits)
	out := uint32(exp<<mantBits) | (mant >> shift)
	if shift > 0 && (mant&(1<<(shift-1))) != 0 {
		if out < maxVal {
			out++
		}
	}
	return out
}

func decodeUnsignedFloat(v uint32, mantBits uint) float32 {
	exp := (v >> mantBits) & 0x1F
	mant := v & ((1 << mantBits) - 1)
	if exp == 0 {
		if mant == 0 {
			return 0
		}
		return float32(mant) * float32(math.Ldexp(1, -14-int(mantBits)))
	}
	if exp == 31 {
		if mant == 0 {
			return float32(math.Inf(1))
		}
		return float32(math.NaN())
	}
	return float32(math.Ldexp(1.0+float64(mant)/float64(uint32(1)<<mantBits), int(exp)-15))
}

func packRG11B10F(r, g, b float32) uint32 {
	ru := encodeUnsignedFloat(r, 6) & 0x7FF
	gu := encodeUnsignedFloat(g, 6) & 0x7FF
	bu := encodeUnsignedFloat(b, 5) & 0x3FF
	return ru | (gu << 11) | (bu << 22)
}

func unpackRG11B10F(v uint32) (r, g, b float32) {
	r = decodeUnsignedFloat(v&0x7FF, 6)
	g = decodeUnsignedFloat((v>>11)&0x7FF, 6)
	b = decodeUnsignedFloat((v>>22)&0x3FF, 5)
	return
}

func quantizeColorForFormat(format GPUTextureFormat, c [4]float32) [4]float32 {
	switch format {
	case GPUFormatRGBA8UnormSRGB, GPUFormatBGRA8UnormSRGB:
		return [4]float32{
			srgbToLinear(float32(floatToByte(linearToSRGB(c[0]))) / 255.0),
			srgbToLinear(float32(floatToByte(linearToSRGB(c[1]))) / 255.0),
			srgbToLinear(float32(floatToByte(linearToSRGB(c[2]))) / 255.0),
			float32(floatToByte(c[3])) / 255.0,
		}
	case GPUFormatR8Unorm:
		return [4]float32{float32(floatToByte(c[0])) / 255.0, 0, 0, 1}
	case GPUFormatRG8Unorm:
		return [4]float32{float32(floatToByte(c[0])) / 255.0, float32(floatToByte(c[1])) / 255.0, 0, 1}
	case GPUFormatRGBA16Float:
		return [4]float32{
			float16ToFloat32(float32ToFloat16(c[0])),
			float16ToFloat32(float32ToFloat16(c[1])),
			float16ToFloat32(float32ToFloat16(c[2])),
			float16ToFloat32(float32ToFloat16(c[3])),
		}
	case GPUFormatRG11B10Float:
		r, g, b := unpackRG11B10F(packRG11B10F(c[0], c[1], c[2]))
		return [4]float32{r, g, b, 1}
	case GPUFormatR32Float:
		return [4]float32{c[0], 0, 0, 1}
	case GPUFormatRGBA32Float:
		return c
	default: // RGBA8Unorm, BGRA8Unorm
		return [4]float32{
			float32(floatToByte(c[0])) / 255.0,
			float32(floatToByte(c[1])) / 255.0,
			float32(floatToByte(c[2])) / 255.0,
			float32(floatToByte(c[3])) / 255.0,
		}
	}
}

func decodePixelBytes(src []byte, format GPUTextureFormat) [4]float32 {
	switch format {
	case GPUFormatRGBA8UnormSRGB:
		return [4]float32{
			srgbToLinear(float32(src[0]) / 255.0),
			srgbToLinear(float32(src[1]) / 255.0),
			srgbToLinear(float32(src[2]) / 255.0),
			float32(src[3]) / 255.0,
		}
	case GPUFormatBGRA8Unorm:
		return [4]float32{
			float32(src[2]) / 255.0,
			float32(src[1]) / 255.0,
			float32(src[0]) / 255.0,
			float32(src[3]) / 255.0,
		}
	case GPUFormatBGRA8UnormSRGB:
		return [4]float32{
			srgbToLinear(float32(src[2]) / 255.0),
			srgbToLinear(float32(src[1]) / 255.0),
			srgbToLinear(float32(src[0]) / 255.0),
			float32(src[3]) / 255.0,
		}
	case GPUFormatR8Unorm:
		return [4]float32{float32(src[0]) / 255.0, 0, 0, 1}
	case GPUFormatRG8Unorm:
		return [4]float32{float32(src[0]) / 255.0, float32(src[1]) / 255.0, 0, 1}
	case GPUFormatRGBA16Float:
		return [4]float32{
			float16ToFloat32(binary.LittleEndian.Uint16(src[0:2])),
			float16ToFloat32(binary.LittleEndian.Uint16(src[2:4])),
			float16ToFloat32(binary.LittleEndian.Uint16(src[4:6])),
			float16ToFloat32(binary.LittleEndian.Uint16(src[6:8])),
		}
	case GPUFormatRG11B10Float:
		r, g, b := unpackRG11B10F(binary.LittleEndian.Uint32(src[0:4]))
		return [4]float32{r, g, b, 1}
	case GPUFormatR32Float:
		return [4]float32{math.Float32frombits(binary.LittleEndian.Uint32(src[0:4])), 0, 0, 1}
	case GPUFormatRGBA32Float:
		return [4]float32{
			math.Float32frombits(binary.LittleEndian.Uint32(src[0:4])),
			math.Float32frombits(binary.LittleEndian.Uint32(src[4:8])),
			math.Float32frombits(binary.LittleEndian.Uint32(src[8:12])),
			math.Float32frombits(binary.LittleEndian.Uint32(src[12:16])),
		}
	default:
		return [4]float32{
			float32(src[0]) / 255.0,
			float32(src[1]) / 255.0,
			float32(src[2]) / 255.0,
			float32(src[3]) / 255.0,
		}
	}
}

func readbackBytesPerPixel(format GPUTextureFormat) int {
	switch format {
	case GPUFormatRGBA16Float:
		return 8
	case GPUFormatRG11B10Float, GPUFormatR32Float:
		return 4
	case GPUFormatRGBA32Float:
		return 16
	default:
		return 4
	}
}

func encodePixelBytes(dst []byte, format GPUTextureFormat, c [4]float32) {
	switch format {
	case GPUFormatRGBA8UnormSRGB, GPUFormatBGRA8UnormSRGB:
		dst[0] = floatToByte(linearToSRGB(c[0]))
		dst[1] = floatToByte(linearToSRGB(c[1]))
		dst[2] = floatToByte(linearToSRGB(c[2]))
		dst[3] = floatToByte(c[3])
	case GPUFormatRGBA16Float:
		binary.LittleEndian.PutUint16(dst[0:2], float32ToFloat16(c[0]))
		binary.LittleEndian.PutUint16(dst[2:4], float32ToFloat16(c[1]))
		binary.LittleEndian.PutUint16(dst[4:6], float32ToFloat16(c[2]))
		binary.LittleEndian.PutUint16(dst[6:8], float32ToFloat16(c[3]))
	case GPUFormatRG11B10Float:
		binary.LittleEndian.PutUint32(dst[0:4], packRG11B10F(c[0], c[1], c[2]))
	case GPUFormatR32Float:
		binary.LittleEndian.PutUint32(dst[0:4], math.Float32bits(c[0]))
	case GPUFormatRGBA32Float:
		binary.LittleEndian.PutUint32(dst[0:4], math.Float32bits(c[0]))
		binary.LittleEndian.PutUint32(dst[4:8], math.Float32bits(c[1]))
		binary.LittleEndian.PutUint32(dst[8:12], math.Float32bits(c[2]))
		binary.LittleEndian.PutUint32(dst[12:16], math.Float32bits(c[3]))
	default:
		dst[0] = floatToByte(c[0])
		dst[1] = floatToByte(c[1])
		dst[2] = floatToByte(c[2])
		dst[3] = floatToByte(c[3])
	}
}

// --- Pure-Go shaderir Interpreter ---

type irVal struct {
	typ    shaderir.Type
	f      [16]float32
	i      [4]int32
	u      [4]uint32
	b      bool
	fields []irVal // for Struct and Array
}

func zeroIRVal(t shaderir.Type) irVal {
	v := irVal{typ: t}
	switch t.Main {
	case shaderir.Struct:
		v.fields = make([]irVal, len(t.Sub))
		for idx, st := range t.Sub {
			v.fields[idx] = zeroIRVal(st)
		}
	case shaderir.Array:
		v.fields = make([]irVal, t.Length)
		for idx := range t.Length {
			v.fields[idx] = zeroIRVal(t.Sub[0])
		}
	}
	return v
}

func cloneIRVal(v irVal) irVal {
	if len(v.fields) > 0 {
		f := make([]irVal, len(v.fields))
		for i := range v.fields {
			f[i] = cloneIRVal(v.fields[i])
		}
		v.fields = f
	}
	return v
}

func lerpIRVal(a, b, c irVal, wa, wb, wc float32) irVal {
	out := irVal{typ: a.typ}
	for i := 0; i < 4; i++ {
		out.f[i] = a.f[i]*wa + b.f[i]*wb + c.f[i]*wc
	}
	return out
}

func decodeVertexAttr(data []byte, format GPUVertexFormat) irVal {
	f32 := func(off int) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(data[off : off+4])) }
	switch format {
	case GPUVertexFloat32:
		return irVal{typ: shaderir.Type{Main: shaderir.Float}, f: [16]float32{f32(0)}}
	case GPUVertexFloat32x2:
		return irVal{typ: shaderir.Type{Main: shaderir.Vec2}, f: [16]float32{f32(0), f32(4)}}
	case GPUVertexFloat32x3:
		return irVal{typ: shaderir.Type{Main: shaderir.Vec3}, f: [16]float32{f32(0), f32(4), f32(8)}}
	case GPUVertexFloat32x4:
		return irVal{typ: shaderir.Type{Main: shaderir.Vec4}, f: [16]float32{f32(0), f32(4), f32(8), f32(12)}}
	case GPUVertexUnorm8x4:
		return irVal{typ: shaderir.Type{Main: shaderir.Vec4}, f: [16]float32{
			float32(data[0]) / 255.0, float32(data[1]) / 255.0, float32(data[2]) / 255.0, float32(data[3]) / 255.0,
		}}
	case GPUVertexUint32:
		return irVal{typ: shaderir.Type{Main: shaderir.Uint}, u: [4]uint32{binary.LittleEndian.Uint32(data[0:4])}}
	case GPUVertexSint32:
		return irVal{typ: shaderir.Type{Main: shaderir.Int}, i: [4]int32{int32(binary.LittleEndian.Uint32(data[0:4]))}}
	}
	return irVal{}
}

func decodeUniformBytes(t shaderir.Type, data []byte, offset int) irVal {
	readF32 := func(off int) float32 {
		if off < 0 || off+4 > len(data) {
			return 0
		}
		return math.Float32frombits(binary.LittleEndian.Uint32(data[off : off+4]))
	}
	readU32 := func(off int) uint32 {
		if off < 0 || off+4 > len(data) {
			return 0
		}
		return binary.LittleEndian.Uint32(data[off : off+4])
	}
	v := irVal{typ: t}
	switch t.Main {
	case shaderir.Float:
		v.f[0] = readF32(offset)
	case shaderir.Int:
		v.i[0] = int32(readU32(offset))
	case shaderir.Uint:
		v.u[0] = readU32(offset)
	case shaderir.Bool:
		v.b = readU32(offset) != 0
	case shaderir.Vec2:
		v.f[0], v.f[1] = readF32(offset), readF32(offset+4)
	case shaderir.Vec3:
		v.f[0], v.f[1], v.f[2] = readF32(offset), readF32(offset+4), readF32(offset+8)
	case shaderir.Vec4:
		v.f[0], v.f[1], v.f[2], v.f[3] = readF32(offset), readF32(offset+4), readF32(offset+8), readF32(offset+12)
	case shaderir.IVec2, shaderir.IVec3, shaderir.IVec4:
		n := t.VectorElementCount()
		for k := 0; k < n; k++ {
			v.i[k] = int32(readU32(offset + k*4))
		}
	case shaderir.UVec2, shaderir.UVec3, shaderir.UVec4:
		n := t.VectorElementCount()
		for k := 0; k < n; k++ {
			v.u[k] = readU32(offset + k*4)
		}
	case shaderir.Mat2, shaderir.Mat3, shaderir.Mat4,
		shaderir.Mat2x3, shaderir.Mat2x4, shaderir.Mat3x2, shaderir.Mat3x4, shaderir.Mat4x2, shaderir.Mat4x3:
		cols, rows := t.MatrixCols(), t.MatrixRows()
		colStride := rows * 4
		if rows == 3 {
			colStride = 16 // WGSL/std140 column alignment for vec3
		}
		for c := 0; c < cols; c++ {
			for r := 0; r < rows; r++ {
				v.f[c*rows+r] = readF32(offset + c*colStride + r*4)
			}
		}
	case shaderir.Struct:
		v.fields = make([]irVal, len(t.Sub))
		for i, ft := range t.Sub {
			v.fields[i] = decodeUniformBytes(ft, data, offset+t.StructFieldOffset(i))
		}
	case shaderir.Array:
		v.fields = make([]irVal, t.Length)
		elem := t.Sub[0]
		stride := ((elem.SizeBytes() + max(elem.AlignBytes(), 16) - 1) / max(elem.AlignBytes(), 16)) * max(elem.AlignBytes(), 16)
		for i := 0; i < t.Length; i++ {
			v.fields[i] = decodeUniformBytes(elem, data, offset+i*stride)
		}
	}
	return v
}

type irVM struct {
	ctx         *softDrawCtx
	locals      []irVal
	vertID      uint32
	instID      uint32
	frontFacing bool
	fragCoord   [4]float32
	returned    bool
	discarded   bool
	broken      bool
	continued   bool
	retVal      irVal
}

func (vm *irVM) execBlock(b *shaderir.Block) {
	if b == nil {
		return
	}
	for _, s := range b.Stmts {
		if vm.returned || vm.discarded || vm.broken || vm.continued {
			return
		}
		switch s.Type {
		case shaderir.ExprStmt:
			vm.evalExpr(s.Exprs[0])
		case shaderir.BlockStmt:
			vm.execBlock(s.Blocks[0])
		case shaderir.Init:
			if s.InitIndex >= 0 && s.InitIndex < len(vm.locals) {
				vm.locals[s.InitIndex] = zeroIRVal(vm.locals[s.InitIndex].typ)
			}
		case shaderir.Assign:
			val := vm.evalExpr(s.Exprs[1])
			vm.assignExpr(s.Exprs[0], cloneIRVal(val))
		case shaderir.If:
			cond := vm.evalExpr(s.Exprs[0])
			if cond.b {
				vm.execBlock(s.Blocks[0])
			} else if len(s.Blocks) > 1 {
				vm.execBlock(s.Blocks[1])
			}
		case shaderir.For:
			vm.execFor(s)
		case shaderir.Break:
			vm.broken = true
		case shaderir.Continue:
			vm.continued = true
		case shaderir.Return:
			if len(s.Exprs) > 0 {
				vm.retVal = vm.evalExpr(s.Exprs[0])
			}
			vm.returned = true
		case shaderir.Discard:
			vm.discarded = true
		}
	}
}

func (vm *irVM) execFor(s shaderir.Stmt) {
	var cur, end, delta int64
	if len(s.Exprs) >= 2 {
		cur = int64(vm.evalExpr(s.Exprs[0]).i[0])
		end = int64(vm.evalExpr(s.Exprs[1]).i[0])
	} else {
		cur, _ = constant.Int64Val(s.ForInit)
		end, _ = constant.Int64Val(s.ForEnd)
	}
	delta, _ = constant.Int64Val(s.ForDelta)
	if delta == 0 {
		return
	}
	for iter := 0; iter < 4096; iter++ {
		var ok bool
		switch s.ForOp {
		case shaderir.LessThanOp:
			ok = cur < end
		case shaderir.LessThanEqualOp:
			ok = cur <= end
		case shaderir.GreaterThanOp:
			ok = cur > end
		case shaderir.GreaterThanEqualOp:
			ok = cur >= end
		default:
			ok = cur < end
		}
		if !ok {
			break
		}
		if s.ForVarIndex >= 0 && s.ForVarIndex < len(vm.locals) {
			vm.locals[s.ForVarIndex] = irVal{typ: s.ForVarType, i: [4]int32{int32(cur)}, u: [4]uint32{uint32(cur)}}
		}
		vm.execBlock(s.Blocks[0])
		if vm.returned || vm.discarded {
			return
		}
		if vm.broken {
			vm.broken = false
			return
		}
		vm.continued = false
		cur += delta
	}
}

func (vm *irVM) assignExpr(lhs shaderir.Expr, val irVal) {
	switch lhs.Type {
	case shaderir.LocalVariable:
		if lhs.Index >= 0 && lhs.Index < len(vm.locals) {
			vm.locals[lhs.Index] = val
		}
	case shaderir.FieldSelector:
		base := vm.evalPtr(lhs.Exprs[0])
		if base == nil {
			return
		}
		switch m := lhs.Exprs[1]; m.Type {
		case shaderir.StructMember:
			if m.Index >= 0 && m.Index < len(base.fields) {
				base.fields[m.Index] = val
			}
		case shaderir.SwizzlingExpr:
			for i, ch := range m.Swizzling {
				ci := swizzleIdx(byte(ch))
				if ci >= 0 && ci < 4 {
					base.f[ci] = val.f[i]
				}
			}
		}
	case shaderir.Index:
		base := vm.evalPtr(lhs.Exprs[0])
		if base == nil {
			return
		}
		idx := int(vm.evalExpr(lhs.Exprs[1]).i[0])
		if base.typ.Main == shaderir.Array && idx >= 0 && idx < len(base.fields) {
			base.fields[idx] = val
		} else if idx >= 0 && idx < 4 {
			base.f[idx] = val.f[0]
		}
	}
}

func (vm *irVM) evalPtr(e shaderir.Expr) *irVal {
	switch e.Type {
	case shaderir.LocalVariable:
		if e.Index >= 0 && e.Index < len(vm.locals) {
			return &vm.locals[e.Index]
		}
	case shaderir.FieldSelector:
		base := vm.evalPtr(e.Exprs[0])
		if base != nil && e.Exprs[1].Type == shaderir.StructMember {
			idx := e.Exprs[1].Index
			if idx >= 0 && idx < len(base.fields) {
				return &base.fields[idx]
			}
		}
	case shaderir.Index:
		base := vm.evalPtr(e.Exprs[0])
		if base != nil && base.typ.Main == shaderir.Array {
			idx := int(vm.evalExpr(e.Exprs[1]).i[0])
			if idx >= 0 && idx < len(base.fields) {
				return &base.fields[idx]
			}
		}
	}
	return nil
}

func (vm *irVM) evalExpr(e shaderir.Expr) irVal {
	switch e.Type {
	case shaderir.NumberExpr:
		switch e.Const.Kind() {
		case constant.Bool:
			return irVal{typ: shaderir.Type{Main: shaderir.Bool}, b: constant.BoolVal(e.Const)}
		case constant.Int:
			if e.Unsigned {
				v, _ := constant.Uint64Val(e.Const)
				return irVal{typ: shaderir.Type{Main: shaderir.Uint}, u: [4]uint32{uint32(v)}, f: [16]float32{float32(v)}}
			}
			v, _ := constant.Int64Val(e.Const)
			return irVal{typ: shaderir.Type{Main: shaderir.Int}, i: [4]int32{int32(v)}, f: [16]float32{float32(v)}}
		case constant.Float:
			f, _ := constant.Float64Val(e.Const)
			return irVal{typ: shaderir.Type{Main: shaderir.Float}, f: [16]float32{float32(f)}}
		}
	case shaderir.LocalVariable:
		if e.Index >= 0 && e.Index < len(vm.locals) {
			return vm.locals[e.Index]
		}
	case shaderir.BindingVariable:
		return vm.evalBinding(e.Index)
	case shaderir.BuiltinInput:
		switch shaderir.StageBuiltin(e.Index) {
		case shaderir.BuiltinVertexIndex:
			return irVal{typ: shaderir.Type{Main: shaderir.Uint}, u: [4]uint32{vm.vertID}}
		case shaderir.BuiltinInstanceIndex:
			return irVal{typ: shaderir.Type{Main: shaderir.Uint}, u: [4]uint32{vm.instID}}
		case shaderir.BuiltinFrontFacing:
			return irVal{typ: shaderir.Type{Main: shaderir.Bool}, b: vm.frontFacing}
		case shaderir.BuiltinPosition:
			return irVal{typ: shaderir.Type{Main: shaderir.Vec4}, f: [16]float32{vm.fragCoord[0], vm.fragCoord[1], vm.fragCoord[2], vm.fragCoord[3]}}
		}
	case shaderir.StructConstruct:
		v := zeroIRVal(*e.StructType)
		for i, a := range e.Exprs {
			if i < len(v.fields) {
				v.fields[i] = vm.evalExpr(a)
			}
		}
		return v
	case shaderir.Unary:
		x := vm.evalExpr(e.Exprs[0])
		switch e.Op {
		case shaderir.Sub:
			out := x
			for i := 0; i < 4; i++ {
				out.f[i] = -x.f[i]
				out.i[i] = -x.i[i]
			}
			return out
		case shaderir.NotOp:
			return irVal{typ: shaderir.Type{Main: shaderir.Bool}, b: !x.b}
		}
	case shaderir.Binary:
		return vm.evalBinary(e.Op, vm.evalExpr(e.Exprs[0]), vm.evalExpr(e.Exprs[1]))
	case shaderir.Selection:
		if vm.evalExpr(e.Exprs[0]).b {
			return vm.evalExpr(e.Exprs[1])
		}
		return vm.evalExpr(e.Exprs[2])
	case shaderir.FieldSelector:
		base := vm.evalExpr(e.Exprs[0])
		switch m := e.Exprs[1]; m.Type {
		case shaderir.StructMember:
			if m.Index >= 0 && m.Index < len(base.fields) {
				return base.fields[m.Index]
			}
		case shaderir.SwizzlingExpr:
			return evalSwizzle(base, m.Swizzling)
		}
	case shaderir.Index:
		base := vm.evalExpr(e.Exprs[0])
		idxVal := vm.evalExpr(e.Exprs[1])
		idx := int(idxVal.i[0])
		if idxVal.typ.Main == shaderir.Uint {
			idx = int(idxVal.u[0])
		}
		switch {
		case base.typ.Main == shaderir.Array:
			if idx >= 0 && idx < len(base.fields) {
				return base.fields[idx]
			}
		case base.typ.IsMatrix():
			rows := base.typ.MatrixRows()
			out := irVal{typ: shaderir.Type{Main: shaderir.VectorOf(shaderir.Float, rows)}}
			for r := 0; r < rows; r++ {
				out.f[r] = base.f[idx*rows+r]
			}
			return out
		default:
			if idx >= 0 && idx < 4 {
				return irVal{typ: shaderir.Type{Main: base.typ.Scalar()}, f: [16]float32{base.f[idx]}, i: [4]int32{base.i[idx]}, u: [4]uint32{base.u[idx]}}
			}
		}
	case shaderir.Call:
		return vm.evalCall(e)
	}
	return irVal{}
}

func (vm *irVM) evalBinding(idx int) irVal {
	if idx < 0 || idx >= len(vm.ctx.prog.Bindings) {
		return irVal{}
	}
	b := vm.ctx.prog.Bindings[idx]
	bg := vm.ctx.bindGroups[b.Group]
	if bg == nil {
		return zeroIRVal(b.Type)
	}
	entry, ok := bg.entries[b.Binding]
	if !ok {
		return zeroIRVal(b.Type)
	}
	switch b.Kind {
	case shaderir.BindingUniformBuffer, shaderir.BindingStorageBufferRead:
		buf := vm.ctx.dev.buffers[entry.Buffer]
		if buf == nil {
			return zeroIRVal(b.Type)
		}
		return decodeUniformBytes(b.Type, buf.data, int(entry.Offset))
	}
	return zeroIRVal(b.Type)
}

func (vm *irVM) evalBinary(op shaderir.Op, a, b irVal) irVal {
	if op == shaderir.MatrixMul {
		return evalMatMul(a, b)
	}
	switch op {
	case shaderir.AndAnd:
		return irVal{typ: shaderir.Type{Main: shaderir.Bool}, b: a.b && b.b}
	case shaderir.OrOr:
		return irVal{typ: shaderir.Type{Main: shaderir.Bool}, b: a.b || b.b}
	case shaderir.LessThanOp:
		return irVal{typ: shaderir.Type{Main: shaderir.Bool}, b: cmpVal(a, b) < 0}
	case shaderir.LessThanEqualOp:
		return irVal{typ: shaderir.Type{Main: shaderir.Bool}, b: cmpVal(a, b) <= 0}
	case shaderir.GreaterThanOp:
		return irVal{typ: shaderir.Type{Main: shaderir.Bool}, b: cmpVal(a, b) > 0}
	case shaderir.GreaterThanEqualOp:
		return irVal{typ: shaderir.Type{Main: shaderir.Bool}, b: cmpVal(a, b) >= 0}
	case shaderir.EqualOp:
		return irVal{typ: shaderir.Type{Main: shaderir.Bool}, b: cmpVal(a, b) == 0}
	case shaderir.NotEqualOp:
		return irVal{typ: shaderir.Type{Main: shaderir.Bool}, b: cmpVal(a, b) != 0}
	}
	// Component-wise float/int/uint arithmetic with scalar broadcast.
	resT := a.typ
	n := max(a.typ.VectorElementCount(), b.typ.VectorElementCount())
	if n < 1 {
		n = 1
	}
	if b.typ.VectorElementCount() > a.typ.VectorElementCount() {
		resT = b.typ
	}
	out := irVal{typ: resT}
	aScalar := a.typ.VectorElementCount() < 2
	bScalar := b.typ.VectorElementCount() < 2
	for k := 0; k < n; k++ {
		ai, bi := k, k
		if aScalar {
			ai = 0
		}
		if bScalar {
			bi = 0
		}
		af, bf := a.f[ai], b.f[bi]
		if a.typ.Scalar() == shaderir.Int {
			iv, jv := a.i[ai], b.i[bi]
			switch op {
			case shaderir.Add:
				out.i[k] = iv + jv
			case shaderir.Sub:
				out.i[k] = iv - jv
			case shaderir.ComponentWiseMul:
				out.i[k] = iv * jv
			case shaderir.Div:
				if jv != 0 {
					out.i[k] = iv / jv
				}
			case shaderir.ModOp:
				if jv != 0 {
					out.i[k] = iv % jv
				}
			}
			out.f[k] = float32(out.i[k])
			continue
		}
		if a.typ.Scalar() == shaderir.Uint {
			uv, wv := a.u[ai], b.u[bi]
			switch op {
			case shaderir.Add:
				out.u[k] = uv + wv
			case shaderir.Sub:
				out.u[k] = uv - wv
			case shaderir.ComponentWiseMul:
				out.u[k] = uv * wv
			case shaderir.Div:
				if wv != 0 {
					out.u[k] = uv / wv
				}
			case shaderir.ModOp:
				if wv != 0 {
					out.u[k] = uv % wv
				}
			}
			out.f[k] = float32(out.u[k])
			continue
		}
		switch op {
		case shaderir.Add:
			out.f[k] = af + bf
		case shaderir.Sub:
			out.f[k] = af - bf
		case shaderir.ComponentWiseMul:
			out.f[k] = af * bf
		case shaderir.Div:
			if bf != 0 {
				out.f[k] = af / bf
			}
		}
	}
	return out
}

func cmpVal(a, b irVal) int {
	switch a.typ.Main {
	case shaderir.Int:
		if a.i[0] < b.i[0] {
			return -1
		}
		if a.i[0] > b.i[0] {
			return 1
		}
		return 0
	case shaderir.Uint:
		if a.u[0] < b.u[0] {
			return -1
		}
		if a.u[0] > b.u[0] {
			return 1
		}
		return 0
	default:
		if a.f[0] < b.f[0] {
			return -1
		}
		if a.f[0] > b.f[0] {
			return 1
		}
		return 0
	}
}

func evalMatMul(a, b irVal) irVal {
	// Column-major matrices: M[col*rows + row]
	switch {
	case a.typ.IsMatrix() && b.typ.IsFloatVector():
		cols, rows := a.typ.MatrixCols(), a.typ.MatrixRows()
		out := irVal{typ: shaderir.Type{Main: shaderir.VectorOf(shaderir.Float, rows)}}
		for r := 0; r < rows; r++ {
			var sum float32
			for c := 0; c < cols; c++ {
				sum += a.f[c*rows+r] * b.f[c]
			}
			out.f[r] = sum
		}
		return out
	case a.typ.IsFloatVector() && b.typ.IsMatrix():
		cols, rows := b.typ.MatrixCols(), b.typ.MatrixRows()
		out := irVal{typ: shaderir.Type{Main: shaderir.VectorOf(shaderir.Float, cols)}}
		for c := 0; c < cols; c++ {
			var sum float32
			for r := 0; r < rows; r++ {
				sum += a.f[r] * b.f[c*rows+r]
			}
			out.f[c] = sum
		}
		return out
	case a.typ.IsMatrix() && b.typ.IsMatrix():
		ac, ar := a.typ.MatrixCols(), a.typ.MatrixRows()
		bc, br := b.typ.MatrixCols(), b.typ.MatrixRows()
		out := irVal{typ: shaderir.Type{Main: shaderir.MatrixOf(bc, ar)}}
		for c := 0; c < bc; c++ {
			for r := 0; r < ar; r++ {
				var sum float32
				for k := 0; k < ac && k < br; k++ {
					sum += a.f[k*ar+r] * b.f[c*br+k]
				}
				out.f[c*ar+r] = sum
			}
		}
		return out
	case a.typ.IsMatrix():
		out := a
		n := a.typ.MatrixCols() * a.typ.MatrixRows()
		for i := 0; i < n; i++ {
			out.f[i] *= b.f[0]
		}
		return out
	case b.typ.IsMatrix():
		out := b
		n := b.typ.MatrixCols() * b.typ.MatrixRows()
		for i := 0; i < n; i++ {
			out.f[i] *= a.f[0]
		}
		return out
	}
	return irVal{}
}

func (vm *irVM) evalCall(e shaderir.Expr) irVal {
	callee := e.Exprs[0]
	if callee.Type == shaderir.FunctionExpr {
		var fn *shaderir.Func
		for i := range vm.ctx.prog.Funcs {
			if vm.ctx.prog.Funcs[i].Index == callee.Index {
				fn = &vm.ctx.prog.Funcs[i]
				break
			}
		}
		if fn == nil {
			return irVal{}
		}
		locals := make([]irVal, len(fn.InParams)+len(fn.Block.LocalVars))
		for i, a := range e.Exprs[1:] {
			if i < len(fn.InParams) {
				locals[i] = vm.evalExpr(a)
			}
		}
		for i, lt := range fn.Block.LocalVars {
			locals[len(fn.InParams)+i] = zeroIRVal(lt)
		}
		sub := &irVM{
			ctx:         vm.ctx,
			locals:      locals,
			vertID:      vm.vertID,
			instID:      vm.instID,
			frontFacing: vm.frontFacing,
			fragCoord:   vm.fragCoord,
		}
		sub.execBlock(fn.Block)
		if sub.discarded {
			vm.discarded = true
		}
		return sub.retVal
	}

	// Texture sampling needs raw binding indices before evaluating args.
	switch callee.BuiltinFunc {
	case shaderir.TextureSample, shaderir.TextureSampleLevel, shaderir.TextureSampleBias:
		return vm.evalTextureSample(e.Exprs[1].Index, e.Exprs[2].Index, vm.evalExpr(e.Exprs[3]))
	case shaderir.TextureLoad:
		var extra irVal
		if len(e.Exprs) > 3 {
			extra = vm.evalExpr(e.Exprs[3])
		}
		return vm.evalTextureLoad(e.Exprs[1].Index, vm.evalExpr(e.Exprs[2]), extra)
	case shaderir.TextureDimensions:
		return vm.evalTextureDimensions(e.Exprs[1].Index)
	}

	args := make([]irVal, len(e.Exprs)-1)
	for i, a := range e.Exprs[1:] {
		args[i] = vm.evalExpr(a)
	}

	switch callee.BuiltinFunc {
	case shaderir.FloatF:
		if args[0].typ.Main == shaderir.Int {
			return irVal{typ: shaderir.Type{Main: shaderir.Float}, f: [16]float32{float32(args[0].i[0])}}
		}
		if args[0].typ.Main == shaderir.Uint {
			return irVal{typ: shaderir.Type{Main: shaderir.Float}, f: [16]float32{float32(args[0].u[0])}}
		}
		return irVal{typ: shaderir.Type{Main: shaderir.Float}, f: [16]float32{args[0].f[0]}}
	case shaderir.IntF:
		return irVal{typ: shaderir.Type{Main: shaderir.Int}, i: [4]int32{int32(args[0].f[0])}}
	case shaderir.UintF:
		return irVal{typ: shaderir.Type{Main: shaderir.Uint}, u: [4]uint32{uint32(args[0].f[0])}}
	case shaderir.Vec2F:
		return packFloatVec(shaderir.Vec2, 2, args)
	case shaderir.Vec3F:
		return packFloatVec(shaderir.Vec3, 3, args)
	case shaderir.Vec4F:
		return packFloatVec(shaderir.Vec4, 4, args)
	case shaderir.Mat3F:
		return packMat(shaderir.Mat3, 3, args)
	case shaderir.Mat4F:
		return packMat(shaderir.Mat4, 4, args)
	case shaderir.Dot:
		n := max(args[0].typ.VectorElementCount(), 1)
		var sum float32
		for i := 0; i < n; i++ {
			sum += args[0].f[i] * args[1].f[i]
		}
		return irVal{typ: shaderir.Type{Main: shaderir.Float}, f: [16]float32{sum}}
	case shaderir.Cross:
		a, b := args[0].f, args[1].f
		return irVal{typ: shaderir.Type{Main: shaderir.Vec3}, f: [16]float32{
			a[1]*b[2] - a[2]*b[1],
			a[2]*b[0] - a[0]*b[2],
			a[0]*b[1] - a[1]*b[0],
		}}
	case shaderir.Length:
		n := max(args[0].typ.VectorElementCount(), 1)
		var sum float32
		for i := 0; i < n; i++ {
			sum += args[0].f[i] * args[0].f[i]
		}
		return irVal{typ: shaderir.Type{Main: shaderir.Float}, f: [16]float32{float32(math.Sqrt(float64(sum)))}}
	case shaderir.Normalize:
		n := max(args[0].typ.VectorElementCount(), 1)
		var sum float32
		for i := 0; i < n; i++ {
			sum += args[0].f[i] * args[0].f[i]
		}
		inv := float32(0)
		if sum > 1e-12 {
			inv = float32(1.0 / math.Sqrt(float64(sum)))
		}
		out := irVal{typ: args[0].typ}
		for i := 0; i < n; i++ {
			out.f[i] = args[0].f[i] * inv
		}
		return out
	case shaderir.Reflect:
		n := max(args[0].typ.VectorElementCount(), 1)
		var d float32
		for i := 0; i < n; i++ {
			d += args[0].f[i] * args[1].f[i]
		}
		out := irVal{typ: args[0].typ}
		for i := 0; i < n; i++ {
			out.f[i] = args[0].f[i] - 2*d*args[1].f[i]
		}
		return out
	case shaderir.Min:
		return mapVec2(args[0], args[1], func(a, b float32) float32 { return min(a, b) })
	case shaderir.Max:
		return mapVec2(args[0], args[1], func(a, b float32) float32 { return max(a, b) })
	case shaderir.Clamp:
		return mapVec3(args[0], args[1], args[2], func(x, lo, hi float32) float32 { return min(max(x, lo), hi) })
	case shaderir.Mix:
		return mapVec3(args[0], args[1], args[2], func(a, b, t float32) float32 { return a*(1-t) + b*t })
	case shaderir.Abs:
		return mapVec1(args[0], func(x float32) float32 { return float32(math.Abs(float64(x))) })
	case shaderir.Floor:
		return mapVec1(args[0], func(x float32) float32 { return float32(math.Floor(float64(x))) })
	case shaderir.Ceil:
		return mapVec1(args[0], func(x float32) float32 { return float32(math.Ceil(float64(x))) })
	case shaderir.Fract:
		return mapVec1(args[0], func(x float32) float32 { return x - float32(math.Floor(float64(x))) })
	case shaderir.Sqrt:
		return mapVec1(args[0], func(x float32) float32 { return float32(math.Sqrt(float64(x))) })
	case shaderir.Inversesqrt:
		return mapVec1(args[0], func(x float32) float32 { return float32(1.0 / math.Sqrt(float64(x))) })
	case shaderir.Sin:
		return mapVec1(args[0], func(x float32) float32 { return float32(math.Sin(float64(x))) })
	case shaderir.Cos:
		return mapVec1(args[0], func(x float32) float32 { return float32(math.Cos(float64(x))) })
	case shaderir.Tan:
		return mapVec1(args[0], func(x float32) float32 { return float32(math.Tan(float64(x))) })
	case shaderir.Pow:
		return mapVec2(args[0], args[1], func(a, b float32) float32 { return float32(math.Pow(float64(a), float64(b))) })
	case shaderir.Step:
		return mapVec2(args[1], args[0], func(x, edge float32) float32 {
			if x < edge {
				return 0
			}
			return 1
		})
	case shaderir.Smoothstep:
		return mapVec3(args[2], args[0], args[1], func(x, e0, e1 float32) float32 {
			t := clamp01((x - e0) / (e1 - e0))
			return t * t * (3 - 2*t)
		})
	}
	return irVal{}
}

func (vm *irVM) lookupTexture(bindIdx int) *softTexture {
	if bindIdx < 0 || bindIdx >= len(vm.ctx.prog.Bindings) {
		return nil
	}
	b := vm.ctx.prog.Bindings[bindIdx]
	bg := vm.ctx.bindGroups[b.Group]
	if bg == nil {
		return nil
	}
	e, ok := bg.entries[b.Binding]
	if !ok {
		return nil
	}
	return vm.ctx.dev.textures[e.Texture]
}

func (vm *irVM) lookupSampler(bindIdx int) *softSampler {
	if bindIdx < 0 || bindIdx >= len(vm.ctx.prog.Bindings) {
		return nil
	}
	b := vm.ctx.prog.Bindings[bindIdx]
	bg := vm.ctx.bindGroups[b.Group]
	if bg == nil {
		return nil
	}
	e, ok := bg.entries[b.Binding]
	if !ok {
		return nil
	}
	return vm.ctx.dev.samplers[e.Sampler]
}

func wrapCoord(u float32, mode GPUAddressMode) float32 {
	switch mode {
	case GPUAddressRepeat:
		return u - float32(math.Floor(float64(u)))
	case GPUAddressMirrorRepeat:
		f := u - float32(math.Floor(float64(u*0.5)))*2
		if f > 1 {
			return 2 - f
		}
		return f
	default:
		return clamp01(u)
	}
}

func (vm *irVM) evalTextureSample(texIdx, sampIdx int, uv irVal) irVal {
	tex := vm.lookupTexture(texIdx)
	if tex == nil || tex.color == nil {
		return irVal{typ: shaderir.Type{Main: shaderir.Vec4}, f: [16]float32{0, 0, 0, 1}}
	}
	var desc GPUSamplerDesc
	if s := vm.lookupSampler(sampIdx); s != nil {
		desc = s.desc
	}
	w, h := int(tex.desc.Width), int(tex.desc.Height)
	u := wrapCoord(uv.f[0], desc.AddressU)
	v := wrapCoord(uv.f[1], desc.AddressV)
	samples := max(tex.samples, 1)

	readPx := func(x, y int) [4]float32 {
		x = min(max(x, 0), w-1)
		y = min(max(y, 0), h-1)
		off := (y*w + x) * samples * 4
		return [4]float32{
			tex.color[off+0],
			tex.color[off+1],
			tex.color[off+2],
			tex.color[off+3],
		}
	}

	if desc.MagFilter == GPUFilterLinear || desc.MinFilter == GPUFilterLinear {
		fx := u*float32(w) - 0.5
		fy := v*float32(h) - 0.5
		x0 := int(math.Floor(float64(fx)))
		y0 := int(math.Floor(float64(fy)))
		tx := fx - float32(x0)
		ty := fy - float32(y0)
		c00 := readPx(x0, y0)
		c10 := readPx(x0+1, y0)
		c01 := readPx(x0, y0+1)
		c11 := readPx(x0+1, y0+1)
		out := irVal{typ: shaderir.Type{Main: shaderir.Vec4}}
		for ch := 0; ch < 4; ch++ {
			top := c00[ch]*(1-tx) + c10[ch]*tx
			bot := c01[ch]*(1-tx) + c11[ch]*tx
			out.f[ch] = top*(1-ty) + bot*ty
		}
		return out
	}

	ix := int(math.Floor(float64(u * float32(w))))
	iy := int(math.Floor(float64(v * float32(h))))
	c := readPx(ix, iy)
	return irVal{typ: shaderir.Type{Main: shaderir.Vec4}, f: [16]float32{c[0], c[1], c[2], c[3]}}
}

func (vm *irVM) evalTextureLoad(texIdx int, coord, extra irVal) irVal {
	tex := vm.lookupTexture(texIdx)
	if tex == nil || tex.color == nil {
		return irVal{typ: shaderir.Type{Main: shaderir.Vec4}, f: [16]float32{0, 0, 0, 1}}
	}
	w, h := int(tex.desc.Width), int(tex.desc.Height)
	ix := min(max(int(coord.i[0]), 0), w-1)
	iy := min(max(int(coord.i[1]), 0), h-1)
	samples := max(tex.samples, 1)
	samp := 0
	if samples > 1 {
		samp = int(extra.i[0])
		if extra.typ.Main == shaderir.Uint {
			samp = int(extra.u[0])
		}
		samp = min(max(samp, 0), samples-1)
	}
	off := ((iy*w+ix)*samples + samp) * 4
	return irVal{typ: shaderir.Type{Main: shaderir.Vec4}, f: [16]float32{
		tex.color[off+0],
		tex.color[off+1],
		tex.color[off+2],
		tex.color[off+3],
	}}
}

func (vm *irVM) evalTextureDimensions(texIdx int) irVal {
	tex := vm.lookupTexture(texIdx)
	if tex == nil {
		return irVal{typ: shaderir.Type{Main: shaderir.UVec2}}
	}
	return irVal{typ: shaderir.Type{Main: shaderir.UVec2}, u: [4]uint32{tex.desc.Width, tex.desc.Height}}
}

func packFloatVec(bt shaderir.BasicType, n int, args []irVal) irVal {
	out := irVal{typ: shaderir.Type{Main: bt}}
	if len(args) == 0 {
		return out
	}
	if len(args) == 1 && args[0].typ.VectorElementCount() < 2 {
		for i := 0; i < n; i++ {
			out.f[i] = args[0].f[0]
		}
		return out
	}
	k := 0
	for _, a := range args {
		cnt := a.typ.VectorElementCount()
		if cnt < 1 {
			cnt = 1
		}
		for i := 0; i < cnt && k < n; i++ {
			out.f[k] = a.f[i]
			k++
		}
	}
	return out
}

func packMat(bt shaderir.BasicType, dim int, args []irVal) irVal {
	out := irVal{typ: shaderir.Type{Main: bt}}
	if len(args) == 1 && args[0].typ.IsMatrix() {
		src := args[0]
		sc, sr := src.typ.MatrixCols(), src.typ.MatrixRows()
		for c := 0; c < dim && c < sc; c++ {
			for r := 0; r < dim && r < sr; r++ {
				out.f[c*dim+r] = src.f[c*sr+r]
			}
		}
		return out
	}
	k := 0
	for _, a := range args {
		cnt := a.typ.VectorElementCount()
		if cnt < 1 {
			cnt = 1
		}
		for i := 0; i < cnt && k < dim*dim; i++ {
			out.f[k] = a.f[i]
			k++
		}
	}
	return out
}

func evalSwizzle(v irVal, sw string) irVal {
	if len(sw) == 1 {
		idx := swizzleIdx(sw[0])
		if idx < 0 || idx >= 4 {
			idx = 0
		}
		return irVal{typ: shaderir.Type{Main: v.typ.Scalar()}, f: [16]float32{v.f[idx]}, i: [4]int32{v.i[idx]}, u: [4]uint32{v.u[idx]}}
	}
	out := irVal{typ: shaderir.Type{Main: shaderir.VectorOf(v.typ.Scalar(), len(sw))}}
	for i := 0; i < len(sw) && i < 4; i++ {
		idx := swizzleIdx(sw[i])
		if idx >= 0 && idx < 4 {
			out.f[i] = v.f[idx]
			out.i[i] = v.i[idx]
			out.u[i] = v.u[idx]
		}
	}
	return out
}

func swizzleIdx(c byte) int {
	switch c {
	case 'x', 'r', 's':
		return 0
	case 'y', 'g', 't':
		return 1
	case 'z', 'b', 'p':
		return 2
	case 'w', 'a', 'q':
		return 3
	}
	return -1
}

func mapVec1(a irVal, fn func(float32) float32) irVal {
	n := max(a.typ.VectorElementCount(), 1)
	out := irVal{typ: a.typ}
	for i := 0; i < n; i++ {
		out.f[i] = fn(a.f[i])
	}
	return out
}

func mapVec2(a, b irVal, fn func(float32, float32) float32) irVal {
	resT := a.typ
	n := max(a.typ.VectorElementCount(), b.typ.VectorElementCount())
	if n < 1 {
		n = 1
	}
	if b.typ.VectorElementCount() > a.typ.VectorElementCount() {
		resT = b.typ
	}
	out := irVal{typ: resT}
	aS, bS := a.typ.VectorElementCount() < 2, b.typ.VectorElementCount() < 2
	for i := 0; i < n; i++ {
		ai, bi := i, i
		if aS {
			ai = 0
		}
		if bS {
			bi = 0
		}
		out.f[i] = fn(a.f[ai], b.f[bi])
	}
	return out
}

func mapVec3(a, b, c irVal, fn func(float32, float32, float32) float32) irVal {
	n := max(a.typ.VectorElementCount(), max(b.typ.VectorElementCount(), c.typ.VectorElementCount()))
	if n < 1 {
		n = 1
	}
	out := irVal{typ: a.typ}
	if n > 1 && a.typ.VectorElementCount() < 2 {
		out.typ = shaderir.Type{Main: shaderir.VectorOf(shaderir.Float, n)}
	}
	aS, bS, cS := a.typ.VectorElementCount() < 2, b.typ.VectorElementCount() < 2, c.typ.VectorElementCount() < 2
	for i := 0; i < n; i++ {
		ai, bi, ci := i, i, i
		if aS {
			ai = 0
		}
		if bS {
			bi = 0
		}
		if cS {
			ci = 0
		}
		out.f[i] = fn(a.f[ai], b.f[bi], c.f[ci])
	}
	return out
}

func clamp01(v float32) float32 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func floatToByte(v float32) byte {
	v = clamp01(v)
	return byte(math.Round(float64(v * 255.0)))
}
