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

package runtimebroker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// WorkspaceLocks coordinates operations on shared local paths (project
// directories, the shared worktree base, NFS workspace paths, agent
// directories) across every Runtime Broker server of one process
// (ptone/scion#3274). The flat host creates one and injects it into every
// instance (ServerConfig.WorkspaceLocks); a server without one gets its own.
// A CLI or a Runtime Broker in another process is coordinated only through
// the existing cross-process provisioning lock.
//
// A single Runtime Broker coordinates only with itself, but it takes these
// locks too, so some of its operations that used to overlap now run one at
// a time: an agent delete that touches files holds the agent's file paths
// (agentFileLockPaths) for the whole delete, a project delete holds the
// project directory, and the file cleanup after a failed start is skipped,
// leaving the files, when the agent's file lock is not free within 60
// seconds.
//
// Keys are canonical paths (CanonicalWorkspacePath): absolute, cleaned and
// with symlinks resolved, also for paths that do not exist yet, so two
// spellings of one directory (a symlink alias, a relative path, a trailing
// separator) are one key. A lock on a path also excludes every path inside
// it and every directory containing it, so removing a project directory
// waits for (and blocks) work on any workspace under it.
//
// Lock takes all of an operation's paths at once or none of them: it never
// holds some keys while waiting for others, so two operations taking
// overlapping sets in any order cannot deadlock. A holder keeps the lock
// through its cleanup (the release function is called last).
//
// Locks coordinate operations; they do not establish ownership. Who may
// act on an agent or a path is decided by the ownership records.
type WorkspaceLocks struct {
	mu      sync.Mutex
	held    map[string]int // canonical key -> holders (always 1 while held)
	changed chan struct{}  // closed and replaced on every release
	waiters []*lockWaiter  // waiting requests, in arrival order

	// users report, per server, what it uses in shared project
	// directories (a flat instance: its ownership records), so a project
	// removal by one server never removes a workspace another server still
	// uses, and no server reserves a slug another server holds.
	usersMu sync.Mutex
	users   map[*Server]workspaceUser

	// reserveMu serializes slug reservations across servers (reserveSlug).
	reserveMu sync.Mutex
}

// workspaceUser is how a server reports what it uses in shared project
// directories.
type workspaceUser struct {
	// instance names the server in errors (its Runtime Broker ID).
	instance string
	// projectInUse reports whether the server has live agents in a project.
	projectInUse func(projectID string) (bool, error)
	// slugReservation reports the agent holding a slug in a project and
	// whether that agent's delete is unfinished (OwnershipStore.SlugReservation).
	slugReservation func(projectID, slug string) (holder string, pending bool, err error)
}

// NewWorkspaceLocks returns an empty lock service.
func NewWorkspaceLocks() *WorkspaceLocks {
	return &WorkspaceLocks{held: map[string]int{}, changed: make(chan struct{})}
}

// registerWorkspaceUser records how server reports what it uses; nil
// removes it.
func (l *WorkspaceLocks) registerWorkspaceUser(s *Server, u *workspaceUser) {
	l.usersMu.Lock()
	defer l.usersMu.Unlock()
	if l.users == nil {
		l.users = map[*Server]workspaceUser{}
	}
	if u == nil {
		delete(l.users, s)
		return
	}
	l.users[s] = *u
}

// projectInUseByOthers reports whether a server other than self sharing this
// service has live agents in the project. A server whose answer cannot be
// read counts as using it.
func (l *WorkspaceLocks) projectInUseByOthers(self *Server, projectID string) bool {
	l.usersMu.Lock()
	defer l.usersMu.Unlock()
	for s, u := range l.users {
		if s == self || u.projectInUse == nil {
			continue
		}
		if used, err := u.projectInUse(projectID); err != nil || used {
			return true
		}
	}
	return false
}

// ErrOwnershipSlugReservedElsewhere is a slug another Runtime Broker
// instance of the host holds in the same project.
var ErrOwnershipSlugReservedElsewhere = errors.New("agent slug is reserved by another Runtime Broker instance of this host")

// slugReservedElsewhereError is a slug another server sharing the service
// holds in the project. It matches ErrOwnershipSlugReservedElsewhere and,
// when the holder's delete is unfinished, ErrOwnershipSlugPending.
type slugReservedElsewhereError struct {
	projectID, slug, instance, holder string
	pending                           bool
	readErr                           error // the reservation could not be read
}

