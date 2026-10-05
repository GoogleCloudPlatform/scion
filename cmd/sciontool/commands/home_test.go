//go:build linux

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

package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/homeprep"
)

func TestVersionFeatures(t *testing.T) {
	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"version", "--features"})
	t.Cleanup(func() { versionFeatures = false })
	require.NoError(t, rootCmd.Execute())
	assert.Equal(t, "home-v1\n", buf.String())
	assert.Equal(t, []string{homeprep.FeatureToken}, Features())
}

func runHome(t *testing.T, args ...string) (string, error) {
	t.Helper()
	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs(append([]string{"home"}, args...))
	err := rootCmd.Execute()
	return buf.String(), err
}

func TestHomeCommands(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	mem := filepath.Join(base, "mem")
	agentDir := filepath.Join(base, "agent")
	for _, d := range []string{home, mem, agentDir} {
		require.NoError(t, os.Mkdir(d, 0o755))
	}
	termLog := filepath.Join(base, "termination-log")
	require.NoError(t, os.WriteFile(termLog, nil, 0o644))
	old := terminationLogPath
	terminationLogPath = termLog
	t.Cleanup(func() { terminationLogPath = old })

	const agentID = "0b9f6a52-3c1e-4f43-9d8e-2a6f1c7b5e10"
	t.Setenv(envHomeAgentID, agentID)
	t.Setenv(envHomeStartID, "start-1")
	t.Setenv(envHomeLinks, `[{"target":".scion/secrets.json","source":"/run/scion/agent-secrets/secrets.json","mode":"0600"}]`)
	t.Setenv(envHomeSkeletonSrc, "")

	out, err := runHome(t, "prepare", "--home", home, "--mem-dir", mem)
	require.NoError(t, err, out)
	assert.Contains(t, out, "seed")
	_, err = os.Stat(filepath.Join(mem, homeprep.ModeFileName))
	require.NoError(t, err)

	out, err = runHome(t, "mark-seeded", "--home", home, "--agent-id", agentID, "--start-id", "start-1")
	require.NoError(t, err, out)

	// A failure is printed with its class and left in the termination log.
	t.Setenv(envHomeAgentID, "not-an-id")
	_, err = runHome(t, "prepare", "--home", home, "--mem-dir", mem)
	require.Error(t, err)
	data, rerr := os.ReadFile(termLog)
	require.NoError(t, rerr)
	assert.Equal(t, "home_storage_unavailable: no agent ID", string(data))

	t.Setenv(envHomeAgentID, agentID)
	t.Setenv(envHomeGID, "")
	_, err = runHome(t, "leaf", "--agent-dir", agentDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), homeprep.ErrClassLeafFailed)
}
