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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setReincarnateThinkingLevel sets --thinking-level through the flag set (so
// Changed is recorded, as on the command line) and restores it on cleanup.
func setReincarnateThinkingLevel(t *testing.T, value string) {
	t.Helper()
	f := reincarnateCmd.Flags().Lookup("thinking-level")
	require.NotNil(t, f)
	saved := reincarnateThinkingLevel
	t.Cleanup(func() {
		reincarnateThinkingLevel = saved
		f.Changed = false
	})
	require.NoError(t, reincarnateCmd.Flags().Set("thinking-level", value))
}

// TestReincarnateThinkingLevel_AcceptsShorthands checks that reincarnate
// --thinking-level takes the same values as start (ptone/scion#3490) and
// patches the integer level start would send.
func TestReincarnateThinkingLevel_AcceptsShorthands(t *testing.T) {
	for in, want := range map[string]int{
		"low": 25, "medium": 50, "HIGH": 75, "Max": 100, "0": 0, "42": 42, "100": 100,
	} {
		t.Run(in, func(t *testing.T) {
			setReincarnateThinkingLevel(t, in)
			require.NoError(t, validateReincarnatePatchFlags())

			req := &hubclient.ReincarnateAgentRequest{}
			applyReincarnatePatchFlags(req)
			require.NotNil(t, req.ThinkingLevel)
			assert.Equal(t, want, *req.ThinkingLevel)
			assert.Contains(t, requestedPatchFields(req), "thinkingLevel")

			startLevel, err := parseThinkingLevel(in)
			require.NoError(t, err)
			assert.Equal(t, startLevel, *req.ThinkingLevel, "reincarnate must send the level start sends")
		})
	}
}

// TestReincarnateThinkingLevel_InvalidIsUsageError checks that invalid values,
// including an explicitly empty one, are usage errors from validation.
func TestReincarnateThinkingLevel_InvalidIsUsageError(t *testing.T) {
	for _, in := range []string{"xhigh", "101", "-1", "1.5", ""} {
		t.Run("value="+in, func(t *testing.T) {
			setReincarnateThinkingLevel(t, in)
			err := validateReincarnatePatchFlags()
			require.Error(t, err)
			assert.True(t, strings.Contains(err.Error(), "--thinking-level"), "error %q should name the flag", err)
			assert.True(t, isUsageError(err), "a bad flag value is a usage error")
		})
	}
}

// TestReincarnateThinkingLevel_UnsetIsNoPatch checks that an unset flag
// patches nothing, and that the flag is a string with an unset default and
// help naming the shorthands.
func TestReincarnateThinkingLevel_UnsetIsNoPatch(t *testing.T) {
	setReincarnatePatchFlags(t, "", "", "", -1, "", "")
	require.NoError(t, validateReincarnatePatchFlags())
	req := &hubclient.ReincarnateAgentRequest{}
	applyReincarnatePatchFlags(req)
	assert.Nil(t, req.ThinkingLevel)

	f := reincarnateCmd.Flags().Lookup("thinking-level")
	require.NotNil(t, f)
	assert.Equal(t, "string", f.Value.Type())
	assert.Equal(t, "", f.DefValue)
	for _, want := range []string{"0-100", "low (25)", "medium (50)", "high (75)", "max (100)"} {
		assert.Contains(t, f.Usage, want)
	}
}
