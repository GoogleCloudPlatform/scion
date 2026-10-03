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

package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// localAgent returns a fixture for a container started in a purely local
// (never Hub-linked) project: it carries "scion.project_path" — the
// identity run.go records from config.GetResolvedProjectDir — but
// deliberately no "scion.project_id" label at all, matching a real unlinked
// project's containers (pkg/agent/run.go only adds that label from a
// non-empty settings.Hub.ProjectID).
func localAgent(path, containerID, agentID string) api.AgentInfo {
	return api.AgentInfo{
		ContainerID: containerID,
		Name:        "test-agent",
		Phase:       string(state.PhaseRunning),
		Labels: map[string]string{
			"scion.name":                 "test-agent",
			"agent_id":                   agentID,
			projectkeys.LabelProjectPath: path,
		},
	}
}

// newSendKeysLocalMock builds a MockRuntime whose ListFunc replicates a real
// runtime backend's label-filtering semantics via
// projectkeys.LabelValuesMatch (pkg/runtime/docker.go and friends apply the
// same rule for "scion.project_path": alias-aware, through
// ResolvedPathEqual, not raw string equality) — unlike newSendKeysMock's
// filter-blind mock, this one's whole point is to prove the path filter
// SendKeysLocal passes actually does the isolating.
func newSendKeysLocalMock(agents []api.AgentInfo, captured *[]execRecord) *runtime.MockRuntime {
	var mu sync.Mutex
	record := func(cmd []string, stdin string) {
		mu.Lock()
		*captured = append(*captured, execRecord{argv: strings.Join(cmd, " "), stdin: stdin})
		mu.Unlock()
	}
	return &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			var out []api.AgentInfo
			for _, a := range agents {
				match := true
				for k, v := range filter {
					if !projectkeys.LabelValuesMatch(k, a.Labels[k], v) {
						match = false
						break
					}
				}
				if match {
					out = append(out, a)
				}
			}
			return out, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			record(cmd, "")
			return "", nil
		},
		ExecWithStdinFunc: func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
			buf, _ := io.ReadAll(stdin)
			record(cmd, string(buf))
			return "", nil
		},
	}
}

// TestSendKeysLocal_UnlinkedProjectWorks covers the core correction this
// method exists for: a project with NO Hub-linked project ID (no
// "scion.project_id" label on its containers at all) must still be able to
// deliver keys, scoped by its resolved project-config directory path.
func TestSendKeysLocal_UnlinkedProjectWorks(t *testing.T) {
	agent := localAgent("/home/user/proj-a", "container-a", "agent-a")
	var captured []execRecord
	mock := newSendKeysLocalMock([]api.AgentInfo{agent}, &captured)
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeysLocal(context.Background(), "/home/user/proj-a", "test-agent", "agent-a", "Escape")
	if err != nil {
		t.Fatalf("SendKeysLocal failed: %v", err)
	}
	if len(captured) == 0 {
		t.Fatal("expected at least one delivery call")
	}
	last := captured[len(captured)-1]
	if last.argv != "tmux source-file -" {
		t.Errorf("argv = %q, want the stdin-transport delivery command", last.argv)
	}
	// The exact script shape (octal-escaped keys, no added Enter) is already
	// pinned by TestSendKeys_ArgvExactness for the shared sendKeysCore; this
	// only confirms SendKeysLocal reaches the same delivery mechanism with
	// the right payload.
	if want := sendKeysScript(keysTarget, "Escape"); last.stdin != want {
		t.Errorf("stdin = %q, want %q", last.stdin, want)
	}
}

// TestSendKeysLocal_SameSlugDifferentPaths_Isolated proves two agents
// sharing a slug in two different local project directories are never
// confused: a call scoped to one path only ever delivers to that path's
// container.
func TestSendKeysLocal_SameSlugDifferentPaths_Isolated(t *testing.T) {
	a := localAgent("/home/user/proj-a", "container-a", "agent-a")
	b := localAgent("/home/user/proj-b", "container-b", "agent-b")
	var captured []execRecord
	mock := newSendKeysLocalMock([]api.AgentInfo{a, b}, &captured)
	mgr := &AgentManager{Runtime: mock}

	if err := mgr.SendKeysLocal(context.Background(), "/home/user/proj-a", "test-agent", "agent-a", "C-c"); err != nil {
		t.Fatalf("SendKeysLocal(proj-a) failed: %v", err)
	}
	for _, c := range captured {
		if strings.Contains(c.argv, "container-b") || strings.Contains(c.stdin, "container-b") {
			t.Fatalf("proj-a's call must never touch proj-b's container, got: %v", captured)
		}
	}

	captured = nil
	if err := mgr.SendKeysLocal(context.Background(), "/home/user/proj-b", "test-agent", "agent-b", "C-c"); err != nil {
		t.Fatalf("SendKeysLocal(proj-b) failed: %v", err)
	}
	if len(captured) == 0 {
		t.Fatal("expected proj-b's call to deliver")
	}
}

