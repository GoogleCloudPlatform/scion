package sub

var registry = map[string]bool{}

func Register(name string) bool {
	registry[name] = true
	return true
}

// builtinCount is initialised after registered in the original package.
var BuiltinCount = len(registry)
