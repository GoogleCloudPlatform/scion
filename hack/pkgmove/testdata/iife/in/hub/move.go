package hub

var registry = map[string]bool{}

func register(name string) bool {
	registry[name] = true
	return true
}

var builtinCount = len(registry)
