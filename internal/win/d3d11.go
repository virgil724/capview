//go:build windows

package win

import "unsafe"

// vtable indices (verified against mingw-w64 d3d11.h).
const (
	vtDeviceCreateBuffer             = 3
	vtDeviceCreateTexture2D          = 5
	vtDeviceCreateShaderResourceView = 7
	vtDeviceCreateRenderTargetView   = 9
	vtDeviceCreateVertexShader       = 12
	vtDeviceCreatePixelShader        = 15
	vtDeviceCreateSamplerState       = 23

	vtContextPSSetShaderResources   = 8
	vtContextPSSetShader            = 9
	vtContextPSSetSamplers          = 10
	vtContextVSSetShader            = 11
	vtContextDraw                   = 13
	vtContextMap                    = 14
	vtContextUnmap                  = 15
	vtContextPSSetConstantBuffers   = 16
	vtContextIASetPrimitiveTopology = 24
	vtContextOMSetRenderTargets     = 33
	vtContextRSSetViewports         = 44
	vtContextCopyResource           = 47
	vtContextClearRenderTargetView  = 50
	vtContextFlush                  = 111
)

// D3D11 constants.
const (
	D3D_DRIVER_TYPE_UNKNOWN  = 0
	D3D_DRIVER_TYPE_HARDWARE = 1
	D3D_DRIVER_TYPE_WARP     = 5
	D3D11_SDK_VERSION        = 7

	D3D11_CREATE_DEVICE_VIDEO_SUPPORT = 0x800

	D3D11_USAGE_DEFAULT = 0
	D3D11_USAGE_DYNAMIC = 2
	D3D11_USAGE_STAGING = 3

	D3D11_BIND_CONSTANT_BUFFER = 0x4
	D3D11_BIND_SHADER_RESOURCE = 0x8
	D3D11_BIND_RENDER_TARGET   = 0x20

	D3D11_CPU_ACCESS_WRITE = 0x10000
	D3D11_CPU_ACCESS_READ  = 0x20000

	D3D11_MAP_READ          = 1
	D3D11_MAP_WRITE         = 2
	D3D11_MAP_WRITE_DISCARD = 4

	D3D11_SRV_DIMENSION_TEXTURE2D = 4

	D3D11_FILTER_MIN_MAG_MIP_LINEAR = 0x15
	D3D11_TEXTURE_ADDRESS_CLAMP     = 3
	D3D11_COMPARISON_NEVER          = 1

	D3D_PRIMITIVE_TOPOLOGY_TRIANGLELIST = 4
)

// Texture2DDesc is D3D11_TEXTURE2D_DESC.
type Texture2DDesc struct {
	Width          uint32
	Height         uint32
	MipLevels      uint32
	ArraySize      uint32
	Format         uint32
	SampleCount    uint32
	SampleQuality  uint32
	Usage          uint32
	BindFlags      uint32
	CPUAccessFlags uint32
	MiscFlags      uint32
}

// BufferDesc is D3D11_BUFFER_DESC.
type BufferDesc struct {
	ByteWidth           uint32
	Usage               uint32
	BindFlags           uint32
	CPUAccessFlags      uint32
	MiscFlags           uint32
	StructureByteStride uint32
}

// ShaderResourceViewDesc is D3D11_SHADER_RESOURCE_VIEW_DESC for Texture2D.
// The C union is 16 bytes wide (4 uint32s); Texture2D uses the first two.
type ShaderResourceViewDesc struct {
	Format          uint32
	ViewDimension   uint32
	MostDetailedMip uint32
	MipLevels       uint32
	_               [2]uint32 // padding for union size
}

// SamplerDesc is D3D11_SAMPLER_DESC.
type SamplerDesc struct {
	Filter         uint32
	AddressU       uint32
	AddressV       uint32
	AddressW       uint32
	MipLODBias     float32
	MaxAnisotropy  uint32
	ComparisonFunc uint32
	BorderColor    [4]float32
	MinLOD         float32
	MaxLOD         float32
}

