package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"runtime"
	"sort"
	"sync"
)

// Header video metadata [big-endian format].
type Header struct {
	Width      uint32
	Height     uint32
	FrameCount uint32
}

// RectRule represents one compositing rectangle.
type RectRule struct {
	Src   [4]uint32 // x, y, width, height
	Dest  [2]uint32 // x, y
	Alpha float32   // blend factor
	Z     uint32    // stacking order
}

// RulesConfig is the top‑level JSON config.
type RulesConfig struct {
	Size  [2]uint32  // output width, height
	Rects []RectRule // list of rectangles
}

// Frame represents a single frame and its metadata
type Frame struct {
	id     uint32
	inBuf  []byte
	outBuf []byte
}

// Global zero buffer for faster clearing (create once, reuse many times)
var zeroBuffer []byte

// processFrame composites a single frame according to the rules
func processFrame(frame *Frame, rules *RulesConfig, inWidth, inHeight, outWidth, outHeight uint32) {
	// Clear output buffer (much faster than zeroing each byte individually)
	copy(frame.outBuf, zeroBuffer[:len(frame.outBuf)])

	// composite all rectangles
	for _, r := range rules.Rects {
		// Convert float alpha to 8-bit integer (0-255) for fixed-point math
		alpha := uint32(r.Alpha * 255)
		invAlpha := 255 - alpha

		// Precompute bounds to avoid redundant calculations in pixel loops
		srcStartX := r.Src[0]
		srcStartY := r.Src[1]
		srcWidth := r.Src[2]
		srcHeight := r.Src[3]
		dstStartX := r.Dest[0]
		dstStartY := r.Dest[1]

		// Calculate effective bounds with clipping to screen boundaries
		effectiveWidth := srcWidth
		effectiveHeight := srcHeight

		// Apply clipping for source rectangle
		if srcStartX >= inWidth || srcStartY >= inHeight {
			continue // Rectangle is completely off-screen (source)
		}

		// Clip width if source extends beyond right edge
		if srcStartX+srcWidth > inWidth {
			effectiveWidth = inWidth - srcStartX
		}

		// Clip height if source extends beyond bottom edge
		if srcStartY+srcHeight > inHeight {
			effectiveHeight = inHeight - srcStartY
		}

		// Apply clipping for destination rectangle
		if dstStartX >= outWidth || dstStartY >= outHeight {
			continue // Rectangle is completely off-screen (destination)
		}

		// Clip width if destination extends beyond right edge
		if dstStartX+effectiveWidth > outWidth {
			// Adjust source width based on destination clipping
			excessWidth := dstStartX + effectiveWidth - outWidth
			effectiveWidth -= excessWidth
		}

		// Clip height if destination extends beyond bottom edge
		if dstStartY+effectiveHeight > outHeight {
			// Adjust source height based on destination clipping
			excessHeight := dstStartY + effectiveHeight - outHeight
			effectiveHeight -= excessHeight
		}

		// Skip if rectangle is fully transparent (alpha = 0)
		if alpha == 0 {
			continue
		}

		// Skip this rectangle if it has 100% transparency or zero size
		if effectiveWidth == 0 || effectiveHeight == 0 {
			continue
		}

		// Use optimized SIMD blend function for the entire rectangle at once
		// This function will choose the best implementation (SIMD vs fallback) based on platform support
		blendRectangle(
			frame,
			srcStartX, srcStartY,
			dstStartX, dstStartY,
			effectiveWidth, effectiveHeight,
			inWidth, outWidth,
			alpha, invAlpha,
		)
	}
}

