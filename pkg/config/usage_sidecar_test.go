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
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/GoogleCloudPlatform/scion/harnesses"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func TestParseUsageSidecar_Valid(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want *api.HarnessUsageCapabilities
	}{
		{
			name: "native full canonical tokens",
			yaml: `schema_version: 1
usage:
  live:
    source: native
    token_types: [input, output, cache_read, cache_write, reasoning]
    not_applicable_token_types: []
    counts_failed_calls: true
`,
			want: &api.HarnessUsageCapabilities{
				SchemaVersion: 1,
				Live: api.HarnessUsageLiveCapabilities{
					Source:            api.UsageLiveSourceNative,
					TokenTypes:        []string{"input", "output", "cache_read", "cache_write", "reasoning"},
					CountsFailedCalls: true,
				},
			},
		},
		{
			name: "native with not_applicable_token_types",
			yaml: `schema_version: 1
usage:
  live:
    source: native
    token_types: [input, output, cache_read, reasoning]
    not_applicable_token_types: [cache_write]
    counts_failed_calls: true
`,
			want: &api.HarnessUsageCapabilities{
				SchemaVersion: 1,
				Live: api.HarnessUsageLiveCapabilities{
					Source:                  api.UsageLiveSourceNative,
					TokenTypes:              []string{"input", "output", "cache_read", "reasoning"},
					NotApplicableTokenTypes: []string{"cache_write"},
					CountsFailedCalls:       true,
				},
			},
		},
		{
			name: "hooks calls-only",
			yaml: `schema_version: 1
usage:
  live:
    source: hooks
    token_types: []
    not_applicable_token_types: []
    counts_failed_calls: false
`,
			want: &api.HarnessUsageCapabilities{
				SchemaVersion: 1,
				Live: api.HarnessUsageLiveCapabilities{
					Source:            api.UsageLiveSourceHooks,
					CountsFailedCalls: false,
				},
			},
		},
		{
			name: "hooks full canonical tokens",
			yaml: `schema_version: 1
usage:
  live:
    source: hooks
    token_types: [input, output, cache_read, cache_write, reasoning]
    counts_failed_calls: false
`,
			want: &api.HarnessUsageCapabilities{
				SchemaVersion: 1,
				Live: api.HarnessUsageLiveCapabilities{
					Source:     api.UsageLiveSourceHooks,
					TokenTypes: []string{"input", "output", "cache_read", "cache_write", "reasoning"},
				},
			},
		},
		{
			name: "sqlite_native full canonical tokens",
			yaml: `schema_version: 1
usage:
  live:
    source: sqlite_native
    token_types: [input, output, cache_read, cache_write, reasoning]
    counts_failed_calls: false
`,
			want: &api.HarnessUsageCapabilities{
				SchemaVersion: 1,
				Live: api.HarnessUsageLiveCapabilities{
					Source:     api.UsageLiveSourceSQLiteNative,
					TokenTypes: []string{"input", "output", "cache_read", "cache_write", "reasoning"},
				},
			},
		},
		{
			name: "file_tailer",
			yaml: `schema_version: 1
usage:
  live:
    source: file_tailer
    token_types: [input, output, cache_read, cache_write]
    counts_failed_calls: false
`,
			want: &api.HarnessUsageCapabilities{
				SchemaVersion: 1,
				Live: api.HarnessUsageLiveCapabilities{
					Source:     api.UsageLiveSourceFileTailer,
					TokenTypes: []string{"input", "output", "cache_read", "cache_write"},
				},
			},
		},
		{
			name: "none",
			yaml: `schema_version: 1
usage:
  live:
    source: none
    token_types: []
    not_applicable_token_types: []
    counts_failed_calls: false
`,
			want: &api.HarnessUsageCapabilities{
				SchemaVersion: 1,
				Live: api.HarnessUsageLiveCapabilities{
					Source: api.UsageLiveSourceNone,
				},
			},
		},
		{
			name: "unknown keys and opaque trajectory section ignored",
			yaml: `schema_version: 1
future_root_key: "ignored"
usage:
  future_usage_key: 42
  live:
    source: native
    token_types: [input, output, cache_read, cache_write]
    counts_failed_calls: true
    future_live_flag: true
  trajectory:
    artifacts:
      - key: claude-projects
        glob: ".claude/projects/**/*.jsonl"
        kind: jsonl_append
    normalizer:
      interface_version: 1
      parser_version: "claude-1"
`,
			want: &api.HarnessUsageCapabilities{
				SchemaVersion: 1,
				Live: api.HarnessUsageLiveCapabilities{
					Source:            api.UsageLiveSourceNative,
					TokenTypes:        []string{"input", "output", "cache_read", "cache_write"},
					CountsFailedCalls: true,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseUsageSidecar([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("ParseUsageSidecar() unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseUsageSidecar() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseUsageSidecar_Invalid(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"malformed yaml", ":\n\tbad: ["},
		{"empty file", ""},
		{"missing schema_version", "usage:\n  live:\n    source: none\n"},
		{"unsupported schema_version", "schema_version: 2\nusage:\n  live:\n    source: none\n"},
		{"missing source", "schema_version: 1\nusage:\n  live:\n    counts_failed_calls: false\n"},
		{"unknown source enum", "schema_version: 1\nusage:\n  live:\n    source: future_source\n"},
		{"unknown token_type", "schema_version: 1\nusage:\n  live:\n    source: native\n    token_types: [input, prompt]\n"},
		{"unknown not_applicable_token_type", "schema_version: 1\nusage:\n  live:\n    source: native\n    not_applicable_token_types: [bogus]\n"},
		{"duplicate token_type", "schema_version: 1\nusage:\n  live:\n    source: native\n    token_types: [input, input]\n"},
		{"duplicate not_applicable_token_type", "schema_version: 1\nusage:\n  live:\n    source: native\n    not_applicable_token_types: [cache_write, cache_write]\n"},
		{"overlapping token_types and not_applicable", "schema_version: 1\nusage:\n  live:\n    source: native\n    token_types: [input, cache_write]\n    not_applicable_token_types: [cache_write]\n"},
		{"source none with token_types", "schema_version: 1\nusage:\n  live:\n    source: none\n    token_types: [input]\n"},
		{"source none with counts_failed_calls", "schema_version: 1\nusage:\n  live:\n    source: none\n    counts_failed_calls: true\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseUsageSidecar([]byte(tt.yaml))
			if err == nil {
				t.Errorf("ParseUsageSidecar() = %+v, want error", got)
			}
		})
	}
}

func TestLoadHarnessConfigDir_UsageSidecar(t *testing.T) {
	baseConfig := "harness: claude\nimage: scion-claude:latest\nuser: scion\n"

	t.Run("missing usage.yaml yields nil capability", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(baseConfig), 0o644); err != nil {
			t.Fatal(err)
		}
		hc, err := LoadHarnessConfigDir(dir)
		if err != nil {
			t.Fatalf("LoadHarnessConfigDir: %v", err)
		}
		if hc.Config.UsageSidecar != nil {
			t.Errorf("UsageSidecar = %+v, want nil", hc.Config.UsageSidecar)
		}
	})

	t.Run("valid usage.yaml populates UsageSidecar", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(baseConfig), 0o644); err != nil {
			t.Fatal(err)
		}
		sidecar := "schema_version: 1\nusage:\n  live:\n    source: native\n    token_types: [input, output, cache_read, cache_write]\n    counts_failed_calls: true\n"
		if err := os.WriteFile(filepath.Join(dir, UsageSidecarFile), []byte(sidecar), 0o644); err != nil {
			t.Fatal(err)
		}
		hc, err := LoadHarnessConfigDir(dir)
		if err != nil {
			t.Fatalf("LoadHarnessConfigDir: %v", err)
		}
		if hc.Config.UsageSidecar == nil {
			t.Fatal("UsageSidecar = nil, want populated")
		}
		if hc.Config.UsageSidecar.Live.Source != api.UsageLiveSourceNative {
			t.Errorf("Source = %q, want native", hc.Config.UsageSidecar.Live.Source)
		}
	})

	t.Run("invalid or future source usage.yaml degrades to nil without failing load", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(baseConfig), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, UsageSidecarFile), []byte("schema_version: 1\nusage:\n  live:\n    source: future_v2_source\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		hc, err := LoadHarnessConfigDir(dir)
		if err != nil {
			t.Fatalf("LoadHarnessConfigDir failed on invalid usage.yaml: %v", err)
		}
		if hc.Config.UsageSidecar != nil {
			t.Errorf("UsageSidecar = %+v, want nil", hc.Config.UsageSidecar)
		}
	})

	t.Run("unreadable usage.yaml degrades to nil without failing load", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(baseConfig), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(dir, UsageSidecarFile), 0o755); err != nil {
			t.Fatal(err)
		}
		hc, err := LoadHarnessConfigDir(dir)
		if err != nil {
			t.Fatalf("LoadHarnessConfigDir failed on directory usage.yaml: %v", err)
		}
		if hc.Config.UsageSidecar != nil {
			t.Errorf("UsageSidecar = %+v, want nil", hc.Config.UsageSidecar)
		}
	})
}

