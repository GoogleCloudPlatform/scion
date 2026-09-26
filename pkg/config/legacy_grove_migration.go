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

// Package config: legacy "grove" -> "project" migration.
//
// This file holds the legacy grove->project migration support: the Reporter
// contract that the CLI, hub, and broker each render in their own idiom, and
// the warning for legacy environment variables that are no longer read. It
// is meant to be the only non-test file that still knows legacy "grove"
// names once the rest of the rename lands: the legacy names become
// unexported constants here, and every reader/writer in the rest of the tree
// goes through the canonical "project" names only.
//
// On-disk layout migration is expected to reuse the same Reporter and hook
// points (see ptone/scion#1919).
package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// legacyProjectIDFile is the pre-rename name of the per-project id file.
// Once migrated to projectcompat.ProjectIDFile, this name is never read
// again outside this file.
const legacyProjectIDFile = "grove-id"

// Reporter receives events from the legacy grove->project migration so that
// each host (CLI, hub, broker) can render them in its own idiom: the CLI
// prints "scion: ..." lines to stderr, and hub/broker log structured slog
// records. Implementations must be safe to reuse across a whole process,
// since migration can run for many projects during a single boot.
type Reporter interface {
	// Migrated reports that old was successfully migrated to new. tracked
	// indicates the legacy path was tracked in a user's git repository, so
	// the caller should mention committing the rename.
	Migrated(old, new string, tracked bool)

	// Conflict reports that both old and new exist with different content,
	// so the migration left both in place and used new. detail is a
	// complete, human-readable sentence describing the conflict and how to
	// resolve it.
	Conflict(old, new, detail string)

	// Skipped reports that old could not be migrated (e.g. read-only
	// filesystem, cross-device, not owner). reason is a short cause and
	// manual is a copy-pasteable command the user can run instead.
	Skipped(old, reason, manual string)

	// EnvIgnored reports that the legacy environment variable name is set
	// but is no longer read; replacement is the canonical variable to use
	// instead. The legacy value is never adopted.
	EnvIgnored(name, replacement string)
}

// legacyRemovedEnv pairs a removed legacy environment variable with its
// canonical replacement.
type legacyRemovedEnv struct {
	name        string
	replacement string
}

// removedLegacyEnvVars lists the legacy environment variables that are no
// longer read anywhere in scion. This currently covers SCION_HUB_GROVE_ID;
// SCION_GROVE_ID, SCION_GROVE and SCION_GROVE_PATH join it once agent
// containers stop needing them.
var removedLegacyEnvVars = []legacyRemovedEnv{
	{name: "SCION_HUB_GROVE_ID", replacement: "SCION_HUB_PROJECT_ID"},
}

// isRemovedLegacyEnv reports whether name is one of removedLegacyEnvVars.
// The env-key mappers in koanf.go and settings_v1.go call this to make sure
// a removed legacy variable is never picked up by the generic SCION_*
// fallback mapping (e.g. SCION_HUB_GROVE_ID would otherwise still land on
// the koanf key hub.grove_id, which readers keep honouring as a *file*
// fallback).
func isRemovedLegacyEnv(name string) bool {
	for _, e := range removedLegacyEnvVars {
		if e.name == name {
			return true
		}
	}
	return false
}

// WarnRemovedLegacyEnv reports every removed legacy environment variable
// that is currently set, via report.EnvIgnored. The value is never read for
// any other purpose here; callers must not fall back to it. Unguarded: call
// this directly only where the caller already deduplicates (e.g. the CLI's
// own sync.Once). Hub and broker boot should call WarnRemovedLegacyEnvOnce
// instead.
func WarnRemovedLegacyEnv(getenv func(string) string, report Reporter) {
	if getenv == nil || report == nil {
		return
	}
	for _, e := range removedLegacyEnvVars {
		if getenv(e.name) != "" {
			report.EnvIgnored(e.name, e.replacement)
		}
	}
}

// warnRemovedLegacyEnvOnce guards WarnRemovedLegacyEnvOnce so that a single
// process reports each removed variable at most once, no matter how many
// boot hooks call it. This matters for a combined
// `scion server start --enable-hub --enable-runtime-broker` process: both
// the hub and the runtime broker boot hooks call WarnRemovedLegacyEnvOnce,
// and without a shared guard each would report independently.
var warnRemovedLegacyEnvOnce sync.Once

