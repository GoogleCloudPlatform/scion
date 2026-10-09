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

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Same-name sandbox Run fence (ptone/scion#3738): two overlapping Runs of
// one agent must not remove or take over each other's sandbox.

// newFenceSandboxRuntime returns a runtime whose mock sandbox binary logs
// every call except wait and answers exec with execExit. wait blocks like a
// live sandbox's, so the exit watcher a successful Run starts never writes
// the state file; a cleanup registered after the TempDirs (cleanups run
// last-in first-out) cancels the watchers before those directories are
// removed.
func newFenceSandboxRuntime(t *testing.T, execExit int) (*CloudRunSandboxRuntime, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "calls.log")
	bin := writeMockBin(t, fmt.Sprintf(`[ "$1" = wait ] && exec sleep 60
echo "$1" >> %q
case "$1" in
  exec) exit %d ;;
  *)    exit 0 ;;
esac`, log, execExit))
	rt := newWorkaroundTestRuntime(t, bin)
	t.Cleanup(func() {
		rt.watchMu.Lock()
		defer rt.watchMu.Unlock()
		for name, cancel := range rt.watchCancels {
			cancel()
			delete(rt.watchCancels, name)
		}
	})
	return rt, log
}

func fenceRunConfig(t *testing.T, runID string) RunConfig {
	t.Helper()
	home := filepath.Join(t.TempDir(), "agent-home")
	if err := os.MkdirAll(home, 0755); err != nil {
		t.Fatal(err)
	}
	return RunConfig{
		Name:    "dev",
		HomeDir: home,
		Image:   "img",
		Harness: &mockHarness{command: []string{"claude"}, env: map[string]string{}},
		Labels:  map[string]string{"scion.name": "dev", api.LabelRunID: runID},
	}
}

func setAfterLaunchHook(t *testing.T, hook func(name string)) {
	t.Helper()
	prev := sandboxAfterLaunchHook
	sandboxAfterLaunchHook = hook
	t.Cleanup(func() { sandboxAfterLaunchHook = prev })
}

// Without another run's entry the dead-on-arrival cleanup still deletes.
func TestCloudRunSandboxRun_DOACleanupDeletesOwn(t *testing.T) {
	rt, log := newFenceSandboxRuntime(t, 1)
	if _, err := rt.Run(context.Background(), fenceRunConfig(t, "run-a")); err == nil {
		t.Fatal("Run A succeeded, want a dead-on-arrival error")
	}
	if calls := sandboxCalls(t, log); !strings.Contains(calls, "delete") {
		t.Errorf("no cleanup delete issued; calls:\n%s", calls)
	}
}

// The newer run's live sandbox is recorded under the name. The older Run
// refuses with ErrRunConflict before launching, so it neither starts a
// sandbox nor replaces the newer run's entry.
func TestCloudRunSandboxRun_DoesNotTakeOverNewerRun(t *testing.T) {
	rt, log := newFenceSandboxRuntime(t, 0)
	addSandboxEntry(rt, "dev", "run-b")

	_, err := rt.Run(context.Background(), fenceRunConfig(t, "run-a"))
	if !errors.Is(err, ErrRunConflict) {
		t.Fatalf("Run A = %v, want ErrRunConflict", err)
	}
	if calls := sandboxCalls(t, log); calls != "" {
		t.Errorf("run A called the sandbox CLI:\n%s", calls)
	}
	if e := rt.state.get("dev"); e == nil || e.Labels[api.LabelRunID] != "run-b" {
		t.Errorf("state entry = %+v, want run B's", e)
	}
}

// A stopped entry of another run, or the same run's entry, does not block.
func TestCloudRunSandboxRun_StoppedOrOwnEntryDoesNotBlock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		run     string
		stopped bool
	}{
		{"stopped entry of another run", "run-b", true},
		{"own run", "run-a", false},
		{"legacy entry with no run", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, _ := newFenceSandboxRuntime(t, 0)
			addSandboxEntry(rt, "dev", tc.run)
			if tc.stopped {
				rt.state.markStopped("dev", nil, nil)
			}
			if _, err := rt.Run(context.Background(), fenceRunConfig(t, "run-a")); err != nil {
				t.Fatalf("Run A: %v", err)
			}
			if e := rt.state.get("dev"); e == nil || e.Labels[api.LabelRunID] != "run-a" {
				t.Errorf("state entry = %+v, want run A's", e)
			}
		})
	}
}

// Two overlapping same-name Runs in one process are serialized: the newer
// Run does not launch while the older one is between its launch and its
// probe outcome, so the older run's by-name cleanup cannot hit the newer
// run's sandbox.
func TestCloudRunSandboxRun_SameNameRunsSerialized(t *testing.T) {
	rt, log := newFenceSandboxRuntime(t, 1) // every sandbox is dead on arrival
	inWindow := make(chan struct{})
	release := make(chan struct{})
	first := true
	setAfterLaunchHook(t, func(string) {
		if first {
			first = false
			close(inWindow)
			<-release
		}
	})

	cfgA := fenceRunConfig(t, "run-a")
	cfgB := fenceRunConfig(t, "run-b")

	errA := make(chan error, 1)
	go func() {
		_, err := rt.Run(context.Background(), cfgA)
		errA <- err
	}()
	<-inWindow

	errB := make(chan error, 1)
	go func() {
		_, err := rt.Run(context.Background(), cfgB)
		errB <- err
	}()

	time.Sleep(300 * time.Millisecond)
	if n := strings.Count(sandboxCalls(t, log), "run\n"); n != 1 {
		t.Errorf("%d sandbox launches while run A was in its probe window, want 1", n)
	}
	close(release)
	<-errA
	<-errB

	// Run A's cleanup delete comes before run B's launch.
	calls := strings.Fields(sandboxCalls(t, log))
	firstDelete, secondRun := -1, -1
	runs := 0
	for i, c := range calls {
		switch c {
		case "delete":
			if firstDelete < 0 {
				firstDelete = i
			}
		case "run":
			runs++
			if runs == 2 {
				secondRun = i
			}
		}
	}
	if firstDelete < 0 || secondRun < 0 || firstDelete > secondRun {
		t.Errorf("calls = %v, want run A's delete before run B's launch", calls)
	}
}
