// Package decode turns an H.264 Annex-B byte stream into an image.Image
// using an OpenH264 decoder compiled to wasm32-wasip1 and run in pure Go
// with wazero. No cgo, no external decoder process: CGO_ENABLED=0 builds
// keep working on every target this project cross-compiles for.
//
// See tools/wasm-h264 for the build that produces h264dec.wasm, and
// dev_docs/t0-decoder-spike.md for the licence and patent analysis of the
// shipped decoder.
package decode

import (
	"context"
	_ "embed"
	"fmt"
	"image"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:embed h264dec.wasm
var wasmModule []byte

// sharedRuntime and sharedModule are created once per process and shared by
// every Decoder, since compiling the wasm bytecode is the expensive part of
// a cold start (see the New (cold) vs New (warm) numbers in
// dev_docs/t0-decoder-spike.md). wazero's Runtime and CompiledModule are
// both safe for concurrent use, so every Decoder can instantiate its own
// module instance from the same compiled code without additional locking.
//
// The runtime is configured with WithCloseOnContextDone so that a cancelled
// or timed-out ctx aborts an in-flight wasm call instead of running to
// completion: the input is network-sourced H.264 from a camera, and every
// exported call here takes a ctx, so callers need cancellation to actually
// work. This setting closes only the api.Module instance the interrupted
// call belongs to, i.e. one Decoder's own instance (see New); it does not
// touch sharedModule, which holds precompiled machine code, not live
// decoder state, and is unaffected by any Decoder's module being closed.
// This package does not configure a separate wazero CompilationCache
// (the persistent, on-disk cache), so there is nothing else for this
// setting to interact with. See the Decoder and call doc comments for what
// happens to a Decoder after one of its calls is interrupted this way.
// sharedRuntime and sharedModule are intentionally never closed - nothing
// calls sharedRuntime.Close - since they are meant to live for the whole
// process, shared by every Decoder created over its lifetime.
var (
	sharedOnce    sync.Once
	sharedRuntime wazero.Runtime
	sharedModule  wazero.CompiledModule
	sharedErr     error
)

func getCompiled(ctx context.Context) (wazero.Runtime, wazero.CompiledModule, error) {
	sharedOnce.Do(func() {
		config := wazero.NewRuntimeConfig().WithCloseOnContextDone(true)
		sharedRuntime = wazero.NewRuntimeWithConfig(ctx, config)
		if _, err := wasi_snapshot_preview1.Instantiate(ctx, sharedRuntime); err != nil {
			sharedErr = fmt.Errorf("decode: instantiate wasi_snapshot_preview1: %w", err)
			return
		}
		sharedModule, sharedErr = sharedRuntime.CompileModule(ctx, wasmModule)
	})
	return sharedRuntime, sharedModule, sharedErr
}

// Decoder decodes H.264 Annex-B keyframes to images. Create one with New,
// feed it a byte stream that contains at least one SPS, PPS and IDR NAL
// unit (in any order relative to leading garbage, since DecodeKeyframe
// skips non-VCL NAL units and anything before the first IDR), and call
// Close when done. A Decoder is not safe for concurrent use.
//
// DecodeKeyframe gives every call a fresh wasm module instance internally
// (see its doc comment), so calls do not need to be related to each other:
// the same Decoder can be reused across many unrelated DecodeKeyframe
// calls over the life of a long-running process, which is the whole point
// of keeping one around (see the New (cold) vs New (warm) timings in
// dev_docs/t0-decoder-spike.md).
//
// If the ctx passed to a DecodeKeyframe call is cancelled or reaches its
// deadline while a wasm call is in flight, the underlying module instance
// is closed (see the sharedRuntime doc comment) and the Decoder is left
// closed: that call returns an error immediately, and every later call on
// the same Decoder also returns an error, the same as after Close. Start a
// fresh Decoder to keep decoding.
type Decoder struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule

	rt       api.Module
	init     api.Function
	decode   api.Function
	width    api.Function
	height   api.Function
	strideY  api.Function
	strideUV api.Function
	planeY   api.Function
	planeU   api.Function
	planeV   api.Function
	alloc    api.Function
	free     api.Function

	closed bool
}

