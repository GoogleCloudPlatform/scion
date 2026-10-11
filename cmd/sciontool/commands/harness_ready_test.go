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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
)

// useReadinessPaths points the readiness files at dir for one test.
func useReadinessPaths(t *testing.T, dir string) (initRun, ready string) {
	t.Helper()
	oldInit, oldReady := initRunFilePath, harnessReadyFilePath
	initRunFilePath = filepath.Join(dir, "scion-init-run")
	harnessReadyFilePath = filepath.Join(dir, "scion-harness-ready")
	t.Cleanup(func() { initRunFilePath, harnessReadyFilePath = oldInit, oldReady })
	return initRunFilePath, harnessReadyFilePath
}

func readInitRun(t *testing.T, path string) (nonce, mode string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	nonce, mode, ok := state.ParseInitRun(string(data))
	require.True(t, ok, "init-run content %q", data)
	return nonce, mode
}

func readMarker(t *testing.T, path string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return state.ParseReadyMarker(string(data))
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".tmp-", "leftover temp file")
	}
}

func sessionStart(source string) *hooks.Event {
	return &hooks.Event{Name: hooks.EventSessionStart, Data: hooks.EventData{Source: source}}
}

func TestWriteInitRunFile_ModeContentAndPermissions(t *testing.T) {
	dir := t.TempDir()
	initRun, _ := useReadinessPaths(t, dir)

	for harness, wantMode := range map[string]string{
		"claude":  state.ReadyModeHook,
		"gemini":  state.ReadyModeHook,
		"codex":   state.ReadyModeSettle,
		"generic": state.ReadyModeSettle,
		"":        state.ReadyModeSettle,
	} {
		nonce, err := writeInitRunFile(harness)
		require.NoError(t, err, harness)
		gotNonce, gotMode := readInitRun(t, initRun)
		assert.Equal(t, nonce, gotNonce, harness)
		assert.Equal(t, wantMode, gotMode, harness)

		fi, err := os.Stat(initRun)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm(), harness)
		data, _ := os.ReadFile(initRun)
		assert.Equal(t, 1, strings.Count(string(data), "\n"), "one line")
	}
	assertNoTempFiles(t, dir)
}

func TestWriteInitRunFile_NewNonceAndStaleMarkerRemoved(t *testing.T) {
	dir := t.TempDir()
	initRun, ready := useReadinessPaths(t, dir)

	first, err := writeInitRunFile("claude")
	require.NoError(t, err)
	markHarnessReady(sessionStart("startup"))
	got, ok := readMarker(t, ready)
	require.True(t, ok)
	require.Equal(t, first, got)

	second, err := writeInitRunFile("claude")
	require.NoError(t, err)
	assert.NotEqual(t, first, second, "each run gets a fresh nonce")
	_, err = os.Lstat(ready)
	assert.True(t, os.IsNotExist(err), "a new run removes the previous run's marker")
	gotNonce, _ := readInitRun(t, initRun)
	assert.Equal(t, second, gotNonce)
}

func TestWriteInitRunFile_SymlinkedDirectoryRefused(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "realDir")
	require.NoError(t, os.Mkdir(realDir, 0o755))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(realDir, link))
	useReadinessPaths(t, link)

	_, err := writeInitRunFile("claude")
	require.Error(t, err)
	entries, _ := os.ReadDir(realDir)
	assert.Empty(t, entries, "nothing written through the symlinked directory")
}

func TestWriteInitRunFile_SymlinkAtLeafReplacedNotFollowed(t *testing.T) {
	dir := t.TempDir()
	initRun, _ := useReadinessPaths(t, dir)
	victim := filepath.Join(t.TempDir(), "victim")
	require.NoError(t, os.WriteFile(victim, []byte("keep"), 0o600))
	require.NoError(t, os.Symlink(victim, initRun))

	_, err := writeInitRunFile("claude")
	require.NoError(t, err)
	data, _ := os.ReadFile(victim)
	assert.Equal(t, "keep", string(data))
	fi, err := os.Lstat(initRun)
	require.NoError(t, err)
	assert.True(t, fi.Mode().IsRegular())
}

func TestMarkHarnessReady_QualifyingEvents(t *testing.T) {
	qualifying := []*hooks.Event{
		sessionStart("startup"), sessionStart("resume"), sessionStart(""),
		{Name: hooks.EventPromptSubmit}, {Name: hooks.EventAgentStart},
		{Name: hooks.EventModelStart}, {Name: hooks.EventModelEnd},
		{Name: hooks.EventToolStart}, {Name: hooks.EventToolEnd},
		{Name: hooks.EventAgentEnd}, {Name: hooks.EventNotification},
		{Name: hooks.EventResponseComplete},
	}
	for _, ev := range qualifying {
		dir := t.TempDir()
		_, ready := useReadinessPaths(t, dir)
		nonce, err := writeInitRunFile("claude")
		require.NoError(t, err)
		markHarnessReady(ev)
		got, ok := readMarker(t, ready)
		assert.True(t, ok, "%s/%s should write the marker", ev.Name, ev.Data.Source)
		assert.Equal(t, nonce, got)
		fi, err := os.Stat(ready)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o644), fi.Mode().Perm())
		assertNoTempFiles(t, dir)
	}
}

