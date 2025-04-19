package main

import (
	"unsafe"
)

// This file provides implementations for blending operations

// prefetchForRead prefetches memory address for reading
// This helps to make more efficient use of CPU cache
func prefetchForRead(addr unsafe.Pointer) {
	// This is a no-op on most platforms
	// Assembly version would use PREFETCHT0 instruction on x86
}

// blendRowSIMD blends a row of pixels using SIMD instructions when supported
// Fallback implementation for systems that don't support assembly
func blendRowSIMD(dst, src []byte, alpha, invAlpha uint32) {
	for i := 0; i < len(dst) && i < len(src); i += 3 {
		// Standard alpha blending for RGB channels
		for c := 0; c < 3; c++ {
			blend := (uint32(src[i+c])*alpha + uint32(dst[i+c])*invAlpha) >> 8
			dst[i+c] = byte(blend)
		}
	}
}

// blendRectangle applies alpha blending to a rectangle
// This version optimizes for different alpha values and uses SIMD when available
func blendRectangle(frame *Frame, srcStartX, srcStartY, dstStartX, dstStartY, width, height,
	inWidth, outWidth, alpha, invAlpha uint32) {

	// Early return for fully transparent rectangles
	if alpha == 0 {
		return
	}

	// Special case for 100% opacity - just copy (fast path)
	if alpha == 255 && invAlpha == 0 {
		for y := uint32(0); y < height; y++ {
			srcY := srcStartY + y
			dstY := dstStartY + y

			srcRowIdx := int(srcY * inWidth * 3)
			dstRowIdx := int(dstY * outWidth * 3)

			// Calculate the start of the row slice
			srcRowStart := srcRowIdx + int(srcStartX*3)
			dstRowStart := dstRowIdx + int(dstStartX*3)

			// Direct copy for fully opaque pixels
			copy(frame.outBuf[dstRowStart:dstRowStart+int(width*3)],
				frame.inBuf[srcRowStart:srcRowStart+int(width*3)])
		}
		return
	}

	for y := uint32(0); y < height; y++ {
		srcY := srcStartY + y
		dstY := dstStartY + y

		// Calculate base indices for the entire row
		srcRowIdx := int(srcY * inWidth * 3)
		dstRowIdx := int(dstY * outWidth * 3)

		// Calculate the start of the row slice
		srcRowStart := srcRowIdx + int(srcStartX*3)
		dstRowStart := dstRowIdx + int(dstStartX*3)

		// Get row slices
		srcRow := frame.inBuf[srcRowStart : srcRowStart+int(width*3)]
		dstRow := frame.outBuf[dstRowStart : dstRowStart+int(width*3)]

		// Use SIMD acceleration for blending the row
		blendRowSIMD(dstRow, srcRow, alpha, invAlpha)
	}
}