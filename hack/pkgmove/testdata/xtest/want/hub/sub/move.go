package sub

import (
	_ "embed"
	_ "unsafe"
)

//go:embed data.txt
var data string

//go:linkname helper
func helper() int { return 1 }

// Data returns the embedded data.
func Data() string { return data }
