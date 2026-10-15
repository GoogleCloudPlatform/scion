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

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ptone/scion#3494: in JSON mode the stop --all --rm confirmation never
// writes to stdout. With --yes it is skipped; without it, the list and the
// prompt go to stderr. Text mode keeps the list on stdout.
func TestConfirmStopAllRm_JSONModeKeepsStdoutClean(t *testing.T) {
	origConfirm := autoConfirm
	t.Cleanup(func() { autoConfirm = origConfirm })

	t.Run("json/yes", func(t *testing.T) {
		setJSONOutput(t)
		autoConfirm = true
		var ok bool
		stdout, stderr := captureStdIO(t, func() { ok = confirmStopAllRm([]string{"alpha", "beta"}) })
		assert.True(t, ok)
		assert.Empty(t, stdout)
		assert.Empty(t, stderr)
	})
	t.Run("json/prompt", func(t *testing.T) {
		setJSONOutput(t)
		autoConfirm = false
		withStdin(t, "y\n")
		var ok bool
		stdout, stderr := captureStdIO(t, func() { ok = confirmStopAllRm([]string{"alpha", "beta"}) })
		assert.True(t, ok)
		assert.Empty(t, stdout)
		assert.Contains(t, stderr, "The following 2 agent(s) will be stopped and removed:")
		assert.Contains(t, stderr, "  - alpha")
		assert.Contains(t, stderr, "Continue? (y/N): ")
	})
	t.Run("json/prompt declined", func(t *testing.T) {
		setJSONOutput(t)
		autoConfirm = false
		withStdin(t, "\n")
		var ok bool
		stdout, _ := captureStdIO(t, func() { ok = confirmStopAllRm([]string{"alpha"}) })
		assert.False(t, ok, "the default is no")
		assert.Empty(t, stdout)
	})
	t.Run("json/prompt y at EOF without newline", func(t *testing.T) {
		setJSONOutput(t)
		autoConfirm = false
		withStdin(t, "y")
		var ok bool
		stdout, _ := captureStdIO(t, func() { ok = confirmStopAllRm([]string{"alpha"}) })
		assert.False(t, ok, "a read error, including EOF, means the default (no)")
		assert.Empty(t, stdout)
	})
	t.Run("text", func(t *testing.T) {
		orig := outputFormat
		outputFormat = ""
		t.Cleanup(func() { outputFormat = orig })
		autoConfirm = true
		var ok bool
		stdout, _ := captureStdIO(t, func() { ok = confirmStopAllRm([]string{"alpha"}) })
		assert.True(t, ok)
		assert.Contains(t, stdout, "The following 1 agent(s) will be stopped and removed:")
		assert.Contains(t, stdout, "  - alpha")
	})
}
