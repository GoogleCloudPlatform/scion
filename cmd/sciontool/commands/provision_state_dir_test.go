/*
Copyright 2026 The Scion Authors.
*/
package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// newStateDirLayout returns <tmp>/projects/proj-1/{workspace,provision}, as
// the init container sees them through its two subPath mounts.
func newStateDirLayout(t *testing.T) (workspace, stateDir string) {
	t.Helper()
	projectDir := filepath.Join(t.TempDir(), "projects", "proj-1")
	workspace = filepath.Join(projectDir, "workspace")
	stateDir = filepath.Join(projectDir, "provision")
	for _, dir := range []string{workspace, stateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return workspace, stateDir
}

func dirMode(t *testing.T, dir string) uint32 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		t.Fatal(err)
	}
	return st.Mode & 0o7777
}

func TestProvisionStateDir(t *testing.T) {
	const ws = "/workspace"
	for _, tc := range []struct {
		value   string
		want    string
		wantErr bool
	}{
		{value: "", want: ""},
		{value: "/scion-provision", want: "/scion-provision"},
		{value: "scion-provision", wantErr: true},
		{value: "/scion-provision/", wantErr: true},
		{value: "/a/../scion-provision", wantErr: true},
		{value: "/", wantErr: true},
		{value: "/workspace", wantErr: true},
		{value: "/workspace/provision", wantErr: true},
		{value: "/workspace-provision", want: "/workspace-provision"},
	} {
		got, err := provisionStateDir(func(key string) string {
			if key == provisionStateDirEnv {
				return tc.value
			}
			return ""
		}, ws)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected an error, got %q", tc.value, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q: got %q, %v; want %q", tc.value, got, err, tc.want)
		}
	}
	// The workspace inside the state directory is rejected too.
	if _, err := provisionStateDir(func(string) string { return "/data" }, "/data/workspace"); err == nil {
		t.Error("a state directory containing the workspace must be rejected")
	}
}

// With the state directory mounted (#2670) the sentinel and the lock are
// kept there: the clone lands in the workspace, which holds no sentinel and
// no live lock. Without the broker's preparation the directory is given to
// the workspace owner with mode 2775.
func TestRunProvision_StateDir_SentinelOutsideWorkspace(t *testing.T) {
	origin := provisionTestRepo(t)
	workspace, stateDir := newStateDirLayout(t)
	setupProvisionCmd(t, workspace, "shared-plain", origin)
	t.Setenv(provisionStateDirEnv, stateDir)

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("runProvision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "README.md")); err != nil {
		t.Errorf("clone missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, provision.ProvisionSentinelFile)); err != nil {
		t.Errorf("sentinel not in the state directory: %v", err)
	}
	for _, name := range []string{provision.ProvisionSentinelFile, ".scion-provision.lock"} {
		if _, err := os.Lstat(filepath.Join(workspace, name)); err == nil {
			t.Errorf("%s left in the workspace root", name)
		}
	}
	if got := dirMode(t, stateDir); got != 0o2775 {
		t.Errorf("state directory mode = %o, want 2775", got)
	}
	if out := gitT(t, workspace, "status", "--porcelain", "--ignored=no"); out != "" {
		t.Errorf("git status of the workspace is not clean:\n%s", out)
	}

	// A waiter sees the sentinel in the state directory.
	setWaitFlags(t, workspace)
	if err := runWaitForSentinel(context.Background()); err != nil {
		t.Errorf("wait-for-sentinel: %v", err)
	}
}

// A state directory the broker prepared (ChownBestEffortEnv) is left as it
// is.
func TestRunProvision_StateDir_BrokerPreparedLeftAlone(t *testing.T) {
	workspace, stateDir := newStateDirLayout(t)
	setupProvisionCmd(t, workspace, "shared-plain", "")
	t.Setenv(provisionStateDirEnv, stateDir)
	t.Setenv(provision.ChownBestEffortEnv, "1")

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("runProvision: %v", err)
	}
	if got := dirMode(t, stateDir); got != 0o755 {
		t.Errorf("state directory mode = %o, want it unchanged (755)", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, provision.ProvisionSentinelFile)); err != nil {
		t.Errorf("sentinel not in the state directory: %v", err)
	}
}

