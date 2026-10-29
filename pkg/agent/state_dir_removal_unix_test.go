//go:build unix

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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// stubPodmanUnshare replaces podmanUnshareRemove with one that records the
// paths it is called with and returns err.
func stubPodmanUnshare(t *testing.T, err error) *[]string {
	t.Helper()
	var calls []string
	setSeam(t, &podmanUnshareRemove, func(_ context.Context, path string) error {
		calls = append(calls, path)
		return err
	})
	return &calls
}

// TestRetryRemovalInUserNamespace_PermissionAndNotEmptyOnly checks that the podman
// unshare retry is taken only for a permission error or ENOTEMPTY (a
// mode-700 subuid-owned directory that is not empty), and that every other
// error is returned unchanged.
func TestRetryRemovalInUserNamespace_PermissionAndNotEmptyOnly(t *testing.T) {
	const path = "/state/agent"
	tests := []struct {
		name      string
		removeErr error
		wantRetry bool
	}{
		{"EACCES", &fs.PathError{Op: "remove", Path: path, Err: syscall.EACCES}, true},
		{"EPERM", &fs.PathError{Op: "remove", Path: path, Err: syscall.EPERM}, true},
		{"fs.ErrPermission", fmt.Errorf("remove: %w", fs.ErrPermission), true},
		{"ErrUnsupported", fmt.Errorf("remove symlink: %w", errors.ErrUnsupported), false},
		{"ErrUnsupported and permission", fmt.Errorf("%w: %w", errors.ErrUnsupported, fs.ErrPermission), false},
		{"ENOTSUP", &fs.PathError{Op: "remove", Path: path, Err: syscall.ENOTSUP}, false},
		{"ENOTEMPTY", &fs.PathError{Op: "remove", Path: path, Err: syscall.ENOTEMPTY}, true},
		{"ErrUnsupported and ENOTEMPTY", fmt.Errorf("%w: %w", errors.ErrUnsupported, syscall.ENOTEMPTY), false},
		{"ENOENT", &fs.PathError{Op: "remove", Path: path, Err: syscall.ENOENT}, false},
		{"EBUSY", &fs.PathError{Op: "remove", Path: path, Err: syscall.EBUSY}, false},
		{"other", errors.New("some other failure"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := stubPodmanUnshare(t, nil)
			err := retryRemovalInUserNamespace(path, tt.removeErr)
			if tt.wantRetry {
				if err != nil {
					t.Fatalf("err = %v; want nil after a successful retry", err)
				}
				if len(*calls) != 1 || (*calls)[0] != path {
					t.Fatalf("podman unshare calls = %v; want [%s]", *calls, path)
				}
				return
			}
			if err != tt.removeErr {
				t.Fatalf("err = %v; want the removal error returned unchanged (%v)", err, tt.removeErr)
			}
			if len(*calls) != 0 {
				t.Fatalf("podman unshare called %v; want no retry", *calls)
			}
		})
	}
}

// TestRetryRemovalInUserNamespace_RetryFailureReturned checks that a failed
// retry is returned, wrapping both the removal error and the retry's error.
func TestRetryRemovalInUserNamespace_RetryFailureReturned(t *testing.T) {
	removeErr := &fs.PathError{Op: "remove", Path: "/state/agent", Err: syscall.EACCES}
	retryErr := errors.New("podman: not found")
	calls := stubPodmanUnshare(t, retryErr)

	err := retryRemovalInUserNamespace("/state/agent", removeErr)
	if !errors.Is(err, syscall.EACCES) || !errors.Is(err, retryErr) {
		t.Fatalf("err = %v; want it to wrap both EACCES and the retry error", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("podman unshare calls = %v; want one", *calls)
	}
}

// failRemoveAllSafeWith makes removeAllSafe fail with err for the directory
// named name, and remove every other path normally.
func failRemoveAllSafeWith(t *testing.T, name string, err error) {
	t.Helper()
	orig := removeAllSafe
	setSeam(t, &removeAllSafe, func(path string) error {
		if filepath.Base(path) == name {
			return err
		}
		return orig(path)
	})
}

// TestDeleteAgentFiles_ExternalStateDirPermissionRetried checks the external
// state directory's retry path end to end: a successful podman unshare retry
// (after a permission error or ENOTEMPTY) lets the delete succeed; a failed
// one is returned.
func TestDeleteAgentFiles_ExternalStateDirPermissionRetried(t *testing.T) {
	permErr := &fs.PathError{Op: "remove", Path: "x", Err: syscall.EACCES}

	t.Run("retry succeeds", func(t *testing.T) {
		const agentName = "ext-perm-ok"
		scionDir, extAgentDir := setupExternalStateDir(t, agentName)
		failRemoveAllSafeWith(t, agentName, permErr)
		calls := stubPodmanUnshare(t, nil)

		if _, err := DeleteAgentFiles(agentName, scionDir, false); err != nil {
			t.Fatalf("DeleteAgentFiles: %v", err)
		}
		if len(*calls) != 1 || filepath.Base((*calls)[0]) != agentName {
			t.Fatalf("podman unshare calls = %v; want one for %s", *calls, extAgentDir)
		}
	})

	t.Run("not empty retried", func(t *testing.T) {
		const agentName = "ext-notempty"
		scionDir, _ := setupExternalStateDir(t, agentName)
		failRemoveAllSafeWith(t, agentName, &fs.PathError{Op: "remove", Path: "x", Err: syscall.ENOTEMPTY})
		calls := stubPodmanUnshare(t, nil)

		if _, err := DeleteAgentFiles(agentName, scionDir, false); err != nil {
			t.Fatalf("DeleteAgentFiles: %v", err)
		}
		if len(*calls) != 1 {
			t.Fatalf("podman unshare calls = %v; want one", *calls)
		}
	})

	t.Run("retry fails", func(t *testing.T) {
		const agentName = "ext-perm-fail"
		scionDir, extAgentDir := setupExternalStateDir(t, agentName)
		failRemoveAllSafeWith(t, agentName, permErr)
		retryErr := errors.New("podman: not found")
		stubPodmanUnshare(t, retryErr)

		_, err := DeleteAgentFiles(agentName, scionDir, false)
		if !errors.Is(err, syscall.EACCES) || !errors.Is(err, retryErr) {
			t.Fatalf("DeleteAgentFiles error = %v; want the removal and retry errors", err)
		}
		if _, statErr := os.Stat(extAgentDir); statErr != nil {
			t.Fatalf("external state dir should remain: %v", statErr)
		}
	})

	t.Run("unsupported is not retried", func(t *testing.T) {
		const agentName = "ext-unsupported"
		scionDir, _ := setupExternalStateDir(t, agentName)
		unsupported := fmt.Errorf("remove symlink: %w", errors.ErrUnsupported)
		failRemoveAllSafeWith(t, agentName, unsupported)
		calls := stubPodmanUnshare(t, nil)

		_, err := DeleteAgentFiles(agentName, scionDir, false)
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("DeleteAgentFiles error = %v; want errors.ErrUnsupported", err)
		}
		if len(*calls) != 0 {
			t.Fatalf("podman unshare called %v; want no retry", *calls)
		}
	})
}
