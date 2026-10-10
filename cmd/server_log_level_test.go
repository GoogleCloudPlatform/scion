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

package cmd

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
)

// TestApplyServerLogLevelSetting checks the startup precedence for
// server.log_level: flag > SCION_LOG_LEVEL > setting > info, and the one
// startup line naming the resolved level and its source.
func TestApplyServerLogLevelSetting(t *testing.T) {
	tests := []struct {
		name     string
		env      string
		flag     bool
		setting  string
		want     slog.Level
		wantLine string
	}{
		{name: "default", setting: "info", want: slog.LevelInfo, wantLine: `"log_level":"info","source":"default"`},
		{name: "setting applies at boot", setting: "debug", want: slog.LevelDebug, wantLine: `"log_level":"debug","source":"setting"`},
		{name: "env beats setting", env: "warn", setting: "debug", want: slog.LevelWarn, wantLine: `"log_level":"warn","source":"env"`},
		{name: "flag beats env and setting", env: "error", flag: true, setting: "warn", want: slog.LevelDebug, wantLine: `"log_level":"debug","source":"flag"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(loglevel.EnvLogLevel, tt.env)
			t.Setenv(loglevel.EnvDebug, "")
			loglevel.SetWarningOutput(io.Discard)
			loglevel.Reset(true)
			origLogger := slog.Default()
			t.Cleanup(func() {
				slog.SetDefault(origLogger)
				loglevel.Reset(true)
				loglevel.SetWarningOutput(os.Stderr)
			})
			var buf bytes.Buffer
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))

			logging.ApplyDebugFlag(tt.flag)
			applyServerLogLevelSetting(tt.setting)

			if got := logging.EffectiveLevel(""); got != tt.want {
				t.Errorf("effective level = %v, want %v", got, tt.want)
			}
			out := buf.String()
			if n := strings.Count(out, "Log level resolved"); n != 1 {
				t.Errorf("startup line logged %d times, want 1:\n%s", n, out)
			}
			if !strings.Contains(out, tt.wantLine) {
				t.Errorf("startup line lacks %s:\n%s", tt.wantLine, out)
			}
		})
	}
}
