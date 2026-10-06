/*
Copyright 2026 The Scion Authors.
*/
package commands

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

func TestProvisionCmd_WaitForSentinel_Found(t *testing.T) {
	dir := t.TempDir()

	sentinelPath := filepath.Join(dir, ".scion-provisioned")
	if err := os.WriteFile(sentinelPath, []byte("provisioned_at=test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	oldWorkspace := provisionWorkspace
	oldWait := provisionWaitSentinel
	oldTimeout := provisionTimeout
	oldInterval := provisionPollInterval
	defer func() {
		provisionWorkspace = oldWorkspace
		provisionWaitSentinel = oldWait
		provisionTimeout = oldTimeout
		provisionPollInterval = oldInterval
	}()

	provisionWorkspace = dir
	provisionWaitSentinel = true
	provisionTimeout = 5
	provisionPollInterval = 1

	if err := runWaitForSentinel(context.Background()); err != nil {
		t.Fatalf("expected success when sentinel exists, got: %v", err)
	}
}

func TestProvisionCmd_WaitForSentinel_Timeout(t *testing.T) {
	dir := t.TempDir()

	oldWorkspace := provisionWorkspace
	oldWait := provisionWaitSentinel
	oldTimeout := provisionTimeout
	oldInterval := provisionPollInterval
	defer func() {
		provisionWorkspace = oldWorkspace
		provisionWaitSentinel = oldWait
		provisionTimeout = oldTimeout
		provisionPollInterval = oldInterval
	}()

	provisionWorkspace = dir
	provisionWaitSentinel = true
	provisionTimeout = 3
	provisionPollInterval = 1

	start := time.Now()
	err := runWaitForSentinel(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error when sentinel is missing")
	}
	if elapsed < 2*time.Second {
		t.Errorf("should have waited at least 2s, only waited %s", elapsed)
	}
}

func TestProvisionCmd_WaitForSentinel_DelayedWrite(t *testing.T) {
	dir := t.TempDir()

	oldWorkspace := provisionWorkspace
	oldWait := provisionWaitSentinel
	oldTimeout := provisionTimeout
	oldInterval := provisionPollInterval
	defer func() {
		provisionWorkspace = oldWorkspace
		provisionWaitSentinel = oldWait
		provisionTimeout = oldTimeout
		provisionPollInterval = oldInterval
	}()

	provisionWorkspace = dir
	provisionWaitSentinel = true
	provisionTimeout = 10
	provisionPollInterval = 1

	go func() {
		time.Sleep(2 * time.Second)
		sentinelPath := filepath.Join(dir, ".scion-provisioned")
		_ = os.WriteFile(sentinelPath, []byte("provisioned_at=test\n"), 0644)
	}()

	if err := runWaitForSentinel(context.Background()); err != nil {
		t.Fatalf("expected success after delayed sentinel write, got: %v", err)
	}
}

func TestProvisionCmd_WaitForSentinel_ContextCancel(t *testing.T) {
	dir := t.TempDir()

	oldWorkspace := provisionWorkspace
	oldWait := provisionWaitSentinel
	oldTimeout := provisionTimeout
	oldInterval := provisionPollInterval
	defer func() {
		provisionWorkspace = oldWorkspace
		provisionWaitSentinel = oldWait
		provisionTimeout = oldTimeout
		provisionPollInterval = oldInterval
	}()

	provisionWorkspace = dir
	provisionWaitSentinel = true
	// Long timeout/interval: the loop would block well past the test budget if
	// cancellation were not honoured.
	provisionTimeout = 60
	provisionPollInterval = 30

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := runWaitForSentinel(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error when context is cancelled")
	}
	if elapsed > 5*time.Second {
		t.Errorf("cancellation should interrupt the poll sleep promptly, waited %s", elapsed)
	}
}

func TestProvisionCmd_Clone_Idempotent(t *testing.T) {
	dir := t.TempDir()
	wsDir := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsDir, 0770); err != nil {
		t.Fatal(err)
	}

	sentinelPath := filepath.Join(wsDir, ".scion-provisioned")
	if err := os.WriteFile(sentinelPath, []byte("provisioned_at=test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	oldWorkspace := provisionWorkspace
	oldMode := provisionMode
	oldDepth := provisionDepth
	oldUID := provisionUID
	oldGID := provisionGID
	defer func() {
		provisionWorkspace = oldWorkspace
		provisionMode = oldMode
		provisionDepth = oldDepth
		provisionUID = oldUID
		provisionGID = oldGID
	}()

	provisionWorkspace = wsDir
	provisionMode = "shared-plain"
	provisionDepth = 1
	provisionUID = os.Getuid()
	provisionGID = os.Getgid()

	t.Setenv("SCION_CLONE_URL", "https://nonexistent.example.com/repo.git")
	t.Setenv("SCION_CLONE_BRANCH", "main")
	t.Setenv("SCION_WORKSPACE_MODE", "")
	t.Setenv("SCION_PROJECT_ID", "test-proj")

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("idempotent provision (sentinel exists) should succeed, got: %v", err)
	}
}

// TestProvisionCmd_SharedDirPaths_ParsedAndProvisioned pins the F-111 review
// fix (tf-lead nit): SCION_SHARED_DIR_PATHS carries "name=mountPath" pairs,
// keyed explicitly, not bare paths keyed by filepath.Base on this side. Two
// entries with the SAME basename but different full paths (the exact
// collision risk named in review — InWorkspace vs not can produce this) must
// both still be mkdir'd, proving the key actually came from the name field,
// not a re-derived basename that would have collided.
func TestProvisionCmd_SharedDirPaths_ParsedAndProvisioned(t *testing.T) {
	dir := t.TempDir()
	sharedRootA := filepath.Join(dir, "a", "scratchpad")
	sharedRootB := filepath.Join(dir, "b", "scratchpad") // same basename as A, different path

	oldWorkspace := provisionWorkspace
	oldMode := provisionMode
	oldUID := provisionUID
	oldGID := provisionGID
	defer func() {
		provisionWorkspace = oldWorkspace
		provisionMode = oldMode
		provisionUID = oldUID
		provisionGID = oldGID
	}()

	provisionWorkspace = filepath.Join(dir, "workspace")
	provisionMode = "shared-plain"
	provisionUID = os.Getuid()
	provisionGID = os.Getgid()

	t.Setenv("SCION_CLONE_URL", "")
	t.Setenv("SCION_WORKSPACE_MODE", "")
	t.Setenv("SCION_PROJECT_ID", "test-proj-shared-dirs")
	t.Setenv("SCION_SHARED_DIR_PATHS", "scratchpad="+sharedRootA+",other-scratchpad="+sharedRootB)

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("provision with shared dirs should succeed, got: %v", err)
	}

	for _, p := range []string{sharedRootA, sharedRootB} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("shared dir %s was not created: %v", p, err)
		}
	}
}

