// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtime

import "fmt"

// CommandExitError reports that a command passed to Runtime.Exec ran inside
// the agent's container and exited non-zero. It is a command result, not a
// runtime failure: the container answered, whatever Output says (a command
// printing "sh: 1: foo: not found" must never be read as the container being
// gone; ptone/scion#3470). Callers tell the two apart with errors.As.
//
// Runtimes whose exec is not an os/exec or client-go exit error return this
// type. Output is the command's diagnostic output as the runtime chose to
// embed it, already redacted and truncated, so the error is safe to log.
type CommandExitError struct {
	Runtime string // runtime name prefixing the message, e.g. "substrate"
	Target  string // the exec target as the runtime names it
	Code    int
	Output  string
}

func (e *CommandExitError) Error() string {
	return fmt.Sprintf("%s: exec on %s exited %d: %s", e.Runtime, e.Target, e.Code, e.Output)
}

// ExitStatus returns the command's exit code.
func (e *CommandExitError) ExitStatus() int {
	return e.Code
}
