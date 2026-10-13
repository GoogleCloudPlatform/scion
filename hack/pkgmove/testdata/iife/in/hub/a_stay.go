package hub

var registered = func() bool { return register("builtin") }()

func BuiltinCount() int { return builtinCount }