// WarnRemovedLegacyEnvOnce is the boot-hook entry point for hub server boot
// (cmd/server_foreground.go) and runtime broker boot
// (pkg/runtimebroker/server.go): it behaves like WarnRemovedLegacyEnv, but
// only the first call in the process has any effect.
func WarnRemovedLegacyEnvOnce(getenv func(string) string, report Reporter) {
	warnRemovedLegacyEnvOnce.Do(func() {
		WarnRemovedLegacyEnv(getenv, report)
	})
}

// slogReporter implements Reporter for long-running processes (hub server
// and runtime broker boot) that log structured records instead of writing
// to stderr. Every event uses subsystem=layout-migration (the repo's
// finer-grained-logging convention, pkg/util/logging.Subsystem); Migrated
// logs at Info; everything else logs at Warn.
type slogReporter struct{}

// NewSlogReporter returns a Reporter that logs via logging.Subsystem
// ("layout-migration"), for use at hub and runtime broker boot.
func NewSlogReporter() Reporter {
	return slogReporter{}
}

func (slogReporter) log() *slog.Logger {
	return logging.Subsystem("layout-migration")
}

func (r slogReporter) Migrated(old, new string, tracked bool) {
	r.log().Info("migrated legacy layout", "old", old, "new", new, "tracked", tracked)
}

func (r slogReporter) Conflict(old, new, detail string) {
	r.log().Warn("legacy layout conflict", "old", old, "new", new, "detail", detail)
}

func (r slogReporter) Skipped(old, reason, manual string) {
	r.log().Warn("legacy layout migration skipped", "old", old, "reason", reason, "manual", manual)
}

func (r slogReporter) EnvIgnored(name, replacement string) {
	r.log().Warn("legacy environment variable ignored", "name", name, "replacement", replacement)
}

// ProjectOverrides carries values a caller should use for the current
// invocation when MigrateLegacyProject could not rewrite a legacy file on
// disk (read-only filesystem, not owner). An empty field means "nothing to
// override here; read the canonical file normally" — this covers both the
// common case (no legacy file existed) and the conflict case (a canonical
// file already exists and wins, so the normal read already returns the
// right value).
type ProjectOverrides struct {
	// ProjectID is the value recovered from a legacy .scion/grove-id file
	// when it could not be renamed to .scion/project-id.
	ProjectID string
}

// projectMigrationReporterBox exists only so sync/atomic.Value (which
// requires every Store to use the same concrete type) can hold a Reporter
// interface value, whose concrete type varies (stderrReporter vs
// slogReporter).
type projectMigrationReporterBox struct{ Reporter }

// projectMigrationReporter holds the Reporter used by MigrateLegacyProject.
// It defaults to slog, which is right for hub, runtime broker, and
// sciontool (none of which call SetProjectMigrationReporter); the CLI calls
// SetProjectMigrationReporter(stderrReporter{}) once at boot, from the same
// hook point used for the legacy-env warning (cmd/legacy_grove_migration.go).
var projectMigrationReporter atomic.Value

func init() {
	projectMigrationReporter.Store(projectMigrationReporterBox{NewSlogReporter()})
}

// SetProjectMigrationReporter sets the Reporter that ReadProjectID uses,
// through MigrateLegacyProject, for the remainder of the process. Called
// once at each of the three process-boot hook points (CLI, hub server boot,
// runtime broker boot) so a project's on-disk migration is reported in the
// idiom that fits the process, without threading a Reporter through every
// one of ReadProjectID's callers.
func SetProjectMigrationReporter(r Reporter) {
	projectMigrationReporter.Store(projectMigrationReporterBox{r})
}

func currentProjectMigrationReporter() Reporter {
	return projectMigrationReporter.Load().(projectMigrationReporterBox).Reporter
}

// geteuid is indirected through a package var, the same seam used by
// pkg/shareddirs/walk_unix.go, so a test can simulate "this process does
// not own the file" by overriding it, without needing to actually chown a
// fixture to another uid (which requires root).
var geteuid = os.Geteuid

// linkFile mimics os.Link. Tests override it to force the no-hard-link
// fallback path (as if the filesystem returned EXDEV, EPERM or ENOTSUP), or
// to simulate a concurrent writer finishing between this process's checks
// and its own Link call, without needing an actual such filesystem or a
// second process.
var linkFile = os.Link

// chmodFile mimics (*os.File).Chmod. Tests override it to force the
// no-hard-link copy fallback's mode fix-up to fail, so the cleanup path in
// copyNoClobber can be exercised without a filesystem that actually rejects
// chmod.
var chmodFile = func(f *os.File, mode os.FileMode) error {
	return f.Chmod(mode)
}

