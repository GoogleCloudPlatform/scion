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
	"strings"
	"testing"
)

func TestComputeHarnessConfigRevision(t *testing.T) {
	t.Run("empty path returns empty", func(t *testing.T) {
		if got := ComputeHarnessConfigRevision(""); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("nonexistent dir returns empty", func(t *testing.T) {
		if got := ComputeHarnessConfigRevision(filepath.Join(t.TempDir(), "missing")); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("empty dir returns empty", func(t *testing.T) {
		dir := t.TempDir()
		if got := ComputeHarnessConfigRevision(dir); got != "" {
			t.Errorf("expected empty, got %q", got)
		}
	})

	t.Run("hashes content deterministically", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "config.yaml"), "harness: claude\n")
		writeFile(t, filepath.Join(dir, "home", ".bashrc"), "echo hi\n")

		first := ComputeHarnessConfigRevision(dir)
		second := ComputeHarnessConfigRevision(dir)
		if first == "" {
			t.Fatal("expected non-empty hash")
		}
		if first != second {
			t.Errorf("hash not deterministic: %q vs %q", first, second)
		}
		if !strings.HasPrefix(first, "sha256:") {
			t.Errorf("missing sha256 prefix: %q", first)
		}
	})

	t.Run("file content change updates hash", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "config.yaml"), "harness: claude\n")
		before := ComputeHarnessConfigRevision(dir)

		writeFile(t, filepath.Join(dir, "config.yaml"), "harness: gemini\n")
		after := ComputeHarnessConfigRevision(dir)
		if before == after {
			t.Error("hash did not change when file content changed")
		}
	})

	t.Run("file rename updates hash", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "a.txt"), "data")
		before := ComputeHarnessConfigRevision(dir)

		if err := os.Rename(filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")); err != nil {
			t.Fatal(err)
		}
		after := ComputeHarnessConfigRevision(dir)
		if before == after {
			t.Error("hash did not change when file path changed")
		}
	})

	t.Run("nested files contribute to hash", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "config.yaml"), "harness: claude\n")
		before := ComputeHarnessConfigRevision(dir)

		writeFile(t, filepath.Join(dir, "examples", "ex.yaml"), "x: 1\n")
		after := ComputeHarnessConfigRevision(dir)
		if before == after {
			t.Error("expected nested file to change hash")
		}
	})
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
