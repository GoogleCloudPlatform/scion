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

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agentinfolock"
)

func TestHostStateMissingFileIsZero(t *testing.T) {
	dir := t.TempDir()
	hs, err := ReadHostState(dir)
	if err != nil {
		t.Fatalf("ReadHostState: %v", err)
	}
	if hs.RunID != "" || hs.unknown != nil {
		t.Fatalf("got %+v, want zero", hs)
	}
}

func TestHostStateCorruptFileErrors(t *testing.T) {
	for name, content := range map[string]string{
		"garbage": "{not json",
		"array":   "[1,2]",
		"null":    "null",
		"empty":   "",
		"badtype": `{"runId": 7}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := HostStatePath(dir)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadHostState(dir); err == nil {
				t.Fatal("ReadHostState: want error for corrupt file")
			}
			called := false
			err := UpdateHostState(dir, func(*HostState) error { called = true; return nil })
			if err == nil || called {
				t.Fatalf("UpdateHostState: err=%v called=%v, want error and no mutate", err, called)
			}
			got, _ := os.ReadFile(path)
			if string(got) != content {
				t.Fatalf("corrupt file changed: %q", got)
			}
		})
	}
}

func TestHostStateUnreadableFileErrors(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a non-root unix user")
	}
	dir := t.TempDir()
	if err := os.WriteFile(HostStatePath(dir), []byte(`{"runId":"x"}`), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHostState(dir); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadHostState: err=%v, want a non-NotExist error", err)
	}
}

func TestHostStateRunIDRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := UpdateHostState(dir, func(hs *HostState) error { hs.RunID = "run-123"; return nil }); err != nil {
		t.Fatalf("UpdateHostState: %v", err)
	}
	hs, err := ReadHostState(dir)
	if err != nil {
		t.Fatalf("ReadHostState: %v", err)
	}
	if hs.RunID != "run-123" {
		t.Fatalf("RunID = %q, want run-123", hs.RunID)
	}
	if err := UpdateHostState(dir, func(hs *HostState) error { hs.RunID = ""; return nil }); err != nil {
		t.Fatalf("UpdateHostState clear: %v", err)
	}
	raw := readRawHostState(t, dir)
	if _, ok := raw["runId"]; ok {
		t.Fatalf("cleared runId still in file: %v", raw)
	}
}

func TestHostStateMutateErrorWritesNothing(t *testing.T) {
	dir := t.TempDir()
	want := errors.New("boom")
	err := UpdateHostState(dir, func(hs *HostState) error { hs.RunID = "x"; return want })
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if _, err := os.Lstat(HostStatePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file written despite mutate error: %v", err)
	}
}

func TestHostStateUnknownFieldsSurviveUpdate(t *testing.T) {
	dir := t.TempDir()
	orig := `{"runId":"old","futureKey":{"a":[1,2]},"other":"v"}`
	if err := os.WriteFile(HostStatePath(dir), []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateHostState(dir, func(hs *HostState) error { hs.RunID = "new"; return nil }); err != nil {
		t.Fatalf("UpdateHostState: %v", err)
	}
	raw := readRawHostState(t, dir)
	if string(raw["runId"]) != `"new"` {
		t.Fatalf("runId = %s, want \"new\"", raw["runId"])
	}
	if string(compactJSON(t, raw["futureKey"])) != `{"a":[1,2]}` {
		t.Fatalf("futureKey = %s", raw["futureKey"])
	}
	if string(raw["other"]) != `"v"` {
		t.Fatalf("other = %s", raw["other"])
	}
}

func TestHostStateSymlinkRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target.json")
	const content = `{"runId":"target"}`
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, HostStatePath(dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHostState(dir); !errors.Is(err, errHostStateSymlink) {
		t.Fatalf("ReadHostState: err=%v, want errHostStateSymlink", err)
	}
	err := UpdateHostState(dir, func(hs *HostState) error { hs.RunID = "x"; return nil })
	if !errors.Is(err, errHostStateSymlink) {
		t.Fatalf("UpdateHostState: err=%v, want errHostStateSymlink", err)
	}
	if got, _ := os.ReadFile(target); string(got) != content {
		t.Fatalf("symlink target changed: %q", got)
	}
	if fi, err := os.Lstat(HostStatePath(dir)); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: fi=%v err=%v", fi, err)
	}
}

func TestHostStateFileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	dir := t.TempDir()
	// A pre-existing file with a wider mode is replaced by a 0600 one.
	if err := os.WriteFile(HostStatePath(dir), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UpdateHostState(dir, func(hs *HostState) error { hs.RunID = "r"; return nil }); err != nil {
		t.Fatalf("UpdateHostState: %v", err)
	}
	fi, err := os.Stat(HostStatePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != HostStateFile && e.Name() != HostStateFile+agentinfolock.LockSuffix {
			t.Fatalf("unexpected leftover %q in agentDir", e.Name())
		}
	}
}

// TestHostStateParallelUpdatesLoseNothing has each caller add its own
// unknown key under the lock; every key must survive.
func TestHostStateParallelUpdatesLoseNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("agentinfolock has no flock on this platform")
	}
	t.Setenv(agentinfolock.TestWaitEnv, "30s")
	dir := t.TempDir()
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
					hs.unknown[key] = json.RawMessage(strconv.Itoa(r))
					hs.RunID = key
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
	for w := 0; w < workers; w++ {
		for r := 0; r < rounds; r++ {
			key := fmt.Sprintf("k-%d-%d", w, r)
			if _, ok := raw[key]; !ok {
				t.Fatalf("lost update %s (have %d keys)", key, len(raw))
			}
		}
	}
	if len(raw) != workers*rounds+1 {
		t.Fatalf("have %d keys, want %d", len(raw), workers*rounds+1)
	}
}

func readRawHostState(t *testing.T, dir string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(HostStatePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("file is not a JSON object: %v: %s", err, data)
	}
	return raw
}

func compactJSON(t *testing.T, in json.RawMessage) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(in, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
