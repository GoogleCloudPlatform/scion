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

package util

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenLstatVerified(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("opens an unchanged regular file", func(t *testing.T) {
		f, err := openLstatVerified(target, os.Open)
		if err != nil {
			t.Fatalf("openLstatVerified: %v", err)
		}
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(f)
		if err != nil || string(data) != "target" {
			t.Fatalf("got %q, %v; want %q", data, err, "target")
		}
	})

	t.Run("refuses a different regular file swapped in before open", func(t *testing.T) {
		var swapped *os.File
		swap := func(string) (*os.File, error) {
			f, err := os.Open(other)
			swapped = f
			return f, err
		}
		if f, err := openLstatVerified(target, swap); err == nil {
			_ = f.Close()
			t.Fatal("got a file; want refusal")
		}
		if swapped == nil {
			t.Fatal("open step was not called")
		}
		if _, err := swapped.Stat(); err == nil {
			t.Error("swapped-in file was left open; want it closed on refusal")
		}
	})

	t.Run("refuses a non-regular file swapped in before open", func(t *testing.T) {
		swap := func(string) (*os.File, error) { return os.Open(dir) }
		if f, err := openLstatVerified(target, swap); err == nil {
			_ = f.Close()
			t.Fatal("got a file; want refusal")
		}
	})

	t.Run("refuses a directory", func(t *testing.T) {
		if f, err := openLstatVerified(dir, os.Open); err == nil {
			_ = f.Close()
			t.Fatal("got a file; want refusal")
		}
	})

	t.Run("refuses a symlink without opening it", func(t *testing.T) {
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		opened := false
		open := func(p string) (*os.File, error) { opened = true; return os.Open(p) }
		if f, err := openLstatVerified(link, open); err == nil {
			_ = f.Close()
			t.Fatal("got a file; want refusal")
		}
		if opened {
			t.Error("open step ran for a symlink; want refusal at Lstat")
		}
	})
}
