package sub

import "fmt"

// WriteError formats an error line. Callers use WriteError, never writeErrors
// or xwriteError (partial words are not renamed).
func WriteError(msg string) string { return fmt.Sprintf("error: %s (see writeError)", msg) }

// Run starts the job. Do not run it twice; call Run() or [Run] once.
func Run() string { return WriteError("run") }

// Limits for the job.
const (
	// MaxRetries bounds run retries.
	MaxRetries  = 3
	defaultName = "job"
)

// Job is processed by [Job.Handle].
type Job struct {
	// name is not renamed (it is not used across the boundary).
	name string
}

// Handle processes a job; it satisfies handler, like stayJob.Handle.
func (j *Job) Handle() string { return j.name + defaultName } /* handle and WriteError in a block comment */

//go:generate echo writeError

// Helper uses WriteError (fmt.writeError would name another package) but is
// not renamed itself.
func Helper() string { return WriteError("helper") }

// NewJob returns a job named name; see [NewJob] and NewJob's tests.
func NewJob(name string) *Job { return &Job{name: name} }
