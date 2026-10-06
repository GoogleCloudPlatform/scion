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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// logLinesWithMessage returns the text-handler log lines that contain message.
func logLinesWithMessage(logs *bytes.Buffer, message string) []string {
	var lines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, message) {
			lines = append(lines, line)
		}
	}
	return lines
}

// assertSingleLogLine asserts that exactly one log line carries message, that
// it has every key=value in fields, and that it contains none of absent.
// It returns the line.
func assertSingleLogLine(t *testing.T, logs *bytes.Buffer, message string, fields map[string]string, absent ...string) string {
	t.Helper()
	lines := logLinesWithMessage(logs, message)
	require.Len(t, lines, 1, "expected one %q line in:\n%s", message, logs.String())
	line := lines[0]
	for key, value := range fields {
		assert.Contains(t, line, " "+key+"="+value+"", "line should carry %s=%s", key, value)
	}
	for _, s := range absent {
		require.NotEmpty(t, s, "absent strings must be non-empty to be meaningful")
		assert.NotContains(t, line, s)
	}
	return line
}

func TestFSErrorClass(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"workspace timeout", fmt.Errorf("probe: %w", errWorkspaceContentTimeout), fsErrorClassTimeout},
		{"context deadline", context.DeadlineExceeded, fsErrorClassTimeout},
		{"os deadline", os.ErrDeadlineExceeded, fsErrorClassTimeout},
		{"not exist", &fs.PathError{Op: "remove", Path: "/some/dir", Err: fs.ErrNotExist}, fsErrorClassNotExist},
		{"permission", &fs.PathError{Op: "remove", Path: "/some/dir", Err: fs.ErrPermission}, fsErrorClassPermission},
		{"bare permission", os.ErrPermission, fsErrorClassPermission},
		{"other", errors.New("something else"), fsErrorClassOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, fsErrorClass(tt.err))
		})
	}
}

// makeReadOnlyDir makes dir read-only for the rest of the test, so entries in
// it cannot be removed, and restores it on cleanup so t.TempDir can remove it.
func makeReadOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not restrict root")
	}
	require.NoError(t, os.Chmod(dir, 0500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
}

// When the hub-managed project directory cannot be removed, the warning
// carries the project ID and a fixed error class, and no slug, path or error
// text.
func TestRemoveProjectDirUnderProjectsRoot_FailureLogsProjectIDAndErrorClass(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	srv, _ := testServer(t)
	logs := captureProjectsLog(t, srv)

	const (
		projectID = "bbbbbbbb-0000-4000-8000-000000000002"
		slug      = "remove-fails-project"
	)
	projectsRoot := filepath.Join(tmpHome, ".scion", "projects")
	projectPath := filepath.Join(projectsRoot, slug)
	writeProjectDirFile(t, projectPath)
	// The projects root is read-only, so the project directory itself cannot
	// be unlinked from it.
	makeReadOnlyDir(t, projectsRoot)

	srv.removeProjectDirUnderProjectsRoot(projectID, projectPath)

	assert.DirExists(t, projectPath, "the directory is left in place when removal fails")
	assertSingleLogLine(t, logs, "failed to remove hub-managed project directory",
		map[string]string{"project_id": projectID, "error_class": fsErrorClassPermission},
		slug, tmpHome, ".scion", "permission denied", "path=", "error=")
}