// A state directory that is a symlink or a regular file fails the init
// container, and nothing is provisioned.
func TestRunProvision_StateDir_NotADirectoryFailsClosed(t *testing.T) {
	for _, kind := range []string{"symlink", "file"} {
		t.Run(kind, func(t *testing.T) {
			workspace, stateDir := newStateDirLayout(t)
			if err := os.Remove(stateDir); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				if err := os.Symlink(t.TempDir(), stateDir); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(stateDir, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			setupProvisionCmd(t, workspace, "shared-plain", "")
			t.Setenv(provisionStateDirEnv, stateDir)

			err := runProvision(context.Background())
			if err == nil || !strings.Contains(err.Error(), "not a directory") {
				t.Fatalf("expected a not-a-directory error, got %v", err)
			}
			if _, err := os.Lstat(filepath.Join(workspace, provision.ProvisionSentinelFile)); err == nil {
				t.Error("sentinel written despite the failure")
			}
		})
	}
}

// A workspace provisioned before the state directory existed (sentinel in
// the workspace root) is not provisioned again: the clone URL here does not
// exist, and no new sentinel is written.
func TestRunProvision_StateDir_LegacySentinelHonoured(t *testing.T) {
	workspace, stateDir := newStateDirLayout(t)
	if err := os.WriteFile(filepath.Join(workspace, provision.ProvisionSentinelFile), []byte("provisioned_at=test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setupProvisionCmd(t, workspace, "shared-plain", filepath.Join(t.TempDir(), "missing.git"))
	t.Setenv(provisionStateDirEnv, stateDir)

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("runProvision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, provision.ProvisionSentinelFile)); err == nil {
		t.Error("a new sentinel was written for an already-provisioned workspace")
	}

	// A waiter accepts the legacy sentinel too.
	setWaitFlags(t, workspace)
	if err := runWaitForSentinel(context.Background()); err != nil {
		t.Errorf("wait-for-sentinel with the legacy sentinel: %v", err)
	}
}

// The agent-directory modes ignore the variable: their sentinel stays in the
// mounted agent directory.
func TestRunProvision_StateDir_IgnoredInAgentDirMode(t *testing.T) {
	agentDir := newTestAgentDir(t)
	setupProvisionCmd(t, agentDir, "shared-plain", "")
	setAgentDirEnv(t, "agent-1", "scion/agent-1")
	t.Setenv(provisionStateDirEnv, filepath.Join(t.TempDir(), "absent"))

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("runProvision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentDir, provision.ProvisionSentinelFile)); err != nil {
		t.Errorf("sentinel not in the agent directory: %v", err)
	}
}

// An invalid variable fails the waiter instead of polling the wrong place.
func TestWaitForSentinel_InvalidStateDir(t *testing.T) {
	setWaitFlags(t, t.TempDir())
	t.Setenv(provisionStateDirEnv, "relative")
	if err := runWaitForSentinel(context.Background()); err == nil {
		t.Fatal("expected an error for a relative state directory")
	}
}

// setWaitFlags sets the --wait-for-sentinel flags for a short wait.
func setWaitFlags(t *testing.T, workspace string) {
	t.Helper()
	oldWorkspace, oldWait, oldTimeout, oldInterval := provisionWorkspace, provisionWaitSentinel, provisionTimeout, provisionPollInterval
	t.Cleanup(func() {
		provisionWorkspace, provisionWaitSentinel, provisionTimeout, provisionPollInterval = oldWorkspace, oldWait, oldTimeout, oldInterval
	})
	provisionWorkspace = workspace
	provisionWaitSentinel = true
	provisionTimeout = 2
	provisionPollInterval = 1
}

// A waiter that cannot search the state directory (EACCES) keeps falling
// back to the legacy sentinel in the workspace root, and otherwise keeps
// waiting until its timeout.
func TestWaitForSentinel_StateDirUnreadableFallsBackToLegacy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	workspace, stateDir := newStateDirLayout(t)
	if err := os.WriteFile(filepath.Join(stateDir, provision.ProvisionSentinelFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o755) })
	setWaitFlags(t, workspace)
	t.Setenv(provisionStateDirEnv, stateDir)

	if err := runWaitForSentinel(context.Background()); err == nil {
		t.Fatal("expected a timeout: the state directory cannot be searched and there is no legacy sentinel")
	}
	if err := os.WriteFile(filepath.Join(workspace, provision.ProvisionSentinelFile), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runWaitForSentinel(context.Background()); err != nil {
		t.Errorf("legacy sentinel not accepted: %v", err)
	}
}
