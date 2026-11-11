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

package loglevel

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// resetState clears leaked SCION_* level variables and the process-wide
// state, restoring it when the test ends.
func resetState(t *testing.T) {
	t.Helper()
	t.Setenv(EnvLogLevel, "")
	t.Setenv(EnvDebug, "")
	SetWarningOutput(io.Discard)
	Reset(true)
	t.Cleanup(func() {
		Reset(true)
		SetWarningOutput(os.Stderr)
	})
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"debug", slog.LevelDebug, false},
		{"DEBUG", slog.LevelDebug, false},
		{" Info ", slog.LevelInfo, false},
		{"warn", slog.LevelWarn, false},
		{"Warning", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"", slog.LevelInfo, true},
		{"trace", slog.LevelInfo, true},
		{"1", slog.LevelInfo, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseLevel(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseLevel(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseLevel(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseLevelSpec(t *testing.T) {
	tests := []struct {
		name           string
		in             string
		wantDefault    slog.Level
		wantComponents map[string]slog.Level
		wantErr        string
	}{
		{name: "empty", in: "", wantDefault: slog.LevelInfo},
		{name: "debug", in: "debug", wantDefault: slog.LevelDebug},
		{name: "upper warn", in: "WARN", wantDefault: slog.LevelWarn},
		{name: "error", in: "error", wantDefault: slog.LevelError},
		{
			name: "per-component", in: "info,hub.auth=debug,hubsync=debug",
			wantDefault:    slog.LevelInfo,
			wantComponents: map[string]slog.Level{"hub.auth": slog.LevelDebug, "hubsync": slog.LevelDebug},
		},
		{
			name: "whitespace and case", in: " Warn , Hub.Auth = DEBUG ",
			wantDefault:    slog.LevelWarn,
			wantComponents: map[string]slog.Level{"hub.auth": slog.LevelDebug},
		},
		{
			name: "components only keep info default", in: "hub=error",
			wantDefault:    slog.LevelInfo,
			wantComponents: map[string]slog.Level{"hub": slog.LevelError},
		},
		{
			name: "unknown component kept", in: "info,no.such.component=debug",
			wantDefault:    slog.LevelInfo,
			wantComponents: map[string]slog.Level{"no.such.component": slog.LevelDebug},
		},
		{name: "invalid default falls back to info", in: "loud", wantDefault: slog.LevelInfo, wantErr: `invalid log level "loud"`},
		{
			name: "invalid component dropped", in: "debug,hub.auth=loud,hubsync=warn",
			wantDefault:    slog.LevelDebug,
			wantComponents: map[string]slog.Level{"hubsync": slog.LevelWarn},
			wantErr:        `component "hub.auth"`,
		},
		{name: "second bare level rejected", in: "info,debug", wantDefault: slog.LevelInfo, wantErr: "only the first entry"},
		{name: "missing component name", in: "info,=debug", wantDefault: slog.LevelInfo, wantErr: "missing component name"},
		{name: "empty entries ignored", in: "warn,,", wantDefault: slog.LevelWarn},
		{name: "leading empty entry", in: ",debug", wantDefault: slog.LevelDebug},
		{name: "leading blank entry", in: " ,warn", wantDefault: slog.LevelWarn},
		{
			name: "leading empty entries then components", in: ",, error ,hub.auth=debug",
			wantDefault:    slog.LevelError,
			wantComponents: map[string]slog.Level{"hub.auth": slog.LevelDebug},
		},
		{
			name: "bare level after a component rejected", in: "hub.auth=debug,warn",
			wantDefault:    slog.LevelInfo,
			wantComponents: map[string]slog.Level{"hub.auth": slog.LevelDebug},
			wantErr:        "only the first entry",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLevelSpec(tt.in)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			if got.Default != tt.wantDefault {
				t.Errorf("Default = %v, want %v", got.Default, tt.wantDefault)
			}
			if len(got.Components) != 0 || len(tt.wantComponents) != 0 {
				if !reflect.DeepEqual(got.Components, tt.wantComponents) {
					t.Errorf("Components = %v, want %v", got.Components, tt.wantComponents)
				}
			}
		})
	}
}

func TestSpecLevelMatching(t *testing.T) {
	spec, err := ParseLevelSpec("warn,hub=info,hub.auth=debug,hubsync=error")
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]slog.Level{
		"":                            slog.LevelWarn,
		"broker.heartbeat":            slog.LevelWarn,
		"hub":                         slog.LevelInfo,
		"hub.web":                     slog.LevelInfo,
		"hub.maintenance.pull-images": slog.LevelInfo,
		"hub.auth":                    slog.LevelDebug,
		"HUB.AUTH":                    slog.LevelDebug,
		"hub.auth.oidc":               slog.LevelDebug,
		"hubsync":                     slog.LevelError,
		"hubx":                        slog.LevelWarn, // no partial-word prefix match
	}
	for comp, want := range tests {
		if got := spec.Level(comp); got != want {
			t.Errorf("Level(%q) = %v, want %v", comp, got, want)
		}
	}
	if got := spec.MinLevel(); got != slog.LevelDebug {
		t.Errorf("MinLevel = %v, want debug", got)
	}
	if got := spec.String(); got != "warn,hub=info,hub.auth=debug,hubsync=error" {
		t.Errorf("String = %q", got)
	}
}

func TestSpecFromEnv(t *testing.T) {
	tests := []struct {
		name        string
		logLevel    string
		debug       string
		wantOK      bool
		wantDefault slog.Level
		wantWarn    string
	}{
		{name: "unset", wantDefault: slog.LevelInfo},
		{name: "log level", logLevel: "error", wantOK: true, wantDefault: slog.LevelError},
		{name: "deprecated alias", debug: "1", wantOK: true, wantDefault: slog.LevelDebug, wantWarn: "SCION_DEBUG is deprecated"},
		{name: "any non-empty alias value", debug: "false", wantOK: true, wantDefault: slog.LevelDebug, wantWarn: "use SCION_LOG_LEVEL=debug"},
		{name: "SCION_LOG_LEVEL wins", logLevel: "warn", debug: "1", wantOK: true, wantDefault: slog.LevelWarn},
		{name: "invalid level warns", logLevel: "loud", wantOK: true, wantDefault: slog.LevelInfo, wantWarn: "SCION_LOG_LEVEL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{EnvLogLevel: tt.logLevel, EnvDebug: tt.debug}
			spec, ok, warns := SpecFromEnv(func(k string) string { return env[k] })
			if ok != tt.wantOK {
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if spec.Default != tt.wantDefault {
				t.Errorf("Default = %v, want %v", spec.Default, tt.wantDefault)
			}
			joined := strings.Join(warns, "\n")
			if tt.wantWarn == "" && joined != "" {
				t.Errorf("unexpected warnings: %q", joined)
			}
			if tt.wantWarn != "" && !strings.Contains(joined, tt.wantWarn) {
				t.Errorf("warnings = %q, want containing %q", joined, tt.wantWarn)
			}
		})
	}
}

func TestStateFromEnvAndDeprecationWarningOnce(t *testing.T) {
	resetState(t)
	t.Setenv(EnvDebug, "1")
	var buf bytes.Buffer
	SetWarningOutput(&buf)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !DebugEnabled("") {
				t.Error("SCION_DEBUG should enable debug")
			}
		}()
	}
	wg.Wait()
	// A later re-resolution in the same process must not repeat it.
	Reset(false)
	_ = Effective("")

	if got := strings.Count(buf.String(), DeprecationWarning); got != 1 {
		t.Errorf("deprecation warning printed %d times, want 1: %q", got, buf.String())
	}
	if _, src := Current(); src != SourceEnv {
		t.Errorf("source = %v, want env", src)
	}
}

