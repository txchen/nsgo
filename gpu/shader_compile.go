//go:build !nsgo_noshader

package gpu

import (
	"slices"

	"github.com/nsgo-dev/nsgo/internal/shader"
	"github.com/nsgo-dev/nsgo/internal/shader/wgsl"
	"github.com/nsgo-dev/nsgo/internal/shaderir"
)

// ShaderModule is a portable compiled WGSL or Kage shader module ready to be
// attached to one or more Pipelines or ComputePipelines.
type ShaderModule struct {
	ir              *shaderir.Program
	vertexEntries   []string
	fragmentEntries []string
	computeEntries  []string
	workgroupSize   [3]uint32
	source          string
}

// HasVertexEntry reports whether m defines a @vertex entry point named name
// (or any vertex entry point when name is empty).
func (m *ShaderModule) HasVertexEntry(name string) bool {
	if m == nil || m.ir == nil || m.ir.VertexFunc.Block == nil {
		return false
	}
	if name == "" {
		return true
	}
	return slices.Contains(m.vertexEntries, name) || m.ir.VertexEntry == name
}

// HasFragmentEntry reports whether m defines a @fragment entry point named name
// (or any fragment entry point when name is empty).
func (m *ShaderModule) HasFragmentEntry(name string) bool {
	if m == nil || m.ir == nil || m.ir.FragmentFunc.Block == nil {
		return false
	}
	if name == "" {
		return true
	}
	return slices.Contains(m.fragmentEntries, name) || m.ir.FragmentEntry == name
}

// HasComputeEntry reports whether m defines a @compute entry point named name
// (or any compute entry point when name is empty).
func (m *ShaderModule) HasComputeEntry(name string) bool {
	if m == nil || m.ir == nil || m.ir.ComputeFunc.Block == nil {
		return false
	}
	if name == "" {
		return true
	}
	return slices.Contains(m.computeEntries, name) || m.ir.ComputeEntry == name
}

// WorkgroupSize returns the @workgroup_size(X, Y, Z) of m's compute entry point,
// or [0, 0, 0] if m has no compute entry point.
func (m *ShaderModule) WorkgroupSize() [3]uint32 {
	if m == nil {
		return [3]uint32{}
	}
	return m.workgroupSize
}

// CompileWGSL parses, type-checks, and lowers a WGSL 3D/compute pipeline module
// into portable shaderir.
func CompileWGSL(src string) (*ShaderModule, error) {
	p, err := wgsl.CompilePipeline(src)
	if err != nil {
		return nil, Error{Code: ErrShaderCompile, Message: err.Error(), Cause: err}
	}
	return &ShaderModule{
		ir:              p.IR,
		vertexEntries:   p.VertexEntries,
		fragmentEntries: p.FragmentEntries,
		computeEntries:  p.ComputeEntries,
		workgroupSize:   p.WorkgroupSize,
		source:          src,
	}, nil
}

// MustCompileWGSL compiles src and panics if compilation fails.
func MustCompileWGSL(src string) *ShaderModule {
	m, err := CompileWGSL(src)
	if err != nil {
		panic(err)
	}
	return m
}

// CompileKage compiles a Kage fragment shader into a ShaderModule.
func CompileKage(src string) (*ShaderModule, error) {
	ir, err := shader.CompileFragment([]byte(src))
	if err != nil {
		return nil, Error{Code: ErrShaderCompile, Message: err.Error(), Cause: err}
	}
	return &ShaderModule{
		ir:              ir,
		fragmentEntries: []string{"Fragment"},
		source:          src,
	}, nil
}