// TestSendKeysLocal_SameDirectoryName_DifferentFullPaths_Isolated proves
// project *name* alone is not the scope: two directories named identically
// ("myproj") under different parents must never be confused, because the
// label/filter comparison is on the full resolved path, not the trailing
// path component.
func TestSendKeysLocal_SameDirectoryName_DifferentFullPaths_Isolated(t *testing.T) {
	a := localAgent("/home/alice/myproj", "container-a", "agent-a")
	b := localAgent("/home/bob/myproj", "container-b", "agent-b")
	var captured []execRecord
	mock := newSendKeysLocalMock([]api.AgentInfo{a, b}, &captured)
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeysLocal(context.Background(), "/home/alice/myproj", "test-agent", "agent-a", "C-c")
	if err != nil {
		t.Fatalf("SendKeysLocal failed: %v", err)
	}
	// Re-resolution (pre-delivery revalidation) also scopes by the full
	// path, so delivery must have reached container-a only; prove it by
	// re-running against bob's identical-name directory and requiring a
	// *different* identity check (agent-a there must fail).
	err = mgr.SendKeysLocal(context.Background(), "/home/bob/myproj", "test-agent", "agent-a", "C-c")
	if !errors.Is(err, agentkeys.ErrTargetNotFound) {
		t.Fatalf("SendKeysLocal(bob/myproj, agent-a) error = %v, want ErrTargetNotFound (agent-a does not live there)", err)
	}
}

// TestSendKeysLocal_Ambiguous_Fails proves two distinct containers matching
// the same (path, slug) scope fail closed rather than guessing.
func TestSendKeysLocal_Ambiguous_Fails(t *testing.T) {
	a := localAgent("/home/user/proj-a", "container-a", "agent-a")
	b := localAgent("/home/user/proj-a", "container-b", "agent-b")
	var captured []execRecord
	mock := newSendKeysLocalMock([]api.AgentInfo{a, b}, &captured)
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeysLocal(context.Background(), "/home/user/proj-a", "test-agent", "agent-a", "C-c")
	if !errors.Is(err, agentkeys.ErrTargetNotFound) {
		t.Fatalf("SendKeysLocal error = %v, want ErrTargetNotFound for an ambiguous scope", err)
	}
	if len(captured) != 0 {
		t.Fatalf("expected zero delivery calls for an ambiguous scope, got %v", captured)
	}
}

// TestSendKeysLocal_MissingOrWrongAgentID_FailsClosed covers both a missing
// "agent_id" label and a mismatched expectedAgentID.
func TestSendKeysLocal_MissingOrWrongAgentID_FailsClosed(t *testing.T) {
	t.Run("missing_label", func(t *testing.T) {
		a := localAgent("/home/user/proj-a", "container-a", "")
		delete(a.Labels, "agent_id")
		var captured []execRecord
		mock := newSendKeysLocalMock([]api.AgentInfo{a}, &captured)
		mgr := &AgentManager{Runtime: mock}

		err := mgr.SendKeysLocal(context.Background(), "/home/user/proj-a", "test-agent", "agent-a", "C-c")
		if !errors.Is(err, agentkeys.ErrTargetNotFound) {
			t.Fatalf("error = %v, want ErrTargetNotFound", err)
		}
		if len(captured) != 0 {
			t.Fatalf("expected zero delivery calls, got %v", captured)
		}
	})

	t.Run("wrong_expected_id", func(t *testing.T) {
		a := localAgent("/home/user/proj-a", "container-a", "agent-a")
		var captured []execRecord
		mock := newSendKeysLocalMock([]api.AgentInfo{a}, &captured)
		mgr := &AgentManager{Runtime: mock}

		err := mgr.SendKeysLocal(context.Background(), "/home/user/proj-a", "test-agent", "agent-WRONG", "C-c")
		if !errors.Is(err, agentkeys.ErrTargetNotFound) {
			t.Fatalf("error = %v, want ErrTargetNotFound", err)
		}
		if len(captured) != 0 {
			t.Fatalf("expected zero delivery calls, got %v", captured)
		}
	})
}

