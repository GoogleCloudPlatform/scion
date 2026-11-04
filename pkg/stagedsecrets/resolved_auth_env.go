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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// ResolvedAuthEnvFile is the file, directly in the in-memory volume's
// mount point (never inside the harness secrets or outputs directories,
// the agent home or shared storage), that carries Staged.ResolvedAuthEnv
// from sciontool init's staging pass to the harness launch across init's
// environ-purge re-exec.
const ResolvedAuthEnvFile = ".resolved-auth-env.json"

// maxResolvedAuthEnvFileBytes bounds the carry file; the staged blob it
// comes from is capped at 32 KiB by the runtime that sets the field.
const maxResolvedAuthEnvFileBytes = 32 * 1024

// ErrNoResolvedAuthEnvRoot is returned by WriteResolvedAuthEnv when the
// blob carries resolved-auth entries but the runtime named no in-memory
// harness directory. They are never written anywhere else.
var ErrNoResolvedAuthEnvRoot = errors.New("resolved-auth environment entries were staged but no in-memory harness directory is configured")

// resolvedAuthEnvOwner returns the uid and gid the carry file is owned by:
// the calling process (sciontool init). A variable so tests can check the
// owner refusal.
var resolvedAuthEnvOwner = func() (uid, gid int) { return os.Getuid(), os.Getgid() }

// reservedResolvedAuthEnvNames are the names a resolved-auth entry in the
// staged blob may never set: the names sciontool init reads from its own
// environment (the staged blob and the staged-token marker) and the names
// a runtime sets on the instance itself (the Cloud Run runtime keys).
var reservedResolvedAuthEnvNames = map[string]bool{
	EnvVar:                        true,
	HubTokenStagedEnvVar:          true,
	"SCION_HOST_UID":              true,
	"SCION_HOST_GID":              true,
	"SCION_STARTUP_LISTENER_PORT": true,
	"SCION_HARNESS_SECRETS_DIR":   true,
	"SCION_HARNESS_OUTPUTS_DIR":   true,
}

// ReservedResolvedAuthEnvName reports whether name may not be set by a
// resolved-auth entry carried in the staged blob. The broker drops such an
// entry when it builds the blob, and sciontool init drops it again before
// the harness starts.
func ReservedResolvedAuthEnvName(name string) bool {
	return reservedResolvedAuthEnvNames[name]
}

// ExportableResolvedAuthEnv returns the entries of env whose name is a
// valid environment name (a letter or underscore, then letters, digits or
// underscores) and not reserved (ReservedResolvedAuthEnvName), and the
// number it dropped. It never returns or logs a value of a dropped entry.
func ExportableResolvedAuthEnv(env map[string]string) (map[string]string, int) {
	out := make(map[string]string, len(env))
	dropped := 0
	for k, v := range env {
		if !validEnvName(k) || ReservedResolvedAuthEnvName(k) {
			dropped++
			continue
		}
		out[k] = v
	}
	return out, dropped
}

// validEnvName reports whether k is a letter or underscore followed by
// letters, digits or underscores.
func validEnvName(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// WriteResolvedAuthEnv writes env as JSON to ResolvedAuthEnvFile directly
// in root, the in-memory volume's mount point. harnessSecretsDir is the
// in-memory harness secrets directory the runtime named
// (SCION_HARNESS_SECRETS_DIR); it must be set and below root, which is the
// condition under which root is an in-memory volume. Nothing is written
// when env is empty.
//
// root is opened O_DIRECTORY|O_NOFOLLOW. A stale entry at the file name is
// unlinked without following it, then the file is created
// O_CREAT|O_EXCL|O_NOFOLLOW mode 0600, fchowned to the calling process,
// written and fsynced. Any error removes the partial file. Errors never
// carry a value.
func WriteResolvedAuthEnv(root, harnessSecretsDir string, env map[string]string) (err error) {
	if len(env) == 0 {
		return nil
	}
	if harnessSecretsDir == "" {
		return ErrNoResolvedAuthEnvRoot
	}
	if err := checkBelowRoot(root, harnessSecretsDir); err != nil {
		return err
	}
	dir, err := openResolvedAuthEnvRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	data, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("failed to encode the resolved-auth environment entries")
	}
	path := filepath.Join(root, ResolvedAuthEnvFile)
	fd := int(dir.Fd())
	if err := dirfd.UnlinkAt(fd, ResolvedAuthEnvFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to remove a stale %s: %w", path, err)
	}
	f, err := dirfd.CreateExclAt(fd, ResolvedAuthEnvFile, 0o600)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("failed to close %s: %w", path, cerr)
		}
		if err != nil {
			_ = dirfd.UnlinkAt(fd, ResolvedAuthEnvFile)
		}
	}()
	uid, gid := resolvedAuthEnvOwner()
	if err := dirfd.Fchown(int(f.Fd()), uid, gid); err != nil {
		return fmt.Errorf("failed to chown %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("failed to sync %s: %w", path, err)
	}
	return nil
}

// TakeResolvedAuthEnv reads ResolvedAuthEnvFile in root and returns its
// entries, or nil when there is none. The file is opened O_NOFOLLOW and
// must be a regular file with one link, owned by the calling process and
// at most maxResolvedAuthEnvFileBytes; otherwise it is refused. It is
// unlinked on every return path, so it never outlives this call. Callers
// filter the entries (ExportableResolvedAuthEnv) before use. Errors never
// carry a value.
func TakeResolvedAuthEnv(root string) (env map[string]string, err error) {
	dir, err := openResolvedAuthEnvRoot(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	path := filepath.Join(root, ResolvedAuthEnvFile)
	fd := int(dir.Fd())
	defer func() {
		if uerr := dirfd.UnlinkAt(fd, ResolvedAuthEnvFile); uerr != nil && !errors.Is(uerr, os.ErrNotExist) && err == nil {
			env, err = nil, fmt.Errorf("failed to remove %s: %w", path, uerr)
		}
	}()
	data, st, err := dirfd.ReadAtNoFollowStat(fd, ResolvedAuthEnvFile, maxResolvedAuthEnvFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("refusing %s: %w", path, err)
	}
	if uid, _ := resolvedAuthEnvOwner(); int64(st.Uid) != int64(uid) {
		return nil, fmt.Errorf("refusing %s: not owned by uid %d", path, uid)
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("failed to decode %s", path)
	}
	return env, nil
}

// openResolvedAuthEnvRoot opens root O_DIRECTORY|O_NOFOLLOW without
// following a link at any component; a missing root wraps
// os.ErrNotExist.
func openResolvedAuthEnvRoot(root string) (*os.File, error) {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, fmt.Errorf("in-memory volume path %q is not an absolute clean path", root)
	}
	dir, err := dirfd.OpenDirNoFollow(root)
	if err != nil {
		return nil, fmt.Errorf("failed to open the in-memory volume %s: %w", root, err)
	}
	return dir, nil
}
