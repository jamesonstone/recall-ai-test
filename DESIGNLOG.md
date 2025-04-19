# Design Log

## 2025-04-19

* Removed attempted SIMD optimization (`blendRowSIMD` assembly and related Go code) due to build/linking issues. Reverted to pure Go implementation for pixel blending.
