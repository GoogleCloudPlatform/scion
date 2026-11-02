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

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTxDiagnosticsSettings(t *testing.T) {
	f := false
	tr := true
	cases := []struct {
		name    string
		in      TxDiagnosticsConfig
		want    TxDiagnosticsSettings
		wantErr string
	}{
		{
			name: "defaults",
			want: TxDiagnosticsSettings{Enabled: true, HoldWarnThreshold: 2 * time.Second, LockWaitWarnThreshold: time.Second, WarnInterval: time.Minute},
		},
		{
			name: "explicit values",
			in:   TxDiagnosticsConfig{Enabled: &tr, HoldWarnThreshold: "5s", LockWaitWarnThreshold: "250ms", WarnInterval: "30s"},
			want: TxDiagnosticsSettings{Enabled: true, HoldWarnThreshold: 5 * time.Second, LockWaitWarnThreshold: 250 * time.Millisecond, WarnInterval: 30 * time.Second},
		},
		{
			name: "disabled and zero thresholds turn WARNs off",
			in:   TxDiagnosticsConfig{Enabled: &f, HoldWarnThreshold: "0", LockWaitWarnThreshold: "0"},
			want: TxDiagnosticsSettings{Enabled: false, WarnInterval: time.Minute},
		},
		{name: "malformed", in: TxDiagnosticsConfig{HoldWarnThreshold: "soon"}, wantErr: "hold_warn_threshold"},
		{name: "negative", in: TxDiagnosticsConfig{LockWaitWarnThreshold: "-1s"}, wantErr: "must not be negative"},
		{name: "zero interval", in: TxDiagnosticsConfig{WarnInterval: "0"}, wantErr: "must be positive"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.in.Settings()
			if c.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), c.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.want, got)
		})
	}
}

// TestTxDiagnostics_V1RoundTrip checks the V1 <-> GlobalConfig conversion
// keeps the settings in both directions, and omits them when unset.
func TestTxDiagnostics_V1RoundTrip(t *testing.T) {
	f := false
	gc := ConvertV1ServerToGlobalConfig(&V1ServerConfig{
		Database: &V1DatabaseConfig{Driver: "sqlite", TxDiagnostics: &TxDiagnosticsConfig{Enabled: &f, HoldWarnThreshold: "3s"}},
	})
	require.NotNil(t, gc.Database.TxDiagnostics.Enabled)
	assert.False(t, *gc.Database.TxDiagnostics.Enabled)
	assert.Equal(t, "3s", gc.Database.TxDiagnostics.HoldWarnThreshold)

	v1 := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, v1.Database)
	require.NotNil(t, v1.Database.TxDiagnostics)
	assert.Equal(t, "3s", v1.Database.TxDiagnostics.HoldWarnThreshold)

	plain := ConvertGlobalToV1ServerConfig(ConvertV1ServerToGlobalConfig(&V1ServerConfig{Database: &V1DatabaseConfig{Driver: "sqlite"}}))
	assert.Nil(t, plain.Database.TxDiagnostics)
}

// TestLoadGlobalConfig_TxDiagnostics loads the settings from settings.yaml.
func TestLoadGlobalConfig_TxDiagnostics(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	path := filepath.Join(tmp, "settings.yaml")
	content := "schema_version: \"1\"\nserver:\n  hub:\n    port: 9999\n  database:\n    driver: sqlite\n" +
		"    tx_diagnostics:\n      enabled: false\n      hold_warn_threshold: 4s\n      lock_wait_warn_threshold: 500ms\n      warn_interval: 2m\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	cfg, err := LoadGlobalConfig(path)
	require.NoError(t, err)
	s, err := cfg.Database.TxDiagnostics.Settings()
	require.NoError(t, err)
	assert.Equal(t, TxDiagnosticsSettings{Enabled: false, HoldWarnThreshold: 4 * time.Second, LockWaitWarnThreshold: 500 * time.Millisecond, WarnInterval: 2 * time.Minute}, s)
}
