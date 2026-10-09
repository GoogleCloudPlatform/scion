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

package logging

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
)

// TestSetLogLevelSettingPrecedence checks flag > environment > setting >
// default for a settings value such as server.log_level.
func TestSetLogLevelSettingPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		flag    bool
		setting string
		want    slog.Level
		wantSrc loglevel.Source
	}{
		{name: "default", want: slog.LevelInfo, wantSrc: loglevel.SourceDefault},
		{name: "setting applies", setting: "warn", want: slog.LevelWarn, wantSrc: loglevel.SourceSetting},
		{name: "env beats setting", env: "error", setting: "debug", want: slog.LevelError, wantSrc: loglevel.SourceEnv},
		{name: "flag beats env and setting", env: "error", flag: true, setting: "warn", want: slog.LevelDebug, wantSrc: loglevel.SourceFlag},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetLevelState(t)
			t.Setenv(loglevel.EnvLogLevel, tt.env)
			ApplyDebugFlag(tt.flag)
			if tt.setting != "" {
				if _, err := SetLogLevelSetting(tt.setting); err != nil {
					t.Fatal(err)
				}
			}
			if got := EffectiveLevel(""); got != tt.want {
				t.Errorf("EffectiveLevel = %v, want %v", got, tt.want)
			}
			if _, src := loglevel.Current(); src != tt.wantSrc {
				t.Errorf("source = %v, want %v", src, tt.wantSrc)
			}
		})
	}
}

// TestSetLogLevelSettingClearReverts checks that an empty setting (the
// key removed on reload) reverts to the default level.
func TestSetLogLevelSettingClearReverts(t *testing.T) {
	resetLevelState(t)
	if _, err := SetLogLevelSetting("debug"); err != nil {
		t.Fatal(err)
	}
	if got := EffectiveLevel(""); got != slog.LevelDebug {
		t.Fatalf("EffectiveLevel = %v, want debug", got)
	}
	if _, err := SetLogLevelSetting(""); err != nil {
		t.Fatal(err)
	}
	if got := EffectiveLevel(""); got != slog.LevelInfo {
		t.Errorf("EffectiveLevel after clear = %v, want info", got)
	}
}

// TestCloudHandlerFollowsSettingChange checks that a CloudHandler built
// with ResolveLogLeveler passes DEBUG records after the setting changes to
// debug, and stops again when it is cleared.
func TestCloudHandlerFollowsSettingChange(t *testing.T) {
	resetLevelState(t)
	h := &CloudHandler{level: ResolveLogLeveler(false), component: "test"}
	ctx := context.Background()

	if h.Enabled(ctx, slog.LevelDebug) {
		t.Fatal("debug enabled at the default level")
	}
	if _, err := SetLogLevelSetting("debug"); err != nil {
		t.Fatal(err)
	}
	if !h.Enabled(ctx, slog.LevelDebug) {
		t.Error("debug not enabled after the setting changed to debug")
	}
	if _, err := SetLogLevelSetting(""); err != nil {
		t.Fatal(err)
	}
	if h.Enabled(ctx, slog.LevelDebug) {
		t.Error("debug still enabled after the setting was cleared")
	}

	// A raised level never drops INFO from the floor: the level filter,
	// not the floor, raises the main log.
	if _, err := SetLogLevelSetting("error"); err != nil {
		t.Fatal(err)
	}
	if !h.Enabled(ctx, slog.LevelInfo) {
		t.Error("info dropped by the floor at level error")
	}
}

// TestCloudHandlerNilLevelIsInfo checks the zero value of the level.
func TestCloudHandlerNilLevelIsInfo(t *testing.T) {
	h := &CloudHandler{component: "test"}
	ctx := context.Background()
	if !h.Enabled(ctx, slog.LevelInfo) || h.Enabled(ctx, slog.LevelDebug) {
		t.Error("nil level should behave as info")
	}
}

// TestRequestLogFollowsSettingChange checks that the request logger, built
// as the server builds it, emits DEBUG records after the setting changes to
// debug.
func TestRequestLogFollowsSettingChange(t *testing.T) {
	resetLevelState(t)
	path := filepath.Join(t.TempDir(), "requests.log")
	logger, cleanup, err := NewRequestLogger(RequestLoggerConfig{
		FilePath:  path,
		Component: "test",
		Level:     ResolveLogLeveler(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug("before-change")
	if _, err := SetLogLevelSetting("debug"); err != nil {
		t.Fatal(err)
	}
	logger.Debug("after-change")
	if cleanup != nil {
		cleanup()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if strings.Contains(out, "before-change") {
		t.Errorf("debug entry emitted at the default level:\n%s", out)
	}
	if !strings.Contains(out, "after-change") {
		t.Errorf("debug entry missing after the setting changed to debug:\n%s", out)
	}
}
