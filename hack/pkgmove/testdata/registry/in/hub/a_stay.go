package hub

// registered fills the moved registry during package initialisation.
var registered = register("builtin")

// BuiltinCount reports how many entries the registry had when builtinCount
// was initialised.
func BuiltinCount() int { return builtinCount }
