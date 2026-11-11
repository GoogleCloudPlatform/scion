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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Run-scoped Cloud Run Sandbox Stop and Delete (ptone/scion#2550 P4). The
// mock sandbox binary records every invocation, so "no delete issued" is
// observable.

func newRecordingSandboxRuntime(t *testing.T) (*CloudRunSandboxRuntime, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "calls.log")
	bin := writeMockBin(t, fmt.Sprintf(`echo "$@" >> %q`, log))
	return newWorkaroundTestRuntime(t, bin), log
}

func sandboxCalls(t *testing.T, log string) string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func addSandboxEntry(rt *CloudRunSandboxRuntime, name, runID string) {
	labels := map[string]string{"scion.name": "dev"}
	if runID != "" {
		labels[api.LabelRunID] = runID
	}
	rt.state.add(&sandboxStateEntry{SandboxName: name, AgentID: "dev", Labels: labels})
}

type sbOp struct {
	name string
	call func(rt *CloudRunSandboxRuntime, ref RunRef) error
}

var sbOps = []sbOp{
	{"Stop", func(rt *CloudRunSandboxRuntime, ref RunRef) error { return rt.Stop(context.Background(), ref) }},
	{"Delete", func(rt *CloudRunSandboxRuntime, ref RunRef) error { return rt.Delete(context.Background(), ref) }},
}

// Run A's stop or delete after run B took the sandbox name leaves run B's
// sandbox, state entry and watcher alone, with ErrRunMismatch.
func TestCloudRunSandboxRunScoped_OtherRunUntouched(t *testing.T) {
	for _, op := range sbOps {
		t.Run(op.name, func(t *testing.T) {
			rt, log := newRecordingSandboxRuntime(t)
			addSandboxEntry(rt, "dev", "run-b")
			cancelled := false
			rt.watchCancels["dev"] = func() { cancelled = true }

			err := op.call(rt, RunRef{ID: "dev", RunID: "run-a"})
			if !errors.Is(err, ErrRunMismatch) {
				t.Fatalf("%s = %v, want ErrRunMismatch", op.name, err)
			}
			if calls := sandboxCalls(t, log); calls != "" {
				t.Errorf("sandbox binary called: %q, want no call", calls)
			}
			if rt.state.get("dev") == nil {
				t.Error("run B's state entry was removed")
			}
			if cancelled {
				t.Error("run B's watcher was cancelled")
			}
		})
	}
}

// D2: a run-scoped stop or delete with no state entry treats the run's
// sandbox as gone: nil, no delete issued, no watcher cancelled.
func TestCloudRunSandboxRunScoped_NoEntryIsGone(t *testing.T) {
	for _, op := range sbOps {
		t.Run(op.name, func(t *testing.T) {
			rt, log := newRecordingSandboxRuntime(t)
			cancelled := false
			rt.watchCancels["dev"] = func() { cancelled = true }

			if err := op.call(rt, RunRef{ID: "dev", RunID: "run-a"}); err != nil {
				t.Fatalf("%s = %v, want nil", op.name, err)
			}
			if calls := sandboxCalls(t, log); calls != "" {
				t.Errorf("sandbox binary called: %q, want no call", calls)
			}
			if cancelled {
				t.Error("watcher cancelled")
			}
		})
	}
}

// The run's own entry, a legacy entry with no run label, and a ref with no
// run ID (whatever run holds the name) are deleted by name as before.
func TestCloudRunSandboxRunScoped_DeletesOwnLegacyAndRunless(t *testing.T) {
	cases := []struct{ name, entryRun, refRun string }{
		{"own run", "run-a", "run-a"},
		{"legacy entry", "", "run-a"},
		{"no run ID", "run-b", ""},
	}
	for _, op := range sbOps {
		for _, tc := range cases {
			t.Run(op.name+"/"+tc.name, func(t *testing.T) {
				rt, log := newRecordingSandboxRuntime(t)
				addSandboxEntry(rt, "dev", tc.entryRun)

				if err := op.call(rt, RunRef{ID: "dev", RunID: tc.refRun}); err != nil {
					t.Fatalf("%s: %v", op.name, err)
				}
				if calls := sandboxCalls(t, log); !strings.Contains(calls, "delete --force dev") {
					t.Errorf("sandbox calls = %q, want delete --force dev", calls)
				}
				if rt.state.get("dev") != nil {
					t.Error("state entry still present after delete")
				}
			})
		}
	}
}