func TestInvalidEnvWarnsOnceAndFallsBack(t *testing.T) {
	resetState(t)
	t.Setenv(EnvLogLevel, "loud")
	var buf bytes.Buffer
	SetWarningOutput(&buf)

	for i := 0; i < 3; i++ {
		if got := Effective(""); got != slog.LevelInfo {
			t.Fatalf("Effective = %v, want info fallback", got)
		}
	}
	if got := strings.Count(buf.String(), "invalid log level"); got != 1 {
		t.Errorf("invalid-level warning printed %d times, want 1: %q", got, buf.String())
	}
}

func TestPrecedence(t *testing.T) {
	resetState(t)
	t.Setenv(EnvLogLevel, "warn")

	// Setting is lower precedence than env.
	if applied, _ := SetSetting("debug"); applied {
		t.Error("setting must not override env")
	}
	if got := Effective(""); got != slog.LevelWarn {
		t.Errorf("Effective = %v, want warn from env", got)
	}

	// Flag beats env.
	if applied, err := ApplyString("error,hub.auth=debug", SourceFlag); !applied || err != nil {
		t.Fatalf("flag apply = %v, %v", applied, err)
	}
	if got := Effective(""); got != slog.LevelError {
		t.Errorf("Effective = %v, want error from flag", got)
	}
	if got := Effective("hub.auth"); got != slog.LevelDebug {
		t.Errorf("Effective(hub.auth) = %v, want debug", got)
	}
	if got := MinLevel().Level(); got != slog.LevelDebug {
		t.Errorf("MinLevel = %v, want debug", got)
	}
	if got := DefaultLevel().Level(); got != slog.LevelError {
		t.Errorf("DefaultLevel = %v, want error", got)
	}

	// Env cannot override a flag.
	if Apply(Spec{Default: slog.LevelInfo}, SourceEnv) {
		t.Error("env must not override flag")
	}
}

