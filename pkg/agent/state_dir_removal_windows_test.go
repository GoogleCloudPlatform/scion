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
//go:build windows

package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"
)

// TestWindowsRetryRemovalInUserNamespace_ReturnsOriginal checks that off unix
// a failed state directory removal is returned unchanged: there is no podman
// unshare retry, for errors.ErrUnsupported or a permission error.
func TestWindowsRetryRemovalInUserNamespace_ReturnsOriginal(t *testing.T) {
	for _, removeErr := range []error{
		fmt.Errorf("remove symlink: %w on this platform", errors.ErrUnsupported),
		&fs.PathError{Op: "remove", Path: "C:\\state\\agent", Err: fs.ErrPermission},
	} {
		if err := retryRemovalInUserNamespace("C:\\state\\agent", removeErr); err != removeErr {
			t.Fatalf("retryRemovalInUserNamespace(%v) = %v; want the error unchanged", removeErr, err)
		}
	}
}