// syncFile mimics (*os.File).Sync. Tests override it to force a failure
// after the file's content is already fully written (e.g. an fsync error
// on a filesystem without hard-link support), including one that itself
// runs a concurrent migration against the same directory before returning,
// so copyNoClobber's complete-file-survives behavior can be exercised
// without a real fsync failure or a second process.
var syncFile = func(f *os.File) error {
	return f.Sync()
}

// checkGitTracked is indirected through a package var so tests can count
// calls and assert it is never invoked on a path that did not migrate
// anything (the no-op, conflict, and skipped cases).
var checkGitTracked = gitTracked

// projectMigrationDirState serialises and deduplicates MigrateLegacyProject
// for one project directory: mu makes concurrent or repeated calls run the
// algorithm one at a time (so a filesystem race between two goroutines in
// this process can produce at most one successful migration and one
// report, matching the cross-process safety the algorithm already provides
// via Link/Rename semantics), and the reportedX flags make sure each event
// kind (Migrated, Conflict, Skipped) is reported at most once per process
// for this directory, no matter how many times it recurs across separate
// calls with unchanged on-disk state.
//
// Results are never cached here: every call re-runs the cheap Lstat(old)
// check (and the rest of the algorithm, if there is work to do), so a
// project-id deleted after a successful migration is not hidden behind a
// stale id, and a grove-id that appears later in the process's life is
// still picked up.
type projectMigrationDirState struct {
	mu               sync.Mutex
	reportedMigrated bool
	reportedConflict bool
	reportedSkipped  bool
}

// projectMigrationDirs is keyed by projectMigrationDirKey(absolute dir).
var projectMigrationDirs sync.Map

// projectMigrationDirKey resolves dir through symlinks so that two spellings
// of the same directory (e.g. one through a symlinked home) share one
// projectMigrationDirState and don't each report independently. Falls back
// to filepath.Clean when the directory can't be resolved (e.g. it doesn't
// exist yet).
func projectMigrationDirKey(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return filepath.Clean(dir)
}

func projectMigrationStateFor(dir string) *projectMigrationDirState {
	key := projectMigrationDirKey(dir)
	v, _ := projectMigrationDirs.LoadOrStore(key, &projectMigrationDirState{})
	return v.(*projectMigrationDirState)
}

// projectMigrationDedupReporter wraps a Reporter so that Migrated, Conflict,
// and Skipped each fire at most once per process for one projectMigrationDirState,
// while still reflecting the freshly recomputed result of every call.
type projectMigrationDedupReporter struct {
	st   *projectMigrationDirState
	next Reporter
}

func (d projectMigrationDedupReporter) Migrated(old, new string, tracked bool) {
	if d.st.reportedMigrated {
		return
	}
	d.st.reportedMigrated = true
	d.next.Migrated(old, new, tracked)
}

func (d projectMigrationDedupReporter) Conflict(old, new, detail string) {
	if d.st.reportedConflict {
		return
	}
	d.st.reportedConflict = true
	d.next.Conflict(old, new, detail)
}

func (d projectMigrationDedupReporter) Skipped(old, reason, manual string) {
	if d.st.reportedSkipped {
		return
	}
	d.st.reportedSkipped = true
	d.next.Skipped(old, reason, manual)
}

func (d projectMigrationDedupReporter) EnvIgnored(name, replacement string) {
	d.next.EnvIgnored(name, replacement)
}

// MigrateLegacyProject migrates a project's legacy .scion/grove-id file to
// .scion/project-id. It is called from ReadProjectID, so every one of that
// function's callers gets the same behaviour: on a writable filesystem the
// file is renamed on disk; when it cannot be rewritten, the returned
// ProjectOverrides carries the legacy value for this invocation only.
// ProjectOverrides is otherwise empty: on success the canonical file now
// holds the right content, so the caller's own, immediately following read
// of it already returns the right value, and a stale override is never
// left behind for a later call to trip over.
//
// Safe to call concurrently and repeatedly, from any number of goroutines
// and call sites, for the same or different project directories: calls for
// the same directory are serialised, and each event (Migrated, Conflict,
// Skipped) is reported at most once per process for that directory. Every
// call still re-examines the filesystem, so a project-id removed after a
// successful migration, or a grove-id that appears later, are both seen.
func MigrateLegacyProject(projectDir string, report Reporter) ProjectOverrides {
	dir := projectDir
	if abs, err := filepath.Abs(projectDir); err == nil {
		dir = abs
	}
	st := projectMigrationStateFor(dir)
	st.mu.Lock()
	defer st.mu.Unlock()
	id := migrateLegacyProjectFile(dir, projectMigrationDedupReporter{st: st, next: report})
	return ProjectOverrides{ProjectID: id}
}

