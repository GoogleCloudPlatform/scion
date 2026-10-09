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

package hubsync

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// captureWarnings redirects warnf output for the duration of the test.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := warnOut
	warnOut = &buf
	t.Cleanup(func() { warnOut = orig })
	return &buf
}

// captureStderr returns what fn writes to os.Stderr.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = orig
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

// debugOff makes sure debug output is disabled for the test.
func debugOff(t *testing.T) {
	t.Helper()
	util.SetExplicitDebug(false)
	util.SetAgentDebugPolicy(false)
	t.Setenv("SCION_DEBUG", "")
	t.Setenv("SCION_LOG_LEVEL", "")
	t.Cleanup(func() { util.SetExplicitDebug(false) })
}

func TestDebugf_SilentByDefault(t *testing.T) {
	debugOff(t)
	warnings := captureWarnings(t)

	// An empty project path only produces a debug line.
	out := captureStderr(t, func() { UpdateLastSyncedAt("", time.Now()) })

	if out != "" {
		t.Errorf("expected no stderr output with debug off, got %q", out)
	}
	if warnings.Len() != 0 {
		t.Errorf("expected no warning, got %q", warnings.String())
	}
}

func TestDebugf_ShownWithDebugOn(t *testing.T) {
	debugOff(t)
	util.EnableDebug()

	out := captureStderr(t, func() { UpdateLastSyncedAt("", time.Now()) })

	if !strings.HasPrefix(out, "[hubsync] ") {
		t.Errorf("expected a [hubsync] debug line with debug on, got %q", out)
	}
}

func TestBestEffortFailure_PrintsWarningWithDebugOff(t *testing.T) {
	debugOff(t)
	warnings := captureWarnings(t)

	// A regular file where the project directory should be makes the
	// state.yaml save fail.
	projectPath := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(projectPath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	stderr := captureStderr(t, func() { UpdateLastSyncedAt(projectPath, time.Now()) })

	got := warnings.String()
	if !strings.Contains(got, "Warning: failed to save lastSyncedAt to state.yaml: ") {
		t.Errorf("expected a save-failure warning, got %q", got)
	}
	if strings.Contains(stderr, "[hubsync]") {
		t.Errorf("expected no [hubsync] debug lines with debug off, got %q", stderr)
	}
}

func TestWarnf_DefaultsToStderr(t *testing.T) {
	if warnOut != io.Writer(os.Stderr) {
		t.Error("warnOut should default to os.Stderr")
	}
	warnings := captureWarnings(t)
	warnf("probe %d", 1)
	if got := warnings.String(); got != "Warning: probe 1\n" {
		t.Errorf("warnf output = %q, want %q", got, "Warning: probe 1\n")
	}
}
