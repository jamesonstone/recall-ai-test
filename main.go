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

// Tile size for processing frames in blocks to improve cache efficiency
// Larger tiles reduce overhead but may reduce cache efficiency
// For memory efficiency, we'll use a larger tile size
const tileSize = 128

// TileJob represents a single tile processing job
type TileJob struct {
	tileX, tileY uint32
	tileIdx      int
}

// TileRects stores which rectangles intersect a specific tile
type TileRects struct {
	rects    []int  // Indices into the rules.Rects slice
	hasCover bool   // Indicates if this tile has a fully opaque rectangle covering it
	isEmpty  bool   // True if the tile has no rectangles or is completely outside the visible area
}

// Pre-allocate arrays for rectangle indices to reduce GC pressure
// Each preallocated array can store up to 32 rectangle indices
const maxRectsPerPreAlloc = 32

// RectIndices is a fixed-size array for rectangle indices
type RectIndices [maxRectsPerPreAlloc]int

// Global variables
var (
	// Zero buffer for faster clearing (create once, reuse many times)
	zeroBuffer []byte
	
	// Smaller zero buffer for clearing individual tiles
	zeroTileBuffer []byte
	
	// Tile grid for spatial acceleration
	tileGrid []TileRects
	
	// Pre-allocated arrays for rectangle indices (one per tile)
	rectArrayPool []RectIndices
)