func TestProvisionCmd_Clone_NoURL(t *testing.T) {
	dir := t.TempDir()

	oldWorkspace := provisionWorkspace
	oldMode := provisionMode
	oldDepth := provisionDepth
	oldUID := provisionUID
	oldGID := provisionGID
	defer func() {
		provisionWorkspace = oldWorkspace
		provisionMode = oldMode
		provisionDepth = oldDepth
		provisionUID = oldUID
		provisionGID = oldGID
	}()

	provisionWorkspace = dir
	provisionMode = "shared-plain"
	provisionDepth = 1
	provisionUID = os.Getuid()
	provisionGID = os.Getgid()

	t.Setenv("SCION_CLONE_URL", "")
	t.Setenv("SCION_CLONE_BRANCH", "")
	t.Setenv("SCION_WORKSPACE_MODE", "")
	t.Setenv("SCION_PROJECT_ID", "test-proj-no-url")

	if err := runProvision(context.Background()); err != nil {
		t.Fatalf("provision without clone URL should succeed (non-git project), got: %v", err)
	}

	sentinelPath := filepath.Join(dir, ".scion-provisioned")
	if _, err := os.Stat(sentinelPath); err != nil {
		t.Errorf("sentinel should be written for non-git project: %v", err)
	}
}

// runProvisionWithFailingChown runs the real provisioning path with a chown
// target uid this (non-root) test process cannot chown to, so the chown
// fails for real, and reports the error and whether the sentinel was written.
func runProvisionWithFailingChown(t *testing.T, bestEffortValue *string) (error, bool) {
	t.Helper()
	if os.Getuid() == 0 {
		t.Skip("running as root: chown to another uid would succeed")
	}
	workspace := filepath.Join(t.TempDir(), "workspace")

	t.Setenv("SCION_CLONE_URL", "")
	t.Setenv("SCION_SHARED_DIR_PATHS", "")
	t.Setenv("SCION_WORKSPACE_MODE", "")
	t.Setenv("SCION_PROJECT_ID", "proj-chown")
	if bestEffortValue != nil {
		t.Setenv(provision.ChownBestEffortEnv, *bestEffortValue)
	} else {
		t.Setenv(provision.ChownBestEffortEnv, "")
		_ = os.Unsetenv(provision.ChownBestEffortEnv)
	}

	oldWorkspace, oldMode, oldUID, oldGID := provisionWorkspace, provisionMode, provisionUID, provisionGID
	t.Cleanup(func() {
		provisionWorkspace, provisionMode, provisionUID, provisionGID = oldWorkspace, oldMode, oldUID, oldGID
	})
	provisionWorkspace = workspace
	provisionMode = "shared-plain"
	provisionUID = os.Getuid() + 1
	provisionGID = os.Getgid()

	err := runProvision(context.Background())
	_, statErr := os.Stat(filepath.Join(workspace, provision.ProvisionSentinelFile))
	return err, statErr == nil
}

