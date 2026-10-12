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

package hubprofile

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// The per-run identity cache keeps the test identities issued through a
// profile, one file per alias:
//
//	~/.scion/hub-profiles/<profile>/run-<label>/<alias>.json
//
// Directories are 0700 and files 0600. A new entry is created with
// O_CREAT|O_EXCL|O_NOFOLLOW, so an existing file or a symlink (even a
// dangling one) is refused. A refreshed token replaces the entry by
// writing a new exclusive temporary file in the same directory and
// renaming it over the entry. Reads refuse symlinks, non-regular files and
// files readable by anyone but the owner. The file holds the identity's
// token: it never leaves the container, and the CLI never prints it.

const runDirPrefix = "run-"

// CacheEntry is one cached test identity.
type CacheEntry struct {
	Alias          string    `json:"alias"`
	Run            string    `json:"run"`
	Profile        string    `json:"profile"`
	Endpoint       string    `json:"endpoint"`
	IdentityID     string    `json:"identityId"`
	Email          string    `json:"email"`
	Kind           string    `json:"kind"`
	Role           string    `json:"role"`
	DelegationID   string    `json:"delegationId,omitempty"`
	ExpiresAt      time.Time `json:"expiresAt"`
	TokenExpiresAt time.Time `json:"tokenExpiresAt"`
	Token          string    `json:"token"`
}

// IsMemberTier reports whether the entry is a member-tier identity (role
// member or viewer), the only tier the CLI re-issues tokens for.
func (e *CacheEntry) IsMemberTier() bool {
	return e.Role == "member" || e.Role == "viewer"
}

func profileCacheDir(home, profile string) string {
	return filepath.Join(Dir(home), profile)
}

// RunDir returns the cache directory of a run.
func RunDir(home, profile, run string) (string, error) {
	if err := ValidateName("profile name", profile); err != nil {
		return "", err
	}
	if err := ValidateName("run label", run); err != nil {
		return "", err
	}
	return filepath.Join(profileCacheDir(home, profile), runDirPrefix+run), nil
}

func entryPath(home, profile, run, alias string) (string, error) {
	dir, err := RunDir(home, profile, run)
	if err != nil {
		return "", err
	}
	if err := ValidateName("alias", alias); err != nil {
		return "", err
	}
	return filepath.Join(dir, alias+".json"), nil
}

// ErrEntryExists is returned by CreateEntry when the alias is cached.
var ErrEntryExists = errors.New("alias already exists in this run")

// ReserveEntry creates the cache file for a new alias exclusively, before
// the identity is issued, so a refused path costs no identity. The caller
// fills it with Fill or removes it with Abandon.
func ReserveEntry(home, profile, run, alias string) (*Reservation, error) {
	path, err := entryPath(home, profile, run, alias)
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(Dir(home), filepath.Dir(path)); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) || isSymlinkErr(err) {
			return nil, fmt.Errorf("alias %q in run %q: %w (refusing to overwrite %s)", alias, run, ErrEntryExists, path)
		}
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &Reservation{path: path, f: f}, nil
}

// Reservation is an exclusively created, still empty cache file.
type Reservation struct {
	path string
	f    *os.File
}

// Path returns the cache file path.
func (r *Reservation) Path() string { return r.path }

// Abandon closes and removes the empty file.
func (r *Reservation) Abandon() {
	_ = r.f.Close()
	_ = os.Remove(r.path)
}

// Fill writes e and closes the file.
func (r *Reservation) Fill(e *CacheEntry) error {
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		_ = r.f.Close()
		return err
	}
	if _, err := r.f.Write(append(data, '\n')); err != nil {
		_ = r.f.Close()
		return err
	}
	if err := r.f.Sync(); err != nil {
		_ = r.f.Close()
		return err
	}
	return r.f.Close()
}

// ReadEntry reads a cached alias.
func ReadEntry(home, profile, run, alias string) (*CacheEntry, error) {
	path, err := entryPath(home, profile, run, alias)
	if err != nil {
		return nil, err
	}
	data, err := readPrivateFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no cached identity %q in run %q of profile %q (issue one with 'scion hub test-identity issue --as %s --run %s')", alias, run, profile, alias, run)
	}
	if err != nil {
		return nil, err
	}
	var e CacheEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("cached identity %q: invalid file", alias)
	}
	if e.Alias != alias || e.Run != run || e.Profile != profile || e.Token == "" || e.IdentityID == "" {
		return nil, fmt.Errorf("cached identity %q: the file does not match its path", alias)
	}
	return &e, nil
}

// ReplaceEntry atomically replaces a cached alias with e (a refreshed
// token). The alias must already be cached.
func ReplaceEntry(home string, e *CacheEntry) error {
	path, err := entryPath(home, e.Profile, e.Run, e.Alias)
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err != nil {
		return err
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("refusing %s: it is not a regular file", path)
	}
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// ListRun returns the aliases cached in a run, sorted.
func ListRun(home, profile, run string) ([]string, error) {
	dir, err := RunDir(home, profile, run)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Lstat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("refusing %s: it is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var aliases []string
	for _, e := range entries {
		a, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !e.Type().IsRegular() || ValidateName("alias", a) != nil {
			continue
		}
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	return aliases, nil
}

// ListRuns returns the run labels cached for a profile, sorted.
func ListRuns(home, profile string) ([]string, error) {
	if err := ValidateName("profile name", profile); err != nil {
		return nil, err
	}
	dir := profileCacheDir(home, profile)
	if fi, err := os.Lstat(dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("refusing %s: it is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var runs []string
	for _, e := range entries {
		r, ok := strings.CutPrefix(e.Name(), runDirPrefix)
		if !ok || !e.IsDir() || ValidateName("run label", r) != nil {
			continue
		}
		runs = append(runs, r)
	}
	sort.Strings(runs)
	return runs, nil
}

// RemoveEntry deletes one cached alias file.
func RemoveEntry(home, profile, run, alias string) error {
	path, err := entryPath(home, profile, run, alias)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RemoveRunDir removes a run's cache directory. It must hold no alias
// files (teardown removes each alias as its identity is deleted); other
// files, such as stale temporary files, are removed with it.
func RemoveRunDir(home, profile, run string) error {
	dir, err := RunDir(home, profile, run)
	if err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("refusing %s: it is not a directory", dir)
	}
	aliases, err := ListRun(home, profile, run)
	if err != nil {
		return err
	}
	if len(aliases) > 0 {
		return fmt.Errorf("run %q still caches %s", run, strings.Join(aliases, ", "))
	}
	return os.RemoveAll(dir)
}

func removeProfileCacheDir(home, profile string) error {
	dir := profileCacheDir(home, profile)
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("refusing %s: it is not a directory", dir)
	}
	return os.RemoveAll(dir)
}

// readPrivateFile reads a regular file owned by the caller and readable by
// nobody else, never following a symlink.
func readPrivateFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if isSymlinkErr(err) {
			return nil, fmt.Errorf("refusing %s: it is a symlink", path)
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing %s: it is not a regular file", path)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("refusing %s: its mode %04o lets others read it; it must be 0600", path, fi.Mode().Perm())
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return nil, fmt.Errorf("refusing %s: it is owned by another user", path)
	}
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

// writeFileAtomic writes data to a new exclusive 0600 temporary file in
// path's directory and renames it over path. rename replaces a symlink at
// path itself; it never writes through it.
func writeFileAtomic(path string, data []byte) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+hex.EncodeToString(b[:])+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		ok = true
		return err
	}
	ok = true
	return nil
}

// isSymlinkErr reports whether err is the ELOOP O_NOFOLLOW returns for a
// symlink.
func isSymlinkErr(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}
