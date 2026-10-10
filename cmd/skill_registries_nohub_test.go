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

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegistriesCommands_NoHubConfigured runs every skills registries
// command with no hub configured, both with hub settings absent and with
// --no-hub. Each must return errHubNotConfigured instead of dereferencing a
// nil hub context.
func TestRegistriesCommands_NoHubConfigured(t *testing.T) {
	tests := []struct {
		name string
		cmd  *cobra.Command
		args []string
	}{
		{name: "list", cmd: registriesListCmd},
		{name: "add", cmd: registriesAddCmd, args: []string{"my-registry"}},
		{name: "show", cmd: registriesShowCmd, args: []string{"my-registry"}},
		{name: "update", cmd: registriesUpdateCmd, args: []string{"my-registry"}},
		{name: "remove", cmd: registriesRemoveCmd, args: []string{"my-registry"}},
		{name: "pin", cmd: registriesPinCmd, args: []string{"my-registry", "scion://skills/my-skill"}},
	}
	for _, tt := range tests {
		for _, mode := range noHubModes {
			t.Run(tt.name+"/"+mode.name, func(t *testing.T) {
				isolateNoHubForTest(t, mode.noHubFlag)

				var err error
				assert.NotPanics(t, func() { err = tt.cmd.RunE(tt.cmd, tt.args) })
				require.Error(t, err)
				assert.ErrorIs(t, err, errHubNotConfigured)
				assert.Contains(t, err.Error(), "this command needs a hub")
			})
		}
	}
}
