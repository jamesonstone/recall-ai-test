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
	"unsafe"
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

// Global variables
var (
	// Zero buffer for faster clearing (create once, reuse many times)
	zeroBuffer []byte
	
	// Tile grid for spatial acceleration
	tileGrid []TileRects
)

// TileRects stores which rectangles intersect a specific tile
type TileRects struct {
	rects    []int  // Indices into the rules.Rects slice
	hasCover bool   // Indicates if this tile has a fully opaque rectangle covering it
}

// Tile size for processing frames in blocks to improve cache efficiency
const tileSize = 128

// TileJob represents a single tile processing job
type TileJob struct {
	tileX, tileY uint32
	tileIdx      int
}

// Maximum number of tiles to process in parallel
// This is a tunable parameter that balances parallelism with memory usage
const maxTileWorkers = 1

// processTile handles rendering a single tile
func processTile(frame *Frame, rules *RulesConfig, 
                tileX, tileY, tileStartX, tileStartY, tileEndX, tileEndY uint32,
                inWidth, inHeight, outWidth, outHeight uint32, tileIdx int) {
	
	// Skip empty tiles entirely
	if tileIdx >= len(tileGrid) || len(tileGrid[tileIdx].rects) == 0 {
		return
	}
	
	// Only process rectangles known to intersect this tile
	// Process in original order (from first to last in Z-order)
	// Reverse-iterate since tiles were populated in reverse Z-order
	for i := len(tileGrid[tileIdx].rects) - 1; i >= 0; i-- {
		rectIdx := tileGrid[tileIdx].rects[i]
		r := rules.Rects[rectIdx]
		
		// Convert float alpha to 8-bit integer (0-255) for fixed-point math
		alpha := uint32(r.Alpha * 255)
		invAlpha := 255 - alpha
		
		// Skip if rectangle is fully transparent (shouldn't happen due to preprocessing)
		if alpha == 0 {
			continue
		}
		
		// Precompute bounds to avoid redundant calculations
		srcStartX := r.Src[0]
		srcStartY := r.Src[1]
		srcWidth := r.Src[2]
		srcHeight := r.Src[3]
		dstStartX := r.Dest[0]
		dstStartY := r.Dest[1]
		dstEndX := dstStartX + srcWidth
		dstEndY := dstStartY + srcHeight
		
		// Calculate intersection of tile and rectangle
		effectiveStartX := maxUint32(dstStartX, tileStartX)
		effectiveStartY := maxUint32(dstStartY, tileStartY)
		effectiveEndX := minUint32(dstEndX, tileEndX)
		effectiveEndY := minUint32(dstEndY, tileEndY)
		
		// Calculate effective width and height
		effectiveWidth := effectiveEndX - effectiveStartX
		effectiveHeight := effectiveEndY - effectiveStartY
		
		// Calculate source coordinates after intersection
		srcOffsetX := effectiveStartX - dstStartX
		srcOffsetY := effectiveStartY - dstStartY
		
		// Calculate adjusted source coordinates
		adjSrcStartX := srcStartX + srcOffsetX
		adjSrcStartY := srcStartY + srcOffsetY
		
		// Apply source clipping (shouldn't be needed due to preprocessing, but safety first)
		if adjSrcStartX >= inWidth || adjSrcStartY >= inHeight {
			continue
		}
		
		// Clip width/height if source extends beyond edges
		if adjSrcStartX+effectiveWidth > inWidth {
			effectiveWidth = inWidth - adjSrcStartX
		}
		
		if adjSrcStartY+effectiveHeight > inHeight {
			effectiveHeight = inHeight - adjSrcStartY
		}
		
		// Skip if clipping resulted in zero size
		if effectiveWidth == 0 || effectiveHeight == 0 {
			continue
		}
		
		// Fast path for common alpha values
		if alpha == 255 && invAlpha == 0 {
			// 100% opacity - direct copy with no blending
			for y := uint32(0); y < effectiveHeight; y++ {
				srcY := adjSrcStartY + y
				dstY := effectiveStartY + y
				
				srcRowIdx := int(srcY * inWidth * 3)
				dstRowIdx := int(dstY * outWidth * 3)
				
				// Calculate row offsets
				srcRowOffset := srcRowIdx + int(adjSrcStartX*3)
				dstRowOffset := dstRowIdx + int(effectiveStartX*3)
				
				// Direct copy - much faster than blending
				copy(frame.outBuf[dstRowOffset:dstRowOffset+int(effectiveWidth*3)],
					frame.inBuf[srcRowOffset:srcRowOffset+int(effectiveWidth*3)])
			}
		} else {
			// Use optimized SIMD blend function for this tile's intersection with the rectangle
			blendRectangle(
				frame,
				adjSrcStartX, adjSrcStartY,
				effectiveStartX, effectiveStartY,
				effectiveWidth, effectiveHeight,
				inWidth, outWidth,
				alpha, invAlpha,
			)
		}
	}
}

