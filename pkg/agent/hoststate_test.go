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
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agentinfolock"
)

func TestHostStateMissingFileIsZero(t *testing.T) {
	a := newHostStateAgent(t)
	hs, err := ReadHostState(a.root, a.name)
	if err != nil {
		t.Fatalf("ReadHostState: %v", err)
	}
	if hs.RunID != "" || hs.unknown != nil {
		t.Fatalf("got %+v, want zero", hs)
	}
}

func TestHostStateMissingAgentDir(t *testing.T) {
	root := t.TempDir()
	hs, err := ReadHostState(root, "absent")
	if err != nil || hs.RunID != "" {
		t.Fatalf("ReadHostState: hs=%+v err=%v, want zero and nil", hs, err)
	}
	if err := UpdateHostState(root, "absent", func(hs *HostState) error { hs.RunID = "x"; return nil }); err == nil {
		t.Fatal("UpdateHostState: want error when agentDir is missing")
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("UpdateHostState created entries in root: %v", entries)
	}
}

func TestHostStateBadArgumentsRefused(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"", ".", "..", "a/b", "../x", "x/..", "a\\b", "a\x00b"} {
		if _, err := HostStatePath(root, name); !errors.Is(err, errHostStateAgentName) {
			t.Errorf("HostStatePath(%q): err=%v, want errHostStateAgentName", name, err)
		}
		if _, err := ReadHostState(root, name); !errors.Is(err, errHostStateAgentName) {
			t.Errorf("ReadHostState(%q): err=%v, want errHostStateAgentName", name, err)
		}
		called := false
		err := UpdateHostState(root, name, func(*HostState) error { called = true; return nil })
		if !errors.Is(err, errHostStateAgentName) || called {
			t.Errorf("UpdateHostState(%q): err=%v called=%v, want errHostStateAgentName", name, err, called)
		}
	}
	if _, err := ReadHostState("relative/root", "agent-1"); !errors.Is(err, errHostStateRelativeRoot) {
		t.Errorf("ReadHostState: err=%v, want errHostStateRelativeRoot", err)
	}
	if err := UpdateHostState("relative/root", "agent-1", func(*HostState) error { return nil }); !errors.Is(err, errHostStateRelativeRoot) {
		t.Errorf("UpdateHostState: err=%v, want errHostStateRelativeRoot", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("entries created in root: %v", entries)
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
			a := newHostStateAgent(t)
			path := a.path
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadHostState(a.root, a.name); err == nil {
				t.Fatal("ReadHostState: want error for corrupt file")
			}
			called := false
			err := UpdateHostState(a.root, a.name, func(*HostState) error { called = true; return nil })
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
	a := newHostStateAgent(t)
	if err := os.WriteFile(a.path, []byte(`{"runId":"x"}`), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHostState(a.root, a.name); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadHostState: err=%v, want a non-NotExist error", err)
	}
}

func TestHostStateRunIDRoundTrip(t *testing.T) {
	a := newHostStateAgent(t)
	if err := UpdateHostState(a.root, a.name, func(hs *HostState) error { hs.RunID = "run-123"; return nil }); err != nil {
		t.Fatalf("UpdateHostState: %v", err)
	}
	hs, err := ReadHostState(a.root, a.name)
	if err != nil {
		t.Fatalf("ReadHostState: %v", err)
	}
	if hs.RunID != "run-123" {
		t.Fatalf("RunID = %q, want run-123", hs.RunID)
	}
	if err := UpdateHostState(a.root, a.name, func(hs *HostState) error { hs.RunID = ""; return nil }); err != nil {
		t.Fatalf("UpdateHostState clear: %v", err)
	}
	raw := readRawHostState(t, a.path)
	if _, ok := raw["runId"]; ok {
		t.Fatalf("cleared runId still in file: %v", raw)
	}
}

func TestHostStateMutateErrorWritesNothing(t *testing.T) {
	a := newHostStateAgent(t)
	want := errors.New("boom")
	err := UpdateHostState(a.root, a.name, func(hs *HostState) error { hs.RunID = "x"; return want })
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if _, err := os.Lstat(a.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file written despite mutate error: %v", err)
	}
}

func TestHostStateUnknownFieldsSurviveUpdate(t *testing.T) {
	a := newHostStateAgent(t)
	orig := `{"runId":"old","futureKey":{"a":[1,2]},"other":"v"}`
	if err := os.WriteFile(a.path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := UpdateHostState(a.root, a.name, func(hs *HostState) error { hs.RunID = "new"; return nil }); err != nil {
		t.Fatalf("UpdateHostState: %v", err)
	}
	raw := readRawHostState(t, a.path)
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
	a := newHostStateAgent(t)
	target := filepath.Join(t.TempDir(), "target.json")
	const content = `{"runId":"target"}`
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, a.path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHostState(a.root, a.name); !errors.Is(err, errHostStateSymlink) {
		t.Fatalf("ReadHostState: err=%v, want errHostStateSymlink", err)
	}
	err := UpdateHostState(a.root, a.name, func(hs *HostState) error { hs.RunID = "x"; return nil })
	if !errors.Is(err, errHostStateSymlink) {
		t.Fatalf("UpdateHostState: err=%v, want errHostStateSymlink", err)
	}
	if got, _ := os.ReadFile(target); string(got) != content {
		t.Fatalf("symlink target changed: %q", got)
	}
	if fi, err := os.Lstat(a.path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: fi=%v err=%v", fi, err)
	}
}

func TestHostStateFileMode0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions")
	}
	a := newHostStateAgent(t)
	// A pre-existing file with a wider mode is replaced by a 0600 one.
	if err := os.WriteFile(a.path, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := UpdateHostState(a.root, a.name, func(hs *HostState) error { hs.RunID = "r"; return nil }); err != nil {
		t.Fatalf("UpdateHostState: %v", err)
	}
	fi, err := os.Stat(a.path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
	entries, err := os.ReadDir(a.dir)
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
	a := newHostStateAgent(t)
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
	raw := readRawHostState(t, a.path)
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

func TestHostStateTooLargeErrors(t *testing.T) {
	a := newHostStateAgent(t)
	big := `{"runId":"x","pad":"` + strings.Repeat("a", maxHostStateSize) + `"}`
	if err := os.WriteFile(a.path, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHostState(a.root, a.name); !errors.Is(err, errHostStateTooLarge) {
		t.Fatalf("ReadHostState: err=%v, want errHostStateTooLarge", err)
	}
	err := UpdateHostState(a.root, a.name, func(hs *HostState) error { hs.RunID = "y"; return nil })
	if !errors.Is(err, errHostStateTooLarge) {
		t.Fatalf("UpdateHostState: err=%v, want errHostStateTooLarge", err)
	}
	if got, _ := os.ReadFile(a.path); string(got) != big {
		t.Fatal("too large file changed")
	}

	// Exactly at the cap is accepted.
	exact := `{"pad":"`
	exact += strings.Repeat("a", maxHostStateSize-len(exact)-2) + `"}`
	if len(exact) != maxHostStateSize {
		t.Fatalf("test setup: len %d", len(exact))
	}
	if err := os.WriteFile(a.path, []byte(exact), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHostState(a.root, a.name); err != nil {
		t.Fatalf("ReadHostState at cap: %v", err)
	}
}

func TestHostStateKnownKeyOtherCase(t *testing.T) {
	a := newHostStateAgent(t)
	if err := os.WriteFile(a.path, []byte(`{"RunID":"old","other":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	hs, err := ReadHostState(a.root, a.name)
	if err != nil {
		t.Fatalf("ReadHostState: %v", err)
	}
	if hs.RunID != "old" {
		t.Fatalf("RunID = %q, want old", hs.RunID)
	}
	if err := UpdateHostState(a.root, a.name, func(hs *HostState) error { hs.RunID = "new"; return nil }); err != nil {
		t.Fatalf("UpdateHostState: %v", err)
	}
	raw := readRawHostState(t, a.path)
	if _, ok := raw["RunID"]; ok {
		t.Fatalf("other-case known key kept as unknown: %v", raw)
	}
	if string(raw["runId"]) != `"new"` || string(raw["other"]) != "1" || len(raw) != 2 {
		t.Fatalf("got %v", raw)
	}
}

func TestHostStateDirectoryAtPathErrors(t *testing.T) {
	a := newHostStateAgent(t)
	if err := os.Mkdir(a.path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHostState(a.root, a.name); !errors.Is(err, errHostStateNotRegular) {
		t.Fatalf("ReadHostState: err=%v, want errHostStateNotRegular", err)
	}
	if err := UpdateHostState(a.root, a.name, func(hs *HostState) error { hs.RunID = "x"; return nil }); !errors.Is(err, errHostStateNotRegular) {
		t.Fatalf("UpdateHostState: err=%v, want errHostStateNotRegular", err)
	}
	if fi, err := os.Lstat(a.path); err != nil || !fi.IsDir() {
		t.Fatalf("directory replaced: fi=%v err=%v", fi, err)
	}
}

// TestHostStateMarkerFileAtRootErrors passes a root whose .scion is a
// file, and a root whose .scion/agents is a file. Both are refused with
// their own error and nothing is written.
func TestHostStateMarkerFileAtRootErrors(t *testing.T) {
	cases := map[string]struct {
		file string
		want error
	}{
		".scion file": {file: ".scion", want: errHostStateMarkerFile},
		"agents file": {file: filepath.Join(".scion", "agents"), want: errHostStateNotDirectory},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			p := filepath.Join(root, tc.file)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			const content = "project-id: 1234\n"
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadHostState(root, "agent-1"); !errors.Is(err, tc.want) {
				t.Fatalf("ReadHostState: err=%v, want %v", err, tc.want)
			}
			called := false
			err := UpdateHostState(root, "agent-1", func(*HostState) error { called = true; return nil })
			if !errors.Is(err, tc.want) || called {
				t.Fatalf("UpdateHostState: err=%v called=%v, want %v and no mutate", err, called, tc.want)
			}
			if got, _ := os.ReadFile(p); string(got) != content {
				t.Fatalf("file changed: %q", got)
			}
		})
	}
}

// hostStateAgent is a test agent dir at <root>/.scion/agents/<name>.
type hostStateAgent struct {
	root, name, dir, path string
}

// newHostStateAgent creates an agent dir below a fresh temp root. The root
// is used as t.TempDir() returns it: symlinks above the root (macOS /var)
// are trusted.
func newHostStateAgent(t *testing.T) hostStateAgent {
	t.Helper()
	return newHostStateAgentIn(t, t.TempDir(), "agent-1")
}

func newHostStateAgentIn(t *testing.T, root, name string) hostStateAgent {
	t.Helper()
	dir := filepath.Join(root, ".scion", "agents", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path, err := HostStatePath(root, name)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, HostStateFile) {
		t.Fatalf("HostStatePath = %s, want %s", path, filepath.Join(dir, HostStateFile))
	}
	return hostStateAgent{root: root, name: name, dir: dir, path: path}
}

func readRawHostState(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
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
