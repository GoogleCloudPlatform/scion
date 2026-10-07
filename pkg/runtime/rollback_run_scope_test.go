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
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

// Run-scoped rollback of a cancelled create, and Apple's run-checked Stop
// and Delete (ptone/scion#2550 P4).
//
// The mock engine models run B holding the container name: "ps" with the
// run-A label filter lists run A's own container (cid-run-a) only when
// runAExists is set, and every remove is logged so a remove by name (which
// would hit run B) is visible.

const rbName = "proj--dev"

func writeRollbackEngine(t *testing.T, binName string, runAExists bool) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	listA := ""
	if runAExists {
		listA = "cid-run-a"
	}
	// Apple's list JSON: run B's container holds the name; run A's (if any)
	// is listed under its own ID.
	appleJSON := `[{"status":"running","configuration":{"id":"` + rbName + `","labels":{"` + api.LabelRunID + `":"run-b"}}}`
	if runAExists {
		appleJSON = `[{"status":"running","configuration":{"id":"` + rbName + `","labels":{"` + api.LabelRunID + `":"run-b"}}},` +
			`{"status":"stopped","configuration":{"id":"cid-run-a","labels":{"` + api.LabelRunID + `":"run-a"}}}]`
	} else {
		appleJSON += "]"
	}
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  run) exec sleep 30 ;;
  ps)
    for a in "$@"; do
      if [ "$a" = "label=%s=run-a" ]; then
        [ -n %q ] && echo %q
      fi
    done ;;
  list) echo '%s' ;;
  *) echo "$@" >> %q ;;
esac
`, api.LabelRunID, listA, listA, appleJSON, log)
	bin := filepath.Join(dir, binName)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func readRollbackCalls(t *testing.T, log string) string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func cancelledRun(t *testing.T, rt Runtime, runID string) {
	t.Helper()
	cfg := RunConfig{
		Harness:      &harness.Generic{},
		Name:         rbName,
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Task:         "hello",
	}
	if runID != "" {
		cfg.Labels = map[string]string{api.LabelRunID: runID}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	if _, err := rt.Run(ctx, cfg); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
}

func rollbackRuntimes(bin string) map[string]Runtime {
	return map[string]Runtime{
		"docker": &DockerRuntime{Command: bin},
		"podman": &PodmanRuntime{Command: bin},
		"apple":  &AppleContainerRuntime{Command: bin},
	}
}

func binNameFor(kind string) string {
	if kind == "apple" {
		return "container" // rollbackCancelledCreate detects Apple by name
	}
	return "engine"
}

// A cancelled create for run A removes run A's own container by its ID and
// never removes by name, so run B's container of the same name survives.
func TestRollbackCancelledCreate_RemovesOnlyOwnRun(t *testing.T) {
	for _, kind := range []string{"docker", "podman", "apple"} {
		t.Run(kind, func(t *testing.T) {
			bin, log := writeRollbackEngine(t, binNameFor(kind), true)
			cancelledRun(t, rollbackRuntimes(bin)[kind], "run-a")

			calls := readRollbackCalls(t, log)
			if strings.Contains(calls, rbName) {
				t.Errorf("rollback touched the name %s (run B's container): %q", rbName, calls)
			}
			if !strings.Contains(calls, "rm") || !strings.Contains(calls, "cid-run-a") {
				t.Errorf("rollback calls = %q, want run A's cid-run-a removed", calls)
			}
		})
	}
}

// With no container of run A listed, nothing is removed: no name fallback.
func TestRollbackCancelledCreate_NoOwnContainerRemovesNothing(t *testing.T) {
	for _, kind := range []string{"docker", "podman", "apple"} {
		t.Run(kind, func(t *testing.T) {
			bin, log := writeRollbackEngine(t, binNameFor(kind), false)
			cancelledRun(t, rollbackRuntimes(bin)[kind], "run-a")

			if calls := readRollbackCalls(t, log); calls != "" {
				t.Errorf("rollback calls = %q, want none", calls)
			}
		})
	}
}

// A create with no run label rolls back by name, as before run IDs.
func TestRollbackCancelledCreate_NoRunIDByName(t *testing.T) {
	for _, kind := range []string{"docker", "podman", "apple"} {
		t.Run(kind, func(t *testing.T) {
			bin, log := writeRollbackEngine(t, binNameFor(kind), true)
			cancelledRun(t, rollbackRuntimes(bin)[kind], "")

			calls := readRollbackCalls(t, log)
			if !strings.Contains(calls, "rm") || !strings.Contains(calls, rbName) {
				t.Errorf("rollback calls = %q, want a remove of %s by name", calls, rbName)
			}
		})
	}
}

// Apple's ID is the container name: a stop or delete for run A finds run
// B's container under it and leaves it alone with ErrRunMismatch.
func TestAppleRunScoped_OtherRunUntouched(t *testing.T) {
	bin, log := writeRollbackEngine(t, "container", false)
	rt := &AppleContainerRuntime{Command: bin}
	for name, call := range map[string]func(RunRef) error{
		"Stop":   func(r RunRef) error { return rt.Stop(context.Background(), r) },
		"Delete": func(r RunRef) error { return rt.Delete(context.Background(), r) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(RunRef{ID: rbName, RunID: "run-a"}); !errors.Is(err, ErrRunMismatch) {
				t.Fatalf("%s = %v, want ErrRunMismatch", name, err)
			}
			if calls := readRollbackCalls(t, log); calls != "" {
				t.Errorf("calls = %q, want none", calls)
			}
		})
	}
}

// The run's own container, a legacy container with no run label, a name
// the listing does not show, and a ref with no run ID proceed as before.
func TestAppleRunScoped_Proceeds(t *testing.T) {
	cases := []struct {
		name string
		ref  RunRef
	}{
		{"own run", RunRef{ID: rbName, RunID: "run-b"}},
		{"not listed", RunRef{ID: "other", RunID: "run-a"}},
		{"no run ID", RunRef{ID: rbName}},
	}
	for _, tc := range cases {
		for _, op := range []string{"stop", "rm"} {
			t.Run(tc.name+"/"+op, func(t *testing.T) {
				bin, log := writeRollbackEngine(t, "container", false)
				rt := &AppleContainerRuntime{Command: bin}
				var err error
				if op == "stop" {
					err = rt.Stop(context.Background(), tc.ref)
				} else {
					err = rt.Delete(context.Background(), tc.ref)
				}
				if err != nil {
					t.Fatalf("%s: %v", op, err)
				}
				if calls := readRollbackCalls(t, log); !strings.Contains(calls, op+" "+tc.ref.ID) {
					t.Errorf("calls = %q, want %s %s", calls, op, tc.ref.ID)
				}
			})
		}
	}
	t.Run("legacy container", func(t *testing.T) {
		dir := t.TempDir()
		log := filepath.Join(dir, "calls.log")
		bin := filepath.Join(dir, "container")
		script := fmt.Sprintf(`#!/bin/sh
case "$1" in
  list) echo '[{"status":"running","configuration":{"id":"%s","labels":{}}}]' ;;
  *) echo "$@" >> %q ;;
esac
`, rbName, log)
		if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		rt := &AppleContainerRuntime{Command: bin}
		if err := rt.Delete(context.Background(), RunRef{ID: rbName, RunID: "run-a"}); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if calls := readRollbackCalls(t, log); !strings.Contains(calls, "rm "+rbName) {
			t.Errorf("calls = %q, want rm %s", calls, rbName)
		}
	})
}
