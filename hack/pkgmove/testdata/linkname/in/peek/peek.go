package peek

import _ "unsafe"

import _ "example.com/fx/hub"

//go:linkname counter example.com/fx/hub.counter
var counter int

func Counter() int { return counter }
