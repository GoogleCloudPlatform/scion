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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// The per-run identity cache keeps the test identities issued through a
// profile, one file per alias:
//
//	~/.scion/hub-profiles/<profile>/run-<label>/<alias>.json
//
// Directories and files are private (privatefs.Private: 0700/0600 on unix,
// an owner-only ACL set at creation on Windows), all through a
// privatefs.Tree anchored at Dir(home). A new entry is created exclusively
// (Tree.Create, excl), so an existing file or a symlink (even a dangling
// one) is refused. A refreshed token replaces the entry by writing a new
// private temporary file in the same directory and renaming it over the
// entry through its open handle. Reads refuse symlinks (on Windows any
// reparse point), non-regular files and files anyone but the owner can
// read (Tree.ReadPrivate). The file holds the identity's token: it never
// leaves the container, and the CLI never prints it.

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

// IsMemberTier reports whether the entry is a member-tier identity (kind
// test_fixture, role member or viewer). A test hub-admin also has the
// stored role member, so the kind decides.
func (e *CacheEntry) IsMemberTier() bool {
	return e.Kind == KindTestFixture && (e.Role == "member" || e.Role == "viewer")
}

// AdminTier returns the admin delegation tier of an admin-kind entry
// (TierHubAdmin for kind test_hub_admin, TierSuperAdmin for
// test_super_admin), or "" for any other entry.
func (e *CacheEntry) AdminTier() string {
	switch e.Kind {
	case KindTestHubAdmin:
		return TierHubAdmin
	case KindTestSuperAdmin:
		return TierSuperAdmin
	}
	return ""
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

// entryRel returns an entry's path relative to Dir(home), the anchor of the
// private tree, slash-separated. The names were validated by entryPath.
func entryRel(profile, run, alias string) string {
	return profile + "/" + runDirPrefix + run + "/" + alias + ".json"
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
	t, err := ensurePrivateDir(Dir(home), filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	// Exclusive and private at creation; a symlink (on Windows any reparse
	// point) at the alias path counts as an existing entry.
	f, err := t.Create(entryRel(profile, run, alias), true, privatefs.Private)
	if err != nil {
		_ = t.Close()
		if errors.Is(err, os.ErrExist) || errors.Is(err, privatefs.ErrReparsePoint) {
			return nil, fmt.Errorf("alias %q in run %q: %w (refusing to overwrite %s)", alias, run, ErrEntryExists, path)
		}
		return nil, err
	}
	return &Reservation{path: path, t: t, f: f}, nil
}

// Reservation is an exclusively created, still empty cache file.
type Reservation struct {
	path string
	t    *privatefs.Tree
	f    *os.File
}

// Path returns the cache file path.
func (r *Reservation) Path() string { return r.path }

// Abandon removes the empty file through its open handle and closes it.
func (r *Reservation) Abandon() {
	_ = r.t.RemoveOpen(r.f)
	r.close()
}

func (r *Reservation) close() {
	_ = r.f.Close()
	_ = r.t.Close()
}

// Fill writes e and closes the file.
func (r *Reservation) Fill(e *CacheEntry) error {
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		r.close()
		return err
	}
	if _, err := r.f.Write(append(data, '\n')); err != nil {
		r.close()
		return err
	}
	if err := r.f.Sync(); err != nil {
		r.close()
		return err
	}
	err = r.f.Close()
	_ = r.t.Close()
	return err
}

// ReadEntry reads a cached alias.
func ReadEntry(home, profile, run, alias string) (*CacheEntry, error) {
	path, err := entryPath(home, profile, run, alias)
	if err != nil {
		return nil, err
	}
	data, err := readPrivateFile(home, entryRel(profile, run, alias), path)
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
	return writeFileAtomic(home, entryRel(e.Profile, e.Run, e.Alias), append(data, '\n'))
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

// readPrivateFile reads the regular file rel below Dir(home) through the
// private tree (Tree.ReadPrivate): a symlink (on Windows any reparse point)
// at any component below the anchor, a non-regular file and a file anyone
// but the owner can read are refused. path names the file in errors.
func readPrivateFile(home, rel, path string) ([]byte, error) {
	t, err := privatefs.OpenTree(Dir(home), privatefs.TreeOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = t.Close() }()
	data, err := t.ReadPrivate(rel, 1<<20)
	switch {
	case errors.Is(err, privatefs.ErrReparsePoint):
		return nil, fmt.Errorf("refusing %s: it is a symlink or junction: %w", path, err)
	case errors.Is(err, privatefs.ErrNotPrivate):
		return nil, fmt.Errorf("refusing %s: it is not private to you; it must be private to you (%s): %w", path, privatefs.Describe(privatefs.Private), err)
	}
	return data, err
}

// writeFileAtomic replaces rel below Dir(home) with data: a new private
// temporary file is created exclusively in rel's directory through the
// private tree, written, synced and renamed over rel through its open
// handle (on Windows a handle rename; on unix rename(2), which replaces a
// symlink at rel itself and never writes through it). On failure the
// temporary file is removed through the same handle.
func writeFileAtomic(home, rel string, data []byte) error {
	t, err := privatefs.OpenTree(Dir(home), privatefs.TreeOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = t.Close() }()
	dir, leaf := "", rel
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		dir, leaf = rel[:i], rel[i+1:]
	}
	f, _, err := t.CreateTemp(dir, "."+leaf+".*.tmp", privatefs.Private)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = t.RemoveOpen(f)
			_ = f.Close()
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := t.RenameOpen(f, rel); err != nil {
		return err
	}
	ok = true
	return f.Close()
}