// processFrame composites a single frame according to the rules, using the precomputed tile grid
// This version implements parallel tile processing for significantly better performance
func processFrame(frame *Frame, rules *RulesConfig, inWidth, inHeight, outWidth, outHeight uint32) {
	// Clear output buffer (much faster than zeroing each byte individually)
	copy(frame.outBuf, zeroBuffer[:len(frame.outBuf)])

	// Get the tile dimensions again (though these should match the preprocessed values)
	tilesX := (outWidth + tileSize - 1) / tileSize
	tilesY := (outHeight + tileSize - 1) / tileSize
	
	// Determine the number of tiles and optimize worker count
	totalTiles := int(tilesX * tilesY)
	numTileWorkers := maxTileWorkers
	
	// Don't create more workers than we have tiles
	if numTileWorkers > totalTiles {
		numTileWorkers = totalTiles
	}

	// Don't create more workers than we have CPUs
	cpuCount := runtime.NumCPU()
	if numTileWorkers > cpuCount {
		numTileWorkers = cpuCount
	}
	
	// For very small frame counts, just process sequentially
	if totalTiles < 4 {
		numTileWorkers = 1
	}
	
	// For single-worker case, process tiles sequentially with cache-friendly zigzag pattern
	if numTileWorkers <= 1 {
		for tileY := uint32(0); tileY < tilesY; tileY++ {
			// Zigzag pattern: even rows left-to-right, odd rows right-to-left
			if tileY % 2 == 0 {
				// Even rows: process tiles from left to right
				for tileX := uint32(0); tileX < tilesX; tileX++ {
					// Calculate tile bounds
					tileStartX := tileX * tileSize
					tileStartY := tileY * tileSize
					tileEndX := minUint32(tileStartX+tileSize, outWidth)
					tileEndY := minUint32(tileStartY+tileSize, outHeight)
					
					// Get the precomputed list of rectangles for this tile
					tileIdx := int(tileY*tilesX + tileX)
					
					// Prefetch data for the next tile if we're not at the end of the row
					if tileX + 1 < tilesX {
						nextTileIdx := int(tileY*tilesX + tileX + 1)
						if nextTileIdx < len(tileGrid) && len(tileGrid[nextTileIdx].rects) > 0 {
							// Prefetch the start of next tile's input data
							nextTileStartX := (tileX+1) * tileSize
							if nextTileStartX < inWidth {
								prefetchY := tileStartY
								if prefetchY < inHeight {
									prefetchRowIdx := int(prefetchY * inWidth * 3)
									prefetchPos := prefetchRowIdx + int(nextTileStartX*3)
									if prefetchPos < len(frame.inBuf) {
										prefetchForRead(unsafe.Pointer(&frame.inBuf[prefetchPos]))
									}
								}
							}
						}
					}
					
					// Process this tile
					processTile(frame, rules, 
					            tileX, tileY, tileStartX, tileStartY, tileEndX, tileEndY,
					            inWidth, inHeight, outWidth, outHeight, tileIdx)
				}
			} else {
				// Odd rows: process tiles from right to left
				for tileX := tilesX; tileX > 0; tileX-- {
					actualX := tileX - 1
					
					// Calculate tile bounds
					tileStartX := actualX * tileSize
					tileStartY := tileY * tileSize
					tileEndX := minUint32(tileStartX+tileSize, outWidth)
					tileEndY := minUint32(tileStartY+tileSize, outHeight)
					
					// Get the precomputed list of rectangles for this tile
					tileIdx := int(tileY*tilesX + actualX)
					
					// Prefetch data for the next tile if we're not at the start of the row
					if actualX > 0 {
						nextTileIdx := int(tileY*tilesX + actualX - 1)
						if nextTileIdx < len(tileGrid) && len(tileGrid[nextTileIdx].rects) > 0 {
							// Prefetch the start of next tile's input data
							nextTileStartX := (actualX-1) * tileSize
							if nextTileStartX < inWidth {
								prefetchY := tileStartY
								if prefetchY < inHeight {
									prefetchRowIdx := int(prefetchY * inWidth * 3)
									prefetchPos := prefetchRowIdx + int(nextTileStartX*3)
									if prefetchPos < len(frame.inBuf) {
										prefetchForRead(unsafe.Pointer(&frame.inBuf[prefetchPos]))
									}
								}
							}
						}
					}
					
					// Process this tile
					processTile(frame, rules, 
					            actualX, tileY, tileStartX, tileStartY, tileEndX, tileEndY,
					            inWidth, inHeight, outWidth, outHeight, tileIdx)
				}
			}
		}
		return
	}
	
	// Multi-worker case: parallelize tile processing
	
	// Channel for distributing tile jobs to workers
	jobs := make(chan TileJob, totalTiles)
	
	// Channel to signal when all tiles are done
	done := make(chan struct{})
	
	// Start worker goroutines
	var wg sync.WaitGroup
	for i := 0; i < numTileWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				// Calculate tile bounds
				tileStartX := job.tileX * tileSize
				tileStartY := job.tileY * tileSize
				tileEndX := minUint32(tileStartX+tileSize, outWidth)
				tileEndY := minUint32(tileStartY+tileSize, outHeight)
				
				// Process this tile
				processTile(frame, rules, 
				            job.tileX, job.tileY, tileStartX, tileStartY, tileEndX, tileEndY,
				            inWidth, inHeight, outWidth, outHeight, job.tileIdx)
			}
		}()
	}
	
	// Start a goroutine to close the done channel when all workers are done
	go func() {
		wg.Wait()
		close(done)
	}()
	
	// Fill the job queue with tiles to process
	for tileY := uint32(0); tileY < tilesY; tileY++ {
		for tileX := uint32(0); tileX < tilesX; tileX++ {
			tileIdx := int(tileY*tilesX + tileX)
			// Only enqueue non-empty tiles
			if tileIdx < len(tileGrid) && len(tileGrid[tileIdx].rects) > 0 {
				jobs <- TileJob{tileX, tileY, tileIdx}
			}
		}
	}
	
	// Close the job channel to signal no more jobs
	close(jobs)
	
	// Wait for all tiles to be processed
	<-done
}