func (e *slugReservedElsewhereError) Error() string {
	switch {
	case e.readErr != nil:
		return fmt.Sprintf("%v: slug %q in project %q: the reservations of Runtime Broker instance %s cannot be read: %v", ErrOwnershipSlugReservedElsewhere, e.slug, e.projectID, e.instance, e.readErr)
	case e.pending:
		return fmt.Sprintf("%v: slug %q in project %q on Runtime Broker instance %s: %v (agent %s)", ErrOwnershipSlugReservedElsewhere, e.slug, e.projectID, e.instance, ErrOwnershipSlugPending, e.holder)
	default:
		return fmt.Sprintf("%v: slug %q in project %q is held by agent %s on Runtime Broker instance %s", ErrOwnershipSlugReservedElsewhere, e.slug, e.projectID, e.holder, e.instance)
	}
}

func (e *slugReservedElsewhereError) Unwrap() []error {
	if e.pending {
		return []error{ErrOwnershipSlugReservedElsewhere, ErrOwnershipSlugPending}
	}
	return []error{ErrOwnershipSlugReservedElsewhere}
}

// reserveSlug runs reserve, which reserves slug in self's own records, only
// when no other server sharing this service holds the slug in the project:
// two servers sharing a project directory would otherwise use the same
// agent directory. Every server's reservation runs under one lock, so two
// servers cannot reserve the same slug at once. A reservation that cannot
// be read counts as held. A slug is held from its agent's first run until
// that agent's delete confirms all its objects are gone (the slug release
// needs no lock: it only frees the slug).
func (l *WorkspaceLocks) reserveSlug(self *Server, projectID, slug string, reserve func() error) error {
	l.reserveMu.Lock()
	defer l.reserveMu.Unlock()
	if err := l.slugHeldByOthers(self, projectID, slug); err != nil {
		return err
	}
	return reserve()
}

func (l *WorkspaceLocks) slugHeldByOthers(self *Server, projectID, slug string) error {
	l.usersMu.Lock()
	defer l.usersMu.Unlock()
	for s, u := range l.users {
		if s == self || u.slugReservation == nil {
			continue
		}
		holder, pending, err := u.slugReservation(projectID, slug)
		if err != nil {
			return &slugReservedElsewhereError{projectID: projectID, slug: slug, instance: u.instance, readErr: err}
		}
		if holder != "" {
			return &slugReservedElsewhereError{projectID: projectID, slug: slug, instance: u.instance, holder: holder, pending: pending}
		}
	}
	return nil
}

// withWorkspaceLock runs fn holding the workspace lock on paths, released
// when fn returns or panics. An error means the lock was not taken (the
// context ended) and fn did not run.
func (s *Server) withWorkspaceLock(ctx context.Context, fn func(), paths ...string) error {
	unlock, err := s.locks().Lock(ctx, paths...)
	if err != nil {
		return err
	}
	defer unlock()
	fn()
	return nil
}

// locks returns the server's workspace lock service, creating a private one
// for a server built without New.
func (s *Server) locks() *WorkspaceLocks {
	s.workspaceLocksOnce.Do(func() {
		if s.workspaceLocks == nil {
			s.workspaceLocks = NewWorkspaceLocks()
		}
	})
	return s.workspaceLocks
}

// CanonicalWorkspacePath returns the lock key of path: absolute and
// cleaned, with every symlink in its longest existing prefix resolved and
// the not-yet-existing remainder appended, so the key a path has before it
// is created is the key it has afterwards.
func CanonicalWorkspacePath(path string) (string, error) {
	if path == "" {
		return "", errors.New("workspace lock: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("workspace lock: %w", err)
	}
	abs = filepath.Clean(abs)
	existing, rest := abs, []string{}
	for {
		resolved, err := filepath.EvalSymlinks(existing)
		if err == nil {
			parts := append([]string{resolved}, rest...)
			return filepath.Clean(filepath.Join(parts...)), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("workspace lock: resolving %s: %w", existing, err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			// Nothing of the path exists (not even the root): use it as is.
			return abs, nil
		}
		rest = append([]string{filepath.Base(existing)}, rest...)
		existing = parent
	}
}

// overlaps reports whether a and b are the same path or one contains the
// other.
func overlaps(a, b string) bool {
	if a == b {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(a, strings.TrimSuffix(b, sep)+sep) || strings.HasPrefix(b, strings.TrimSuffix(a, sep)+sep)
}

// lockWaiter is a Lock request waiting for its keys.
type lockWaiter struct{ keys []string }

// Lock acquires every path (empty paths are ignored) and returns the
// release function, which is safe to call more than once. It waits until
// no held key overlaps any requested key and no earlier waiting request
// overlaps it, and then takes them all; it returns ctx's error (holding
// nothing) if ctx ends first. Overlapping requests are admitted in arrival
// order, so a broad request (a project directory) is not passed
// indefinitely by a stream of narrower ones inside it. A holder must not
// call Lock again before it releases, even for paths disjoint from those it
// holds: a request that overlaps both may be queued between them, and the
// second call then waits behind it while it waits for the holder (a nested
// call on overlapping paths deadlocks outright, as with sync.Mutex). Take
// every path an operation needs in one call.
func (l *WorkspaceLocks) Lock(ctx context.Context, paths ...string) (func(), error) {
	keys, err := canonicalKeys(paths)
	if err != nil {
		return nil, err
	}
	var me *lockWaiter
	for {
		l.mu.Lock()
		if !l.conflictsLocked(keys) && !l.overlapsEarlierWaiterLocked(me, keys) {
			for _, k := range keys {
				l.held[k]++
			}
			l.dequeueLocked(me)
			l.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { l.release(keys) }) }, nil
		}
		if me == nil {
			me = &lockWaiter{keys: keys}
			l.waiters = append(l.waiters, me)
		}
		wait := l.changed
		l.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			// Leaving the queue may admit the requests behind this one.
			l.mu.Lock()
			l.dequeueLocked(me)
			l.broadcastLocked()
			l.mu.Unlock()
			return nil, fmt.Errorf("workspace lock on %s: %w", strings.Join(keys, ", "), ctx.Err())
		}
	}
}

