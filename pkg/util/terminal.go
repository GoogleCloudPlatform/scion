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

package util

import (
	"os"

	"golang.org/x/term"
)

// IsTerminal returns true if the current process is running in an interactive terminal.
// Checks whether stdin is attached to a TTY.
func IsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// IsStdoutTerminal returns true if stdout is attached to a TTY (i.e., the
// process output is NOT being piped or redirected to a file).
//
// Use this when deciding whether human-oriented output (progress bars, colors,
// streaming logs, auto --wait defaults) should be emitted.
func IsStdoutTerminal() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}
