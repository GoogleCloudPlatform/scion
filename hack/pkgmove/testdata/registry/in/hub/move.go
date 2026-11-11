package hub

var registry = map[string]bool{}

func register(name string) bool {
	registry[name] = true
	return true
}

// builtinCount is initialised after registered in the original package.
var builtinCount = len(registry)
