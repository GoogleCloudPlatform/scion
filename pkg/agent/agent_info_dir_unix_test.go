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

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestOpenAgentInfoDirRefusesNonDirectory checks that the dir-sync open
// fails fast with ENOTDIR on a FIFO or a regular file rather than blocking
// or opening it.
func TestOpenAgentInfoDirRefusesNonDirectory(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{fifo, regular} {
		done := make(chan error, 1)
		go func() {
			f, err := openAgentInfoDir(p)
			if err == nil {
				_ = f.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, syscall.ENOTDIR) {
				t.Errorf("openAgentInfoDir(%s) error = %v, want ENOTDIR", filepath.Base(p), err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("openAgentInfoDir(%s) blocked", filepath.Base(p))
		}
	}

	f, err := openAgentInfoDir(dir)
	if err != nil {
		t.Fatalf("openAgentInfoDir(dir) error = %v, want nil", err)
	}
	_ = f.Close()
}