func TestSettingAppliesWhenNothingElseSet(t *testing.T) {
	resetState(t)
	if applied, err := SetSetting("warn,hub.web=debug"); !applied || err != nil {
		t.Fatalf("SetSetting = %v, %v", applied, err)
	}
	if got := Effective("hub.web"); got != slog.LevelDebug {
		t.Errorf("Effective(hub.web) = %v, want debug", got)
	}
	// Reload with a new value.
	if applied, _ := SetSetting("error"); !applied {
		t.Fatal("setting reload should apply")
	}
	if got := Effective("hub.web"); got != slog.LevelError {
		t.Errorf("after reload Effective(hub.web) = %v, want error", got)
	}
	// A bad reload still applies the info fallback and reports the error.
	applied, err := SetSetting("loud")
	if !applied || err == nil {
		t.Errorf("bad setting = %v, %v; want applied with error", applied, err)
	}
	if got := Effective(""); got != slog.LevelInfo {
		t.Errorf("Effective = %v, want info fallback", got)
	}
}

func TestEnableDebugKeepsComponents(t *testing.T) {
	resetState(t)
	t.Setenv(EnvLogLevel, "info,hub.web=error")
	if !EnableDebug(SourceFlag) {
		t.Fatal("EnableDebug(flag) should apply over env")
	}
	if got := Effective(""); got != slog.LevelDebug {
		t.Errorf("Effective = %v, want debug", got)
	}
	if got := Effective("hub.web"); got != slog.LevelError {
		t.Errorf("Effective(hub.web) = %v, want error kept", got)
	}
}

func TestHasComponents(t *testing.T) {
	resetState(t)
	if HasComponents() {
		t.Error("default spec should have no components")
	}
	t.Setenv(EnvLogLevel, "warn")
	Reset(true)
	if HasComponents() {
		t.Error("warn should have no components")
	}
	t.Setenv(EnvLogLevel, "warn,hub.auth=debug")
	Reset(true)
	if !HasComponents() {
		t.Error("warn,hub.auth=debug should have components")
	}
}

