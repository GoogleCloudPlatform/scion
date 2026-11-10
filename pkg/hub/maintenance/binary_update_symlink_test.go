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

package maintenance

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBinaryUpdate_SymlinkEndToEnd runs the executor with the real symlink
// installer on a temporary /opt/scion layout.
func TestBinaryUpdate_SymlinkEndToEnd(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")

	root := installLayout(t, "nightly-20261022")
	opts := symlinkOpts(root)
	signalled := false
	opts.Signal = func() error { signalled = true; return nil }
	inst := Detect(context.Background(), opts)
	if inst.Name() != StrategySymlink {
		t.Fatalf("Detect = %s (%s)", inst.Name(), inst.Status().Reason)
	}

	e := newTestExecutor(gh, inst, "nightly-20261022")
	var hookSawCurrent string
	err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"},
		func(context.Context, PendingRestart) error {
			hookSawCurrent = readLink(t, root, currentLinkName)
			if signalled {
				t.Error("signalled before the hook")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if hookSawCurrent != "releases/nightly-20261023" {
		t.Fatalf("current at hook time = %q", hookSawCurrent)
	}
	if prev := readLink(t, root, prevLinkName); prev != "releases/nightly-20261022" {
		t.Fatalf("previous = %q", prev)
	}
	if !signalled {
		t.Fatal("hub was not signalled")
	}
	out, err := os.ReadFile(filepath.Join(root, currentLinkName, binaryName))
	if err != nil || !strings.Contains(string(out), "nightly-20261023") {
		t.Fatalf("current/scion = %q, %v", out, err)
	}
}

func TestBinaryUpdate_HookFailureRollsBackSymlink(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")

	root := installLayout(t, "nightly-20261022", "nightly-20261021")
	if err := os.Symlink("releases/nightly-20261021", filepath.Join(root, prevLinkName)); err != nil {
		t.Fatal(err)
	}
	opts := symlinkOpts(root)
	signals := 0
	opts.Signal = func() error { signals++; return nil }
	inst := Detect(context.Background(), opts)
	if inst.Name() != StrategySymlink {
		t.Fatalf("Detect = %s (%s)", inst.Name(), inst.Status().Reason)
	}
	e := newTestExecutor(gh, inst, "nightly-20261022")
	params := map[string]string{"target_version": "nightly-20261023"}

	err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, params,
		func(context.Context, PendingRestart) error { return errors.New("db down") })
	if err == nil || !strings.Contains(err.Error(), "rolled back, nothing changed") {
		t.Fatalf("err = %v, want rollback", err)
	}
	if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261022" {
		t.Fatalf("current = %q after rollback", cur)
	}
	if prev := readLink(t, root, prevLinkName); prev != "releases/nightly-20261021" {
		t.Fatalf("previous = %q after rollback", prev)
	}
	if signals != 0 {
		t.Fatal("signalled although the hook failed")
	}

	// A retry of the same target now goes through.
	if err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, params,
		func(context.Context, PendingRestart) error { return nil }); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261023" {
		t.Fatalf("current after retry = %q", cur)
	}
	if signals != 1 {
		t.Fatalf("signals = %d, want 1", signals)
	}
}

func TestBinaryUpdate_RestartFailureRollsBackSymlink(t *testing.T) {
	requireLinux(t)
	gh := newFakeGitHub(t)
	gh.addRelease("nightly-20261023", "2026-10-23T04:00:00Z", "nightly-20261023")

	root := installLayout(t, "nightly-20261022", "nightly-20261021")
	if err := os.Symlink("releases/nightly-20261021", filepath.Join(root, prevLinkName)); err != nil {
		t.Fatal(err)
	}
	opts := symlinkOpts(root)
	opts.Signal = func() error { return errors.New("kill: operation not permitted") }
	inst := Detect(context.Background(), opts)
	if inst.Name() != StrategySymlink {
		t.Fatalf("Detect = %s (%s)", inst.Name(), inst.Status().Reason)
	}
	e := newTestExecutor(gh, inst, "nightly-20261022")
	hooked := false
	err := e.RunWithBeforeRestart(context.Background(), &bytes.Buffer{}, map[string]string{"target_version": "nightly-20261023"},
		func(context.Context, PendingRestart) error { hooked = true; return nil })
	if !hooked {
		t.Fatal("hook not called before the restart")
	}
	if err == nil || !strings.Contains(err.Error(), "restarting the hub failed") || !strings.Contains(err.Error(), "rolled back, nothing changed") {
		t.Fatalf("err = %v", err)
	}
	if cur := readLink(t, root, currentLinkName); cur != "releases/nightly-20261022" {
		t.Fatalf("current = %q after rollback", cur)
	}
	if prev := readLink(t, root, prevLinkName); prev != "releases/nightly-20261021" {
		t.Fatalf("previous = %q after rollback", prev)
	}
}