// overlapsEarlierWaiterLocked reports whether a request waiting ahead of me
// (every waiting request when me has not queued yet) overlaps keys.
func (l *WorkspaceLocks) overlapsEarlierWaiterLocked(me *lockWaiter, keys []string) bool {
	for _, w := range l.waiters {
		if w == me {
			return false
		}
		for _, a := range w.keys {
			for _, b := range keys {
				if overlaps(a, b) {
					return true
				}
			}
		}
	}
	return false
}

func (l *WorkspaceLocks) dequeueLocked(me *lockWaiter) {
	for i, w := range l.waiters {
		if w == me {
			l.waiters = append(l.waiters[:i], l.waiters[i+1:]...)
			return
		}
	}
}

// broadcastLocked wakes every waiting request to try again.
func (l *WorkspaceLocks) broadcastLocked() {
	close(l.changed)
	l.changed = make(chan struct{})
}

func canonicalKeys(paths []string) ([]string, error) {
	seen := map[string]bool{}
	var keys []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		k, err := CanonicalWorkspacePath(p)
		if err != nil {
			return nil, err
		}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (l *WorkspaceLocks) conflictsLocked(keys []string) bool {
	for held := range l.held {
		for _, k := range keys {
			if overlaps(held, k) {
				return true
			}
		}
	}
	return false
}

func (l *WorkspaceLocks) release(keys []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		if l.held[k]--; l.held[k] <= 0 {
			delete(l.held, k)
		}
	}
	l.broadcastLocked()
}

// agentFileLockPaths are the shared paths an agent's file operations
// (provisioning cleanup, DeleteAgentFiles, delete marks) touch: the
// project root, which holds the shared base clone, its worktrees and the
// agent directories, and the enclosing repository when it differs; and the
// agent's external directory (split storage). For the global project only
// the agent's own entries are taken, never the global directory itself.
// DeleteAgentFiles touches the global project's agent entries only when the
// global project is its target, so no other project's lock set includes
// them.
func agentFileLockPaths(projectPath, agentName string) []string {
	projectDir, err := config.GetResolvedProjectDir(projectPath)
	if err != nil || projectDir == "" {
		projectDir = projectPath
	}
	var paths []string
	if config.IsGlobalProjectDir(projectDir) {
		paths = append(paths, filepath.Join(projectDir, "agents", agentName), filepath.Join(projectDir, "workspace", agentName))
	} else {
		root := projectDir
		if filepath.Base(projectDir) == config.DotScion {
			root = filepath.Dir(projectDir)
		}
		paths = append(paths, root)
		if repoRoot, err := util.RepoRootDir(projectDir); err == nil && repoRoot != "" {
			paths = append(paths, repoRoot)
		}
		if ext, err := config.GetGitProjectExternalAgentsDir(projectDir); err == nil && ext != "" {
			paths = append(paths, filepath.Join(ext, agentName))
		}
	}
	return paths
}

// lockAgentFiles takes the process-wide workspace lock on every shared path
// an agent's file operations touch (agentFileLockPaths), all at once.
func (s *Server) lockAgentFiles(ctx context.Context, projectPath, agentName string) (func(), error) {
	if projectPath == "" || agentName == "" {
		return func() {}, nil
	}
	return s.locks().Lock(ctx, agentFileLockPaths(projectPath, agentName)...)
}
