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

//go:build unix

package transfer

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// within fails the test if fn does not return within a few seconds, which
// is how a blocking open of a FIFO would show up.
func within(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("call blocked on a FIFO")
	}
}

// TestReadOnlyFlags_FIFOOpenDoesNotBlock pins that the read-only open used
// by ReadCollectedFile returns at once for a FIFO with no writer.
func TestReadOnlyFlags_FIFOOpenDoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	within(t, func() {
		f, err := root.OpenFile("fifo", readOnlyFlags, 0)
		if err != nil {
			t.Errorf("open: %v", err)
			return
		}
		_ = f.Close()
	})
}

// TestHashFile_FIFODoesNotBlock pins that HashFile returns at once for a
// FIFO with no writer.
func TestHashFile_FIFODoesNotBlock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	within(t, func() { _, _ = HashFile(p) })
}

// TestReadCollectedFile_ReplacedByFIFO pins that a collected file replaced
// by a FIFO is refused without blocking.
func TestReadCollectedFile_ReplacedByFIFO(t *testing.T) {
	dir := t.TempDir()
	writeReadTestFile(t, dir, "a.txt", "hello\n")
	root, fi := collectOne(t, dir, "a.txt")

	p := filepath.Join(dir, "a.txt")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	within(t, func() {
		data, err := ReadCollectedFile(root, fi)
		if err == nil || !strings.Contains(err.Error(), "no longer a regular file") {
			t.Errorf("got %q, %v; want a not-regular error", data, err)
		}
	})
}
