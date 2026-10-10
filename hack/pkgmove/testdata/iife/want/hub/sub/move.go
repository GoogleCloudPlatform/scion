package sub

var registry = map[string]bool{}

func Register(name string) bool {
	registry[name] = true
	return true
}

var BuiltinCount = len(registry)