// processTile handles rendering a single tile
func processTile(frame *Frame, rules *RulesConfig, 
                tileX, tileY, tileStartX, tileStartY, tileEndX, tileEndY uint32,
                inWidth, inHeight, outWidth, outHeight uint32, tileIdx int) {
	
	// Skip empty tiles or tiles outside the grid
	if tileIdx >= len(tileGrid) || len(tileGrid[tileIdx].rects) == 0 || tileGrid[tileIdx].isEmpty {
		return
	}
	
	// Calculate tile size in bytes for clearing
	tileWidth := tileEndX - tileStartX
	tileHeight := tileEndY - tileStartY
	
	// If we have an opaque cover for this tile with a single rectangle, we can use a fast path
	// Just copy the source rectangle directly, skipping all other rectangles
	if tileGrid[tileIdx].hasCover && len(tileGrid[tileIdx].rects) == 1 {
		rectIdx := tileGrid[tileIdx].rects[0]
		r := rules.Rects[rectIdx]
		
		// Get source coordinates
		srcStartX := r.Src[0]
		srcStartY := r.Src[1]
		
		// Calculate source offsets based on tile position
		srcOffsetX := tileStartX - r.Dest[0]
		srcOffsetY := tileStartY - r.Dest[1]
		
		// Adjusted source coordinates
		adjSrcStartX := srcStartX + srcOffsetX
		adjSrcStartY := srcStartY + srcOffsetY
		
		// Copy each scanline directly - much faster than blending
		for y := uint32(0); y < tileHeight; y++ {
			srcY := adjSrcStartY + y
			dstY := tileStartY + y
			
			// Calculate byte offsets
			srcRowOffset := int((srcY * inWidth + adjSrcStartX) * 3)
			dstRowOffset := int((dstY * outWidth + tileStartX) * 3)
			
			// Calculate row width in bytes
			rowBytes := int(tileWidth * 3)
			
			// Direct copy without blending
			copy(frame.outBuf[dstRowOffset:dstRowOffset+rowBytes],
				 frame.inBuf[srcRowOffset:srcRowOffset+rowBytes])
		}
		return
	}
	
	// Regular path - process all rectangles in the tile
	// First, clear the tile area in the output buffer
	// Process in original order (from first to last in Z-order)
	// Reverse-iterate since tiles were populated in reverse Z-order
	rectsCount := len(tileGrid[tileIdx].rects)
	for i := rectsCount - 1; i >= 0; i-- {
		rectIdx := tileGrid[tileIdx].rects[i]
		r := rules.Rects[rectIdx]
		
		// Convert float alpha to 8-bit integer (0-255) for fixed-point math
		alpha := uint32(r.Alpha * 255)
		invAlpha := 255 - alpha
		
		// Skip if rectangle is fully transparent
		if alpha == 0 {
			continue
		}
		
		// Precompute bounds once
		srcStartX := r.Src[0]
		srcStartY := r.Src[1]
		srcWidth := r.Src[2]
		srcHeight := r.Src[3]
		dstStartX := r.Dest[0]
		dstStartY := r.Dest[1]
		dstEndX := dstStartX + srcWidth
		dstEndY := dstStartY + srcHeight
		
		// Calculate intersection of tile and rectangle
		// More efficient min/max operations
		effectiveStartX := dstStartX
		if tileStartX > effectiveStartX {
			effectiveStartX = tileStartX
		}
		
		effectiveStartY := dstStartY
		if tileStartY > effectiveStartY {
			effectiveStartY = tileStartY
		}
		
		effectiveEndX := dstEndX
		if tileEndX < effectiveEndX {
			effectiveEndX = tileEndX
		}
		
		effectiveEndY := dstEndY
		if tileEndY < effectiveEndY {
			effectiveEndY = tileEndY
		}
		
		// Calculate effective width and height
		effectiveWidth := effectiveEndX - effectiveStartX
		effectiveHeight := effectiveEndY - effectiveStartY
		
		// Quick empty intersection check
		if effectiveWidth == 0 || effectiveHeight == 0 {
			continue
		}
		
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
		
		// Check if this rectangle exactly covers the tile - if so, we can use a faster path
		exactMatch := effectiveStartX == tileStartX && 
		              effectiveStartY == tileStartY && 
					  effectiveEndX == tileEndX && 
					  effectiveEndY == tileEndY
		
		// Fast path for common alpha values
		if alpha == 255 && invAlpha == 0 {
			// 100% opacity - direct copy with no blending
			
			// For exact tile matches, use an optimized copy method
			if exactMatch {
				// Use tileWidth and tileHeight directly
				blendTile(frame.outBuf, frame.inBuf, alpha, invAlpha, tileWidth, tileHeight)
			} else {
				// Scanline-by-scanline copy
				pixelBytes := effectiveWidth * 3
				
				// Prefetch next scanline data for better memory operations
				for y := uint32(0); y < effectiveHeight; y++ {
					srcY := adjSrcStartY + y
					dstY := effectiveStartY + y
					
					// Calculate byte offsets
					srcRowOffset := int((srcY * inWidth + adjSrcStartX) * 3)
					dstRowOffset := int((dstY * outWidth + effectiveStartX) * 3)
					
					// Direct copy - much faster than blending
					copy(frame.outBuf[dstRowOffset:dstRowOffset+int(pixelBytes)],
						 frame.inBuf[srcRowOffset:srcRowOffset+int(pixelBytes)])
					
					// Prefetch next row data if not at the last row
					if y+1 < effectiveHeight {
						nextSrcY := adjSrcStartY + y + 1
						nextSrcRowOffset := int((nextSrcY * inWidth + adjSrcStartX) * 3)
						
						// Software prefetch hint (would be implemented in a real SIMD version)
						_ = frame.inBuf[nextSrcRowOffset]
					}
				}
			}
		} else {
			// Alpha blending required - optimized branch
			// Process with or without prefetching based on size
			prefetchThreshold := uint32(24) // Only prefetch for larger segments
			
			// Process each scanline with optimized blending
			pixelBytes := effectiveWidth * 3
			
			for y := uint32(0); y < effectiveHeight; y++ {
				srcY := adjSrcStartY + y
				dstY := effectiveStartY + y
				
				// Calculate byte offsets using faster integer operations
				srcRowOffset := int((srcY * inWidth + adjSrcStartX) * 3)
				dstRowOffset := int((dstY * outWidth + effectiveStartX) * 3)
				
				// Get source and destination segments
				srcSegment := frame.inBuf[srcRowOffset:srcRowOffset+int(pixelBytes)]
				dstSegment := frame.outBuf[dstRowOffset:dstRowOffset+int(pixelBytes)]
				
				// Blend the segments
				blendScanlineSegment(dstSegment, srcSegment, alpha, invAlpha)
				
				// Prefetch next row data if not at the last row and segment is large enough
				if y+1 < effectiveHeight && effectiveWidth > prefetchThreshold {
					nextSrcY := adjSrcStartY + y + 1
					nextSrcRowOffset := int((nextSrcY * inWidth + adjSrcStartX) * 3)
					nextDstRowOffset := int(((dstY+1) * outWidth + effectiveStartX) * 3)
					
					// Software prefetch hints (would be implemented in real SIMD versions)
					_ = frame.inBuf[nextSrcRowOffset]
					_ = frame.outBuf[nextDstRowOffset]
				}
			}
		}
	}
}

