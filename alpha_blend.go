package main

// This file provides Go implementations for blending operations

// blendRectangle applies alpha blending to a rectangle using Go implementation
func blendRectangle(frame *Frame, srcStartX, srcStartY, dstStartX, dstStartY, width, height,
	inWidth, outWidth, alpha, invAlpha uint32) {

	for y := uint32(0); y < height; y++ {
		srcY := srcStartY + y
		dstY := dstStartY + y

		// Calculate base indices for the entire row
		srcRowIdx := int(srcY * inWidth * 3)
		dstRowIdx := int(dstY * outWidth * 3)

		// Use pixel-by-pixel processing (Go implementation)
		for x := uint32(0); x < width; x++ {
			srcX := srcStartX + x
			dstX := dstStartX + x

			srcIdx := srcRowIdx + int(srcX*3)
			dstIdx := dstRowIdx + int(dstX*3)

			// Standard alpha blending for individual pixels
			for c := 0; c < 3; c++ {
				src := uint32(frame.inBuf[srcIdx+c])
				dst := uint32(frame.outBuf[dstIdx+c])
				frame.outBuf[dstIdx+c] = byte((src*alpha + dst*invAlpha) >> 8)
			}
		}
	}
}