// New instantiates a fresh copy of the wasm decoder module and initializes
// the underlying OpenH264 decoder. Each Decoder owns an independent module
// instance (and thus independent decoder state), so multiple Decoders can
// be used concurrently from different goroutines.
func New(ctx context.Context) (*Decoder, error) {
	rt, mod, err := getCompiled(ctx)
	if err != nil {
		return nil, fmt.Errorf("decode: compile wasm module: %w", err)
	}

	d := &Decoder{runtime: rt, compiled: mod}
	if err := d.instantiate(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

// instantiate creates a brand new wasm module instance from the shared
// compiled module, binds its exports and (re)initializes the OpenH264
// decoder inside it, closing whatever instance the Decoder held before
// (if any). New calls this once; DecodeKeyframe calls it again at the
// start of every call, so that a Decoder reused across many calls never
// carries decoder state from one call into the next (see DecodeKeyframe's
// doc comment for why that matters). Re-instantiating from the shared
// CompiledModule is cheap (the "New (warm)" case in
// dev_docs/t0-decoder-spike.md, roughly 1-3 ms), unlike compiling the wasm
// module itself, which getCompiled already does only once per process.
//
// Any failure here leaves the Decoder closed rather than holding onto a
// stale or partially set up instance: a Decoder that cannot be reset to a
// known-good state is not safe to keep using.
func (d *Decoder) instantiate(ctx context.Context) error {
	if d.rt != nil {
		_ = d.rt.Close(ctx)
		d.rt = nil
	}

	instance, err := d.runtime.InstantiateModule(ctx, d.compiled, wazero.NewModuleConfig())
	if err != nil {
		d.closed = true
		return fmt.Errorf("decode: instantiate wasm module: %w", err)
	}

	d.rt = instance
	d.init = instance.ExportedFunction("h264dec_init")
	d.decode = instance.ExportedFunction("h264dec_decode")
	d.width = instance.ExportedFunction("h264dec_width")
	d.height = instance.ExportedFunction("h264dec_height")
	d.strideY = instance.ExportedFunction("h264dec_stride_y")
	d.strideUV = instance.ExportedFunction("h264dec_stride_uv")
	d.planeY = instance.ExportedFunction("h264dec_plane_y")
	d.planeU = instance.ExportedFunction("h264dec_plane_u")
	d.planeV = instance.ExportedFunction("h264dec_plane_v")
	d.alloc = instance.ExportedFunction("h264dec_alloc")
	d.free = instance.ExportedFunction("h264dec_free")

	for name, fn := range map[string]api.Function{
		"h264dec_init": d.init, "h264dec_decode": d.decode,
		"h264dec_width": d.width, "h264dec_height": d.height,
		"h264dec_stride_y": d.strideY, "h264dec_stride_uv": d.strideUV,
		"h264dec_plane_y": d.planeY, "h264dec_plane_u": d.planeU, "h264dec_plane_v": d.planeV,
		"h264dec_alloc": d.alloc, "h264dec_free": d.free,
	} {
		if fn == nil {
			_ = instance.Close(ctx)
			d.closed = true
			return fmt.Errorf("decode: wasm module missing export %q", name)
		}
	}

	res, err := d.call(ctx, d.init)
	if err != nil {
		_ = instance.Close(ctx)
		d.closed = true
		return fmt.Errorf("decode: init decoder: %w", err)
	}
	if rc := int32(res[0]); rc != 0 {
		_ = instance.Close(ctx)
		d.closed = true
		return fmt.Errorf("decode: decoder init returned %d", rc)
	}

	return nil
}

// Close releases the wasm module instance. Safe to call more than once.
func (d *Decoder) Close(ctx context.Context) error {
	if d.closed {
		return nil
	}
	d.closed = true
	return d.rt.Close(ctx)
}

// nalUnits splits an Annex-B byte stream into individual NAL units, each
// returned including its start code (3 or 4 bytes), matching what
// OpenH264's DecodeFrameNoDelay expects per call.
func nalUnits(data []byte) [][]byte {
	var starts []int
	for i := 0; i+2 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		return nil
	}

	units := make([][]byte, 0, len(starts))
	for i, start := range starts {
		end := len(data)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		// Prefer a 4-byte start code (0x00000001) over the 3-byte one
		// found above when the extra leading zero is actually present.
		begin := start
		if begin > 0 && data[begin-1] == 0 {
			begin--
		}
		units = append(units, data[begin:end])
	}
	return units
}

// nalType returns the H.264 NAL unit type of a NAL unit that includes its
// Annex-B start code (3 or 4 bytes).
func nalType(unit []byte) int {
	i := 3
	if len(unit) >= 4 && unit[2] == 0 {
		i = 4
	}
	if i >= len(unit) {
		return -1
	}
	return int(unit[i] & 0x1f)
}

const (
	nalSPS = 7
	nalPPS = 8
	nalIDR = 5
)

// DecodeKeyframe decodes the first complete IDR keyframe found in annexB,
// an Annex-B H.264 byte stream. Leading bytes before the first SPS, PPS
// and IDR NAL units are skipped, which handles input that starts mid
// stream (as WebRTC RTP depacketization output typically does before the
// first full GOP has arrived). Returns an *image.YCbCr in the decoder's
// native full-range 4:2:0 layout; no colour-space conversion is applied,
// which is correct for the yuvj420p (full range) streams this package
// targets.
//
// Each call starts from a clean decoder instance (see instantiate): a
// picture is only known to be complete once the decoder sees the first
// NAL unit of the following picture, so a call that successfully returns
// a keyframe has necessarily already fed part of the next picture into
// the decoder too. Without resetting, that leftover, never-finished
// picture would still be sitting in the decoder's state on the next call,
// and could corrupt its output. Resetting first makes repeated calls on
// the same Decoder, whether decoding the same bytes again or a completely
// different stream, independent of each other and of that behaviour.
func (d *Decoder) DecodeKeyframe(ctx context.Context, annexB []byte) (image.Image, error) {
	return d.decodeAll(ctx, annexB, true)
}

// DecodeLatest decodes every NAL unit in annexB in stream order, exactly
// like DecodeKeyframe, but instead of returning as soon as the first
// picture completes, it keeps going through every NAL unit in annexB and
// returns whichever picture completed last. This is for a caller that
// passes in a concatenation of several access units in capture order (for
// example internal/daemon's per-printer rolling GOP buffer: a keyframe
// access unit followed by every access unit received since) and wants the
// most recent decodable picture rather than the first one (the keyframe
// itself). Because a picture only becomes complete once the decoder sees
// the first NAL unit of the picture that follows it (see the package doc
// comment), the very last access unit in annexB never itself completes a
// picture - the returned image is therefore whichever access unit's
// picture completed last, one position behind the end of annexB. Returns
// an error if no picture was decoded at all (e.g. annexB holds only a
// single access unit with nothing after it to close it out).
func (d *Decoder) DecodeLatest(ctx context.Context, annexB []byte) (image.Image, error) {
	return d.decodeAll(ctx, annexB, false)
}

// decodeAll is DecodeKeyframe and DecodeLatest's shared implementation:
// stopAtFirst true returns as soon as the first picture completes
// (DecodeKeyframe's contract); false keeps feeding every remaining NAL
// unit and returns whichever picture completed last (DecodeLatest's
// contract).
func (d *Decoder) decodeAll(ctx context.Context, annexB []byte, stopAtFirst bool) (image.Image, error) {
	if d.closed {
		return nil, fmt.Errorf("decode: decoder is closed")
	}

	if err := d.instantiate(ctx); err != nil {
		return nil, fmt.Errorf("decode: reset decoder for new call: %w", err)
	}

	units := nalUnits(annexB)
	if len(units) == 0 {
		return nil, fmt.Errorf("decode: no NAL units found in input")
	}

	// Every NAL unit is fed to the decoder in stream order, including
	// anything before the first SPS/PPS: OpenH264's own parser discards
	// NAL types it is not ready for, so there is no need to duplicate
	// that skip logic here. sawSPS/sawPPS/sawIDR exist only to produce a
	// useful error if no picture ever comes back.
	sawSPS, sawPPS, sawIDR := false, false, false
	var last image.Image
	for _, unit := range units {
		switch nalType(unit) {
		case nalSPS:
			sawSPS = true
		case nalPPS:
			sawPPS = true
		case nalIDR:
			sawIDR = true
		}

		img, err := d.feedNAL(ctx, unit)
		if err != nil {
			return nil, err
		}
		if img != nil {
			if stopAtFirst {
				return img, nil
			}
			last = img
		}
	}

	if last == nil {
		return nil, fmt.Errorf("decode: no keyframe decoded from %d NAL units (sps=%v pps=%v idr=%v)", len(units), sawSPS, sawPPS, sawIDR)
	}
	return last, nil
}

func (d *Decoder) feedNAL(ctx context.Context, unit []byte) (image.Image, error) {
	allocRes, err := d.call(ctx, d.alloc, uint64(len(unit)))
	if err != nil {
		return nil, fmt.Errorf("decode: alloc: %w", err)
	}
	ptr := uint32(allocRes[0])
	if ptr == 0 {
		return nil, fmt.Errorf("decode: alloc returned null for %d bytes", len(unit))
	}
	defer func() { _, _ = d.call(ctx, d.free, uint64(ptr)) }()

	mem := d.rt.Memory()
	if !mem.Write(ptr, unit) {
		return nil, fmt.Errorf("decode: failed to write %d bytes at offset %d", len(unit), ptr)
	}

	res, err := d.call(ctx, d.decode, uint64(ptr), uint64(len(unit)))
	if err != nil {
		return nil, fmt.Errorf("decode: decode call: %w", err)
	}
	status := int32(res[0])
	if status < 0 {
		return nil, fmt.Errorf("decode: decoder returned error %d", status)
	}
	if status == 0 {
		return nil, nil
	}

	return d.readFrame(ctx)
}

// maxDecodeDimension and maxStridePad bound the decoder-reported width,
// height and strides that readFrame trusts before computing plane sizes
// and copying wasm memory: a well-formed OpenH264 decode of any input this
// package targets stays far inside them, so hitting either cap means the
// reported values are not sane and readFrame should refuse them rather
// than compute an oversized or nonsensical read.
const (
	maxDecodeDimension = 4096
	maxStridePad       = 256
)

func (d *Decoder) readFrame(ctx context.Context) (image.Image, error) {
	width, err := d.callInt32(ctx, d.width)
	if err != nil {
		return nil, err
	}
	height, err := d.callInt32(ctx, d.height)
	if err != nil {
		return nil, err
	}
	strideY, err := d.callInt32(ctx, d.strideY)
	if err != nil {
		return nil, err
	}
	strideUV, err := d.callInt32(ctx, d.strideUV)
	if err != nil {
		return nil, err
	}
	yOff, err := d.callUint32(ctx, d.planeY)
	if err != nil {
		return nil, err
	}
	uOff, err := d.callUint32(ctx, d.planeU)
	if err != nil {
		return nil, err
	}
	vOff, err := d.callUint32(ctx, d.planeV)
	if err != nil {
		return nil, err
	}

	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("decode: decoder reported invalid dimensions %dx%d", width, height)
	}
	if width > maxDecodeDimension || height > maxDecodeDimension {
		return nil, fmt.Errorf("decode: decoder reported unreasonable dimensions %dx%d, want at most %dx%d", width, height, maxDecodeDimension, maxDecodeDimension)
	}

	chromaW := (width + 1) / 2
	chromaH := (height + 1) / 2

	// Strides must cover at least the plane's own width, and OpenH264's
	// buffer padding (macroblock and reference-extension alignment, see
	// AllocPicture in pic_queue.cpp) pads the luma linesize to
	// align(width+64, 32) and the chroma linesize accordingly
	// (PADDING_LENGTH 32, PICTURE_RESOLUTION_ALIGNMENT 32 in pic_queue.cpp),
	// so the real overhead is under about 96 bytes in practice. maxStridePad
	// is a conservative cap well above that, there to catch a corrupted or
	// nonsensical stride before it is used to size a memory read, not to
	// model the exact padding formula.
	if strideY < width || strideY > width+maxStridePad {
		return nil, fmt.Errorf("decode: decoder reported unreasonable Y stride %d for width %d", strideY, width)
	}
	if strideUV < chromaW || strideUV > chromaW+maxStridePad {
		return nil, fmt.Errorf("decode: decoder reported unreasonable UV stride %d for chroma width %d", strideUV, chromaW)
	}

	mem := d.rt.Memory()

	ySize := uint32(strideY) * uint32(height)
	cSize := uint32(strideUV) * uint32(chromaH)

	yBytes, ok := mem.Read(yOff, ySize)
	if !ok {
		return nil, fmt.Errorf("decode: failed to read Y plane (%d bytes at %d)", ySize, yOff)
	}
	uBytes, ok := mem.Read(uOff, cSize)
	if !ok {
		return nil, fmt.Errorf("decode: failed to read U plane (%d bytes at %d)", cSize, uOff)
	}
	vBytes, ok := mem.Read(vOff, cSize)
	if !ok {
		return nil, fmt.Errorf("decode: failed to read V plane (%d bytes at %d)", cSize, vOff)
	}

	img := &image.YCbCr{
		Y:              append([]byte(nil), yBytes...),
		Cb:             append([]byte(nil), uBytes...),
		Cr:             append([]byte(nil), vBytes...),
		YStride:        int(strideY),
		CStride:        int(strideUV),
		SubsampleRatio: image.YCbCrSubsampleRatio420,
		Rect:           image.Rect(0, 0, int(width), int(height)),
	}
	return img, nil
}