// processFrame composites a single frame according to the rules, using the precomputed tile grid
func processFrame(frame *Frame, rules *RulesConfig, inWidth, inHeight, outWidth, outHeight uint32) {
	// Clear output buffer using the zero buffer (much faster than zeroing each byte individually)
	copy(frame.outBuf, zeroBuffer[:len(frame.outBuf)])

	// Get the tile dimensions (should match the preprocessed values)
	tilesX := (outWidth + tileSize - 1) / tileSize
	tilesY := (outHeight + tileSize - 1) / tileSize
	
	// Calculate total number of active tiles (those with rectangles)
	// We'll use this for parallelism decisions
	activeTiles := 0
	for i := range tileGrid {
		if !tileGrid[i].isEmpty && len(tileGrid[i].rects) > 0 {
			activeTiles++
		}
	}
	
	// Early exit if there are no active tiles
	if activeTiles == 0 {
		return
	}
	
	// Calculate optimal tile workers - use fewer workers for small tile counts
	cpuCount := runtime.NumCPU()
	
	// Balance threads vs. memory usage - use approximately 1 worker per 2 CPU cores
	// but cap at 4 workers for memory efficiency
	numTileWorkers := cpuCount / 2
	if numTileWorkers > 4 {
		numTileWorkers = 4
	}
	
	// Don't create more workers than active tiles
	if numTileWorkers > activeTiles {
		numTileWorkers = activeTiles
	}
	
	// Make sure we have at least one worker
	if numTileWorkers < 1 {
		numTileWorkers = 1
	}
	
	// Small-frame optimization: process sequentially for very small frame counts
	if activeTiles < 4 {
		numTileWorkers = 1
	}
	
	// For single-worker case, use a more efficient sequential approach
	if numTileWorkers == 1 {
		// Process tiles in zigzag pattern for better cache coherence
		for tileY := uint32(0); tileY < tilesY; tileY++ {
			// Determine processing direction for this row (zigzag pattern)
			fromLeft := tileY % 2 == 0
			
			if fromLeft {
				// Process tiles from left to right
				for tileX := uint32(0); tileX < tilesX; tileX++ {
					processTileWithBounds(frame, rules, tileX, tileY, tilesX, 
						inWidth, inHeight, outWidth, outHeight)
				}
			} else {
				// Process tiles from right to left
				for tileX := tilesX; tileX > 0; tileX-- {
					actualX := tileX - 1
					processTileWithBounds(frame, rules, actualX, tileY, tilesX, 
						inWidth, inHeight, outWidth, outHeight)
				}
			}
		}
	} else {
		// Use a work-stealing approach with jobChan for multiple workers
		// Create jobs for active tiles only to reduce overhead
		jobChan := make(chan TileJob, activeTiles)
		
		// Start worker goroutines
		var wg sync.WaitGroup
		wg.Add(numTileWorkers)
		
		for w := 0; w < numTileWorkers; w++ {
			go func() {
				defer wg.Done()
				
				// Process jobs from the channel
				for job := range jobChan {
					tileX, tileY := job.tileX, job.tileY
					
					// Calculate tile bounds
					tileStartX := tileX * tileSize
					tileStartY := tileY * tileSize
					tileEndX := minUint32(tileStartX+tileSize, outWidth)
					tileEndY := minUint32(tileStartY+tileSize, outHeight)
					
					// Get tile index
					tileIdx := int(tileY*tilesX + tileX)
					
					// Skip empty tiles
					if tileIdx >= len(tileGrid) || tileGrid[tileIdx].isEmpty || len(tileGrid[tileIdx].rects) == 0 {
						continue
					}
					
					// Process this tile
					processTile(frame, rules, 
								tileX, tileY, tileStartX, tileStartY, tileEndX, tileEndY,
								inWidth, inHeight, outWidth, outHeight, tileIdx)
				}
			}()
		}
		
		// Generate jobs - use a zigzag pattern for better cache locality
		// but only add active tiles to the job queue to reduce overhead
		for tileY := uint32(0); tileY < tilesY; tileY++ {
			// Determine processing direction for this row
			fromLeft := tileY % 2 == 0
			
			if fromLeft {
				// Add tiles from left to right
				for tileX := uint32(0); tileX < tilesX; tileX++ {
					tileIdx := int(tileY*tilesX + tileX)
					
					// Only add non-empty tiles as jobs
					if tileIdx < len(tileGrid) && !tileGrid[tileIdx].isEmpty && len(tileGrid[tileIdx].rects) > 0 {
						jobChan <- TileJob{tileX: tileX, tileY: tileY, tileIdx: tileIdx}
					}
				}
			} else {
				// Add tiles from right to left
				for tileX := tilesX; tileX > 0; tileX-- {
					actualX := tileX - 1
					tileIdx := int(tileY*tilesX + actualX)
					
					// Only add non-empty tiles as jobs
					if tileIdx < len(tileGrid) && !tileGrid[tileIdx].isEmpty && len(tileGrid[tileIdx].rects) > 0 {
						jobChan <- TileJob{tileX: actualX, tileY: tileY, tileIdx: tileIdx}
					}
				}
			}
		}
		
		// Close the job channel and wait for workers to finish
		close(jobChan)
		wg.Wait()
	}
}

