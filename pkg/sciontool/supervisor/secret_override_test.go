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

package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSecretOverride_FetchedValueBeatsPlaceholder is the mutation test for
// the override decision (#127, P2d). An empty placeholder of the same name
// must LOSE to the fetched value.
//
// This test runs through supervisor.Run end-to-end: the child writes the
// env var value to a temp file, and we assert it sees the fetched value.
//
// Mutation: change the SecretOverrides application in supervisor.go from
// setEnvVar (replace) to mergeEnvOverlay (additive). The test must go red
// because mergeEnvOverlay is additive-only — existing entries win, so the
// empty placeholder beats the fetched value.
func TestSecretOverride_FetchedValueBeatsPlaceholder(t *testing.T) {
	// Set an empty placeholder in the current process env. The supervisor
	// inherits os.Environ() when no privilege drop is configured.
	t.Setenv("TEST_SECRET_P2D", "")

	outFile := filepath.Join(t.TempDir(), "secret_value.txt")

	config := DefaultConfig()
	config.SecretOverrides = map[string]string{
		"TEST_SECRET_P2D": "FAKE-KEY-SENTINEL-not-a-real-credential",
	}
	sup := New(config)

	exitCode, err := sup.Run(context.Background(),
		[]string{"sh", "-c", `printf '%s' "$TEST_SECRET_P2D" > ` + outFile})
	require.NoError(t, err)
	assert.Equal(t, 0, exitCode)

	got, err := os.ReadFile(outFile)
	require.NoError(t, err)
	assert.Equal(t, "FAKE-KEY-SENTINEL-not-a-real-credential", string(got),
		"fetched value must beat the empty placeholder — "+
			"if this fails, SecretOverrides is being routed through mergeEnvOverlay (additive) "+
			"instead of setEnvVar (replace)")
}

// TestSecretOverride_MergeOverlayDoesNotOverride verifies that
// mergeEnvOverlay (the additive path) does NOT replace an existing entry.
// This is the inverse of the override test — it proves the additive path
// is wrong for fetched secrets.
func TestSecretOverride_MergeOverlayDoesNotOverride(t *testing.T) {
	env := []string{
		"HOME=/home/scion",
		"API_KEY=",
		"PATH=/usr/bin",
	}

	overlay := map[string]string{
		"API_KEY": "FAKE-KEY-SENTINEL-not-a-real-credential",
	}

	// mergeEnvOverlay is additive-only: existing entries win.
	env = mergeEnvOverlay(env, overlay)

	// The placeholder wins — the fetched value is discarded.
	assert.Equal(t, "", getEnvVar(env, "API_KEY"),
		"mergeEnvOverlay must NOT replace existing entries — "+
			"if this assertion flips, the additive rule has been weakened")
}

// TestSecretOverride_NewKeyAdded verifies that a fetched secret for a key
// not already in the environment is added via the supervisor path.
func TestSecretOverride_NewKeyAdded(t *testing.T) {
	// Make sure this key does NOT exist in the current env.
	// t.Setenv registers cleanup to restore the original state on test exit.
	t.Setenv("TEST_NEW_SECRET_P2D", "")
	require.NoError(t, os.Unsetenv("TEST_NEW_SECRET_P2D"))

	outFile := filepath.Join(t.TempDir(), "new_secret.txt")

	config := DefaultConfig()
	config.SecretOverrides = map[string]string{
		"TEST_NEW_SECRET_P2D": "FAKE-KEY-SENTINEL-not-a-real-credential",
	}
	sup := New(config)

	exitCode, err := sup.Run(context.Background(),
		[]string{"sh", "-c", `printf '%s' "$TEST_NEW_SECRET_P2D" > ` + outFile})
	require.NoError(t, err)
	assert.Equal(t, 0, exitCode)

	got, err := os.ReadFile(outFile)
	require.NoError(t, err)
	assert.Equal(t, "FAKE-KEY-SENTINEL-not-a-real-credential", string(got))
}

// TestSecretOverride_AppliedAfterMergeOverlay verifies the ordering:
// mergeEnvOverlay runs first (additive), then SecretOverrides (replaces).
// A placeholder set by the additive merge is overridden by the fetched value.
func TestSecretOverride_AppliedAfterMergeOverlay(t *testing.T) {
	// Ensure the key doesn't exist in process env so mergeEnvOverlay adds it.
	// t.Setenv registers cleanup to restore the original state on test exit.
	t.Setenv("TEST_OVERLAY_SECRET_P2D", "")
	require.NoError(t, os.Unsetenv("TEST_OVERLAY_SECRET_P2D"))

	outFile := filepath.Join(t.TempDir(), "overlay_secret.txt")

	config := DefaultConfig()
	// Harness overlay adds a placeholder.
	config.EnvOverlay = map[string]string{
		"TEST_OVERLAY_SECRET_P2D": "placeholder-from-harness",
	}
	// Fetched secret overrides the placeholder.
	config.SecretOverrides = map[string]string{
		"TEST_OVERLAY_SECRET_P2D": "FAKE-KEY-SENTINEL-not-a-real-credential",
	}
	sup := New(config)

	exitCode, err := sup.Run(context.Background(),
		[]string{"sh", "-c", `printf '%s' "$TEST_OVERLAY_SECRET_P2D" > ` + outFile})
	require.NoError(t, err)
	assert.Equal(t, 0, exitCode)

	got, err := os.ReadFile(outFile)
	require.NoError(t, err)
	assert.Equal(t, "FAKE-KEY-SENTINEL-not-a-real-credential", string(got))
}

// TestSecretOverride_EmptyOverridesIsNoOp verifies that nil/empty
// SecretOverrides does not affect the child environment.
func TestSecretOverride_EmptyOverridesIsNoOp(t *testing.T) {
	t.Setenv("TEST_EXISTING_P2D", "original-value")

	outFile := filepath.Join(t.TempDir(), "existing.txt")

	config := DefaultConfig()
	// No SecretOverrides.
	sup := New(config)

	exitCode, err := sup.Run(context.Background(),
		[]string{"sh", "-c", `printf '%s' "$TEST_EXISTING_P2D" > ` + outFile})
	require.NoError(t, err)
	assert.Equal(t, 0, exitCode)

	got, err := os.ReadFile(outFile)
	require.NoError(t, err)
	assert.Equal(t, "original-value", string(got))
}
