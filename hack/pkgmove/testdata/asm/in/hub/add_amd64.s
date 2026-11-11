#include "textflag.h"

TEXT ·add(SB),NOSPLIT,$0-24
	MOVQ a+0(FP), AX
	ADDQ b+8(FP), AX
	MOVQ AX, ret+16(FP)
	RET