// TestSendKeysLocal_WrongOrMissingPath_FailsClosed covers a projectPath that
// does not match any container's recorded path, and an empty projectPath —
// SendKeysLocal must never fall back to an unscoped (path-blind) lookup.
func TestSendKeysLocal_WrongOrMissingPath_FailsClosed(t *testing.T) {
	a := localAgent("/home/user/proj-a", "container-a", "agent-a")

	t.Run("wrong_path", func(t *testing.T) {
		var captured []execRecord
		mock := newSendKeysLocalMock([]api.AgentInfo{a}, &captured)
		mgr := &AgentManager{Runtime: mock}

		err := mgr.SendKeysLocal(context.Background(), "/home/user/proj-OTHER", "test-agent", "agent-a", "C-c")
		if !errors.Is(err, agentkeys.ErrTargetNotFound) {
			t.Fatalf("error = %v, want ErrTargetNotFound", err)
		}
		if len(captured) != 0 {
			t.Fatalf("expected zero delivery calls, got %v", captured)
		}
	})

	t.Run("empty_path_never_lists", func(t *testing.T) {
		var listCalled bool
		mock := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
				listCalled = true
				return []api.AgentInfo{a}, nil
			},
		}
		mgr := &AgentManager{Runtime: mock}

		err := mgr.SendKeysLocal(context.Background(), "", "test-agent", "agent-a", "C-c")
		if !errors.Is(err, agentkeys.ErrTargetNotFound) {
			t.Fatalf("error = %v, want ErrTargetNotFound", err)
		}
		if listCalled {
			t.Error("SendKeysLocal must not list agents at all for an empty (unscoped) projectPath")
		}
	})

	t.Run("empty_expected_agent_id_never_lists", func(t *testing.T) {
		var listCalled bool
		mock := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
				listCalled = true
				return []api.AgentInfo{a}, nil
			},
		}
		mgr := &AgentManager{Runtime: mock}

		err := mgr.SendKeysLocal(context.Background(), "/home/user/proj-a", "test-agent", "", "C-c")
		if !errors.Is(err, agentkeys.ErrTargetNotFound) {
			t.Fatalf("error = %v, want ErrTargetNotFound", err)
		}
		if listCalled {
			t.Error("SendKeysLocal must not list agents at all for an empty expectedAgentID")
		}
	})
}

// TestSendKeysLocal_RecreateDuringWindow_FailsClosed mirrors
// TestSendKeys_TargetRevalidationFailsClosed for the path-scoped entry
// point: a container recreated (new ContainerID) between the initial
// resolution and the pre-delivery re-verification must never receive
// delivery.
func TestSendKeysLocal_RecreateDuringWindow_FailsClosed(t *testing.T) {
	first := localAgent("/home/user/proj-a", "container-1", "agent-a")
	second := first
	second.ContainerID = "container-2"

	calls := 0
	var capturedCmd []string
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			calls++
			if calls == 1 {
				return []api.AgentInfo{first}, nil
			}
			return []api.AgentInfo{second}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeysLocal(context.Background(), "/home/user/proj-a", "test-agent", "agent-a", "C-c")
	if !errors.Is(err, agentkeys.ErrTargetNotFound) {
		t.Fatalf("SendKeysLocal error = %v, want agentkeys.ErrTargetNotFound", err)
	}
	if calls < 2 {
		t.Fatalf("expected at least 2 List calls (original resolution + pre-delivery re-verification), got %d", calls)
	}
	for _, c := range capturedCmd {
		if strings.Contains(c, "source-file") {
			t.Fatalf("delivery must not run once the pre-delivery re-verification finds a different target, got: %v", capturedCmd)
		}
	}
}

// TestSendKeysLocal_PathAliases_ResolveEqual proves a caller-supplied path
// and a container's recorded path that name the same filesystem location
// through a different (but equally valid) spelling — here, a relative path
// vs. the same directory's absolute path — are treated as identical, via
// the same ResolvedPathEqual rule the real runtime backends already apply
// to the "scion.project_path" label filter.
func TestSendKeysLocal_PathAliases_ResolveEqual(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", dir, err)
	}

	// The container's recorded label is the canonical, resolved path (what
	// run.go would have stored at Start time).
	a := localAgent(real, "container-a", "agent-a")
	var captured []execRecord
	mock := newSendKeysLocalMock([]api.AgentInfo{a}, &captured)
	mgr := &AgentManager{Runtime: mock}

	// The caller supplies an alias: dir + "/." is lexically different but
	// resolves to the same canonical location.
	alias := filepath.Join(dir, ".")
	if err := mgr.SendKeysLocal(context.Background(), alias, "test-agent", "agent-a", "C-c"); err != nil {
		t.Fatalf("SendKeysLocal with a path alias failed: %v (aliases must resolve equal)", err)
	}
	if len(captured) == 0 {
		t.Fatal("expected delivery to proceed once the alias resolved to the same container")
	}
}