func TestIgnoreDebugAlias(t *testing.T) {
	tests := []struct {
		name     string
		logLevel string
		debugEnv string
		// before reads the level before SetIgnoreDebugAlias, so the
		// environment has already been applied when the knob changes.
		before    bool
		wantDebug bool
		wantSrc   Source
	}{
		{name: "SCION_DEBUG ignored", debugEnv: "1", wantSrc: SourceDefault},
		{name: "SCION_DEBUG ignored after env was applied", debugEnv: "1", before: true, wantSrc: SourceDefault},
		{name: "SCION_LOG_LEVEL still honoured", logLevel: "debug", debugEnv: "1", wantDebug: true, wantSrc: SourceEnv},
		{name: "SCION_LOG_LEVEL honoured after env was applied", logLevel: "debug", before: true, wantDebug: true, wantSrc: SourceEnv},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetState(t)
			t.Setenv(EnvLogLevel, tt.logLevel)
			t.Setenv(EnvDebug, tt.debugEnv)
			var buf bytes.Buffer
			SetWarningOutput(&buf)
			if tt.before {
				_ = Effective("")
				buf.Reset()
			}

			SetIgnoreDebugAlias(true)

			if got := DebugEnabled(""); got != tt.wantDebug {
				t.Errorf("DebugEnabled = %v, want %v", got, tt.wantDebug)
			}
			if _, src := Current(); src != tt.wantSrc {
				t.Errorf("source = %v, want %v", src, tt.wantSrc)
			}
			if buf.Len() != 0 {
				t.Errorf("unexpected warning output: %q", buf.String())
			}
		})
	}
}

func TestIgnoreDebugAliasComponentLevel(t *testing.T) {
	resetState(t)
	t.Setenv(EnvLogLevel, "info,hubsync=debug")
	t.Setenv(EnvDebug, "1")
	SetIgnoreDebugAlias(true)
	if !DebugEnabled("hubsync") {
		t.Error("hubsync=debug should still enable the hubsync component")
	}
	if DebugEnabled("") {
		t.Error("default level should stay info")
	}
}

func TestIgnoreDebugAliasToggleBack(t *testing.T) {
	resetState(t)
	t.Setenv(EnvDebug, "1")
	SetIgnoreDebugAlias(true)
	if DebugEnabled("") {
		t.Fatal("SCION_DEBUG should be ignored")
	}
	SetIgnoreDebugAlias(false)
	if !DebugEnabled("") {
		t.Error("SCION_DEBUG should apply again once the alias is honoured")
	}
	if _, src := Current(); src != SourceEnv {
		t.Errorf("source = %v, want env", src)
	}
}

func TestIgnoreDebugAliasKeepsFlagAndSetting(t *testing.T) {
	t.Run("flag-set", func(t *testing.T) {
		resetState(t)
		t.Setenv(EnvDebug, "1")
		if !EnableDebug(SourceFlag) {
			t.Fatal("flag should apply")
		}
		SetIgnoreDebugAlias(true)
		if !DebugEnabled("") {
			t.Error("a flag-set debug level must survive SetIgnoreDebugAlias")
		}
	})
	t.Run("setting-set", func(t *testing.T) {
		resetState(t)
		SetIgnoreDebugAlias(true)
		if applied, err := SetSetting("warn"); !applied || err != nil {
			t.Fatalf("SetSetting = %v, %v", applied, err)
		}
		t.Setenv(EnvDebug, "1")
		SetIgnoreDebugAlias(true)
		if got := Effective(""); got != slog.LevelWarn {
			t.Errorf("Effective = %v, want warn from setting", got)
		}
	})
}

func TestResetClearsIgnoreDebugAlias(t *testing.T) {
	resetState(t)
	t.Setenv(EnvDebug, "1")
	SetIgnoreDebugAlias(true)
	Reset(true)
	if !DebugEnabled("") {
		t.Error("Reset should turn SetIgnoreDebugAlias off")
	}
}