// migrateLegacyProjectFile implements the file-rename algorithm for
// .scion/grove-id -> .scion/project-id and returns the id recovered from
// the legacy file, or "" if there was nothing to migrate or a canonical
// file already exists (it wins; the caller's normal read already returns
// the right value in that case).
func migrateLegacyProjectFile(dir string, report Reporter) string {
	old := filepath.Join(dir, legacyProjectIDFile)
	newPath := filepath.Join(dir, projectcompat.ProjectIDFile)

	oldInfo, err := os.Lstat(old)
	if err != nil {
		return ""
	}

	if uid, ok := fileOwnerUID(oldInfo); ok && uid != geteuid() {
		report.Skipped(old, "owned by another user", manualRenameCommand(old, newPath))
		return readTrimmed(old)
	}

	if _, err := os.Lstat(newPath); err == nil {
		return resolveExistingProjectIDFiles(old, newPath, report)
	}

	if linkErr := linkFile(old, newPath); linkErr != nil {
		if errors.Is(linkErr, fs.ErrExist) {
			return resolveExistingProjectIDFiles(old, newPath, report)
		}
		if errors.Is(linkErr, fs.ErrNotExist) {
			// oldpath is resolved before newpath, so a concurrent process
			// that finished the full migration (Link then Remove(old))
			// between our Lstat(old) above and this Link call surfaces as
			// ENOENT here, not EEXIST. resolveExistingProjectIDFiles
			// already treats a missing old as "someone else finished" and
			// returns silently.
			return resolveExistingProjectIDFiles(old, newPath, report)
		}
		if !linkUnsupported(linkErr) {
			report.Skipped(old, unwrapErrno(linkErr), manualRenameCommand(old, newPath))
			return readTrimmed(old)
		}
		// No hard-link support (EXDEV/EPERM/ENOTSUP): write a fresh copy
		// instead. Never falls back silently to something that could
		// clobber a concurrent writer: O_EXCL still guards the create.
		if copyErr := copyNoClobber(old, newPath, oldInfo.Mode().Perm()); copyErr != nil {
			if errors.Is(copyErr, fs.ErrExist) {
				return resolveExistingProjectIDFiles(old, newPath, report)
			}
			if errors.Is(copyErr, fs.ErrNotExist) {
				// old vanished mid-copy (a concurrent process finished);
				// same reasoning as the Link ENOENT case above.
				return resolveExistingProjectIDFiles(old, newPath, report)
			}
			report.Skipped(old, unwrapErrno(copyErr), manualRenameCommand(old, newPath))
			return readTrimmed(old)
		}
	}

	return finishProjectFileMigration(old, newPath, report)
}

// finishProjectFileMigration removes the now-redundant legacy file and
// reports success. Called once the canonical file is known to hold the
// right content, whether that happened via a hard link or a fresh copy.
// Returns "" (not the migrated id): the canonical file now holds the right
// content, so the caller's own, immediately following read of it already
// returns the right value; see ProjectOverrides.
func finishProjectFileMigration(old, newPath string, report Reporter) string {
	// Best-effort: a lingering duplicate (any error other than "already
	// gone") is caught and cleaned up as a "both exist, same value" case on
	// a later call.
	_ = os.Remove(old)
	report.Migrated(old, newPath, checkGitTracked(old))
	return ""
}

// resolveExistingProjectIDFiles handles both the case where the canonical
// file already existed before migration started, and the case where a
// concurrent process created it between this process's checks and its own
// Link call: a Link EEXIST (or ENOENT — see the caller) re-enters this
// branch exactly once, and it never loops back into a Link attempt itself.
func resolveExistingProjectIDFiles(old, newPath string, report Reporter) string {
	oldData, err := os.ReadFile(old)
	if err != nil {
		// A concurrent process already finished migrating (old is gone by
		// now): nothing left for this caller to do or report.
		return ""
	}
	newData, err := os.ReadFile(newPath)
	if err != nil {
		// new disappeared between the existence check and this read; leave
		// it for a later call to sort out rather than guessing here.
		return strings.TrimSpace(string(oldData))
	}

	oldTrim, newTrim := strings.TrimSpace(string(oldData)), strings.TrimSpace(string(newData))
	if oldTrim == newTrim {
		// Best-effort, as in finishProjectFileMigration.
		_ = os.Remove(old)
		report.Migrated(old, newPath, checkGitTracked(old))
		// "" per ProjectOverrides: newPath already holds the right value.
		return ""
	}

	report.Conflict(old, newPath, fmt.Sprintf(
		"both %s and %s exist with different values; using project-id. "+
			"Remove %s (after checking its value) to silence this warning.",
		old, newPath, old))
	return ""
}

