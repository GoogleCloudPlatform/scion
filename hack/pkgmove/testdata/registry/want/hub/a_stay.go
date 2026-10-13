package hub

import "example.com/fx/hub/sub"

// registered fills the moved registry during package initialisation.
var registered = register("builtin")

// BuiltinCount reports how many entries the registry had when builtinCount
// was initialised.
func BuiltinCount() int { return sub.BuiltinCount }
