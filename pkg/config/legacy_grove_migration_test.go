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
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// fakeReporter records every Reporter call for assertions.
type fakeReporter struct {
	envIgnored []struct{ name, replacement string }
	migrated   int
	conflicts  int
	skipped    int
}

func (f *fakeReporter) Migrated(old, new string, tracked bool) { f.migrated++ }
func (f *fakeReporter) Conflict(old, new, detail string)       { f.conflicts++ }
func (f *fakeReporter) Skipped(old, reason, manual string)     { f.skipped++ }
func (f *fakeReporter) EnvIgnored(name, replacement string) {
	f.envIgnored = append(f.envIgnored, struct{ name, replacement string }{name, replacement})
}

func TestWarnRemovedLegacyEnv(t *testing.T) {
	t.Run("reports SCION_HUB_GROVE_ID when set", func(t *testing.T) {
		env := map[string]string{"SCION_HUB_GROVE_ID": "some-uuid"}
		r := &fakeReporter{}

		WarnRemovedLegacyEnv(func(k string) string { return env[k] }, r)

		if len(r.envIgnored) != 1 {
			t.Fatalf("EnvIgnored called %d times, want 1", len(r.envIgnored))
		}
		if got := r.envIgnored[0]; got.name != "SCION_HUB_GROVE_ID" || got.replacement != "SCION_HUB_PROJECT_ID" {
			t.Fatalf("EnvIgnored(%q, %q), want (SCION_HUB_GROVE_ID, SCION_HUB_PROJECT_ID)", got.name, got.replacement)
		}
		if r.migrated != 0 || r.conflicts != 0 || r.skipped != 0 {
			t.Fatalf("unexpected non-env reports: migrated=%d conflicts=%d skipped=%d", r.migrated, r.conflicts, r.skipped)
		}
	})

	t.Run("silent when unset", func(t *testing.T) {
		r := &fakeReporter{}

		WarnRemovedLegacyEnv(func(string) string { return "" }, r)

		if len(r.envIgnored) != 0 {
			t.Fatalf("EnvIgnored called %d times, want 0", len(r.envIgnored))
		}
	})

	t.Run("nil getenv or report is a no-op", func(t *testing.T) {
		WarnRemovedLegacyEnv(nil, &fakeReporter{})
		WarnRemovedLegacyEnv(func(string) string { return "x" }, nil)
	})
}

// TestWarnRemovedLegacyEnvOnce guards the shared boot-hook Once: hub and
// broker both call WarnRemovedLegacyEnvOnce in the same process
// (`scion server start --enable-hub --enable-runtime-broker`), and it must
// report exactly once total, not once per caller.
func TestWarnRemovedLegacyEnvOnce(t *testing.T) {
	warnRemovedLegacyEnvOnce = sync.Once{}
	t.Cleanup(func() { warnRemovedLegacyEnvOnce = sync.Once{} })

	env := map[string]string{"SCION_HUB_GROVE_ID": "some-uuid"}
	getenv := func(k string) string { return env[k] }
	r := &fakeReporter{}

	WarnRemovedLegacyEnvOnce(getenv, r) // hub's call
	WarnRemovedLegacyEnvOnce(getenv, r) // broker's call, same process

	if len(r.envIgnored) != 1 {
		t.Fatalf("EnvIgnored called %d times across two WarnRemovedLegacyEnvOnce calls, want 1", len(r.envIgnored))
	}
}

// TestSlogReporter captures records through a real slog.JSONHandler (not
// just method calls) so it can assert the right level per event and no
// duplicate keys: the reporter must not add a second "component" key on top
// of the one hub/broker already set on slog.Default().
func TestSlogReporter(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	r := NewSlogReporter()
	r.Migrated("old", "new", true)
	r.Conflict("old", "new", "detail")
	r.Skipped("old", "reason", "manual")
	r.EnvIgnored("SCION_HUB_GROVE_ID", "SCION_HUB_PROJECT_ID")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d log records, want 4:\n%s", len(lines), buf.String())
	}

	wantLevels := []string{"INFO", "WARN", "WARN", "WARN"}
	for i, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("record %d: invalid JSON: %v\n%s", i, err, line)
		}
		if got, _ := rec["level"].(string); got != wantLevels[i] {
			t.Errorf("record %d: level = %q, want %q", i, got, wantLevels[i])
		}
		if got, _ := rec["subsystem"].(string); got != "layout-migration" {
			t.Errorf("record %d: subsystem = %q, want %q", i, got, "layout-migration")
		}
		// Checked against the raw JSON text, not the decoded map: a
		// duplicate key collapses silently under json.Unmarshal (last
		// value wins), which is exactly how the earlier bug stayed
		// invisible to a naive test.
		if n := strings.Count(line, `"subsystem"`); n != 1 {
			t.Errorf("record %d: %q key appears %d times, want 1:\n%s", i, "subsystem", n, line)
		}
		if n := strings.Count(line, `"component"`); n > 1 {
			t.Errorf("record %d: %q key appears %d times, want at most 1:\n%s", i, "component", n, line)
		}
	}
}
