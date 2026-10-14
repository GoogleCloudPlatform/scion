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
	"reflect"
	"strings"
	"testing"
)

// Tests for ptone/scion#4975: unknown top-level harness-config keys are
// tolerated (reported, not fatal) so a binary can load a config.yaml written
// for a newer Scion version, and validation errors name the leaf cause.

func writeCompatHarnessConfig(t *testing.T, yaml string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "hc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestValidateHarnessConfig_UnknownTopLevelKeys(t *testing.T) {
	cases := map[string]struct {
		doc  string
		want []string
	}{
		"none":     {"harness: claude\nimage: x:1\n", nil},
		"one":      {"harness: claude\nfuture_block:\n  enabled: true\n", []string{"future_block"}},
		"two":      {"harness: claude\nzeta: 1\nalpha: {any: thing}\n", []string{"alpha", "zeta"}},
		"bad type": {"harness: claude\nnewer: [1, 2]\n", []string{"newer"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			errs, unknown, err := ValidateHarnessConfig([]byte(tc.doc))
			if err != nil {
				t.Fatalf("ValidateHarnessConfig: %v", err)
			}
			if len(errs) != 0 {
				t.Fatalf("expected no errors, got %v", errs)
			}
			if !reflect.DeepEqual(unknown, tc.want) {
				t.Fatalf("unknown keys = %v, want %v", unknown, tc.want)
			}
		})
	}
}

func TestValidateHarnessConfig_UnknownKeyInKnownBlockIsFatal(t *testing.T) {
	cases := map[string]struct {
		doc, path, field string
	}{
		"auth":     {"harness: claude\nauth:\n  bogus: 1\n", "auth", "bogus"},
		"ready":    {"harness: claude\nready:\n  bogus: 1\n", "ready", "bogus"},
		"thinking": {"harness: claude\nthinking:\n  levels:\n    - {max: 100, value: high, flag: x}\n", "thinking/levels/0", "flag"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			errs, unknown, err := ValidateHarnessConfig([]byte(tc.doc))
			if err != nil {
				t.Fatalf("ValidateHarnessConfig: %v", err)
			}
			if len(unknown) != 0 {
				t.Fatalf("nested key must not be reported as top-level unknown: %v", unknown)
			}
			if len(errs) != 1 {
				t.Fatalf("expected exactly one leaf error, got %v", errs)
			}
			if errs[0].Path != tc.path {
				t.Errorf("path = %q, want %q", errs[0].Path, tc.path)
			}
			if !strings.Contains(errs[0].Message, "'"+tc.field+"'") || !strings.Contains(errs[0].Message, "not allowed") {
				t.Errorf("message %q does not name the leaf cause for %q", errs[0].Message, tc.field)
			}
		})
	}
}

func TestValidateHarnessConfig_TypedErrorWithUnknownRootKey(t *testing.T) {
	errs, unknown, err := ValidateHarnessConfig([]byte("harness: claude\nimage: 42\nfuture: true\n"))
	if err != nil {
		t.Fatalf("ValidateHarnessConfig: %v", err)
	}
	if !reflect.DeepEqual(unknown, []string{"future"}) {
		t.Errorf("unknown keys = %v, want [future]", unknown)
	}
	if len(errs) != 1 || errs[0].Path != "image" {
		t.Fatalf("expected one error at path image, got %v", errs)
	}
	if strings.Contains(errs[0].Message, "validation failed") {
		t.Errorf("message is not the leaf cause: %q", errs[0].Message)
	}
}

func TestValidateHarnessConfig_NonMapDocumentStillRejected(t *testing.T) {
	errs, unknown, err := ValidateHarnessConfig([]byte("- harness\n- claude\n"))
	if err != nil {
		t.Fatalf("ValidateHarnessConfig: %v", err)
	}
	if len(unknown) != 0 {
		t.Errorf("unknown keys = %v, want none", unknown)
	}
	if len(errs) == 0 {
		t.Fatal("expected a list document to be rejected")
	}
}

func TestLoadHarnessConfigDir_UnknownTopLevelKeysWarn(t *testing.T) {
	dir := writeCompatHarnessConfig(t, "harness: claude\nimage: scion-claude:test\nready2:\n  x: 1\nbeta: true\n")
	hcDir, err := LoadHarnessConfigDir(dir)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir: %v", err)
	}
	if hcDir.Config.Harness != "claude" || hcDir.Config.Image != "scion-claude:test" {
		t.Errorf("known fields not parsed: %+v", hcDir.Config)
	}
	want := []string{"config.yaml: unknown top-level field(s) beta, ready2 ignored (typo, or written for a newer Scion version)"}
	if !reflect.DeepEqual(hcDir.Warnings, want) {
		t.Errorf("warnings = %q, want %q", hcDir.Warnings, want)
	}

	clean := writeCompatHarnessConfig(t, "harness: claude\n")
	cleanDir, err := LoadHarnessConfigDir(clean)
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir(clean): %v", err)
	}
	if len(cleanDir.Warnings) != 0 {
		t.Errorf("clean config warnings = %v, want none", cleanDir.Warnings)
	}
}

