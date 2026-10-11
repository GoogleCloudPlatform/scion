package hub

type runner interface {
	run() error
}

// Start runs r.
func Start(r runner) error { return r.run() }

// StartDefault runs a task through the staying interface.
func StartDefault() error { return Start(&task{}) }

// Probe asserts the unexported method dynamically.
func Probe(v any) bool {
	_, ok := v.(interface{ run() error })
	return ok
}
