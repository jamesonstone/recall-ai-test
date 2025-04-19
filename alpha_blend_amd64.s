#include "textflag.h"

// func blendRowSIMD(dst, src []byte, alpha, invAlpha uint32)
TEXT ·blendRowSIMD(SB), NOSPLIT, $0
    MOVQ dst+0(FP), DI  // dst slice base pointer
    MOVQ src+24(FP), SI // src slice base pointer
    MOVQ dst_len+8(FP), CX // length of dst slice (should be divisible by 8)
    MOVL alpha+48(FP), X2  // alpha value
    MOVL invAlpha+52(FP), X3 // inverse alpha value

    // Broadcast alpha and invAlpha to all 4 32-bit elements of the XMM registers
    PXOR X0, X0
    PINSRD $0, X2, X2
    PINSRD $0, X3, X3
    PSHUFD $0, X2, X2  // Broadcast alpha
    PSHUFD $0, X3, X3  // Broadcast invAlpha

    // Check if we need to process at least one block of 16 bytes (4 pixels * 4 bytes)
    CMPQ CX, $16
    JL rest_bytes

main_loop:
    // Process 4 pixels at once (16 bytes - R,G,B,X for 4 pixels)
    MOVOU (SI), X0     // Load 4 src pixels
    MOVOU (DI), X1     // Load 4 dst pixels

    // Unpack to 32-bit integers
    PXOR X4, X4        // Zero X4 for unpacking
    PUNPCKLBW X4, X0   // Unpack low bytes from src to words in X0
    PUNPCKLBW X4, X1   // Unpack low bytes from dst to words in X1
    PUNPCKLWD X4, X0   // Unpack low words from src to dwords in X0
    PUNPCKLWD X4, X1   // Unpack low words from dst to dwords in X1

    // Multiply-Add: (src * alpha + dst * invAlpha)
    PMULLD X2, X0      // src * alpha
    PMULLD X3, X1      // dst * invAlpha
    PADDD  X1, X0      // src*alpha + dst*invAlpha

    // Shift right by 8 (divide by 256)
    PSRLD  $8, X0

    // Pack back to 8-bit
    PACKSSDW X4, X0    // Pack with saturation to 16-bit
    PACKUSWB X4, X0    // Pack with saturation to 8-bit

    // Store back the first 4 bytes (one pixel)
    MOVD X0, (DI)

    // Advance pointers for next iteration
    ADDQ $4, SI
    ADDQ $4, DI
    SUBQ $4, CX

    // Check if we have at least 4 more bytes to process
    CMPQ CX, $4
    JGE main_loop

rest_bytes:
    // Handle remaining bytes (less than 4)
    CMPQ CX, $0
    JLE done

    // Process one byte at a time
    MOVB (SI), AL      // Load src byte
    MOVB (DI), BL      // Load dst byte

    // Convert to 32-bit
    MOVZX AX, EAX
    MOVZX BX, EBX

    // Alpha blending formula: (src*alpha + dst*invAlpha) >> 8
    IMULL alpha+48(FP), EAX
    IMULL invAlpha+52(FP), EBX
    ADDL EBX, EAX
    SHRL $8, EAX

    // Store result
    MOVB AL, (DI)

    // Advance pointers
    INCQ SI
    INCQ DI
    DECQ CX

    // Check if we have more bytes
    CMPQ CX, $0
    JG rest_bytes

done:
    RET
