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
	"bytes"
	"context"
	encjson "encoding/json"
	"errors"
	"fmt"
	"io"
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
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"gopkg.in/yaml.v3"
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

	// PrecedenceChanged reports that migrating a project's own hub.grove_id
	// populated hub.project_id with value, which differs from other, the
	// value an already-loaded global settings file provided. Before
	// per-file migration, a project-level hub.grove_id was only consulted
	// when no file in the merge set hub.project_id, so a global value like
	// other used to win here; after migration the project's own value wins
	// through normal project-over-global precedence instead (see
	// migrateProjectSettingsFile).
	PrecedenceChanged(path, value, other string)
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
// the koanf key hub.grove_id, an unrecognised key nothing reads).
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

func (r slogReporter) PrecedenceChanged(path, value, other string) {
	r.log().Info("project hub.project_id now takes precedence over global",
		"path", path, "value", value, "previous", other)
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

// renameFile mimics os.Rename for algorithm C's commit-only rename (the tmp
// file created by migrateLegacyYAMLKeysLocked onto the real path). Tests
// override it to force a failure after a conflict backup has already been
// written, so the backup-removed-on-rename-failure path can be exercised
// without a filesystem that actually rejects rename.
var renameFile = os.Rename

// backupWriteFile mimics (*os.File).Write for writeConflictBackup's own
// write to the backup file. Tests override it to force a failure after the
// backup file has been created, so the partial-file cleanup can be
// exercised without a filesystem that actually rejects the write.
var backupWriteFile = func(f *os.File, data []byte) (int, error) {
	return f.Write(data)
}

// backupCloseFile mimics (*os.File).Close for writeConflictBackup's own
// backup file, the same way backupWriteFile stands in for its Write.
var backupCloseFile = func(f *os.File) error {
	return f.Close()
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

	// reportedYAMLEvents dedups algorithm C's per-key events ("migrated:
	// grove-id", "conflict:grove_id", ...) at finer granularity than the
	// three booleans above: a single call can legitimately report several
	// distinct keys (a marker file's grove-id, grove-name and grove-slug),
	// so dedup must be keyed by event+key, not just fired once for the
	// whole file.
	reportedYAMLEvents map[string]bool
}

// yamlEventOnce reports whether kind+key has not been reported before for
// this file, recording it as reported either way. Used by
// migrateLegacyYAMLKeysLocked while holding st.mu.
func (st *projectMigrationDirState) yamlEventOnce(kind, key string) bool {
	if st.reportedYAMLEvents == nil {
		st.reportedYAMLEvents = map[string]bool{}
	}
	full := kind + ":" + key
	if st.reportedYAMLEvents[full] {
		return false
	}
	st.reportedYAMLEvents[full] = true
	return true
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

func (d projectMigrationDedupReporter) PrecedenceChanged(path, value, other string) {
	d.next.PrecedenceChanged(path, value, other)
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

// legacyYAMLKeyRename describes one legacy->canonical YAML key that
// migrateLegacyYAMLKeys looks for in a document. parent names the
// containing mapping ("" for a top-level key, "hub" for a key nested one
// level under a top-level "hub:" mapping); scion's legacy keys never nest
// more deeply than that.
type legacyYAMLKeyRename struct {
	parent    string
	legacy    string
	canonical string
}

// markerKeyRenames lists the legacy top-level keys a .scion marker file may
// still use.
var markerKeyRenames = []legacyYAMLKeyRename{
	{legacy: "grove-id", canonical: "project-id"},
	{legacy: "grove-name", canonical: "project-name"},
	{legacy: "grove-slug", canonical: "project-slug"},
}

// hubGroveIDRename is the legacy settings-file key migrateProjectSettingsFile
// looks for.
var hubGroveIDRename = []legacyYAMLKeyRename{
	{parent: "hub", legacy: "grove_id", canonical: "project_id"},
}

// joinKey renders parent+key the way they appear in a dotted settings key
// (e.g. "hub.grove_id"), or just key for a top-level marker key.
func joinKey(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// migrateLegacyMarkerFile migrates a .scion marker file's legacy grove-id,
// grove-name and grove-slug keys to project-id, project-name and
// project-slug in place. The returned map holds the legacy value for every
// canonical field the file lacked, whether or not the on-disk rewrite
// succeeded: ReadProjectMarker's own subsequent read of the file already
// returns the right value when the rewrite did succeed, so it only actually
// consults this map for a field its own read still finds empty (the file
// could not be rewritten) — a field is absent from the map only when the
// file already had that canonical key (nothing for the caller to fall back
// to; its own read is authoritative) or had no legacy key for it at all.
func migrateLegacyMarkerFile(path string) map[string]string {
	return migrateLegacyYAMLKeys(path, markerKeyRenames, currentProjectMigrationReporter())
}

// migrateProjectSettingsFile migrates a project or global settings.yaml's
// legacy hub.grove_id key to hub.project_id in place. path must already be
// resolved to a specific settings file (see loadSettingsFile and
// LoadSingleFileVersioned). migrated reports whether a hub.grove_id key was
// found and had no existing hub.project_id to yield to (the "surgical" case,
// which is also the precedence-change trigger callers check for); override,
// when non-empty, is the value a caller should use in memory if the on-disk
// rewrite failed.
//
// Only YAML settings files are in scope: the surgical rewrite is
// line/column based against a YAML parse, and settings.json is rare enough
// that it is left alone here (its hub.grove_id, if any, simply stops being
// read, like any other unrecognised key).
func migrateProjectSettingsFile(path string) (migrated bool, override string) {
	switch filepath.Ext(path) {
	case ".yaml", ".yml":
	default:
		return false, ""
	}
	overrides := migrateLegacyYAMLKeys(path, hubGroveIDRename, currentProjectMigrationReporter())
	v, ok := overrides[projectcompat.ConfigProjectIDKey]
	return ok, v
}

// settingsJSONHasLegacyHubGroveID reports whether the JSON settings file at
// path has a top-level "hub": {"grove_id": ...} entry with no sibling
// "project_id". Malformed or unreadable JSON is treated as "no", the same
// as any other settings-file read failure elsewhere in this package.
func settingsJSONHasLegacyHubGroveID(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var raw map[string]encjson.RawMessage
	if err := encjson.Unmarshal(data, &raw); err != nil {
		return false
	}
	hubRaw, ok := raw[hubGroveIDRename[0].parent]
	if !ok {
		return false
	}
	var hub map[string]encjson.RawMessage
	if err := encjson.Unmarshal(hubRaw, &hub); err != nil {
		return false
	}
	_, hasLegacy := hub[hubGroveIDRename[0].legacy]
	_, hasCanonical := hub[hubGroveIDRename[0].canonical]
	return hasLegacy && !hasCanonical
}

// warnUnmigratedHubGroveID reports, once per process per file, that a
// settings file's legacy hub.grove_id key exists but was not migrated
// (algorithm C only rewrites a literal "grove_id:" entry directly under a
// YAML "hub:" mapping): unlike the old merged-config remap, which read it
// from any format or shape, this key now simply goes unread outside that
// one shape, so the user needs an explicit pointer rather than a silent,
// unlinked project. reason names the specific gap (JSON format, a YAML
// merge key, ...).
func warnUnmigratedHubGroveID(path string, report Reporter, reason string) {
	st := projectMigrationStateFor(path)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.yamlEventOnce("skipped", hubGroveIDRename[0].legacy) {
		return
	}
	report.Skipped(path+":"+joinKey(hubGroveIDRename[0].parent, hubGroveIDRename[0].legacy),
		reason,
		manualKeyRenameCommand(hubGroveIDRename[0], path))
}

// betweenReadAndRename is called after migrateLegacyYAMLKeys has written its
// replacement content to a temp file but before it re-reads the target path
// to compare against the bytes it started from. Tests override it to land a
// concurrent writer (another scion process, or a hand edit) in that window,
// without needing real timing-dependent concurrency.
var betweenReadAndRename = func() {}

// legacyKeyAction is one legacyYAMLKeyRename found present in a document,
// together with what migrateLegacyYAMLKeys decided to do about it.
type legacyKeyAction struct {
	rename       legacyYAMLKeyRename
	legacyNode   *yaml.Node
	legacyValue  string
	canonicalSet bool // a canonical key already existed alongside the legacy one
	conflict     bool // canonical existed with a different value
}

// migrateLegacyYAMLKeys implements the YAML key rewrite algorithm shared by
// marker keys and hub.grove_id: it finds every rename whose legacy key is
// present in file, migrates each to its canonical name, and returns the
// legacy value for any canonical field that could not be written to disk
// (read-only filesystem, not owner) — the in-memory fallback described in
// the package doc comment.
//
// Safe to call repeatedly and concurrently for the same file, from any
// number of goroutines and call sites: each distinct key's event (Migrated,
// Conflict, Skipped) is reported at most once per process for that file,
// reusing the same per-path state MigrateLegacyProject uses for the id file
// (keyed by the exact path given, so a marker file's state and a project
// directory's state never collide) — but keyed additionally by which legacy
// key the event is about, since one call can legitimately migrate several
// distinct keys (a marker file's grove-id, grove-name and grove-slug) and
// each of those must still be reported the first time. Every call still
// re-examines the filesystem, so a file edited or migrated by another
// process between calls is always picked up correctly.
func migrateLegacyYAMLKeys(file string, renames []legacyYAMLKeyRename, report Reporter) map[string]string {
	path := file
	if resolved, err := filepath.EvalSymlinks(file); err == nil {
		// A dotfile-managed settings file: rewrite the resolved target in
		// place and leave the link alone.
		path = resolved
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	st := projectMigrationStateFor(path)
	st.mu.Lock()
	defer st.mu.Unlock()
	return migrateLegacyYAMLKeysLocked(path, renames, report, st, 0)
}

func migrateLegacyYAMLKeysLocked(path string, renames []legacyYAMLKeyRename, report Reporter, st *projectMigrationDirState, attempt int) map[string]string {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	orig, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(orig, &doc); err != nil || len(doc.Content) == 0 {
		return nil // malformed or empty YAML: leave it alone
	}
	root := doc.Content[0]

	actions := planLegacyKeyActions(root, renames)
	if len(actions) == 0 {
		return nil // no legacy keys present: nothing to migrate, no write
	}
	overrides := legacyValueOverrides(actions)

	if uid, ok := fileOwnerUID(info); ok && uid != geteuid() {
		reportActionsSkipped(report, st, actions, path, "owned by another user")
		return overrides
	}

	allSurgical := allActionsSurgical(actions)
	if !allSurgical && hasMultipleYAMLDocuments(orig) {
		// A re-encode only ever writes the first parsed document; refuse
		// rather than silently drop everything after the first "---".
		reportActionsSkipped(report, st, actions, path,
			"settings file has multiple YAML documents; refusing an automatic rewrite")
		return overrides
	}

	newContent, hasConflict := applyLegacyKeyActions(orig, &doc, root, actions, allSurgical)

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".migrate-*")
	if err != nil {
		reportActionsSkipped(report, st, actions, path, unwrapErrno(err))
		return overrides
	}
	tmpPath := tmp.Name()
	writeOK := func() bool {
		if err := chmodFile(tmp, info.Mode().Perm()); err != nil {
			return false
		}
		if _, err := tmp.Write(newContent); err != nil {
			return false
		}
		return syncFile(tmp) == nil
	}()
	closeErr := tmp.Close()
	if !writeOK || closeErr != nil {
		_ = os.Remove(tmpPath)
		reportActionsSkipped(report, st, actions, path, "could not write migrated file")
		return overrides
	}

	betweenReadAndRename()

	if cur, err := os.ReadFile(path); err != nil || !bytes.Equal(cur, orig) {
		_ = os.Remove(tmpPath)
		if attempt == 0 {
			return migrateLegacyYAMLKeysLocked(path, renames, report, st, attempt+1)
		}
		reportActionsSkipped(report, st, actions, path, "file changed during migration")
		return overrides
	}

	// The backup is written only once every earlier check has already
	// succeeded, immediately before the atomic rename that commits the
	// migration: a backup only belongs on disk paired with a completed
	// migration, so a rename failure below removes it again rather than
	// leaving an orphan behind.
	var backupPath string
	if hasConflict {
		if backupPath, err = writeConflictBackup(path, orig); err != nil {
			_ = os.Remove(tmpPath)
			reportActionsSkipped(report, st, actions, path, unwrapErrno(err))
			return overrides
		}
	}

	if err := renameFile(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		if hasConflict {
			_ = os.Remove(backupPath)
		}
		reportActionsSkipped(report, st, actions, path, unwrapErrno(err))
		return overrides
	}

	tracked := checkGitTracked(path)
	for _, a := range actions {
		key := joinKey(a.rename.parent, a.rename.legacy)
		canonical := joinKey(a.rename.parent, a.rename.canonical)
		if a.conflict {
			if st.yamlEventOnce("conflict", a.rename.legacy) {
				report.Conflict(path+":"+key, path+":"+canonical, fmt.Sprintf(
					"both %s and %s exist in %s with different values; using %s. Original value saved to %s.",
					key, canonical, path, canonical, backupPath))
			}
			continue
		}
		if st.yamlEventOnce("migrated", a.rename.legacy) {
			// Matches the CLI wire format: "scion: migrated hub.grove_id ->
			// hub.project_id in <file>".
			report.Migrated(key, canonical+" in "+path, tracked)
		}
	}
	return overrides
}

// planLegacyKeyActions finds, for each rename, whether its legacy key is
// present in root, and if so whether a canonical key already sits alongside
// it (and with what relationship to the legacy value).
func planLegacyKeyActions(root *yaml.Node, renames []legacyYAMLKeyRename) []legacyKeyAction {
	var actions []legacyKeyAction
	for _, r := range renames {
		mapping := findChildMapping(root, r.parent)
		legacyKey, legacyVal := findMapKey(mapping, r.legacy)
		if legacyKey == nil {
			continue
		}
		legacyVal = resolveAlias(legacyVal)
		if legacyVal == nil {
			continue // a broken alias; leave the file alone rather than guess
		}
		a := legacyKeyAction{rename: r, legacyNode: legacyKey, legacyValue: legacyVal.Value}
		if _, canonicalVal := findMapKey(mapping, r.canonical); canonicalVal != nil {
			canonicalVal = resolveAlias(canonicalVal)
			a.canonicalSet = true
			a.conflict = canonicalVal == nil || canonicalVal.Value != legacyVal.Value
		}
		actions = append(actions, a)
	}
	return actions
}

// resolveAlias follows n through any YAML anchors/aliases (`grove-id: *v`) to
// the node it actually refers to, so value comparisons and the in-memory
// override read the real value rather than the anchor name. Returns nil for
// a dangling alias. Non-alias nodes are returned unchanged.
func resolveAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// allActionsSurgical reports whether every action is a pure key rename (no
// canonical key already present for any of them), the only case where the
// byte-level rewrite path applies.
func allActionsSurgical(actions []legacyKeyAction) bool {
	for _, a := range actions {
		if a.canonicalSet {
			return false
		}
	}
	return true
}

// hasMultipleYAMLDocuments reports whether data contains more than one
// "---"-separated YAML document. yaml.Unmarshal (and this file's own Node
// parse) only ever sees the first; a re-encode built from that parse would
// silently drop everything after it.
func hasMultipleYAMLDocuments(data []byte) bool {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var first yaml.Node
	if err := dec.Decode(&first); err != nil {
		return false
	}
	var second yaml.Node
	// Anything other than a clean end-of-input after the first document —
	// including a second document that is itself malformed — means a
	// re-encode would not round-trip the file, so it must fail closed here
	// too, not just when the second document parses cleanly.
	err := dec.Decode(&second)
	return !errors.Is(err, io.EOF)
}

// legacyValueOverrides returns, for each action whose canonical key was
// absent (the only case where a caller might need the legacy value if the
// write fails), the legacy value keyed by canonical name.
func legacyValueOverrides(actions []legacyKeyAction) map[string]string {
	var overrides map[string]string
	for _, a := range actions {
		if !a.canonicalSet {
			if overrides == nil {
				overrides = map[string]string{}
			}
			overrides[a.rename.canonical] = a.legacyValue
		}
	}
	return overrides
}

// applyLegacyKeyActions builds the replacement file content for actions.
// When every action is a pure rename (no canonical key already present), it
// edits the legacy key tokens directly on orig's bytes, preserving comments,
// order and formatting byte-for-byte. Otherwise (a canonical key already
// exists for at least one action) it re-encodes the whole document from the
// parsed tree; formatting may normalise, which the design accepts as rare.
// allSurgical must equal allActionsSurgical(actions); the caller already
// computes it once to gate the multi-document check.
func applyLegacyKeyActions(orig []byte, doc *yaml.Node, root *yaml.Node, actions []legacyKeyAction, allSurgical bool) (newContent []byte, hasConflict bool) {
	for _, a := range actions {
		if a.conflict {
			hasConflict = true
		}
	}

	if allSurgical {
		lines := bytes.Split(append([]byte(nil), orig...), []byte("\n"))
		ok := true
		for _, a := range actions {
			if !surgicalRenameKey(lines, a.legacyNode, a.rename.canonical) {
				ok = false
				break
			}
		}
		if ok {
			return bytes.Join(lines, []byte("\n")), false
		}
		// A line didn't match what the parser reported for it (e.g. an
		// exotic quoting form): fall through to the safe re-encode path
		// below rather than risk writing a corrupted file.
	}

	for _, a := range actions {
		if !a.canonicalSet {
			a.legacyNode.Value = a.rename.canonical
			continue
		}
		deleteMapKey(findChildMapping(root, a.rename.parent), a.rename.legacy)
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return orig, hasConflict
	}
	return out, hasConflict
}

// utf8BOM is the byte-order-mark yaml.v3 skips before counting columns, but
// which is still physically present at the start of the original file.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// surgicalRenameKey replaces node's key token, at its recorded Line/Column,
// with canonical, in place within lines (as produced by bytes.Split(orig,
// "\n")). node.Column is a 1-indexed *rune* column (yaml.v3's convention),
// not a byte offset, so it is converted by walking runes; line 0 additionally
// accounts for a leading BOM, which yaml.v3 strips before counting columns.
// A key written with quotes ("grove-id": ...) has its Column pointing at the
// opening quote; the token comparison and replacement happen between the
// quotes, leaving the quote characters themselves in place. Returns false,
// leaving lines untouched, if the bytes at that position don't match node's
// own value — a defensive check that never trusts stale coordinates into
// producing a corrupt rewrite.
func surgicalRenameKey(lines [][]byte, node *yaml.Node, canonical string) bool {
	idx := node.Line - 1
	if idx < 0 || idx >= len(lines) {
		return false
	}
	line := lines[idx]
	off, ok := runeColumnToByteOffset(line, node.Column, idx == 0)
	if !ok {
		return false
	}

	var quote byte
	if off < len(line) && (line[off] == '"' || line[off] == '\'') {
		quote = line[off]
		off++
	}

	legacy := node.Value
	end := off + len(legacy)
	if off < 0 || end > len(line) || string(line[off:end]) != legacy {
		return false
	}
	if quote != 0 && (end >= len(line) || line[end] != quote) {
		return false
	}

	newLine := make([]byte, 0, len(line)-len(legacy)+len(canonical))
	newLine = append(newLine, line[:off]...)
	newLine = append(newLine, canonical...)
	newLine = append(newLine, line[end:]...)
	lines[idx] = newLine
	return true
}

// runeColumnToByteOffset converts a 1-indexed, rune-counted yaml.v3 Column
// on line into a 0-indexed byte offset. firstLine skips a leading UTF-8 BOM
// before counting, matching yaml.v3's own column numbering, while still
// returning an offset relative to line's real bytes (BOM included).
func runeColumnToByteOffset(line []byte, column int, firstLine bool) (int, bool) {
	if column < 1 {
		return 0, false
	}
	rest := line
	prefix := 0
	if firstLine && bytes.HasPrefix(rest, utf8BOM) {
		prefix = len(utf8BOM)
		rest = rest[prefix:]
	}
	runeIdx := 1
	byteIdx := 0
	for byteIdx < len(rest) {
		if runeIdx == column {
			return prefix + byteIdx, true
		}
		_, size := utf8.DecodeRune(rest[byteIdx:])
		if size == 0 {
			return 0, false
		}
		byteIdx += size
		runeIdx++
	}
	if runeIdx == column {
		return prefix + byteIdx, true
	}
	return 0, false
}

// findChildMapping returns root itself for name == "", or the mapping node
// of the top-level key name within root (nil if absent or not a mapping,
// following an alias first so `hub: *anchor` resolves to the real mapping).
func findChildMapping(root *yaml.Node, name string) *yaml.Node {
	if name == "" {
		return root
	}
	_, val := findMapKey(root, name)
	val = resolveAlias(val)
	if val == nil || val.Kind != yaml.MappingNode {
		return nil
	}
	return val
}

// findMapKey returns the key and value nodes for name in mapping's Content
// (alternating key/value pairs), or nil, nil if mapping is nil or has no
// such key.
func findMapKey(mapping *yaml.Node, name string) (key, value *yaml.Node) {
	if mapping == nil {
		return nil, nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == name {
			return mapping.Content[i], mapping.Content[i+1]
		}
	}
	return nil, nil
}

// deleteMapKey removes name's key/value pair from mapping's Content, if
// present.
func deleteMapKey(mapping *yaml.Node, name string) {
	if mapping == nil {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == name {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// writeConflictBackup writes orig, unchanged, to path+".grove-migration.bak"
// (or that name with a numeric suffix if it's already taken), using
// O_CREAT|O_EXCL so it never clobbers an existing backup. Returns the name
// actually used. A backup file only ever exists complete or not at all: a
// write or close failure removes the partial candidate before returning the
// error.
func writeConflictBackup(path string, orig []byte) (string, error) {
	base := path + ".grove-migration.bak"
	for n := 0; ; n++ {
		candidate := base
		if n > 0 {
			candidate = fmt.Sprintf("%s.%d", base, n)
		}
		f, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", err
		}
		_, writeErr := backupWriteFile(f, orig)
		closeErr := backupCloseFile(f)
		if writeErr != nil {
			_ = os.Remove(candidate)
			return "", writeErr
		}
		if closeErr != nil {
			_ = os.Remove(candidate)
			return "", closeErr
		}
		return candidate, nil
	}
}

// reportActionsSkipped reports Skipped, once per action, each with its own
// manual rename command.
func reportActionsSkipped(report Reporter, st *projectMigrationDirState, actions []legacyKeyAction, path, reason string) {
	for _, a := range actions {
		if st.yamlEventOnce("skipped", a.rename.legacy) {
			report.Skipped(path+":"+joinKey(a.rename.parent, a.rename.legacy), reason, manualKeyRenameCommand(a.rename, path))
		}
	}
}

// manualKeyRenameCommand returns the copy-pasteable instruction a Skipped
// report suggests for a YAML key rename.
func manualKeyRenameCommand(r legacyYAMLKeyRename, path string) string {
	return fmt.Sprintf("rename key `%s` to `%s` in %s", joinKey(r.parent, r.legacy), joinKey(r.parent, r.canonical), path)
}
