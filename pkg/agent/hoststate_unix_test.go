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
	dir := hostStateTempDir(t)
	if err := syscall.Mkfifo(HostStatePath(dir), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	// O_NONBLOCK keeps the open from blocking; the regular-file check
	// then refuses the FIFO.
	if _, err := ReadHostState(dir); err == nil {
		t.Fatal("ReadHostState: want error for a FIFO at the path")
	}
	if err := UpdateHostState(dir, func(hs *HostState) error { hs.RunID = "x"; return nil }); err == nil {
		t.Fatal("UpdateHostState: want error for a FIFO at the path")
	}
}

// TestHostStateSymlinkedAgentDirParentRefused puts a symlink in a parent
// component of agentDir, and at agentDir itself. Both the read and the
// update are refused, and nothing is written in the real directory the
// symlink points to.
func TestHostStateSymlinkedAgentDirParentRefused(t *testing.T) {
	base := hostStateTempDir(t)
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(outside, "agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	agentLink := filepath.Join(base, "agent-link")
	if err := os.Symlink(filepath.Join(outside, "agent"), agentLink); err != nil {
		t.Fatal(err)
	}
	for name, agentDir := range map[string]string{
		"parent":   filepath.Join(link, "agent"),
		"agentDir": agentLink,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadHostState(agentDir); err == nil {
				t.Fatal("ReadHostState: want error through a symlinked path")
			}
			called := false
			err := UpdateHostState(agentDir, func(hs *HostState) error { called = true; hs.RunID = "x"; return nil })
			if err == nil || called {
				t.Fatalf("UpdateHostState: err=%v called=%v, want error and no mutate", err, called)
			}
			entries, err := os.ReadDir(filepath.Join(outside, "agent"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("written outside agentDir: %v", entries)
			}
		})
	}
}

// TestHostStateUnlockedParallelWritesStayWhole makes the lock unusable (a
// directory at the lock path), so every update runs unlocked. Updates may
// then be lost, but each write is atomic with its own random temp name:
// no update fails, the file always parses, and no temp file is left.
func TestHostStateUnlockedParallelWritesStayWhole(t *testing.T) {
	dir := hostStateTempDir(t)
	if err := os.Mkdir(agentinfolock.LockPath(HostStatePath(dir)), 0o700); err != nil {
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
				errs <- UpdateHostState(dir, func(hs *HostState) error {
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
	raw := readRawHostState(t, dir)
	if len(raw) == 0 {
		t.Fatal("no keys written")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}
