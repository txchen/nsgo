### Added

- Add GPU compute shaders (`@compute @workgroup_size`, compute built-ins), read/write storage buffers (`var<storage, read>`, `var<storage, read_write>`), storage textures (`texture_storage_2d`, `textureStore`), `gpu.ComputePipeline`, `Encoder.BeginComputePass` (`Dispatch` / `DispatchIndirect`), `Device.ReadBuffer`, `Device.Capabilities().Compute` / `SupportsCompute()` capability queries (with `ErrUnsupported` fallback for WebGL 2 / GLES 3.0), MSL/HLSL/GLSL 4.30/GLSL ES 3.10/WGSL compute emitters, and headless software compute execution in `nsgotest` (#1234).
