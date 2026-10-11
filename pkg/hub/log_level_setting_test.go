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

package hub

import (
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
)

// isolateLogLevelState clears leaked level variables and resets the shared
// level state and the standard-library log bridge level, restoring both
// when the test ends.
func isolateLogLevelState(t *testing.T) {
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

// stdLogBridgeLevel returns the current standard-library log bridge level.
func stdLogBridgeLevel() slog.Level {
	lvl := slog.SetLogLoggerLevel(slog.LevelInfo)
	slog.SetLogLoggerLevel(lvl)
	return lvl
}

// TestApplySnapshotLogLevel_ChangesSharedLevel checks that applying
// server.log_level changes the shared handler level, leaves the std-log
// bridge level alone, and reverts to info when cleared.
func TestApplySnapshotLogLevel_ChangesSharedLevel(t *testing.T) {
	isolateLogLevelState(t)

	applySnapshotLogLevel("debug")
	if got := logging.EffectiveLevel(""); got != slog.LevelDebug {
		t.Errorf("effective level = %v, want debug", got)
	}
	if got := logging.ResolveLogLeveler(false).Level(); got != slog.LevelDebug {
		t.Errorf("handler floor = %v, want debug", got)
	}
	if got := stdLogBridgeLevel(); got != slog.LevelInfo {
		t.Errorf("std-log bridge level = %v, want info (unchanged)", got)
	}

	applySnapshotLogLevel("")
	if got := logging.EffectiveLevel(""); got != slog.LevelInfo {
		t.Errorf("effective level after clear = %v, want info", got)
	}
}

// TestApplySnapshotLogLevel_EnvWins checks that SCION_LOG_LEVEL beats
// server.log_level on reload.
func TestApplySnapshotLogLevel_EnvWins(t *testing.T) {
	isolateLogLevelState(t)
	t.Setenv(loglevel.EnvLogLevel, "warn")
	loglevel.Reset(false)

	applySnapshotLogLevel("debug")
	if got := logging.EffectiveLevel(""); got != slog.LevelWarn {
		t.Errorf("effective level = %v, want warn from the environment", got)
	}
}