// Helper functions for uint32 min/max
func minUint32(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}

func maxUint32(a, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
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

	// 6. Sort rectangles by Z-order and then by spatial locality (for better cache coherence)
	// First sort by Z-order (primary key)
	sort.Slice(rules.Rects, func(i, j int) bool {
		if rules.Rects[i].Z != rules.Rects[j].Z {
			return rules.Rects[i].Z < rules.Rects[j].Z
		}
		
		// Secondary sort by Y position for rectangles with same Z-order
		// This improves cache locality by processing rectangles in scan-line order
		if rules.Rects[i].Dest[1] != rules.Rects[j].Dest[1] {
			return rules.Rects[i].Dest[1] < rules.Rects[j].Dest[1]
		}
		
		// Tertiary sort by X position
		return rules.Rects[i].Dest[0] < rules.Rects[j].Dest[0]
	})

	// 7. Preprocess rectangles for more efficient rendering
	// Create tile occupancy grid for efficient rectangle processing
	tilesX := (outWidth + tileSize - 1) / tileSize
	tilesY := (outHeight + tileSize - 1) / tileSize
	totalTiles := tilesX * tilesY
	
	// Create our spatial acceleration structure - a grid of tiles
	// that will store which rectangles affect each tile
	tileGrid = make([]TileRects, totalTiles)
	
	// For each rectangle, determine which tiles it affects (from back to front Z-order)
	// Process rectangles in reverse Z-order (background to foreground)
	// This allows us to identify tiles that have opaque rectangles that fully cover them
	for rectIdx := len(rules.Rects) - 1; rectIdx >= 0; rectIdx-- {
		r := rules.Rects[rectIdx]
		
		// Calculate rectangle bounds
		dstStartX := r.Dest[0]
		dstStartY := r.Dest[1]
		dstEndX := dstStartX + r.Src[2]
		dstEndY := dstStartY + r.Src[3]
		
		// Skip if rectangle is fully transparent
		if r.Alpha == 0 {
			continue
		}
		
		// Check if this rectangle is fully opaque (for culling optimization)
		isOpaque := r.Alpha == 1.0
		
		// Calculate the range of tiles this rectangle overlaps
		startTileX := dstStartX / tileSize
		startTileY := dstStartY / tileSize
		endTileX := (dstEndX + tileSize - 1) / tileSize
		endTileY := (dstEndY + tileSize - 1) / tileSize
		
		// Clamp to grid bounds
		if startTileX >= tilesX { continue }
		if startTileY >= tilesY { continue }
		if endTileX > tilesX { endTileX = tilesX }
		if endTileY > tilesY { endTileY = tilesY }
		
		// Add this rectangle to all overlapping tiles
		for tileY := startTileY; tileY < endTileY; tileY++ {
			for tileX := startTileX; tileX < endTileX; tileX++ {
				tileIdx := tileY*tilesX + tileX
				idx := int(tileIdx)
				
				// Add rectangle to tile's list if it's not covered by a fully opaque rectangle
				// or if this rectangle itself is opaque (in which case it might fully cover the tile)
				if !tileGrid[idx].hasCover || isOpaque {
					tileGrid[idx].rects = append(tileGrid[idx].rects, rectIdx)
					
					// Check if this rectangle fully covers the tile - if so, we can optimize
					// by skipping any rectangles beneath it
					if isOpaque {
						// Calculate tile bounds
						tileStartX := tileX * tileSize
						tileStartY := tileY * tileSize
						tileEndX := minUint32(tileStartX+tileSize, outWidth)
						tileEndY := minUint32(tileStartY+tileSize, outHeight)
						
						// Check if rectangle fully contains this tile
						if dstStartX <= tileStartX && dstStartY <= tileStartY &&
						   dstEndX >= tileEndX && dstEndY >= tileEndY {
							tileGrid[idx].hasCover = true
						}
					}
				}
			}
		}
	}
	
	fmt.Printf("Preprocessed %d rectangles across %d tiles\n", len(rules.Rects), totalTiles)
	
	// 8. Determine the optimal number of worker goroutines - balance between CPU and memory usage
	numCPU := runtime.NumCPU()
	numWorkers := numCPU
	if numWorkers > 4 {
		// Limit workers to reduce memory pressure while maintaining parallelism
		numWorkers = 4
	}
	if numWorkers < 1 {
		numWorkers = 1
	}
	
	// 9. Set up worker pool with memory-optimized buffers
	inFrameSize := int(hdr.Width * hdr.Height * 3)
	outFrameSize := int(outWidth * outHeight * 3)
	
	// Calculate memory limits to prevent excessive allocation
	const maxMemoryMB = 1200 // Target maximum memory usage in MB
	const bytesPerMB = 1024 * 1024
	maxBufferPairs := maxMemoryMB * bytesPerMB / (inFrameSize + outFrameSize)
	
	// Cap workers based on memory constraints - each worker needs at least one buffer pair
	if numWorkers > maxBufferPairs/2 && maxBufferPairs > 0 {
		numWorkers = maxBufferPairs / 2
		if numWorkers < 1 {
			numWorkers = 1
		}
	}
	
	fmt.Printf("Using %d workers (with memory optimization)\n", numWorkers)

	// Initialize the zero buffer with the maximum size we'll need
	zeroBuffer = make([]byte, outFrameSize)

	// Use smaller channel buffer sizes to reduce memory pressure
	// Just enough capacity to keep workers busy
	frameQueue := make(chan *Frame, numWorkers)
	resultQueue := make(chan *Frame, numWorkers)

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

	// Create a buffer pool with limited size for frame data to reduce allocations
	// Only create as many buffer objects as we need to keep workers busy
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

	// Use a slice for pending frames instead of a map to reduce overhead
	// Preallocate space for a small number of pending frames to minimize allocations
	const maxPendingFrames = 16
	pendingFrames := make([]*Frame, maxPendingFrames)
	nextFrame := uint32(0)

	// Process results and write to output in correct order
	for frame := range resultQueue {
		// If this is the next frame we're expecting, write it immediately
		if frame.id == nextFrame {
			if _, err := outFile.Write(frame.outBuf); err != nil {
				log.Fatalf("write frame %d: %v", frame.id, err)
			}
			nextFrame++

			// Return frame buffer to pool immediately after use
			framePool.Put(frame)

			// Check if we have any pending frames that can now be written
			// This is a sliding window approach that's more memory efficient
			for offset := uint32(0); offset < maxPendingFrames; offset++ {
				slotIdx := offset % maxPendingFrames
				if pendingFrames[slotIdx] != nil && pendingFrames[slotIdx].id == nextFrame {
					if _, err := outFile.Write(pendingFrames[slotIdx].outBuf); err != nil {
						log.Fatalf("write frame %d: %v", pendingFrames[slotIdx].id, err)
					}
					
					// Return the frame to the pool immediately to reduce memory pressure
					framePool.Put(pendingFrames[slotIdx])
					pendingFrames[slotIdx] = nil
					nextFrame++
				} else {
					break // No more consecutive frames
				}
			}
		} else {
			// This frame arrived out of order, store it in our sliding window
			// Calculate the position in our circular buffer
			slotIdx := (frame.id - nextFrame) % maxPendingFrames
			
			if slotIdx < maxPendingFrames {
				// If we already have a frame in this slot, return it to the pool first
				if pendingFrames[slotIdx] != nil {
					framePool.Put(pendingFrames[slotIdx])
				}
				pendingFrames[slotIdx] = frame
			} else {
				// The frame is too far ahead to store; we'll need to process it now
				// This is rare but possible with extreme out-of-order arrivals
				log.Printf("Warning: Frame %d arrived too far ahead of current frame %d", 
					frame.id, nextFrame)
				// Return it to the pool to avoid memory leak
				framePool.Put(frame)
			}
		}
	}
	
	// Clean up any remaining pending frames before exit
	for i := range pendingFrames {
		if pendingFrames[i] != nil {
			framePool.Put(pendingFrames[i])
			pendingFrames[i] = nil
		}
	}

	fmt.Println("✅ Done! Composited video written to output/video-out.raw")
}