// TestSendKeysLocal_InvalidKeysShapeNeverExecutes mirrors
// TestSendKeys_InvalidKeysShapeNeverExecutes: SendKeysLocal must validate
// keys before any resolution or Exec attempt.
func TestSendKeysLocal_InvalidKeysShapeNeverExecutes(t *testing.T) {
	a := localAgent("/home/user/proj-a", "container-a", "agent-a")
	var listCalled bool
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			listCalled = true
			return []api.AgentInfo{a}, nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeysLocal(context.Background(), "/home/user/proj-a", "test-agent", "agent-a", "")
	if _, ok := agentkeys.AsValidationError(err); !ok {
		t.Fatalf("error = %v, want *agentkeys.ValidationError", err)
	}
	if listCalled {
		t.Error("SendKeysLocal must not resolve a target at all for invalid keys")
	}
}

// TestSendKeysLocal_UnsupportedBackend mirrors TestSendKeys_UnsupportedBackend.
func TestSendKeysLocal_UnsupportedBackend(t *testing.T) {
	var listCalled bool
	mock := &runtime.MockRuntime{
		NameFunc: func() string { return "cloudrun" },
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			listCalled = true
			return []api.AgentInfo{localAgent("/home/user/proj-a", "container-a", "agent-a")}, nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	err := mgr.SendKeysLocal(context.Background(), "/home/user/proj-a", "test-agent", "agent-a", "C-c")
	if !errors.Is(err, ErrKeysUnsupported) {
		t.Fatalf("error = %v, want ErrKeysUnsupported", err)
	}
	if listCalled {
		t.Error("SendKeysLocal must not resolve a target at all for a backend that does not support keys delivery")
	}
}

// TestSendKeysLocal_DeadlineExpiredWhileWaitingForLock mirrors
// TestSendKeys_DeadlineExpiredWhileWaitingForLock: SendKeysLocal shares the
// same injection lock and deadline enforcement as SendKeys.
func TestSendKeysLocal_DeadlineExpiredWhileWaitingForLock(t *testing.T) {
	a := localAgent("/home/user/proj-a", "container-a", "agent-a")

	var capturedCmd []string
	var mu sync.Mutex
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{a}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			mu.Lock()
			capturedCmd = append(capturedCmd, strings.Join(cmd, " "))
			mu.Unlock()
			return "", nil
		},
	}
	mgr := &AgentManager{Runtime: mock}

	lock := mgr.injectionLock(a.ContainerID)
	if err := lock.Lock(context.Background()); err != nil {
		t.Fatalf("failed to seed the lock: %v", err)
	}
	release := make(chan struct{})
	go func() {
		<-release
		lock.Unlock()
	}()
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := mgr.SendKeysLocal(ctx, "/home/user/proj-a", "test-agent", "agent-a", "C-c")
	if err == nil {
		t.Fatal("expected SendKeysLocal to fail once its deadline expired while waiting for the lock")
	}
	if !errors.Is(err, ErrKeysNotStarted) {
		t.Fatalf("error = %v, want one wrapping ErrKeysNotStarted", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(capturedCmd) != 0 {
		t.Fatalf("expected zero Exec calls once the deadline expired waiting for the lock, got %v", capturedCmd)
	}
}

// TestSendKeysLocal_NoReplay_ConnectionIsNotRetried proves SendKeysLocal
// performs exactly one delivery attempt — no internal retry — matching
// SendKeys's own no-replay guarantee, by failing the first ExecWithStdin
// call and asserting it is never retried.
func TestSendKeysLocal_NoReplay_ConnectionIsNotRetried(t *testing.T) {
	a := localAgent("/home/user/proj-a", "container-a", "agent-a")
	var attempts int
	mock := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{a}, nil
		},
		ExecFunc: func(ctx context.Context, id string, cmd []string) (string, error) {
			return "", nil
		},
		ExecWithStdinFunc: func(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
			attempts++
			return "", os.ErrClosed
		},
	}
	mgr := &AgentManager{Runtime: mock}

	if err := mgr.SendKeysLocal(context.Background(), "/home/user/proj-a", "test-agent", "agent-a", "C-c"); err == nil {
		t.Fatal("expected an error from the failing delivery call")
	}
	if attempts != 1 {
		t.Fatalf("expected exactly 1 delivery attempt (no internal replay), got %d", attempts)
	}
}
