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
	"github.com/stretchr/testify/require"
)

func TestKeysCmd_RequiresExactArgs(t *testing.T) {
	// No args — should fail
	err := keysCmd.Args(keysCmd, []string{})
	require.Error(t, err)

	// One arg — should fail
	err = keysCmd.Args(keysCmd, []string{"agent1"})
	require.Error(t, err)

	// Two args — should pass
	err = keysCmd.Args(keysCmd, []string{"agent1", "Escape"})
	require.NoError(t, err)

	// Three args — should fail (ExactArgs(2))
	err = keysCmd.Args(keysCmd, []string{"agent1", "Escape", "extra"})
	require.Error(t, err)
}

func TestKeysCmd_HasCorrectUse(t *testing.T) {
	assert.Equal(t, "keys <agent-name> <keystrokes>", keysCmd.Use)
}

func TestKeysCmd_IsRegistered(t *testing.T) {
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "keys" {
			found = true
			break
		}
	}
	assert.True(t, found, "keys command should be registered on rootCmd")
}
