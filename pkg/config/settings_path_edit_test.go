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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	yamlv3 "gopkg.in/yaml.v3"
)

const pathEditFixture = `# operator notes
schema_version: "1"
server:
  # hub listener
  hub:
    port: 9810 # keep
    admin_emails:
      - a@example.com
  broker:
    enabled: true
    broker_id: b-1
    broker_token: tok
unknown_top: kept
`

func writePathEditFixture(t *testing.T, content string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func TestPrepareSettingsPathEdits_InPlace(t *testing.T) {
	dir, path := writePathEditFixture(t, pathEditFixture)
	unlock := LockSettingsFile()
	staged, err := PrepareSettingsPathEdits(dir, []SettingsPathEdit{
		{Path: []string{"server", "hub", "port"}, Value: 9999},
		{Path: []string{"server", "broker", "enabled"}, Delete: true},
		{Path: []string{"server", "log_level"}, Value: "debug"},
		{Path: []string{"server", "broker", "broker_id"}, Value: "b-1"}, // unchanged
		{Path: []string{"server", "storage", "bucket"}, Delete: true},   // absent
	})
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	want := []string{"server.hub.port", "server.broker.enabled", "server.log_level"}
	if fmt.Sprint(staged.Changed) != fmt.Sprint(want) {
		t.Errorf("Changed = %v, want %v", staged.Changed, want)
	}
	if err := staged.Commit(); err != nil {
		unlock()
		t.Fatal(err)
	}
	unlock()

	data, _ := os.ReadFile(path)
	out := string(data)
	for _, s := range []string{"# operator notes", "# hub listener", "port: 9999 # keep", "unknown_top: kept", "broker_token: tok", "log_level: debug"} {
		if !strings.Contains(out, s) {
			t.Errorf("output missing %q:\n%s", s, out)
		}
	}
	if strings.Contains(out, "enabled: true") {
		t.Errorf("deleted key still present:\n%s", out)
	}
	// Key order kept: hub before broker, as in the input.
	if strings.Index(out, "hub:") > strings.Index(out, "broker:") {
		t.Errorf("key order changed:\n%s", out)
	}
}

func TestPrepareSettingsPathEdits_NoOpWritesNothing(t *testing.T) {
	dir, path := writePathEditFixture(t, pathEditFixture)
	before, _ := os.Stat(path)
	time.Sleep(10 * time.Millisecond)
	unlock := LockSettingsFile()
	staged, err := PrepareSettingsPathEdits(dir, []SettingsPathEdit{
		{Path: []string{"server", "hub", "port"}, Value: 9810},
		{Path: []string{"server", "hub", "admin_emails"}, Value: []string{"a@example.com"}},
		{Path: []string{"server", "missing"}, Delete: true},
	})
	if err == nil {
		err = staged.Commit()
	}
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(staged.Changed) != 0 {
		t.Errorf("Changed = %v, want none", staged.Changed)
	}
	after, _ := os.Stat(path)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("a no-op edit rewrote the file")
	}
}

func TestPrepareSettingsPathEdits_RefusesAlias(t *testing.T) {
	dir, _ := writePathEditFixture(t, "schema_version: \"1\"\nbase: &b\n  port: 1\nserver:\n  hub: *b\n")
	unlock := LockSettingsFile()
	defer unlock()
	_, err := PrepareSettingsPathEdits(dir, []SettingsPathEdit{{Path: []string{"server", "hub", "port"}, Value: 2}})
	if !errors.Is(err, ErrSettingsPathEditUnsupported) {
		t.Errorf("err = %v, want ErrSettingsPathEditUnsupported", err)
	}
}

func TestPrepareSettingsPathEdits_CreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	unlock := LockSettingsFile()
	staged, err := PrepareSettingsPathEdits(dir, []SettingsPathEdit{{Path: []string{"active_profile"}, Value: "local"}})
	if err == nil {
		err = staged.Commit()
	}
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]interface{}
	data, _ := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	if err := yamlv3.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m["active_profile"] != "local" || m["schema_version"] != "1" {
		t.Errorf("new file = %v", m)
	}
}

// A writer holding the lock from read to rename (as the workstation
// server-config PUT does, across its DB writes) and UpdateVersionedSetting
// (as broker registration does for hub.brokerToken) must not lose each
// other's change.
func TestSettingsFileLock_ConcurrentWritersKeepBothUpdates(t *testing.T) {
	for i := 0; i < 20; i++ {
		dir, path := writePathEditFixture(t, pathEditFixture)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		wg.Add(2)
		go func() {
			defer wg.Done()
			unlock := LockSettingsFile()
			defer unlock()
			staged, err := PrepareSettingsPathEdits(dir, []SettingsPathEdit{{Path: []string{"server", "hub", "port"}, Value: 9999}})
			if err != nil {
				errs <- err
				return
			}
			time.Sleep(5 * time.Millisecond) // the DB writes happen here
			errs <- staged.Commit()
		}()
		go func() {
			defer wg.Done()
			errs <- UpdateVersionedSetting(dir, "hub.brokerToken", fmt.Sprintf("tok-%d", i))
		}()
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		data, _ := os.ReadFile(path)
		out := string(data)
		if !strings.Contains(out, "port: 9999") || !strings.Contains(out, fmt.Sprintf("broker_token: tok-%d", i)) {
			t.Fatalf("iteration %d lost an update:\n%s", i, out)
		}
	}
}