// linkUnsupported reports whether err from linkFile indicates the
// filesystem does not support hard links across old and new (EXDEV, EPERM,
// or ENOTSUP), as opposed to some other failure that should be reported as
// Skipped outright.
func linkUnsupported(err error) bool {
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) {
		return false
	}
	return errors.Is(linkErr.Err, syscall.EXDEV) ||
		errors.Is(linkErr.Err, syscall.EPERM) ||
		errors.Is(linkErr.Err, syscall.ENOTSUP)
}

// copyNoClobber writes old's content to newPath with the given permissions,
// failing with fs.ErrExist if newPath already exists (O_EXCL), and fsyncs
// before closing so the content is durable before old is removed. Once the
// O_EXCL create has succeeded, a Chmod or Write failure removes newPath
// before returning, since the file this call created is then incomplete —
// its content can never equal the legacy file's, so nothing else could have
// started relying on it, and leaving an empty or partial project-id behind
// (which would win every future "both exist" comparison against the intact
// legacy file) is never an acceptable outcome of a failed attempt.
//
// A Sync or Close failure past that point does NOT remove newPath: the
// content is already complete and identical to the legacy file, and a
// concurrent process on the same directory may already be relying on
// exactly that content to finish its own migration (the "both exist, same
// value" branch), including removing the legacy file. Deleting newPath out
// from under that process would lose the id entirely, with neither file
// left — worse than the empty/partial-file problem this cleanup exists to
// prevent. The caller still reports the Sync/Close failure as Skipped, so
// the legacy file is not removed by this process either.
func copyNoClobber(old, newPath string, perm os.FileMode) error {
	data, err := os.ReadFile(old)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(newPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}

	var complete bool
	writeErr := func() error {
		// OpenFile's perm argument is filtered by the process umask (e.g. a
		// 0664 legacy file becomes 0644 under a 0022 umask), so fix the
		// mode up explicitly to match the original file exactly.
		if err := chmodFile(f, perm); err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			return err
		}
		complete = true
		return syncFile(f)
	}()
	closeErr := f.Close()

	// !complete already implies writeErr != nil (the closure above only
	// leaves complete false by returning early with a non-nil error), so
	// this doesn't need to check writeErr/closeErr again.
	if !complete {
		_ = os.Remove(newPath)
	}
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// fileOwnerUID returns the uid that owns the file described by info. ok is
// false only if the platform does not expose *syscall.Stat_t; scion is
// built for linux and darwin, both of which do.
func fileOwnerUID(info os.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

// readTrimmed returns the trimmed content of path, or "" if it cannot be
// read.
func readTrimmed(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// manualRenameCommand returns the copy-pasteable command a Skipped report
// suggests the user run by hand.
func manualRenameCommand(old, newPath string) string {
	return fmt.Sprintf("mv %s %s", old, newPath)
}

// unwrapErrno returns the innermost errno-level message from a link or path
// error — e.g. "read-only file system" rather than the full
// "link /a/grove-id /a/project-id: read-only file system" — matching the
// wire format used in the Skipped warning.
func unwrapErrno(err error) string {
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return linkErr.Err.Error()
	}
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

// gitTrackedTimeout bounds how long gitTracked waits for git before giving
// up. A test can shrink this to exercise the timeout path.
var gitTrackedTimeout = 5 * time.Second

// gitTracked reports whether path is tracked by a git repository, by
// running `git ls-files --error-unmatch` in path's directory. Callers must
// only call this after a migration actually happened: it is meant to
// produce the "commit the rename" hint, not a general-purpose git check.
// Returns false if git is not installed, path's directory is not a
// repository, the file is not tracked, or the command does not finish
// within gitTrackedTimeout: a timeout only loses the hint, it does not fail
// the migration.
func gitTracked(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTrackedTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", filepath.Dir(abs), "ls-files", "--error-unmatch", filepath.Base(abs))
	return cmd.Run() == nil
}
