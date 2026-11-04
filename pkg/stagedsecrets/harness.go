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

package stagedsecrets

import (
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// ErrNoHarnessSecretsDir is returned by WriteHarnessSecrets when the blob
// carries harness secrets but no in-memory directory is configured for
// them. They are never written anywhere else.
var ErrNoHarnessSecretsDir = errors.New("harness secrets were staged but no in-memory harness secrets directory is configured")

// ValidHarnessSecretName reports whether name is a plain file name a
// harness secret may have: non-empty, not "." or "..", no path separator
// and no NUL byte.
func ValidHarnessSecretName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, "/\\\x00")
}

// WriteHarnessSecrets writes secrets into dir, the in-memory harness
// secrets directory (SCION_HARNESS_SECRETS_DIR), which must be an
// absolute path strictly below root (the in-memory volume's mount point).
// Nothing is written when secrets is empty. With secrets and no dir, it
// fails with ErrNoHarnessSecretsDir rather than writing them into the home.
//
// dir is created component by component below root without following a
// link (dirfd.EnsureDirNoFollowUnderRoot), mode 0700; a component this
// call creates is owned by the workload uid:gid when init runs as root.
// Each file is written 0600 through the no-follow leaf writer, refusing a
// link or non-regular entry at its name. Errors name the step and the file
// name only, never a value.
func WriteHarnessSecrets(root, dir string, secrets []HarnessSecret) error {
	uid, gid := workloadIDs()
	return writeHarnessSecretsAs(root, dir, secrets, uid, gid)
}

func writeHarnessSecretsAs(root, dir string, secrets []HarnessSecret, uid, gid int) error {
	if len(secrets) == 0 {
		return nil
	}
	if dir == "" {
		return ErrNoHarnessSecretsDir
	}
	if err := checkBelowRoot(root, dir); err != nil {
		return err
	}
	for _, s := range secrets {
		if !ValidHarnessSecretName(s.Name) {
			return fmt.Errorf("harness secret name %q is not a plain file name", s.Name)
		}
	}
	dirFd, under, err := dirfd.EnsureDirNoFollowUnderRoot(root, dir, 0o700, uid, gid)
	if err != nil {
		return fmt.Errorf("failed to create the harness secrets directory: %w", err)
	}
	defer func() { _ = dirfd.Close(dirFd) }()
	if !under {
		return fmt.Errorf("harness secrets directory %s is not below %s", dir, root)
	}
	for _, s := range secrets {
		data, err := base64.StdEncoding.DecodeString(s.Value)
		if err != nil {
			return fmt.Errorf("failed to base64-decode harness secret %s", s.Name)
		}
		if err := dirfd.WriteAtNoFollowWithChown(dirFd, s.Name, filepath.Join(dir, s.Name), data, 0o600, uid, gid, dirfd.RefuseSymlink, dirfd.Fchown); err != nil {
			return fmt.Errorf("failed to write harness secret %s: %w", s.Name, err)
		}
	}
	return nil
}

// PrepareHarnessDir creates dir, an in-memory harness directory
// (SCION_HARNESS_SECRETS_DIR or SCION_HARNESS_OUTPUTS_DIR) strictly below
// root, mode 0700, owned by the workload uid:gid, when init runs as root.
// It is a no-op for an empty dir or an unprivileged init, which cannot
// give the directory to another user and leaves its creation to the
// harness as before.
func PrepareHarnessDir(root, dir string) error {
	uid, gid := workloadIDs()
	if dir == "" || uid <= 0 {
		return nil
	}
	if err := checkBelowRoot(root, dir); err != nil {
		return err
	}
	fd, under, err := dirfd.EnsureDirNoFollowUnderRoot(root, dir, 0o700, uid, gid)
	if err != nil {
		return fmt.Errorf("failed to create the in-memory harness directory: %w", err)
	}
	defer func() { _ = dirfd.Close(fd) }()
	if !under {
		return fmt.Errorf("in-memory harness directory %s is not below %s", dir, root)
	}
	return nil
}

// checkBelowRoot requires dir to be an absolute, clean path strictly below
// root.
func checkBelowRoot(root, dir string) error {
	root = filepath.Clean(root)
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("in-memory harness directory %q is not an absolute clean path", dir)
	}
	if !strings.HasPrefix(dir, root+string(filepath.Separator)) {
		return fmt.Errorf("in-memory harness directory %s is not below %s", dir, root)
	}
	return nil
}
