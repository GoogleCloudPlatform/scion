package sub

import (
	_ "embed"
	_ "unsafe"
)

//go:embed data.txt
var data string

//go:linkname nanotime runtime.nanotime
func nanotime() int64

// Data returns the embedded data.
func Data() string { return data }
