package hub

var registered = registerBuiltins()

func registerBuiltins() bool { return register("builtin") }

func BuiltinCount() int { return builtinCount }
