package hub

// Runs reports whether v satisfies the unexported interface dynamically.
func Runs(v any) bool {
	_, ok := v.(interface{ run() string })
	return ok
}

// Check must stay true after the move.
func Check() bool { return Runs(NewBox()) }