func main() {
	fmt.Print("🔄 Compositing video... ")

	// 1. Load the JSON rules
	rulesFile, err := os.Open("input/rules.json")
	if err != nil {
		log.Fatalf("failed to open rules.json: %v", err)
	}
	defer rulesFile.Close()

	// 2. Read the JSON rules
	var rules RulesConfig
	if err := json.NewDecoder(rulesFile).Decode(&rules); err != nil {
		log.Fatalf("failed to parse rules.json: %v", err)
	}

	// 3. Open the raw input video
	inFile, err := os.Open("input/video.rvid")
	if err != nil {
		log.Fatalf("failed to open input video.rvid.gz: %v", err)
	}
	defer inFile.Close()

	// 4. Read its header
	var hdr Header
	if err := binary.Read(inFile, binary.BigEndian, &hdr); err != nil {
		log.Fatalf("failed to read header: %v", err)
	}
	fmt.Printf("📹 Input video: %dx%d, %d frames\n", hdr.Width, hdr.Height, hdr.FrameCount)

	// 5. Create the output file and write its header
	outFile, err := os.Create("output/video-out.raw")
	if err != nil {
		log.Fatalf("failed to create output file: %v", err)
	}
	defer outFile.Close()

	outWidth := rules.Size[0]
	outHeight := rules.Size[1]
	if err := binary.Write(outFile, binary.BigEndian, outWidth); err != nil {
		log.Fatalf("write width: %v", err)
	}
	if err := binary.Write(outFile, binary.BigEndian, outHeight); err != nil {
		log.Fatalf("write height: %v", err)
	}
	if err := binary.Write(outFile, binary.BigEndian, hdr.FrameCount); err != nil {
		log.Fatalf("write frame count: %v", err)
	}

	// 6. Sort rectangles by Z-order
	sort.Slice(rules.Rects, func(i, j int) bool {
		return rules.Rects[i].Z < rules.Rects[j].Z
	})

	// 7. Determine the number of worker goroutines based on CPU cores
	numWorkers := runtime.GOMAXPROCS(0)
	if numWorkers < 1 {
		numWorkers = 1
	} // 8. Set up worker pool
	inFrameSize := int(hdr.Width * hdr.Height * 3)
	outFrameSize := int(outWidth * outHeight * 3)

	// Initialize the zero buffer with the maximum size we'll need
	zeroBuffer = make([]byte, outFrameSize)

	// Channel to send frames to workers
	frameQueue := make(chan *Frame, numWorkers*2)

	// Channel for processed frames ready to be written
	resultQueue := make(chan *Frame, numWorkers*2)

	// Use a wait group to know when all workers are done
	var wg sync.WaitGroup

	// Start worker goroutines
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for frame := range frameQueue {
				// Process the frame
				processFrame(frame, &rules, hdr.Width, hdr.Height, outWidth, outHeight)

				// Send to result queue for writing
				resultQueue <- frame
			}
		}()
	}

	// Start a goroutine to close result queue when all workers are done
	go func() {
		wg.Wait()
		close(resultQueue)
	}()

	// Create a buffer pool for frame data to reduce allocations
	framePool := sync.Pool{
		New: func() interface{} {
			return &Frame{
				inBuf:  make([]byte, inFrameSize),
				outBuf: make([]byte, outFrameSize),
			}
		},
	}

	// Start a goroutine to read frames and enqueue them for processing
	go func() {
		for frame := uint32(0); frame < hdr.FrameCount; frame++ {
			// Get a frame from the pool
			frameData := framePool.Get().(*Frame)
			frameData.id = frame

			// Read frame data
			if _, err := io.ReadFull(inFile, frameData.inBuf); err != nil {
				log.Fatalf("read frame %d: %v", frame, err)
				close(frameQueue)
				return
			}

			// Send to worker queue
			frameQueue <- frameData
		}
		close(frameQueue)
	}()

	// Create a map to store frames that arrive out of order
	pendingFrames := make(map[uint32]*Frame)
	nextFrame := uint32(0)

	// Process results and write to output in correct order
	for frame := range resultQueue {
		// If this is the next frame we're expecting, write it immediately
		if frame.id == nextFrame {
			if _, err := outFile.Write(frame.outBuf); err != nil {
				log.Fatalf("write frame %d: %v", frame.id, err)
			}
			nextFrame++

			// Return frame buffer to pool
			framePool.Put(frame)

			// Check if we have any pending frames that can now be written
			for {
				if pendingFrame, exists := pendingFrames[nextFrame]; exists {
					if _, err := outFile.Write(pendingFrame.outBuf); err != nil {
						log.Fatalf("write frame %d: %v", pendingFrame.id, err)
					}
					delete(pendingFrames, nextFrame)
					framePool.Put(pendingFrame)
					nextFrame++
				} else {
					break
				}
			}
		} else {
			// This frame arrived out of order, store it for later
			pendingFrames[frame.id] = frame
		}
	}

	fmt.Println("✅ Done! Composited video written to output/video-out.raw")
}