func TestSeedHarnessConfigFromDir_UsageSidecarAtRoot(t *testing.T) {
	target := t.TempDir()
	if err := SeedHarnessConfigFromDir(target, harnesses.FS, "claude", false); err != nil {
		t.Fatalf("SeedHarnessConfigFromDir: %v", err)
	}
	bundled, err := fs.ReadFile(harnesses.FS, "claude/"+UsageSidecarFile)
	if err != nil {
		t.Fatalf("read bundled %s: %v", UsageSidecarFile, err)
	}
	got, err := os.ReadFile(filepath.Join(target, UsageSidecarFile))
	if err != nil {
		t.Fatalf("read seeded %s: %v", UsageSidecarFile, err)
	}
	if string(got) != string(bundled) {
		t.Errorf("seeded %s = %q, want %q", UsageSidecarFile, got, bundled)
	}
	if _, err := os.Stat(filepath.Join(target, "home", ".claude", UsageSidecarFile)); !os.IsNotExist(err) {
		t.Errorf("%s leaked into home/.claude/: err=%v", UsageSidecarFile, err)
	}

	// Non-force re-seed refreshes a stale usage.yaml.
	if err := os.WriteFile(filepath.Join(target, UsageSidecarFile), []byte("schema_version: 1\nusage:\n  live:\n    source: none\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SeedHarnessConfigFromDir(target, harnesses.FS, "claude", false); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	got, err = os.ReadFile(filepath.Join(target, UsageSidecarFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(bundled) {
		t.Errorf("re-seeded %s was not refreshed: got %q, want %q", UsageSidecarFile, got, bundled)
	}
}

func TestUpgradeHarnessConfig_UsageSidecar(t *testing.T) {
	const bundled = "schema_version: 1\nusage:\n  live:\n    source: native\n    token_types: [input, output, cache_read, cache_write]\n    counts_failed_calls: true\n"
	for _, tt := range []struct {
		name     string
		existing string
		action   string
	}{
		{"missing", "", "add_file"},
		{"stale", "schema_version: 1\nusage:\n  live:\n    source: none\n", "refresh_file"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			existing := map[string]string{"config.yaml": upgradeFixtureConfig}
			if tt.existing != "" {
				existing[UsageSidecarFile] = tt.existing
			}
			hcDir, harnessesFS := upgradeFixture(t, "myh", existing)
			harnessesFS["myh/"+UsageSidecarFile] = &fstest.MapFile{Data: []byte(bundled)}
			plan, err := UpgradeHarnessConfig(hcDir, &MockHarness{NameVal: "myh"}, HarnessConfigUpgradeOptions{
				Now:         fixedTime,
				HarnessesFS: harnessesFS,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := actionsOfType(plan, tt.action)[UsageSidecarFile]; !ok {
				t.Errorf("no %s action for %s: %+v", tt.action, UsageSidecarFile, plan.Actions)
			}
			if tt.existing != "" && len(plan.Backups) == 0 {
				t.Error("stale usage.yaml replaced without a backup")
			}
			assertFileContents(t, hcDir, map[string]string{UsageSidecarFile: bundled})
		})
	}
}