func TestLoadHarnessConfigDir_ErrorNamesLeafCause(t *testing.T) {
	cases := map[string]struct {
		doc  string
		want []string
	}{
		"nested unknown":  {"harness: claude\nauth:\n  bogus: 1\n", []string{"auth: ", "'bogus'"}},
		"two errors":      {"harness: claude\nimage: 42\nauth:\n  bogus: 1\n", []string{"auth: ", "'bogus'", "; image: "}},
		"missing harness": {"image: x\n", []string{"'harness'"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadHarnessConfigDir(writeCompatHarnessConfig(t, tc.doc))
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := err.Error()
			if strings.Contains(msg, "validation failed") {
				t.Errorf("error hides the leaf cause: %q", msg)
			}
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error %q does not contain %q", msg, w)
				}
			}
		})
	}
}

// TestLoadHarnessConfigDir_OlderBinaryKnownKeys simulates a binary built
// before ready/delivery were added to the schema (the production break in
// ptone/scion#4975): the current claude config.yaml must still load, with
// one warning naming both keys.
func TestLoadHarnessConfigDir_OlderBinaryKnownKeys(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "harnesses", "claude", "config.yaml"))
	if err != nil {
		t.Fatalf("read claude config.yaml: %v", err)
	}
	known, err := harnessConfigKnownKeys()
	if err != nil {
		t.Fatal(err)
	}
	older := make(map[string]struct{}, len(known))
	for k := range known {
		if k != "ready" && k != "delivery" {
			older[k] = struct{}{}
		}
	}
	harnessConfigKnownKeysOverride = older
	t.Cleanup(func() { harnessConfigKnownKeysOverride = nil })

	hcDir, err := LoadHarnessConfigDir(writeCompatHarnessConfig(t, string(data)))
	if err != nil {
		t.Fatalf("LoadHarnessConfigDir: %v", err)
	}
	if len(hcDir.Warnings) != 1 || !strings.Contains(hcDir.Warnings[0], "delivery, ready") {
		t.Fatalf("warnings = %q, want one naming delivery, ready", hcDir.Warnings)
	}
	if hcDir.Config.Harness != "claude" {
		t.Errorf("harness = %q, want claude", hcDir.Config.Harness)
	}
}

func TestResolveHarnessConfigDir_HydratedUnknownKeyLoads(t *testing.T) {
	dir := writeCompatHarnessConfig(t, "harness: claude\nfuture_block: {on: true}\n")
	hcDir, err := ResolveHarnessConfigDir(dir, "hc", "")
	if err != nil {
		t.Fatalf("ResolveHarnessConfigDir: %v", err)
	}
	if hcDir.Source != HarnessConfigSourceHubHydrated {
		t.Errorf("source = %q, want %q", hcDir.Source, HarnessConfigSourceHubHydrated)
	}
	if len(hcDir.Warnings) != 1 || !strings.Contains(hcDir.Warnings[0], "future_block") {
		t.Errorf("warnings = %q, want one naming future_block", hcDir.Warnings)
	}
}

func TestHarnessConfigKnownKeys_FromEmbeddedSchema(t *testing.T) {
	known, err := harnessConfigKnownKeys()
	if err != nil {
		t.Fatalf("harnessConfigKnownKeys: %v", err)
	}
	for _, k := range []string{"harness", "image", "auth", "ready", "delivery", "thinking"} {
		if _, ok := known[k]; !ok {
			t.Errorf("known keys missing %q", k)
		}
	}
}

// TestValidateSettings_LeafErrorNamesProperty covers the shared error helper:
// an unknown key in a closed object reached through a $ref used to surface
// as "<path>: validation failed"; the leaf cause names the property.
func TestValidateSettings_LeafErrorNamesProperty(t *testing.T) {
	errs, err := ValidateSettings([]byte("schema_version: \"1\"\nserver:\n  maintenance:\n    unknown_field: x\n"), "1")
	if err != nil {
		t.Fatalf("ValidateSettings: %v", err)
	}
	if len(errs) == 0 {
		t.Fatal("expected a validation error")
	}
	for _, e := range errs {
		if e.Message == "validation failed" {
			t.Errorf("non-leaf error reported: %v", e)
		}
	}
	found := false
	for _, e := range errs {
		if e.Path == "server/maintenance" && strings.Contains(e.Message, "'unknown_field'") {
			found = true
		}
	}
	if !found {
		t.Errorf("no leaf error names unknown_field at server/maintenance: %v", errs)
	}
}

// TestInstancePath pins the JSON-pointer rendering of leaf error paths:
// no leading slash, "/" between tokens, and "~" and "/" inside a token
// escaped as "~0" and "~1" (RFC 6901).
func TestInstancePath(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"hub", "endpoint"}, "hub/endpoint"},
		{[]string{"runtimes", "a/b", "c~d"}, "runtimes/a~1b/c~0d"},
		{[]string{"~/"}, "~0~1"},
	}
	for _, tc := range cases {
		if got := instancePath(tc.in); got != tc.want {
			t.Errorf("instancePath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
