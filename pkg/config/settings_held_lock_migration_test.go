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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runWithin fails the test when f does not return within d (a held-lock
// migration that tried to take the settings lock again would wait).
func runWithin(t *testing.T, d time.Duration, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("did not return within %s", d)
		return nil
	}
}

func writeGroveIDSettings(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nhub:\n  grove_id: p1\n"), 0o644))
	return dir
}

func assertMigratedOnce(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "settings.yaml"))
	require.NoError(t, err)
	got := string(data)
	assert.NotContains(t, got, "grove_id")
	assert.Equal(t, 1, strings.Count(got, "project_id"), "one canonical key:\n%s", got)
	return got
}

// LoadModifySaveVersionedSettings holds the settings lock around its load,
// so the key migration in that load writes under the held lock: no wait,
// the file migrated, one canonical key.
func TestLoadModifySaveVersionedSettings_MigratesUnderHeldLock(t *testing.T) {
	t.Run("save", func(t *testing.T) {
		dir := writeGroveIDSettings(t)
		err := runWithin(t, 5*time.Second, func() error {
			// Runs on runWithin's goroutine: report with assert and an
			// error, never require (FailNow would end that goroutine).
			return LoadModifySaveVersionedSettings(dir, func(vs *VersionedSettings) error {
				if !assert.NotNil(t, vs.Hub) {
					return errors.New("no hub settings loaded")
				}
				assert.Equal(t, "p1", vs.Hub.ProjectID)
				vs.ActiveProfile = "dev"
				return nil
			})
		})
		require.NoError(t, err)
		got := assertMigratedOnce(t, dir)
		assert.Contains(t, got, "active_profile: dev")
	})
	t.Run("skip save", func(t *testing.T) {
		dir := writeGroveIDSettings(t)
		err := runWithin(t, 5*time.Second, func() error {
			return LoadModifySaveVersionedSettings(dir, func(*VersionedSettings) error { return ErrSkipSave })
		})
		require.NoError(t, err)
		assertMigratedOnce(t, dir)
	})
}

// setProjectTypeGit holds the settings lock for its edit, so the key
// migration it runs writes under that lock.
func TestSetProjectTypeGit_MigratesUnderHeldLock(t *testing.T) {
	dir := writeGroveIDSettings(t)
	err := runWithin(t, 5*time.Second, func() error { return setProjectTypeGit(dir) })
	require.NoError(t, err)
	got := assertMigratedOnce(t, dir)
	assert.Contains(t, got, "project_type: git")
}