// Viewport is D3D11_VIEWPORT.
type Viewport struct {
	TopLeftX float32
	TopLeftY float32
	Width    float32
	Height   float32
	MinDepth float32
	MaxDepth float32
}

// MappedSubresource is D3D11_MAPPED_SUBRESOURCE.
type MappedSubresource struct {
	Data       uintptr
	RowPitch   uint32
	DepthPitch uint32
}

// Device is ID3D11Device; Context is its immediate ID3D11DeviceContext.
type Device struct{ Object }
type Context struct{ Object }

// CreateDevice creates a D3D11 hardware device. If adapter is 0, the default
// hardware adapter is used.
func CreateDevice(adapter Object) (Device, Context, error) {
	driverType := uintptr(D3D_DRIVER_TYPE_UNKNOWN)
	if adapter == 0 {
		driverType = D3D_DRIVER_TYPE_HARDWARE
	}
	var dev, ctx Object
	r, _, _ := procD3D11CreateDevice.Call(uintptr(adapter), driverType, 0, 0, 0, 0,
		D3D11_SDK_VERSION, uintptr(unsafe.Pointer(&dev)), 0, uintptr(unsafe.Pointer(&ctx)))
	if err := hr(r); err != nil {
		return Device{}, Context{}, err
	}
	return Device{dev}, Context{ctx}, nil
}

// CreateDeviceWARP creates a D3D11 device on the WARP software rasterizer
// (for tests), with video support when available.
func CreateDeviceWARP() (Device, Context, error) {
	var err error
	for _, flags := range []uintptr{D3D11_CREATE_DEVICE_VIDEO_SUPPORT, 0} {
		var dev, ctx Object
		r, _, _ := procD3D11CreateDevice.Call(0, D3D_DRIVER_TYPE_WARP, 0, flags, 0, 0,
			D3D11_SDK_VERSION, uintptr(unsafe.Pointer(&dev)), 0, uintptr(unsafe.Pointer(&ctx)))
		if err = hr(r); err == nil {
			return Device{dev}, Context{ctx}, nil
		}
	}
	return Device{}, Context{}, err
}

// CreateBuffer creates a buffer with no initial data.
func (d Device) CreateBuffer(desc *BufferDesc) (Object, error) {
	var b Object
	err := hr(d.call(vtDeviceCreateBuffer, uintptr(unsafe.Pointer(desc)), 0, uintptr(unsafe.Pointer(&b))))
	return b, err
}

// CreateTexture2D creates a 2D texture.
func (d Device) CreateTexture2D(desc *Texture2DDesc) (Object, error) {
	var tex Object
	err := hr(d.call(vtDeviceCreateTexture2D, uintptr(unsafe.Pointer(desc)), 0, uintptr(unsafe.Pointer(&tex))))
	return tex, err
}

// CreateShaderResourceView creates an SRV for a texture.
func (d Device) CreateShaderResourceView(resource Object, desc *ShaderResourceViewDesc) (Object, error) {
	var srv Object
	err := hr(d.call(vtDeviceCreateShaderResourceView, uintptr(resource), uintptr(unsafe.Pointer(desc)), uintptr(unsafe.Pointer(&srv))))
	return srv, err
}

// CreateRenderTargetView creates a view of a texture with its own format.
func (d Device) CreateRenderTargetView(resource Object) (Object, error) {
	var v Object
	err := hr(d.call(vtDeviceCreateRenderTargetView, uintptr(resource), 0, uintptr(unsafe.Pointer(&v))))
	return v, err
}

// CreateVertexShader creates a vertex shader from compiled DXBC bytecode.
func (d Device) CreateVertexShader(bytecode []byte) (Object, error) {
	var vs Object
	err := hr(d.call(vtDeviceCreateVertexShader, uintptr(unsafe.Pointer(&bytecode[0])), uintptr(len(bytecode)), 0, uintptr(unsafe.Pointer(&vs))))
	return vs, err
}

// CreatePixelShader creates a pixel shader from compiled DXBC bytecode.
func (d Device) CreatePixelShader(bytecode []byte) (Object, error) {
	var ps Object
	err := hr(d.call(vtDeviceCreatePixelShader, uintptr(unsafe.Pointer(&bytecode[0])), uintptr(len(bytecode)), 0, uintptr(unsafe.Pointer(&ps))))
	return ps, err
}

