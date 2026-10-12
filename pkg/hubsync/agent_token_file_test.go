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

package hubsync

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// readAgentTokenFileBounded runs ReadAgentTokenFile with a timeout, so a
// reader that blocks (for example on a FIFO) fails the test instead of
// hanging it. It returns the token and any warning text.
func readAgentTokenFileBounded(t *testing.T) (string, string) {
	t.Helper()
	var warnings bytes.Buffer
	orig := agentTokenFileWarnings
	agentTokenFileWarnings = &warnings
	t.Cleanup(func() { agentTokenFileWarnings = orig })

	done := make(chan string, 1)
	go func() { done <- ReadAgentTokenFile() }()
	select {
	case got := <-done:
		return got, warnings.String()
	case <-time.After(5 * time.Second):
		t.Fatal("ReadAgentTokenFile blocked")
		return "", ""
	}
}

// agentHome creates a temporary HOME with an empty .scion directory and
// returns the home path.
func agentHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Setenv("HOME", home)
	return home
}

func TestReadAgentTokenFile_RegularFile(t *testing.T) {
	home := agentHome(t)
	if err := os.WriteFile(filepath.Join(home, ".scion", "scion-token"), []byte("file-token\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, warn := readAgentTokenFileBounded(t)
	if got != "file-token" {
		t.Errorf("got %q, want %q", got, "file-token")
	}
	if warn != "" {
		t.Errorf("unexpected warning: %q", warn)
	}
}

func TestReadAgentTokenFile_AbsentIsQuiet(t *testing.T) {
	agentHome(t)
	got, warn := readAgentTokenFileBounded(t)
	if got != "" || warn != "" {
		t.Errorf("got %q, warning %q; want both empty", got, warn)
	}
}

func TestReadAgentTokenFile_NoHome(t *testing.T) {
	t.Setenv("HOME", "")
	if got, _ := readAgentTokenFileBounded(t); got != "" {
		t.Errorf("got %q with HOME unset, want empty", got)
	}
}

// TestReadAgentTokenFile_LinkedHomePrefix covers a host whose home
// directory is reached through a linked prefix (e.g. /home -> /var/home).
func TestReadAgentTokenFile_LinkedHomePrefix(t *testing.T) {
	root := t.TempDir()
	realHome := filepath.Join(root, "var", "home", "user")
	if err := os.MkdirAll(filepath.Join(realHome, ".scion"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realHome, ".scion", "scion-token"), []byte("linked-home-token"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "var", "home"), filepath.Join(root, "home")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	t.Setenv("HOME", filepath.Join(root, "home", "user"))

	if got, warn := readAgentTokenFileBounded(t); got != "linked-home-token" {
		t.Errorf("got %q (warning %q), want %q", got, warn, "linked-home-token")
	}
}

func TestReadAgentTokenFile_RefusesFIFO(t *testing.T) {
	home := agentHome(t)
	if err := syscall.Mkfifo(filepath.Join(home, ".scion", "scion-token"), 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	got, warn := readAgentTokenFileBounded(t)
	if got != "" {
		t.Errorf("got %q from a FIFO, want empty", got)
	}
	if warn == "" {
		t.Error("expected a warning for a refused token file")
	}
}

func TestReadAgentTokenFile_RefusesLinkedFile(t *testing.T) {
	home := agentHome(t)
	other := filepath.Join(t.TempDir(), "other")
	if err := os.WriteFile(other, []byte("other-content"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(other, filepath.Join(home, ".scion", "scion-token")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	got, warn := readAgentTokenFileBounded(t)
	if got != "" {
		t.Errorf("got %q through a linked token file, want empty", got)
	}
	if warn == "" {
		t.Error("expected a warning for a refused token file")
	}
}

func TestReadAgentTokenFile_RefusesLinkedScionDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "scion-token"), []byte("other-token"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Symlink(other, filepath.Join(home, ".scion")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if got, _ := readAgentTokenFileBounded(t); got != "" {
		t.Errorf("got %q through a linked .scion directory, want empty", got)
	}
}

func TestReadAgentTokenFile_RefusesOversizedFileWithWarning(t *testing.T) {
	home := agentHome(t)
	big := strings.Repeat("a", 8193)
	if err := os.WriteFile(filepath.Join(home, ".scion", "scion-token"), []byte(big), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, warn := readAgentTokenFileBounded(t)
	if got != "" {
		t.Errorf("got %d bytes from an oversized file, want empty", len(got))
	}
	if !strings.Contains(warn, "size limit") {
		t.Errorf("warning %q does not mention the size limit", warn)
	}
}
