//go:build unix

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

package hubtestutil_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/internal/hubtestutil"
	sciontoollog "github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

func TestSilenceSciontoolLog_SuppressesStderrAndIsolatesLogFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	prevDefault := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(prevDefault)
		sciontoollog.SetLogPath(filepath.Join(home, "agent.log"))
	})

	hubtestutil.SilenceSciontoolLog()

	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()

	sciontoollog.Info("Conduit session established (test-quiet-check)")

	_ = w.Close()
	os.Stderr = origStderr
	stderrBytes, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatalf("io.ReadAll: %v", err)
	}
	if len(stderrBytes) != 0 {
		t.Errorf("expected no stderr output when SilenceSciontoolLog is active, got %q", string(stderrBytes))
	}

	logData, err := os.ReadFile(filepath.Join(home, "agent.log"))
	if err != nil {
		t.Fatalf("expected isolated agent.log under HOME, got read error: %v", err)
	}
	if !strings.Contains(string(logData), "test-quiet-check") {
		t.Errorf("expected isolated agent.log to contain logged line, got %q", string(logData))
	}
}
