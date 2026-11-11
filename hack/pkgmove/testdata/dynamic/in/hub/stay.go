package hub

// Describe calls an unexported method that would become String.
func Describe(t *Task) string { return t.string() }