// Without the variable (what an older pod spec, or any pod whose workspace
// directory the broker did not create, looks like) a failed chown stays
// fatal and no sentinel is written: the behavior before this change.
func TestRunProvision_ChownFailure_FatalByDefault(t *testing.T) {
	err, sentinel := runProvisionWithFailingChown(t, nil)
	if err == nil || !strings.Contains(err.Error(), "chown") {
		t.Fatalf("expected a chown error, got %v", err)
	}
	if sentinel {
		t.Error("sentinel must not be written when the chown is required and fails")
	}
}

// Only the exact value "1" relaxes the chown; anything else stays strict.
func TestRunProvision_ChownFailure_UnrecognizedValueStaysFatal(t *testing.T) {
	for _, v := range []string{"", "0", "true", "yes", " 1", "1 "} {
		t.Run("value="+v, func(t *testing.T) {
			value := v
			err, sentinel := runProvisionWithFailingChown(t, &value)
			if err == nil {
				t.Fatalf("value %q: expected a chown error", v)
			}
			if sentinel {
				t.Errorf("value %q: sentinel must not be written", v)
			}
		})
	}
}

// With the variable set to "1" (broker-created workspace directory) the
// failed chown is logged, provisioning completes and the sentinel is written.
func TestRunProvision_ChownFailure_BestEffortWhenRequested(t *testing.T) {
	value := "1"
	err, sentinel := runProvisionWithFailingChown(t, &value)
	if err != nil {
		t.Fatalf("expected provisioning to succeed despite the chown failure, got %v", err)
	}
	if !sentinel {
		t.Error("sentinel must be written after best-effort provisioning")
	}
}

// TestValidateProvisionOwner covers the --uid/--gid range check: 0 keeps
// meaning "use the default 1000" (provision.resolveUID), so it is accepted;
// negative values (including -1, which chown reads as "leave unchanged")
// and values above fsutil.MaxOwnerID are rejected with the flag named.
func TestValidateProvisionOwner(t *testing.T) {
	tests := []struct {
		name     string
		uid, gid int64
		wantFlag string
	}{
		{"defaults", 1000, 1000, ""},
		{"zero means default", 0, 0, ""},
		{"maximum", 4294967294, 4294967294, ""},
		{"negative one uid", -1, 1000, "--uid"},
		{"negative one gid", 1000, -1, "--gid"},
		{"negative uid", -5, 1000, "--uid"},
		{"unsigned sentinel uid", 4294967295, 1000, "--uid"},
		{"out of range gid", 1000, 1 << 33, "--gid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A 32-bit int cannot hold ids above math.MaxInt32, so the
			// int conversions below would wrap and test a different value.
			if strconv.IntSize < 64 && max(tt.uid, tt.gid) > math.MaxInt32 {
				t.Skipf("%d/%d does not fit in a %d-bit int", tt.uid, tt.gid, strconv.IntSize)
			}
			err := validateProvisionOwner(int(tt.uid), int(tt.gid))
			if tt.wantFlag == "" {
				if err != nil {
					t.Fatalf("validateProvisionOwner(%d, %d) = %v, want nil", tt.uid, tt.gid, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantFlag) {
				t.Fatalf("validateProvisionOwner(%d, %d) = %v, want an error naming %s", tt.uid, tt.gid, err, tt.wantFlag)
			}
		})
	}
}

// TestRunProvision_RejectsInvalidOwnerBeforeChanges checks that an invalid
// --uid stops provisioning before anything is created: no workspace
// directory and no sentinel.
func TestRunProvision_RejectsInvalidOwnerBeforeChanges(t *testing.T) {
	for _, uid := range []int{-1, -1000} {
		t.Run(strconv.Itoa(uid), func(t *testing.T) {
			workspace := filepath.Join(t.TempDir(), "workspace")
			t.Setenv("SCION_CLONE_URL", "")
			t.Setenv("SCION_SHARED_DIR_PATHS", "")
			t.Setenv("SCION_WORKSPACE_MODE", "")
			t.Setenv("SCION_PROVISION_STATE_DIR", "")
			t.Setenv("SCION_PROJECT_ID", "proj-owner-check")

			oldWorkspace, oldMode, oldUID, oldGID := provisionWorkspace, provisionMode, provisionUID, provisionGID
			t.Cleanup(func() {
				provisionWorkspace, provisionMode, provisionUID, provisionGID = oldWorkspace, oldMode, oldUID, oldGID
			})
			provisionWorkspace = workspace
			provisionMode = "shared-plain"
			provisionUID = uid
			provisionGID = os.Getgid()

			err := runProvision(context.Background())
			if err == nil || !strings.Contains(err.Error(), "--uid") {
				t.Fatalf("runProvision with --uid %d = %v, want an error naming --uid", uid, err)
			}
			if _, statErr := os.Stat(workspace); !os.IsNotExist(statErr) {
				t.Errorf("workspace %s was created (stat err %v); want no changes", workspace, statErr)
			}
		})
	}
}
