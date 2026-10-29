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
	"os/exec"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// podmanUnshareRemove runs `podman unshare rm -rf path`. Tests replace it.
var podmanUnshareRemove = func(ctx context.Context, path string) error {
	return exec.CommandContext(ctx, "podman", "unshare", "rm", "-rf", path).Run()
}

// retryRemovalInUserNamespace handles a failed removal of an agent's
// external state directory at path, where removeErr is the error that
// removal returned.
//
// Rootless podman maps the container's root to a subuid on the host, so files
// an agent created as root are owned by that subuid and the host user cannot
// remove them: the removal fails with EACCES or EPERM. `podman unshare` runs
// the removal inside the user namespace, where those subuids map back to the
// user and the files can be removed.
//
// The retry is taken only for a permission error (fs.ErrPermission: EACCES,
// EPERM) or ENOTEMPTY. ENOTEMPTY is the common rootless case: when a
// subuid-owned directory is mode 700 and not empty, RemoveAllSafe's walk
// cannot list it and skips the listing's EACCES, so the error that surfaces
// is the later rmdir's ENOTEMPTY. Every other error, including one wrapping
// errors.ErrUnsupported, is returned unchanged. When the retry fails, the
// returned error wraps both removeErr and the retry's error.
func retryRemovalInUserNamespace(path string, removeErr error) error {
	if errors.Is(removeErr, errors.ErrUnsupported) ||
		(!errors.Is(removeErr, fs.ErrPermission) && !errors.Is(removeErr, syscall.ENOTEMPTY)) {
		return removeErr
	}
	util.Debugf("delete: removal of %s was refused (%v); retrying with podman unshare", path, removeErr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := podmanUnshareRemove(ctx, path); err != nil {
		return fmt.Errorf("%w; podman unshare retry failed: %w", removeErr, err)
	}
	return nil
}
