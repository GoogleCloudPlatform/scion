package hub

import "fmt"

// handler is satisfied by job and stayJob.
type handler interface {
	// Handle processes one item; prose about how to handle errors is kept.
	Handle() string
}

type stayJob struct{}

// Handle returns a fixed name. writeError is not renamed here (the staying
// alias keeps its name), but s.Handle() is.
func (s stayJob) Handle() string { return "stay" }

// Process calls h.handle() (not rewritten: not the doc of a renamed declaration).
func Process(h handler) string { return h.Handle() }

// Run runs the moved job (comments in staying files are untouched: run, writeError).
func Run() string {
	return run() + writeError("x") + Process(newJob("j")) + Process(stayJob{}) + fmt.Sprint(maxRetries)
}