// CreateSamplerState creates a texture sampler.
func (d Device) CreateSamplerState(desc *SamplerDesc) (Object, error) {
	var s Object
	err := hr(d.call(vtDeviceCreateSamplerState, uintptr(unsafe.Pointer(desc)), uintptr(unsafe.Pointer(&s))))
	return s, err
}

// Map maps a dynamic resource for CPU write.
func (c Context) Map(resource Object, mapType uint32) (MappedSubresource, error) {
	var m MappedSubresource
	err := hr(c.call(vtContextMap, uintptr(resource), 0, uintptr(mapType), 0, uintptr(unsafe.Pointer(&m))))
	return m, err
}

// Unmap finishes CPU access to a mapped resource.
func (c Context) Unmap(resource Object) {
	c.call(vtContextUnmap, uintptr(resource), 0)
}

// IASetPrimitiveTopology sets the input assembler topology.
func (c Context) IASetPrimitiveTopology(topology uint32) {
	c.call(vtContextIASetPrimitiveTopology, uintptr(topology))
}

// VSSetShader binds a vertex shader.
func (c Context) VSSetShader(vs Object) {
	c.call(vtContextVSSetShader, uintptr(vs), 0, 0)
}

// PSSetShader binds a pixel shader.
func (c Context) PSSetShader(ps Object) {
	c.call(vtContextPSSetShader, uintptr(ps), 0, 0)
}

// PSSetShaderResources binds SRVs starting at slot startSlot.
func (c Context) PSSetShaderResources(startSlot uint32, srvs []Object) {
	if len(srvs) == 0 {
		return
	}
	c.call(vtContextPSSetShaderResources, uintptr(startSlot), uintptr(len(srvs)), uintptr(unsafe.Pointer(&srvs[0])))
}

// PSSetConstantBuffers binds constant buffers starting at slot startSlot.
func (c Context) PSSetConstantBuffers(startSlot uint32, bufs []Object) {
	if len(bufs) == 0 {
		return
	}
	c.call(vtContextPSSetConstantBuffers, uintptr(startSlot), uintptr(len(bufs)), uintptr(unsafe.Pointer(&bufs[0])))
}

// PSSetSamplers binds samplers starting at slot startSlot.
func (c Context) PSSetSamplers(startSlot uint32, samplers []Object) {
	if len(samplers) == 0 {
		return
	}
	c.call(vtContextPSSetSamplers, uintptr(startSlot), uintptr(len(samplers)), uintptr(unsafe.Pointer(&samplers[0])))
}

// RSSetViewports sets one viewport.
func (c Context) RSSetViewports(vp *Viewport) {
	c.call(vtContextRSSetViewports, 1, uintptr(unsafe.Pointer(vp)))
}

// OMSetRenderTargets binds a single render target view (or unbinds when rtv == 0).
func (c Context) OMSetRenderTargets(rtv Object) {
	if rtv == 0 {
		c.call(vtContextOMSetRenderTargets, 0, 0, 0)
		return
	}
	c.call(vtContextOMSetRenderTargets, 1, uintptr(unsafe.Pointer(&rtv)), 0)
}

// ClearRenderTargetView fills a view with one colour.
func (c Context) ClearRenderTargetView(view Object, rgba *[4]float32) {
	c.call(vtContextClearRenderTargetView, uintptr(view), uintptr(unsafe.Pointer(rgba)))
}

// Draw issues a non-indexed draw call.
func (c Context) Draw(vertexCount, startVertex uint32) {
	c.call(vtContextDraw, uintptr(vertexCount), uintptr(startVertex))
}

// CopyResource copies the entire contents of src to dst on the GPU.
func (c Context) CopyResource(dst, src Object) {
	c.call(vtContextCopyResource, uintptr(dst), uintptr(src))
}

// Flush submits queued commands to the GPU immediately.
func (c Context) Flush() { c.call(vtContextFlush) }
