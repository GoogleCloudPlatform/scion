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
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
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

// ResolvedAuthStagedEnvVar is set by sciontool init, across its
// environ-purge re-exec, when its staging pass wrote ResolvedAuthEnvFile,
// so the next pass reads that file and no other. Its value is the per-pass
// nonce (NewResolvedAuthNonce) the file also carries; it carries no
// credential. The runtimes reserve the name: no container environment or
// staged entry supplies it.
const ResolvedAuthStagedEnvVar = "SCION_RESOLVED_AUTH_STAGED"

// maxResolvedAuthEnvFileBytes bounds the carry file; the staged blob it
// comes from is capped at 32 KiB by the runtime that sets the field.
const maxResolvedAuthEnvFileBytes = 32 * 1024

// ErrNoResolvedAuthEnvRoot is returned by WriteResolvedAuthEnv when the
// blob carries resolved-auth entries but the runtime named no in-memory
// harness directory. They are never written anywhere else.
var ErrNoResolvedAuthEnvRoot = errors.New("resolved-auth environment entries were staged but no in-memory harness directory is configured")

// ErrResolvedAuthEnvNonceMismatch is returned by TakeResolvedAuthEnv when
// the carry file's nonce is not the one the caller holds: the file was not
// written by the pass that set the marker. It is refused and removed.
var ErrResolvedAuthEnvNonceMismatch = errors.New("the resolved-auth carry file does not belong to this start")

// resolvedAuthCarry is the carry file's content.
type resolvedAuthCarry struct {
	Nonce string            `json:"nonce"`
	Env   map[string]string `json:"env"`
}

// NewResolvedAuthNonce returns a fresh random nonce (32 bytes, hex) for one
// staging pass.
func NewResolvedAuthNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to create the resolved-auth carry nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// resolvedAuthEnvOwner returns the uid and gid the carry file is owned by:
// the calling process (sciontool init). A variable so tests can check the
// owner refusal.
var resolvedAuthEnvOwner = func() (uid, gid int) { return os.Getuid(), os.Getgid() }

// harnessStrippedEnvNames are the names sciontool init removes from the
// environment the harness inherits (os.Unsetenv in cmd/sciontool/commands,
// removeEnvVar in pkg/sciontool/supervisor). A test in
// cmd/sciontool/commands ties this list to those calls.
var harnessStrippedEnvNames = []string{
	EnvVar,
	HubTokenStagedEnvVar,
	ResolvedAuthStagedEnvVar,
	"PORT",
	"SCION_TRANSPORT_TOKEN",
	"SCION_TRANSPORT_TOKEN_EXPIRY",
	"SCION_EXTRA_PATH",
	"SCION_NATIVE_TELEMETRY_POLICY",
}

// HarnessStrippedEnvNames returns a copy of the names sciontool init
// removes from the environment the harness inherits. Each is reserved for
// resolved-auth entries (ReservedResolvedAuthEnvName).
func HarnessStrippedEnvNames() []string {
	return append([]string(nil), harnessStrippedEnvNames...)
}

// reservedResolvedAuthEnvNames are the names a resolved-auth entry in the
// staged blob may never set: the names sciontool init removes from the
// harness environment (harnessStrippedEnvNames, which include the names
// init reads from its own environment) and the names a runtime sets on the
// instance itself (the Cloud Run runtime keys).
var reservedResolvedAuthEnvNames = func() map[string]bool {
	m := map[string]bool{
		"SCION_HOST_UID":              true,
		"SCION_HOST_GID":              true,
		"SCION_STARTUP_LISTENER_PORT": true,
		"SCION_HARNESS_SECRETS_DIR":   true,
		"SCION_HARNESS_OUTPUTS_DIR":   true,
	}
	for _, name := range harnessStrippedEnvNames {
		m[name] = true
	}
	return m
}()

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

// WriteResolvedAuthEnv writes env and nonce (the staging pass's
// NewResolvedAuthNonce) as JSON to ResolvedAuthEnvFile directly in root,
// the in-memory volume's mount point. harnessSecretsDir is the
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
func WriteResolvedAuthEnv(root, harnessSecretsDir, nonce string, env map[string]string) (err error) {
	if len(env) == 0 {
		return nil
	}
	if nonce == "" {
		return fmt.Errorf("a resolved-auth carry file needs a nonce")
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
	data, err := json.Marshal(resolvedAuthCarry{Nonce: nonce, Env: env})
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
// entries, or nil when there is none. nonce is the value of the marker the
// staging pass set; it must be non-empty, and the file's nonce must equal
// it (compared in constant time), otherwise the file is refused with
// ErrResolvedAuthEnvNonceMismatch. The file is opened O_NOFOLLOW and must
// be a regular file with one link, owned by the calling process and at
// most maxResolvedAuthEnvFileBytes; otherwise it is refused. Once root is
// open the file is unlinked on every return path, so it never outlives
// this call. Callers filter the entries (ExportableResolvedAuthEnv) before
// use. Errors never carry a value or the nonce.
func TakeResolvedAuthEnv(root, nonce string) (env map[string]string, err error) {
	if nonce == "" {
		return nil, fmt.Errorf("reading the resolved-auth carry file needs the staging pass's nonce")
	}
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
	var carry resolvedAuthCarry
	if err := json.Unmarshal(data, &carry); err != nil {
		return nil, fmt.Errorf("failed to decode %s", path)
	}
	if subtle.ConstantTimeCompare([]byte(carry.Nonce), []byte(nonce)) != 1 {
		return nil, ErrResolvedAuthEnvNonceMismatch
	}
	return carry.Env, nil
}

// DiscardResolvedAuthEnv removes ResolvedAuthEnvFile from root without
// reading it or following a link, for a pass that did not stage it. It
// reports whether an entry was removed; a missing root or file is not an
// error.
func DiscardResolvedAuthEnv(root string) (bool, error) {
	dir, err := openResolvedAuthEnvRoot(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = dir.Close() }()
	if err := dirfd.UnlinkAt(int(dir.Fd()), ResolvedAuthEnvFile); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("failed to remove %s: %w", filepath.Join(root, ResolvedAuthEnvFile), err)
	}
	return true, nil
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
