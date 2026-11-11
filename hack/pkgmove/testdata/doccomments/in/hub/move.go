package hub

import "fmt"

// writeError formats an error line. Callers use writeError, never writeErrors
// or xwriteError (partial words are not renamed).
func writeError(msg string) string { return fmt.Sprintf("error: %s (see writeError)", msg) }

// run starts the job. Do not run it twice; call run() or [run] once.
func run() string { return writeError("run") }

// Limits for the job.
const (
	// maxRetries bounds run retries.
	maxRetries  = 3
	defaultName = "job"
)

// job is processed by [job.handle].
type job struct {
	// name is not renamed (it is not used across the boundary).
	name string
}

// handle processes a job; it satisfies handler, like stayJob.handle.
func (j *job) handle() string { return j.name + defaultName } /* handle and writeError in a block comment */

//go:generate echo writeError

// Helper uses writeError (fmt.writeError would name another package) but is
// not renamed itself.
func Helper() string { return writeError("helper") }

// newJob returns a job named name; see [newJob] and newJob's tests.
func newJob(name string) *job { return &job{name: name} }