// call invokes an exported wasm function through fn, and marks the Decoder
// closed if ctx was cancelled or its deadline was reached during the call.
// wazero's WithCloseOnContextDone (enabled in getCompiled) closes the
// underlying module instance in that situation, so every further wasm call
// on this Decoder would fail anyway; recording it here turns that into an
// explicit, permanent "decoder is closed" state instead of a fresh
// wasm-level error on each subsequent call.
func (d *Decoder) call(ctx context.Context, fn api.Function, params ...uint64) ([]uint64, error) {
	res, err := fn.Call(ctx, params...)
	if err != nil && ctx.Err() != nil {
		d.closed = true
		return nil, fmt.Errorf("decode: context done during wasm call, decoder closed: %w", ctx.Err())
	}
	return res, err
}

func (d *Decoder) callInt32(ctx context.Context, fn api.Function) (int32, error) {
	res, err := d.call(ctx, fn)
	if err != nil {
		return 0, fmt.Errorf("decode: call failed: %w", err)
	}
	return int32(res[0]), nil
}

func (d *Decoder) callUint32(ctx context.Context, fn api.Function) (uint32, error) {
	res, err := d.call(ctx, fn)
	if err != nil {
		return 0, fmt.Errorf("decode: call failed: %w", err)
	}
	return uint32(res[0]), nil
}
