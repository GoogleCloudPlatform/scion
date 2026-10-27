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

//go:build windows

package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// 'scion use <project>' runs on Windows, and the context file it
// writes, ~/.scion/context.yaml, gets the
// owner-only descriptor, replacing a too-open one; so does its lock file.
// It drives the real command end to end against a fake hub, in a
// temporary profile (USERPROFILE, which os.UserHomeDir reads on Windows).
// Control: a file created plainly in the same profile does not have the
// private descriptor, so the assertion is not vacuous.
func TestWindowsUse_SelectWritesPrivateContext(t *testing.T) {
	home := privateHome(t)
	t.Setenv("USERPROFILE", home)
	_, _ = setupUseTest(t)

	ctxPath, err := config.CLIContextPath()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, ".scion", "context.yaml"), ctxPath)
	wintest.MakeUsersReadable(t, ctxPath)

	_, stderr, err := runScion(t, "use", "foo")
	require.NoError(t, err, "stderr: %s", stderr)

	c := readTestContext(t)
	assert.Equal(t, "local", c.CurrentHub)
	require.NotNil(t, c.Hubs["local"])
	require.NotNil(t, c.Hubs["local"].CurrentProject)
	assert.Equal(t, ctxTestProjectID, c.Hubs["local"].CurrentProject.ID)

	assertPrivateSD(t, ctxPath)
	assertPrivateSD(t, filepath.Join(home, ".scion", "context.yaml.lock"))

	plain := filepath.Join(home, ".scion", "plain.txt")
	require.NoError(t, os.WriteFile(plain, []byte("x"), 0o600))
	sd := wintest.ReadSD(t, plain)
	assert.False(t, sd.Protected && len(sd.ACEs) == 2, "control: a plain file in the profile must not look private: %+v", sd)
}