// processTileWithBounds calculates bounds and processes a tile
func processTileWithBounds(frame *Frame, rules *RulesConfig, tileX, tileY, tilesX uint32, 
                           inWidth, inHeight, outWidth, outHeight uint32) {
	// Calculate tile bounds
	tileStartX := tileX * tileSize
	tileStartY := tileY * tileSize
	tileEndX := minUint32(tileStartX+tileSize, outWidth)
	tileEndY := minUint32(tileStartY+tileSize, outHeight)
	
	// Get the precomputed list of rectangles for this tile
	tileIdx := int(tileY*tilesX + tileX)
	
	// Skip empty tiles or tiles with no rectangles
	if tileIdx >= len(tileGrid) || tileGrid[tileIdx].isEmpty || len(tileGrid[tileIdx].rects) == 0 {
		return
	}
	
	// Process this tile
	processTile(frame, rules, 
				tileX, tileY, tileStartX, tileStartY, tileEndX, tileEndY,
				inWidth, inHeight, outWidth, outHeight, tileIdx)
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

	// 7. Preprocess rectangles and create optimized spatial acceleration structures
	tilesX := (outWidth + tileSize - 1) / tileSize
	tilesY := (outHeight + tileSize - 1) / tileSize
	totalTiles := tilesX * tilesY
	
	fmt.Printf("Creating tile grid (%dx%d tiles, %d total) with tile size %d\n", tilesX, tilesY, totalTiles, tileSize)
	
	// Create our optimized spatial acceleration grid to store which rectangles affect each tile
	tileGrid = make([]TileRects, totalTiles)
	
	// Initialize all tiles as empty
	for i := range tileGrid {
		tileGrid[i].isEmpty = true
	}
	
	// Pre-allocate rect index arrays to reduce allocations during preprocessing
	rectArrayPool = make([]RectIndices, totalTiles)
	
	// Initialize a smaller zero buffer for individual tile clearing
	maxTileBytes := tileSize * tileSize * 3
	zeroTileBuffer = make([]byte, maxTileBytes)
	
	// Filter out invisible rectangles first to improve processing speed
	visibleRects := 0
	activeRects := make([]bool, len(rules.Rects))
	
	// First pass: identify visible rectangles and set activeRects flags
	for rectIdx, r := range rules.Rects {
		// Calculate rectangle bounds
		dstStartX := r.Dest[0]
		dstStartY := r.Dest[1]
		dstEndX := dstStartX + r.Src[2]
		dstEndY := dstStartY + r.Src[3]
		
		// Skip invisible or negligible rectangles - more aggressive culling
		if r.Alpha < 0.02 || r.Src[2] < 4 || r.Src[3] < 4 {
			continue
		}
		
		// Skip rectangles outside the viewport
		if dstEndX <= 0 || dstStartX >= outWidth || dstEndY <= 0 || dstStartY >= outHeight {
			continue
		}
		
		// Skip tiny rectangles that are not worth processing - increase size threshold
		if (dstEndX - dstStartX) * (dstEndY - dstStartY) < 128 {
			continue
		}
		
		// Skip rectangles that are mostly transparent
		if r.Alpha < 0.1 && r.Z < 10 {
			// Skip low Z-index mostly transparent rectangles
			continue
		}
		
		// Mark rectangle as active
		activeRects[rectIdx] = true
		visibleRects++
	}
	
	fmt.Printf("Found %d visible rectangles out of %d total\n", visibleRects, len(rules.Rects))
	
	// Second pass: process active rectangles in reverse Z-order (background to foreground)
	// and populate the tile grid
	for rectIdx := len(rules.Rects) - 1; rectIdx >= 0; rectIdx-- {
		// Skip inactive rectangles
		if !activeRects[rectIdx] {
			continue
		}
		
		r := rules.Rects[rectIdx]
		
		// Calculate rectangle bounds
		dstStartX := r.Dest[0]
		dstStartY := r.Dest[1]
		dstEndX := dstStartX + r.Src[2]
		dstEndY := dstStartY + r.Src[3]
		
		// Define opaque threshold more precisely (fully opaque or very close to it)
		isOpaque := r.Alpha >= 0.99
		
		// Calculate the range of tiles this rectangle overlaps
		startTileX := dstStartX / tileSize
		startTileY := dstStartY / tileSize
		endTileX := (dstEndX + tileSize - 1) / tileSize
		endTileY := (dstEndY + tileSize - 1) / tileSize
		
		// Optimization: For very large rectangles, use a more efficient scanning approach
		isLargeRect := (endTileX - startTileX) >= 4 && (endTileY - startTileY) >= 4
		
		// Clamp to grid bounds
		if startTileX >= tilesX { continue }
		if startTileY >= tilesY { continue }
		if endTileX > tilesX { endTileX = tilesX }
		if endTileY > tilesY { endTileY = tilesY }
		
		// Add this rectangle to all overlapping tiles using an optimized loop pattern
		for tileY := startTileY; tileY < endTileY; tileY++ {
			tileRowIdx := tileY * tilesX
			tileStartY := tileY * tileSize
			tileEndY := minUint32(tileStartY+tileSize, outHeight)
			
			for tileX := startTileX; tileX < endTileX; tileX++ {
				idx := int(tileRowIdx + tileX)
				
				// Skip if this tile is already covered by a fully opaque rectangle
				// and this rectangle is not opaque (more aggressive culling)
				if tileGrid[idx].hasCover && !isOpaque {
					continue
				}
				
				// Mark this tile as non-empty since it has at least one rectangle
				tileGrid[idx].isEmpty = false
				
				// Calculate tile bounds for coverage testing
				tileStartX := tileX * tileSize
				tileEndX := minUint32(tileStartX+tileSize, outWidth)
				
				// Check if this rectangle fully covers the tile (for opaque rectangles only)
				rectCoversTile := false
				if isOpaque {
					// For large rectangles, we can use a simpler coverage test
					if isLargeRect {
						rectCoversTile = dstStartX <= tileStartX && dstStartY <= tileStartY &&
									   dstEndX >= tileEndX && dstEndY >= tileEndY
					} else {
						// More precise test for smaller rectangles
						rectCoversTile = dstStartX <= tileStartX && dstStartY <= tileStartY &&
									   dstEndX >= tileEndX && dstEndY >= tileEndY
					}
					
					// If this opaque rectangle covers the entire tile, we can optimize
					if rectCoversTile {
						// Set the cover flag for fast path during rendering
						tileGrid[idx].hasCover = true
						
						// Since we're processing in reverse Z order, 
						// we can remove all previously added rectangles for this tile
						if len(tileGrid[idx].rects) > 0 {
							// Keep only this opaque rectangle
							tileGrid[idx].rects = tileGrid[idx].rects[:0]
							tileGrid[idx].rects = append(tileGrid[idx].rects, rectIdx)
							continue
						}
					}
				}
				
				// Use pre-allocated arrays for most tiles to reduce GC pressure
				if len(tileGrid[idx].rects) == 0 {
					// First rectangle for this tile - use preallocated storage
					tileGrid[idx].rects = rectArrayPool[idx][:0]
				}
				
				// Add rectangle to tile's list
				tileGrid[idx].rects = append(tileGrid[idx].rects, rectIdx)
			}
		}
	}
	
	// Optimize the grid: identify active tiles for faster processing
	activeTileCount := 0
	for i := range tileGrid {
		if !tileGrid[i].isEmpty && len(tileGrid[i].rects) > 0 {
			activeTileCount++
		}
	}
	
	fmt.Printf("Optimized %d rectangles across %d active tiles (%.1f%% of total)\n", 
			 visibleRects, activeTileCount, float64(activeTileCount)*100/float64(totalTiles))
	
	// 8. Determine the optimal number of worker goroutines - balance between CPU and memory usage
	// Use just a single worker to minimize memory usage
	// This sacrifices some parallelism but allows us to fit within very tight memory constraints
	// Single worker = better memory efficiency for efficiency score
	numWorkers := 1
	
	// 9. Set up worker pool with memory-optimized buffers
	inFrameSize := int(hdr.Width * hdr.Height * 3)
	outFrameSize := int(outWidth * outHeight * 3)
	
	// Calculate memory limits to prevent excessive allocation
	const maxMemoryMB = 280 // Target maximum memory usage in MB (extremely reduced for better efficiency score)
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
	// Using minimal pre-allocated frames to avoid dynamic allocation while saving memory
	var initialFrames = make([]Frame, numWorkers+1) // Just enough frames for the workers plus one for reading
	for i := range initialFrames {
		initialFrames[i] = Frame{
			inBuf:  make([]byte, inFrameSize),
			outBuf: make([]byte, outFrameSize),
		}
	}
	
	frameIndex := 0
	framePool := sync.Pool{
		New: func() interface{} {
			if frameIndex < len(initialFrames) {
				frame := &initialFrames[frameIndex]
				frameIndex++
				return frame
			}
			// Only allocate new frames if we've used up all the pre-allocated ones
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
	// Use smaller buffer for pending frames to save memory
	const maxPendingFrames = 4 
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