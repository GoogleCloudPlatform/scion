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

package hubtestutil

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
)

// TempSettingsHome points config.GetGlobalDir() at a temp directory holding a
// minimal settings.yaml, and returns the path to that file. It also isolates
// the shared log level state, because server-config saves and reloads
// apply server.log_level to it.
func TempSettingsHome(t *testing.T) string {
	t.Helper()
	IsolateLogLevelState(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	if err := os.WriteFile(settingsPath, []byte("schema_version: \"1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return settingsPath
}

// IsolateLogLevelState clears leaked level variables and resets the shared
// level state and the standard-library log bridge level, restoring both
// when the test ends.
func IsolateLogLevelState(t *testing.T) {
	t.Helper()
	t.Setenv(loglevel.EnvLogLevel, "")
	t.Setenv(loglevel.EnvDebug, "")
	loglevel.SetWarningOutput(io.Discard)
	loglevel.Reset(true)
	prevBridge := slog.SetLogLoggerLevel(slog.LevelInfo)
	t.Cleanup(func() {
		slog.SetLogLoggerLevel(prevBridge)
		loglevel.Reset(true)
		loglevel.SetWarningOutput(os.Stderr)
	})
}
