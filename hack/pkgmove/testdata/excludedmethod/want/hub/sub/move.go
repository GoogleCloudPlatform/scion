package sub

type runner interface{ run() }

// Start reports whether v satisfies the unexported runner interface.
func Start(v any) bool {
	_, ok := v.(runner)
	return ok
}
