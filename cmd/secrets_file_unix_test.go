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

//go:build unix

package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWarnIfSecretsFileOverlyPermissive(t *testing.T) {
	dir := t.TempDir()

	// Group/world-readable (0644) triggers a warning.
	worldReadable := filepath.Join(dir, "open.yaml")
	require.NoError(t, os.WriteFile(worldReadable, []byte("K: v\n"), 0o644))
	require.NoError(t, os.Chmod(worldReadable, 0o644))

	var warnBuf bytes.Buffer
	warnIfSecretsFileOverlyPermissive(&warnBuf, worldReadable)
	assert.Contains(t, warnBuf.String(), "group/world-readable")
	assert.Contains(t, warnBuf.String(), "0644")

	// Owner-only (0600) produces no warning.
	privateFile := filepath.Join(dir, "private.yaml")
	require.NoError(t, os.WriteFile(privateFile, []byte("K: v\n"), 0o600))
	require.NoError(t, os.Chmod(privateFile, 0o600))

	warnBuf.Reset()
	warnIfSecretsFileOverlyPermissive(&warnBuf, privateFile)
	assert.Empty(t, warnBuf.String())
}
