// Copyright 2026 The Scion Authors.

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// simulateForeignOwnedHome makes every directory under home unwritable by
// the test process, the way an agent home whose files were written by a
// harness running as another uid (for example uid 1000, with the agent
// runtime running as uid 1002) is unwritable by the agent runtime: entries
// cannot be created, removed or renamed in any of its directories. A
// non-root process cannot chown files to another uid, so the mismatch is
// simulated with permissions; the effect on the runtime's writes is the
// same EACCES. restoreHomeOwnership is what an ownership repair does for
// the runtime.
func simulateForeignOwnedHome(t *testing.T, home string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; the uid mismatch cannot be simulated")
	}
	var dirs []string
	if err := filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Deepest first, so the walk above was not blocked; chmod needs only
	// ownership, not write access to the parent.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(dirs[i], 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = restoreHomeOwnership(home) })
}

// restoreHomeOwnership gives the test process write access to every
// directory under home again: the test's stand-in for the runtime's
// ownership repair.
func restoreHomeOwnership(home string) error {
	if err := os.Chmod(home, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0o755)
		}
		return nil
	})
}

// repairingRuntime is a mock runtime that implements
// runtime.AgentHomeOwnershipRepairer.
type repairingRuntime struct {
	*runtime.MockRuntime
	repairErr error
	repairs   []runtime.AgentHomeOwnershipRepair
}

func (r *repairingRuntime) RepairAgentHomeOwnership(ctx context.Context, req runtime.AgentHomeOwnershipRepair) error {
	r.repairs = append(r.repairs, req)
	if r.repairErr != nil {
		return r.repairErr
	}
	return restoreHomeOwnership(req.HomeDir)
}

func newRepairingRuntime(runs *int) *repairingRuntime {
	return &repairingRuntime{MockRuntime: &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			if runs != nil {
				*runs++
			}
			return "mock-id", nil
		},
	}}
}

const staleWrapperMarker = "# stale wrapper from an earlier launch\n"

