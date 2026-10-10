package hub

// int answer(void) { return 42; }
import "C"

// Answer calls C.
func Answer() int { return int(C.answer()) }
