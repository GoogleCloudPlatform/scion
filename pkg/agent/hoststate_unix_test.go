// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build unix

package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agentinfolock"
)

func TestHostStateFIFOAtPathErrors(t *testing.T) {
	a := newHostStateAgent(t)
	if err := syscall.Mkfifo(a.path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	// O_NONBLOCK keeps the open from blocking; the regular-file check
	// then refuses the FIFO.
	if _, err := ReadHostState(a.root, a.name); err == nil {
		t.Fatal("ReadHostState: want error for a FIFO at the path")
	}
	if err := UpdateHostState(a.root, a.name, func(hs *HostState) error { hs.RunID = "x"; return nil }); err == nil {
		t.Fatal("UpdateHostState: want error for a FIFO at the path")
	}
}

// TestHostStateSymlinkAboveRootAccepted reaches the trusted root through
// an ordinary symlink (the macOS /var case). The update works, the file
// lands in the real agentDir, and the lock is taken there (its lock file
// exists), not skipped.
func TestHostStateSymlinkAboveRootAccepted(t *testing.T) {
	base := t.TempDir()
	realAgent := newHostStateAgentIn(t, filepath.Join(base, "real"), "agent-1")
	linkRoot := filepath.Join(base, "link")
	if err := os.Symlink(realAgent.root, linkRoot); err != nil {
		t.Fatal(err)
	}
	if err := UpdateHostState(linkRoot, "agent-1", func(hs *HostState) error { hs.RunID = "r1"; return nil }); err != nil {
		t.Fatalf("UpdateHostState through a symlink above the root: %v", err)
	}
	hs, err := ReadHostState(linkRoot, "agent-1")
	if err != nil || hs.RunID != "r1" {
		t.Fatalf("ReadHostState: hs=%+v err=%v", hs, err)
	}
	fi, err := os.Lstat(realAgent.path)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file in real agentDir: fi=%v err=%v", fi, err)
	}
	if _, err := os.Lstat(agentinfolock.LockPath(realAgent.path)); err != nil {
		t.Fatalf("lock file not created in real agentDir: %v", err)
	}
}

// TestHostStateSymlinkBelowRootRefused puts a symlink at each component
// below the trusted root (.scion, agents, the agent name). The read and the
// update are refused, mutate is not called, and nothing is written in the
// directory the symlink points to.
func TestHostStateSymlinkBelowRootRefused(t *testing.T) {
	for _, level := range []string{".scion", "agents", "agent-1"} {
		t.Run(level, func(t *testing.T) {
			root := t.TempDir()
			outside := newHostStateAgentIn(t, t.TempDir(), "agent-1")
			var linkAt, target string
			switch level {
			case ".scion":
				linkAt, target = filepath.Join(root, ".scion"), filepath.Join(outside.root, ".scion")
			case "agents":
				linkAt, target = filepath.Join(root, ".scion", "agents"), filepath.Join(outside.root, ".scion", "agents")
			default:
				linkAt, target = filepath.Join(root, ".scion", "agents", "agent-1"), outside.dir
			}
			if err := os.MkdirAll(filepath.Dir(linkAt), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, linkAt); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadHostState(root, "agent-1"); err == nil {
				t.Fatal("ReadHostState: want error through a symlink below the root")
			}
			called := false
			err := UpdateHostState(root, "agent-1", func(hs *HostState) error { called = true; hs.RunID = "x"; return nil })
			if err == nil || called {
				t.Fatalf("UpdateHostState: err=%v called=%v, want error and no mutate", err, called)
			}
			entries, err := os.ReadDir(outside.dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("written through the symlink: %v", entries)
			}
		})
	}
}

// TestHostStateUnlockedParallelWritesStayWhole makes the lock unusable (a
// directory at the lock path), so every update runs unlocked. Updates may
// then be lost, but each write is atomic with its own random temp name:
// no update fails, the file always parses, and no temp file is left.
func TestHostStateUnlockedParallelWritesStayWhole(t *testing.T) {
	a := newHostStateAgent(t)
	if err := os.Mkdir(agentinfolock.LockPath(a.path), 0o700); err != nil {
		t.Fatal(err)
	}
	const workers, rounds = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, workers*rounds)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				key := fmt.Sprintf("k-%d-%d", w, r)
				errs <- UpdateHostState(a.root, a.name, func(hs *HostState) error {
					if hs.unknown == nil {
						hs.unknown = map[string]json.RawMessage{}
					}
					hs.unknown[key] = json.RawMessage("1")
					return nil
				})
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("UpdateHostState: %v", err)
		}
	}
	raw := readRawHostState(t, a.path)
	if len(raw) == 0 {
		t.Fatal("no keys written")
	}
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}