// startThenForeignOwnHome starts a container-script agent once, marks its
// staged provisioner wrapper as stale, and then makes its home unwritable
// by the agent runtime.
func startThenForeignOwnHome(t *testing.T, name string) (*policyTestEnv, string) {
	t.Helper()
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	e.projectHC(t, "hc-decl", policyTestDecl)
	if _, err := policyTestManager(nil).Start(context.Background(), api.StartOptions{Name: name, ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	home := config.GetAgentHomePath(e.scion, name)
	wrapper := harness.HarnessProvisionHookPath(home)
	f, err := os.OpenFile(wrapper, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("fixture: staged wrapper: %v", err)
	}
	if _, err := f.WriteString(staleWrapperMarker); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	simulateForeignOwnedHome(t, home)
	return e, home
}

func readSavedRunID(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "agent-info.json"))
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		RunID string `json:"runId"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatal(err)
	}
	return info.RunID
}

// A restart of an existing agent whose home the agent runtime cannot
// write repairs the home's ownership through the runtime once, then clears
// the stale provisioner wrapper and bundle, restages them, and records the
// run ID (ptone/scion#4330).
func TestStart_RestartRepairsForeignOwnedAgentHome(t *testing.T) {
	e, home := startThenForeignOwnHome(t, "own-restart")
	runs := 0
	rt := newRepairingRuntime(&runs)
	opts := api.StartOptions{Name: "own-restart", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true, RunID: "run-after-repair"}
	if _, err := NewManager(rt).Start(context.Background(), opts); err != nil {
		t.Fatalf("restart with a foreign-owned agent home: %v", err)
	}
	if len(rt.repairs) != 1 {
		t.Fatalf("ownership repairs = %d, want exactly 1", len(rt.repairs))
	}
	// Local backend: the advertised owner is the agent runtime itself.
	if got := rt.repairs[0]; got.HomeDir != home || got.Image != "scion-base:test" || got.UID != os.Getuid() || got.GID != os.Getgid() {
		t.Errorf("repair request = %+v, want home %q, the agent image and owner %d:%d", got, home, os.Getuid(), os.Getgid())
	}
	if runs != 1 {
		t.Errorf("runtime Run calls = %d, want 1", runs)
	}
	data, err := os.ReadFile(harness.HarnessProvisionHookPath(home))
	if err != nil {
		t.Fatalf("restaged wrapper: %v", err)
	}
	if strings.Contains(string(data), staleWrapperMarker) {
		t.Error("the stale provisioner wrapper was not removed before restaging")
	}
	if got := readSavedRunID(t, home); got != "run-after-repair" {
		t.Errorf("recorded run ID = %q, want run-after-repair", got)
	}
}

// The stale-wrapper removal still happens on a foreign-owned home when the
// relaunch's harness is not container-script: no wrapper remains.
func TestStart_RelaunchClearsStaleWrapperInForeignOwnedAgentHome(t *testing.T) {
	e, home := startThenForeignOwnHome(t, "own-relaunch")
	rt := newRepairingRuntime(nil)
	opts := api.StartOptions{Name: "own-relaunch", ProjectPath: e.scion, HarnessConfig: "hc-decl", NoAuth: true}
	if _, err := NewManager(rt).Start(context.Background(), opts); err != nil {
		t.Fatalf("relaunch with a foreign-owned agent home: %v", err)
	}
	if len(rt.repairs) != 1 {
		t.Fatalf("ownership repairs = %d, want exactly 1", len(rt.repairs))
	}
	if harness.HarnessProvisionHookStaged(home) {
		t.Error("a stale provisioner wrapper survived the relaunch")
	}
	assertNoStagedProvisioning(t, home)
}

// When the repair fails, the start fails with a clear error naming the
// path and the uid mismatch; nothing is launched, and the stale wrapper is
// not silently left in place for a launch.
func TestStart_RestartFailsClearlyWhenOwnershipRepairFails(t *testing.T) {
	e, home := startThenForeignOwnHome(t, "own-fail")
	runs := 0
	rt := newRepairingRuntime(&runs)
	rt.repairErr = errors.New("helper exited 1")
	opts := api.StartOptions{Name: "own-fail", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true}
	_, err := NewManager(rt).Start(context.Background(), opts)
	assertHomeOwnershipError(t, err, harness.HarnessProvisionHookPath(home), "helper exited 1")
	if len(rt.repairs) != 1 {
		t.Errorf("ownership repairs = %d, want exactly 1 (one attempt per start)", len(rt.repairs))
	}
	if runs != 0 {
		t.Errorf("runtime Run calls = %d, want 0", runs)
	}
}

// A runtime without an ownership repair fails the start with the same
// clear error.
func TestStart_RestartFailsClearlyWithoutOwnershipRepair(t *testing.T) {
	e, home := startThenForeignOwnHome(t, "own-norepair")
	runs := 0
	opts := api.StartOptions{Name: "own-norepair", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true}
	_, err := policyTestManager(&runs).Start(context.Background(), opts)
	assertHomeOwnershipError(t, err, harness.HarnessProvisionHookPath(home), "not supported")
	if runs != 0 {
		t.Errorf("runtime Run calls = %d, want 0", runs)
	}
}

func assertHomeOwnershipError(t *testing.T, err error, path, repairMsg string) {
	t.Helper()
	var ownErr *AgentHomeOwnershipError
	if !errors.As(err, &ownErr) {
		t.Fatalf("expected an *AgentHomeOwnershipError, got %v", err)
	}
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("error does not wrap the permission error: %v", err)
	}
	if ownErr.Path != path {
		t.Errorf("error path = %q, want %q", ownErr.Path, path)
	}
	msg := err.Error()
	for _, want := range []string{
		path,
		"is owned by uid " + strconv.Itoa(os.Getuid()),
		"agent runtime runs as uid " + strconv.Itoa(os.Getuid()),
		repairMsg,
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

// With matching ownership a restart never runs the repair.
func TestStart_RestartWithWritableHomeDoesNotRepair(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	rt := newRepairingRuntime(nil)
	opts := api.StartOptions{Name: "own-match", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true}
	for i := 0; i < 2; i++ {
		if _, err := NewManager(rt).Start(context.Background(), opts); err != nil {
			t.Fatalf("start %d: %v", i+1, err)
		}
	}
	if len(rt.repairs) != 0 {
		t.Errorf("ownership repairs = %d, want 0", len(rt.repairs))
	}
	if !harness.HarnessProvisionHookStaged(config.GetAgentHomePath(e.scion, "own-match")) {
		t.Error("wrapper not staged")
	}
}

func TestWriteAgentHome(t *testing.T) {
	permErr := &fs.PathError{Op: "remove", Path: "/nonexistent/home/x", Err: fs.ErrPermission}
	localOwner := agentHomeOwner{uid: os.Getuid(), gid: os.Getgid()}
	newCtx := func(rt runtime.Runtime) context.Context {
		return contextWithAgentHomeRepair(context.Background(), rt, t.TempDir(), "img:1", localOwner)
	}

	t.Run("other errors are returned unchanged without a repair", func(t *testing.T) {
		rt := newRepairingRuntime(nil)
		other := errors.New("boom")
		if err := writeAgentHome(newCtx(rt), func() error { return other }); err != other {
			t.Fatalf("err = %v, want %v", err, other)
		}
		if len(rt.repairs) != 0 {
			t.Errorf("repairs = %d, want 0", len(rt.repairs))
		}
	})

	t.Run("repairs once per start and retries each write once", func(t *testing.T) {
		rt := newRepairingRuntime(nil)
		ctx := newCtx(rt)
		for i := 0; i < 2; i++ {
			calls := 0
			err := writeAgentHome(ctx, func() error {
				calls++
				if calls == 1 {
					return permErr
				}
				return nil
			})
			if err != nil || calls != 2 {
				t.Fatalf("write %d: err=%v calls=%d, want nil and 2", i+1, err, calls)
			}
		}
		if len(rt.repairs) != 1 {
			t.Errorf("repairs = %d, want 1", len(rt.repairs))
		}
	})

	t.Run("a write still failing after the repair gives the clear error", func(t *testing.T) {
		rt := newRepairingRuntime(nil)
		calls := 0
		err := writeAgentHome(newCtx(rt), func() error { calls++; return permErr })
		var ownErr *AgentHomeOwnershipError
		if !errors.As(err, &ownErr) || ownErr.RepairErr != nil || calls != 2 {
			t.Fatalf("err=%v calls=%d, want an *AgentHomeOwnershipError with no repair error after 2 calls", err, calls)
		}
		if !strings.Contains(err.Error(), "still failed after ownership repair") || !strings.Contains(err.Error(), "/nonexistent/home/x") {
			t.Errorf("unclear error: %v", err)
		}
	})

	t.Run("a missing image fails the repair", func(t *testing.T) {
		rt := newRepairingRuntime(nil)
		ctx := contextWithAgentHomeRepair(context.Background(), rt, t.TempDir(), "", localOwner)
		err := writeAgentHome(ctx, func() error { return permErr })
		var ownErr *AgentHomeOwnershipError
		if !errors.As(err, &ownErr) || ownErr.RepairErr == nil || len(rt.repairs) != 0 {
			t.Fatalf("err=%v repairs=%d, want a repair error and no repair", err, len(rt.repairs))
		}
	})

	t.Run("a missing local image is pulled before the repair", func(t *testing.T) {
		rt := newRepairingRuntime(nil)
		var pulled string
		rt.ImageExistsFunc = func(ctx context.Context, image string) (bool, error) { return false, nil }
		rt.PullImageFunc = func(ctx context.Context, image string) error { pulled = image; return nil }
		calls := 0
		err := writeAgentHome(newCtx(rt), func() error {
			calls++
			if calls == 1 {
				return permErr
			}
			return nil
		})
		if err != nil || pulled != "img:1" || len(rt.repairs) != 1 {
			t.Fatalf("err=%v pulled=%q repairs=%d", err, pulled, len(rt.repairs))
		}
	})

	t.Run("no repair when the advertised owner is not the agent runtime", func(t *testing.T) {
		for _, owner := range []agentHomeOwner{
			{backend: "nfs", uid: 1000, gid: 1000},
			{backend: "nfs", uid: os.Getuid(), gid: os.Getgid()},
			{backend: "", uid: os.Getuid() + 1, gid: os.Getgid()},
		} {
			rt := newRepairingRuntime(nil)
			ctx := contextWithAgentHomeRepair(context.Background(), rt, t.TempDir(), "img:1", owner)
			calls := 0
			err := writeAgentHome(ctx, func() error { calls++; return permErr })
			var ownErr *AgentHomeOwnershipError
			if !errors.As(err, &ownErr) || !errors.Is(ownErr.RepairErr, errAgentHomeRepairNotAllowed) {
				t.Fatalf("owner %+v: err = %v, want a not-allowed repair error", owner, err)
			}
			if len(rt.repairs) != 0 || calls != 1 {
				t.Errorf("owner %+v: repairs=%d calls=%d, want no repair and no retry", owner, len(rt.repairs), calls)
			}
			for _, want := range []string{"must be able to write the agent home", "ptone/scion#4402", strconv.Itoa(owner.uid)} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("owner %+v: error %q lacks %q", owner, err, want)
				}
			}
		}
	})
}

// The repair target is the owner the container runtime advertises for the
// configured workspace backend: the agent runtime's own ids, or the stable
// NFS ids on the nfs backend.
func TestAdvertisedAgentHomeOwner(t *testing.T) {
	ws := func(w *config.V1WorkspaceStorageConfig) *config.VersionedSettings {
		return &config.VersionedSettings{Server: &config.V1ServerConfig{WorkspaceStorage: w}}
	}
	self := func(backend string) agentHomeOwner {
		return agentHomeOwner{backend: backend, uid: os.Getuid(), gid: os.Getgid()}
	}
	for _, tc := range []struct {
		name     string
		settings *config.VersionedSettings
		want     agentHomeOwner
	}{
		{"no settings", nil, self("")},
		{"no workspace storage", &config.VersionedSettings{Server: &config.V1ServerConfig{}}, self("")},
		{"local backend", ws(&config.V1WorkspaceStorageConfig{Backend: "local"}), self("local")},
		{"local backend ignores nfs ids", ws(&config.V1WorkspaceStorageConfig{Backend: "local", NFS: &config.V1NFSConfig{UID: 2001, GID: 2002}}), self("local")},
		{"nfs default ids", ws(&config.V1WorkspaceStorageConfig{Backend: "nfs"}), agentHomeOwner{backend: "nfs", uid: 1000, gid: 1000}},
		{"nfs configured ids", ws(&config.V1WorkspaceStorageConfig{Backend: "nfs", NFS: &config.V1NFSConfig{UID: 2001, GID: 2002}}), agentHomeOwner{backend: "nfs", uid: 2001, gid: 2002}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := advertisedAgentHomeOwner(tc.settings); got != tc.want {
				t.Errorf("owner = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// foreignOwnDir makes only dir unwritable by the test process, simulating
// one directory of the agent home owned by another uid.
func foreignOwnDir(t *testing.T, home, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions; the uid mismatch cannot be simulated")
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restoreHomeOwnership(home) })
}

// With no stale wrapper to remove, a foreign-owned pre-start.d still fails
// the project hook restage unless that write is repaired too.
func TestStart_RestageProjectHookInForeignOwnedPreStartDir(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-decl", policyTestDecl)
	opts := api.StartOptions{Name: "own-hook", ProjectPath: e.scion, HarnessConfig: "hc-decl", NoAuth: true}
	if _, err := policyTestManager(nil).Start(context.Background(), opts); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	home := config.GetAgentHomePath(e.scion, "own-hook")
	preStart := filepath.Join(home, ".scion", "hooks", "pre-start.d")
	if err := os.MkdirAll(preStart, 0o755); err != nil {
		t.Fatal(err)
	}
	if harness.HarnessProvisionHookStaged(home) {
		t.Fatal("fixture: a declarative harness-config stages no wrapper")
	}
	foreignOwnDir(t, home, preStart)

	rt := newRepairingRuntime(nil)
	opts.ProjectPreStartHookScript = "#!/bin/sh\necho project hook\n"
	if _, err := NewManager(rt).Start(context.Background(), opts); err != nil {
		t.Fatalf("restart with a foreign-owned pre-start.d: %v", err)
	}
	if len(rt.repairs) != 1 {
		t.Fatalf("ownership repairs = %d, want exactly 1", len(rt.repairs))
	}
	entries, err := os.ReadDir(preStart)
	if err != nil || len(entries) == 0 {
		t.Fatalf("project pre-start hook not restaged (entries=%v, err=%v)", entries, err)
	}
}

// With the staged provisioning already cleared, a foreign-owned .scion
// still fails the container-script restage of the control-plane inputs
// unless that write is repaired too.
func TestStart_RestageContainerScriptInputsInForeignOwnedScionDir(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-scripted", policyTestScripted)
	opts := api.StartOptions{Name: "own-inputs", ProjectPath: e.scion, HarnessConfig: "hc-scripted", NoAuth: true}
	if _, err := policyTestManager(nil).Start(context.Background(), opts); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	home := config.GetAgentHomePath(e.scion, "own-inputs")
	if err := harness.ClearStagedProvisioning(home); err != nil {
		t.Fatal(err)
	}
	foreignOwnDir(t, home, filepath.Join(home, ".scion"))

	rt := newRepairingRuntime(nil)
	if _, err := NewManager(rt).Start(context.Background(), opts); err != nil {
		t.Fatalf("restart with a foreign-owned .scion: %v", err)
	}
	if len(rt.repairs) != 1 {
		t.Fatalf("ownership repairs = %d, want exactly 1", len(rt.repairs))
	}
	if !harness.HarnessProvisionHookStaged(home) {
		t.Error("the container-script wrapper was not restaged")
	}
}