func TestMarkHarnessReady_NonQualifyingEvents(t *testing.T) {
	nonQualifying := []*hooks.Event{
		nil,
		sessionStart("clear"), sessionStart("compact"),
		{Name: hooks.EventPreStart}, {Name: hooks.EventPostStart},
		{Name: hooks.EventPreStop}, {Name: hooks.EventSessionEnd},
		{Name: "unknown-event"},
	}
	dir := t.TempDir()
	_, ready := useReadinessPaths(t, dir)
	_, err := writeInitRunFile("claude")
	require.NoError(t, err)
	for _, ev := range nonQualifying {
		markHarnessReady(ev)
		_, err := os.Lstat(ready)
		assert.True(t, os.IsNotExist(err), "%v must not write the marker", ev)
	}
}

func TestMarkHarnessReady_ReplacesStaleMarkerKeepsCurrent(t *testing.T) {
	dir := t.TempDir()
	_, ready := useReadinessPaths(t, dir)
	nonce, err := writeInitRunFile("claude")
	require.NoError(t, err)

	// A marker from another run (e.g. written by a hook that raced the
	// init-run rewrite) is replaced.
	require.NoError(t, os.WriteFile(ready, []byte(state.FormatReadyMarker("00ff")), 0o644))
	markHarnessReady(&hooks.Event{Name: hooks.EventToolStart})
	got, ok := readMarker(t, ready)
	require.True(t, ok)
	assert.Equal(t, nonce, got)

	// A marker that already holds the current nonce is left alone.
	before, err := os.Stat(ready)
	require.NoError(t, err)
	markHarnessReady(&hooks.Event{Name: hooks.EventToolEnd})
	after, err := os.Stat(ready)
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "current marker is not rewritten")
}

func TestMarkHarnessReady_MissingOrGarbledInitRun(t *testing.T) {
	for name, content := range map[string]*string{
		"missing":   nil,
		"garbled":   strPtr("not a marker\n"),
		"bad mode":  strPtr("nonce=abcd mode=other\n"),
		"two lines": strPtr("nonce=abcd mode=hook\nnonce=abcd mode=hook\n"),
		"too large": strPtr("nonce=" + strings.Repeat("a", 400) + " mode=hook\n"),
	} {
		dir := t.TempDir()
		initRun, ready := useReadinessPaths(t, dir)
		if content != nil {
			require.NoError(t, os.WriteFile(initRun, []byte(*content), 0o644))
		}
		markHarnessReady(sessionStart("startup"))
		_, err := os.Lstat(ready)
		assert.True(t, os.IsNotExist(err), "%s: no marker expected", name)
	}
}

func TestMarkHarnessReady_SymlinkedInitRunRefused(t *testing.T) {
	dir := t.TempDir()
	initRun, ready := useReadinessPaths(t, dir)
	target := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.WriteFile(target, []byte(state.FormatInitRun("abcd", state.ReadyModeHook)), 0o644))
	require.NoError(t, os.Symlink(target, initRun))

	markHarnessReady(sessionStart("startup"))
	_, err := os.Lstat(ready)
	assert.True(t, os.IsNotExist(err), "a symlinked init-run file is not trusted")
}

func TestMarkHarnessReady_SymlinkedMarkerNotFollowed(t *testing.T) {
	dir := t.TempDir()
	_, ready := useReadinessPaths(t, dir)
	nonce, err := writeInitRunFile("claude")
	require.NoError(t, err)
	victim := filepath.Join(t.TempDir(), "victim")
	require.NoError(t, os.WriteFile(victim, []byte("keep"), 0o600))
	require.NoError(t, os.Symlink(victim, ready))

	markHarnessReady(sessionStart("startup"))
	data, _ := os.ReadFile(victim)
	assert.Equal(t, "keep", string(data), "the symlink target is never written")
	fi, err := os.Lstat(ready)
	require.NoError(t, err)
	assert.True(t, fi.Mode().IsRegular())
	got, ok := readMarker(t, ready)
	require.True(t, ok)
	assert.Equal(t, nonce, got)
}

func TestMarkHarnessReady_SymlinkedDirectoryRefused(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "realDir")
	require.NoError(t, os.Mkdir(realDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(realDir, "scion-init-run"), []byte(state.FormatInitRun("abcd", state.ReadyModeHook)), 0o644))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(realDir, link))
	useReadinessPaths(t, link)

	markHarnessReady(sessionStart("startup"))
	_, err := os.Lstat(filepath.Join(realDir, "scion-harness-ready"))
	assert.True(t, os.IsNotExist(err))
}

// TestProcessHookData_WritesReadyMarker checks the marker is written from
// the normalized event after dialect mapping (claude SessionStart).
func TestProcessHookData_WritesReadyMarker(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	scrubScionEnv(t)
	setTestLogPath(t, filepath.Join(tmpDir, "agent.log"))
	oldDialect := hookDialect
	hookDialect = "claude"
	t.Cleanup(func() { hookDialect = oldDialect })

	_, ready := useReadinessPaths(t, t.TempDir())
	nonce, err := writeInitRunFile("claude")
	require.NoError(t, err)

	clearData, _ := json.Marshal(map[string]interface{}{"hook_event_name": "SessionStart", "source": "clear"})
	require.NoError(t, processHookData(clearData))
	_, err = os.Lstat(ready)
	assert.True(t, os.IsNotExist(err), "SessionStart(clear) does not count")

	start, _ := json.Marshal(map[string]interface{}{"hook_event_name": "SessionStart", "source": "startup"})
	require.NoError(t, processHookData(start))
	got, ok := readMarker(t, ready)
	require.True(t, ok)
	assert.Equal(t, nonce, got)
}

func strPtr(s string) *string { return &s }
