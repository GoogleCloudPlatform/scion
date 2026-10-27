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

package keyring

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLoadFileRefusesNonRegularBeforeOpen(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := LoadFile(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("LoadFile(fifo) = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadFile blocked on a FIFO")
	}
}
