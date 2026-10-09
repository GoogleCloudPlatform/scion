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
	"bytes"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
)

// resetLevelState clears leaked SCION_* level variables and the shared
// level state, restoring it when the test ends.
func resetLevelState(t *testing.T) {
	t.Helper()
	t.Setenv(loglevel.EnvLogLevel, "")
	t.Setenv(loglevel.EnvDebug, "")
	loglevel.SetWarningOutput(io.Discard)
	loglevel.Reset(true)
	t.Cleanup(func() {
		loglevel.Reset(true)
		loglevel.SetWarningOutput(os.Stderr)
	})
}

func newFilteredTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(newLevelFilter(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: passAllLevel})))
}

func TestLevelFilterDefaultLevels(t *testing.T) {
	tests := []struct {
		name     string
		logLevel string
		debugEnv string
		want     []string
	}{
		{name: "default info", want: []string{"info-msg", "warn-msg", "error-msg"}},
		{name: "debug", logLevel: "debug", want: []string{"debug-msg", "info-msg", "warn-msg", "error-msg"}},
		{name: "warn", logLevel: "warn", want: []string{"warn-msg", "error-msg"}},
		{name: "error", logLevel: "Error", want: []string{"error-msg"}},
		{name: "invalid falls back to info", logLevel: "loud", want: []string{"info-msg", "warn-msg", "error-msg"}},
		{name: "deprecated SCION_DEBUG", debugEnv: "1", want: []string{"debug-msg", "info-msg", "warn-msg", "error-msg"}},
		{name: "SCION_LOG_LEVEL wins over SCION_DEBUG", logLevel: "error", debugEnv: "1", want: []string{"error-msg"}},
	}
	all := []string{"debug-msg", "info-msg", "warn-msg", "error-msg"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetLevelState(t)
			t.Setenv(loglevel.EnvLogLevel, tt.logLevel)
			t.Setenv(loglevel.EnvDebug, tt.debugEnv)
			var buf bytes.Buffer
			l := newFilteredTestLogger(&buf)
			l.Debug("debug-msg")
			l.Info("info-msg")
			l.Warn("warn-msg")
			l.Error("error-msg")
			assertLines(t, buf.String(), all, tt.want)
		})
	}
}

func TestLevelFilterPerComponent(t *testing.T) {
	resetLevelState(t)
	t.Setenv(loglevel.EnvLogLevel, "warn,hub=info,hub.auth=debug")
	var buf bytes.Buffer
	l := newFilteredTestLogger(&buf)

	l.With(AttrSubsystem, "hub.auth").Debug("auth-debug")
	l.With(AttrSubsystem, "hub.auth.oidc").Debug("auth-child-debug")
	l.With(AttrSubsystem, "hub.web").Debug("web-debug")
	l.With(AttrSubsystem, "hub.web").Info("web-info")
	l.With(AttrSubsystem, "broker.heartbeat").Info("broker-info")
	l.With(AttrSubsystem, "broker.heartbeat").Warn("broker-warn")
	l.Info("root-info")
	l.Debug("inline-debug", AttrSubsystem, "hub.auth")
	l.Info("inline-info-default", AttrSubsystem, "broker.messages")
	// A subsystem key nested in a group is not the subsystem.
	l.WithGroup("g").With(AttrSubsystem, "hub.auth").Debug("grouped-debug")

	all := []string{
		"auth-debug", "auth-child-debug", "web-debug", "web-info", "broker-info",
		"broker-warn", "root-info", "inline-debug", "inline-info-default", "grouped-debug",
	}
	want := []string{"auth-debug", "auth-child-debug", "web-info", "broker-warn", "inline-debug"}
	assertLines(t, buf.String(), all, want)
}

func TestLevelFilterFollowsRuntimeChanges(t *testing.T) {
	resetLevelState(t)
	var buf bytes.Buffer
	l := newFilteredTestLogger(&buf)
	sub := l.With(AttrSubsystem, "hub.scheduler")

	sub.Debug("before")
	if _, err := SetLogLevelSetting("info,hub.scheduler=debug"); err != nil {
		t.Fatal(err)
	}
	sub.Debug("after-setting")
	if got := EffectiveLevel("hub.scheduler"); got != slog.LevelDebug {
		t.Errorf("EffectiveLevel = %v, want debug", got)
	}
	assertLines(t, buf.String(), []string{"before", "after-setting"}, []string{"after-setting"})
}

func TestLevelFilterSubsystemHelper(t *testing.T) {
	resetLevelState(t)
	t.Setenv(loglevel.EnvLogLevel, "error,hub.auth=debug")
	var buf bytes.Buffer
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })
	slog.SetDefault(newFilteredTestLogger(&buf))

	Subsystem("hub.auth").Debug("subsystem-debug")
	Subsystem("hub.web").Warn("subsystem-warn")
	assertLines(t, buf.String(), []string{"subsystem-debug", "subsystem-warn"}, []string{"subsystem-debug"})
}

func TestSetupDebugFlag(t *testing.T) {
	resetLevelState(t)
	t.Setenv(loglevel.EnvLogLevel, "error,hub.web=warn")
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	Setup("test", true, false)
	if got := EffectiveLevel(""); got != slog.LevelDebug {
		t.Errorf("default = %v, want debug from the debug flag", got)
	}
	if got := EffectiveLevel("hub.web"); got != slog.LevelWarn {
		t.Errorf("hub.web = %v, want explicit component level kept", got)
	}
	if !slog.Default().Enabled(t.Context(), slog.LevelDebug) {
		t.Error("default logger should be enabled at debug")
	}
}

func TestParseLevelSpecWrapper(t *testing.T) {
	spec, err := ParseLevelSpec("warn,hub.auth=debug")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Default != slog.LevelWarn || spec.Level("hub.auth") != slog.LevelDebug {
		t.Errorf("unexpected spec %v", spec)
	}
}

func assertLines(t *testing.T, out string, all, want []string) {
	t.Helper()
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	for _, msg := range all {
		got := strings.Contains(out, `"msg":"`+msg+`"`)
		if got != wantSet[msg] {
			t.Errorf("%s emitted = %v, want %v\noutput:\n%s", msg, got, wantSet[msg], out)
		}
	}
}
