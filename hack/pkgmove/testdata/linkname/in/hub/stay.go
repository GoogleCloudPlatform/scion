package hub

import "unsafe"

func Off() uintptr { r := NewRec(); return unsafe.Offsetof(r.name) }
