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

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"gopkg.in/yaml.v3"
)

// legacyProjectIDFile is the pre-rename name of the per-project id file.
// Once migrated to projectkeys.ProjectIDFile, this name is never read
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
// longer read anywhere in scion.
var removedLegacyEnvVars = []legacyRemovedEnv{
	{name: "SCION_HUB_GROVE_ID", replacement: "SCION_HUB_PROJECT_ID"},
	{name: "SCION_GROVE_ID", replacement: "SCION_PROJECT_ID"},
	{name: "SCION_GROVE", replacement: "SCION_PROJECT"},
	{name: "SCION_GROVE_PATH", replacement: "SCION_PROJECT_PATH"},
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
	newPath := filepath.Join(dir, projectkeys.ProjectIDFile)

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
// suggests the user run by hand. Both paths are shell-quoted: they can come
// from a project directory, a HOME, or an entry name containing a space or
// another shell metacharacter, none of which scion controls.
func manualRenameCommand(old, newPath string) string {
	return fmt.Sprintf("mv %s %s", shellQuote(old), shellQuote(newPath))
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

// legacyProjectsDirName and legacyProjectConfigsDirName are the pre-rename
// names of the two global ~/.scion directories migrated by
// MigrateLegacyGlobalLayout. Once migrated to their projectkeys.ProjectsDir
// / ProjectConfigsDir replacements, these names are never read again outside
// this file.
const (
	legacyProjectsDirName       = "groves"
	legacyProjectConfigsDirName = "grove-configs"
)

// legacyGlobalRoot pairs one legacy global ~/.scion directory with its
// canonical replacement.
type legacyGlobalRoot struct {
	legacyName    string
	canonicalName string
}

// legacyGlobalRoots lists the directories MigrateLegacyGlobalLayout moves,
// in order.
var legacyGlobalRoots = []legacyGlobalRoot{
	{legacyName: legacyProjectsDirName, canonicalName: projectkeys.ProjectsDir},
	{legacyName: legacyProjectConfigsDirName, canonicalName: projectkeys.ProjectConfigsDir},
}

// renameDir renames one legacy global-root entry to its canonical
// destination for migrateLegacyGlobalEntry. It calls syscall.Rename
// directly rather than os.Rename: os.Rename deliberately refuses to ever
// replace an existing directory (see the newname-is-a-directory check in the
// standard library's os.rename), but this migration relies on the underlying
// rename(2) semantics it deliberately routes around — the destination may
// already exist as an *empty* directory, which rename(2) replaces
// atomically the same as if it were absent. The result is wrapped in an
// os.LinkError so the same errno-classification helpers used elsewhere in
// this file work unchanged (isDirConflictError, isCrossDeviceError,
// unwrapErrno, and errors.Is(_, fs.ErrNotExist)).
//
// Tests override this var directly to simulate a cross-device move (EXDEV)
// without needing an actual such filesystem.
var renameDir = func(oldname, newname string) error {
	if err := syscall.Rename(oldname, newname); err != nil {
		return &os.LinkError{Op: "rename", Old: oldname, New: newname, Err: err}
	}
	return nil
}

// MigrateLegacyGlobalLayout moves the legacy ~/.scion/groves and
// ~/.scion/grove-configs directories into their canonical replacements,
// ~/.scion/projects and ~/.scion/project-configs, leaving a relative symlink
// at each old entry so that anything holding the old absolute path outside
// scion's control — git worktree gitdir pointers, container bind-mount
// sources, hub LocalPath fields, a user's shell — keeps resolving. Neither
// legacy root itself is ever removed, since it now holds those symlinks.
//
// Callers are expected to run this once per process, before anything scans
// the canonical directories: the CLI from its Once-guarded boot hook, and
// the hub and runtime broker from their own boot hooks, all before
// discovery or project resolution. A second call in the same process is
// silent, because by then every entry under the legacy roots is either
// already a symlink (migrated, or user-managed) or gone.
//
// Safe to call concurrently, from multiple goroutines in this process and
// from multiple processes on a shared filesystem: entries move with
// syscall.Rename, which is atomic, so exactly one caller wins each entry.
// Every other caller either finds the entry already gone, or — having
// raced past its own checks — finds it already turned into this migration's
// own symlink, and moves on without reporting anything either way. Never
// returns an error that should abort startup; every problem is reported via
// report instead.
func MigrateLegacyGlobalLayout(scionHome string, report Reporter) {
	for _, root := range legacyGlobalRoots {
		migrateLegacyGlobalRoot(scionHome, root, report)
	}
}

// migrateLegacyGlobalLayoutOnce guards MigrateLegacyGlobalLayoutOnce so that
// a single process migrates the global layout at most once, no matter how
// many boot hooks call it. This matters for a combined
// `scion server start --enable-hub --enable-runtime-broker` process: both
// the hub and the runtime broker boot hooks call MigrateLegacyGlobalLayoutOnce,
// and without a shared guard each would run the whole migration
// independently — harmless for a successful migration, which is naturally
// idempotent, but every Conflict or Skipped would then be reported twice.
var migrateLegacyGlobalLayoutOnce sync.Once

// MigrateLegacyGlobalLayoutOnce is the boot-hook entry point for hub server
// boot (cmd/server_foreground.go) and runtime broker boot
// (pkg/runtimebroker/server.go): it behaves like MigrateLegacyGlobalLayout,
// but only the first call in the process has any effect. Mirrors
// WarnRemovedLegacyEnvOnce, which the same two boot hooks already share for
// the same reason.
func MigrateLegacyGlobalLayoutOnce(scionHome string, report Reporter) {
	migrateLegacyGlobalLayoutOnce.Do(func() {
		MigrateLegacyGlobalLayout(scionHome, report)
	})
}

// migrateLegacyGlobalRoot migrates one legacy root (either ~/.scion/groves or
// ~/.scion/grove-configs) into its canonical replacement.
func migrateLegacyGlobalRoot(scionHome string, root legacyGlobalRoot, report Reporter) {
	legacyRoot := filepath.Join(scionHome, root.legacyName)
	canonicalRoot := filepath.Join(scionHome, root.canonicalName)

	info, err := os.Lstat(legacyRoot)
	if err != nil {
		// Covers the common case (ENOENT) and any other Lstat failure: there
		// is nothing this process can usefully migrate either way.
		return
	}

	if info.Mode()&os.ModeSymlink != 0 {
		if legacyRootAlreadyMigrated(legacyRoot, canonicalRoot) {
			// Already migrated: the legacy root is a symlink that resolves
			// to the canonical root itself — exactly what a successful
			// root-symlink migration (or the manual command below) leaves
			// behind. Nothing left to report, the same as a migrated
			// per-entry symlink.
			return
		}
		// User-managed: something else points the legacy name somewhere
		// specific. Leave it alone rather than guessing. The suggested
		// command renames the symlink itself (never its target's content),
		// so it is O(1) regardless of what the link points at, works for an
		// empty or dotfile-only target, and never crosses filesystems. It
		// then leaves a symlink at the old name pointing at the new one, the
		// same way a migrated entry does — recognised as such by the check
		// above on every later run — so old absolute references to
		// legacyRoot keep resolving.
		report.Skipped(legacyRoot,
			"the legacy directory is a symlink",
			fmt.Sprintf("{ [ ! -e %s ] || rmdir %s; } && mv %s %s && ln -s %s %s",
				shellQuote(canonicalRoot), shellQuote(canonicalRoot), shellQuote(legacyRoot), shellQuote(canonicalRoot),
				shellQuote(filepath.Base(canonicalRoot)), shellQuote(legacyRoot)))
		return
	}

	if err := os.MkdirAll(canonicalRoot, info.Mode().Perm()); err != nil {
		report.Skipped(legacyRoot, unwrapErrno(err), manualRenameCommand(legacyRoot, canonicalRoot))
		return
	}

	entries, err := os.ReadDir(legacyRoot)
	if err != nil {
		// canonicalRoot already exists at this point (MkdirAll above
		// succeeded), so a bare "mv legacyRoot canonicalRoot" would nest the
		// legacy root inside it instead of merging entries. Fixing
		// permissions and letting the automated per-entry path run also
		// creates the per-entry symlinks, which a manual move would not.
		report.Skipped(legacyRoot, unwrapErrno(err),
			fmt.Sprintf("chmod u+rwx %s, then re-run scion", shellQuote(legacyRoot)))
		return
	}

	for _, entry := range entries {
		migrateLegacyGlobalEntry(legacyRoot, canonicalRoot, root.canonicalName, entry, report)
	}
}

// migrateLegacyGlobalEntry migrates one entry of a legacy global root.
func migrateLegacyGlobalEntry(legacyRoot, canonicalRoot, canonicalRootName string, entry os.DirEntry, report Reporter) {
	name := entry.Name()
	src := filepath.Join(legacyRoot, name)
	dst := filepath.Join(canonicalRoot, name)
	relLink := filepath.Join("..", canonicalRootName, name)

	if entry.Type()&os.ModeSymlink != 0 {
		removeDanglingOwnGlobalSymlink(src, relLink)
		// Already migrated (the symlink points at the canonical entry), or
		// user-managed (it points somewhere else): either way, leave it.
		return
	}

	srcInfo, err := os.Lstat(src)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// A concurrent process already moved it.
			return
		}
		// A real failure (e.g. EACCES on an unsearchable legacy root):
		// report it. Returning silently here would drop the entry from
		// migration — and, since the pkg/config fallbacks that used to read
		// the legacy directory are gone, from discovery too — with no
		// warning at all.
		report.Skipped(src, unwrapErrno(err), manualDirMoveAndLinkCommand(src, dst, relLink))
		return
	}
	if srcInfo.Mode()&os.ModeSymlink != 0 {
		// A concurrent process finished migrating this entry — renamed it
		// and created the symlink this migration leaves behind — between
		// the caller's ReadDir snapshot (which still saw a plain directory)
		// and this Lstat. Nothing left for this caller to do.
		return
	}
	if !srcInfo.IsDir() {
		// Not a directory. rename(2) would otherwise replace a same-named
		// canonical *file* silently — the ENOTEMPTY/EEXIST no-clobber
		// guard below only ever fires when the source is a directory too —
		// so a stray non-directory entry is left alone rather than risking
		// that clobber. This is not expected in practice: every entry
		// under these roots is normally a project directory.
		report.Skipped(src, "not a directory", manualDirMoveAndLinkCommand(src, dst, relLink))
		return
	}
	if uid, ok := fileOwnerUID(srcInfo); ok && uid != geteuid() {
		report.Skipped(src, "owned by another user", manualDirMoveAndLinkCommand(src, dst, relLink))
		return
	}

	err = renameDir(src, dst)
	switch {
	case err == nil:
		// EEXIST here means a concurrent process already created the
		// symlink; ignore it rather than fail a migration that otherwise
		// fully succeeded.
		if linkErr := os.Symlink(relLink, src); linkErr != nil && !errors.Is(linkErr, fs.ErrExist) {
			report.Skipped(src, unwrapErrno(linkErr), manualSymlinkCommand(src, relLink))
			return
		}
		report.Migrated(src, dst, false)
	case errors.Is(err, fs.ErrNotExist):
		// A concurrent process already migrated it.
	case raceLostAfterConcurrentMigration(src):
		// A concurrent process finished migrating this same entry between
		// our Lstat above and this rename call: src is no longer the plain
		// directory we just confirmed it to be, so the rename failed
		// (typically EISDIR, since dst is now a non-empty directory and
		// src is a symlink). Treat it exactly like the ENOENT case above:
		// nothing left for this caller to do or report. A bare Skipped
		// here would be actively harmful — its manual mv would move this
		// migration's own symlink into the project and break every old
		// absolute path pointing at it.
	case isDirConflictError(err):
		report.Conflict(src, dst, fmt.Sprintf(
			"both %s and %s exist; using %s. Inspect %s and remove it or merge manually.",
			src, dst, dst, src))
	case isCrossDeviceError(err):
		// Never copy: workspaces under these directories can be many GB.
		report.Skipped(src, "on a different filesystem", manualDirMoveAndLinkCommand(src, dst, relLink))
	default:
		report.Skipped(src, unwrapErrno(err), manualDirMoveAndLinkCommand(src, dst, relLink))
	}
}

// raceLostAfterConcurrentMigration reports whether src is now gone or a
// symlink, which can only be true here because a concurrent process finished
// migrating this same entry between the caller's own Lstat (which confirmed
// a plain directory) and its rename call: nothing else ever turns that
// directory into a symlink, or removes it, at the same path. A different
// Lstat error (e.g. EACCES) is a real failure, not a lost race, so it
// returns false and lets the caller's switch fall through to classify the
// original rename error instead of masking it.
func raceLostAfterConcurrentMigration(src string) bool {
	info, err := os.Lstat(src)
	if err != nil {
		return errors.Is(err, fs.ErrNotExist)
	}
	return info.Mode()&os.ModeSymlink != 0
}

// legacyRootAlreadyMigrated reports whether legacyRoot, already known to be a
// symlink, resolves to the same place as canonicalRoot: the state left
// behind by a successful root-symlink migration (this migrator's own
// suggested command, or the equivalent by hand), regardless of whether the
// link was written as a relative or an absolute path. Comparing the fully
// resolved paths (rather than the raw link text) means it also recognises
// the state after further symlinks are layered on top. canonicalRoot not
// existing, or the link resolving anywhere else, means "not recognised as
// already migrated" — report normally rather than guessing.
func legacyRootAlreadyMigrated(legacyRoot, canonicalRoot string) bool {
	resolvedLegacy, err := filepath.EvalSymlinks(legacyRoot)
	if err != nil {
		return false
	}
	resolvedCanonical, err := filepath.EvalSymlinks(canonicalRoot)
	if err != nil {
		return false
	}
	return resolvedLegacy == resolvedCanonical
}

// removeDanglingOwnGlobalSymlink removes the per-entry symlink this migration
// leaves behind, once its target has been deleted: a leftover from an
// earlier migration whose destination was later removed. A symlink whose
// target still exists, or one that points anywhere other than wantTarget, is
// left alone — the latter means something other than this migration manages
// it. Only a target Stat failing with "not exist" proves the link is
// dangling; any other Stat error (e.g. a permission error on the target's
// parent) is not proof of anything and leaves the link untouched.
func removeDanglingOwnGlobalSymlink(src, wantTarget string) {
	if _, err := os.Stat(src); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return // target exists, or the check failed for some other reason.
	}
	target, err := os.Readlink(src)
	if err != nil || target != wantTarget {
		return
	}
	_ = os.Remove(src)
}

// isDirConflictError reports whether err from renameDir indicates the
// destination is a non-empty directory (ENOTEMPTY) or otherwise occupied
// (EEXIST) — i.e. both the legacy and canonical entries exist and need a
// human decision, as opposed to some other failure that should be reported
// as Skipped outright.
func isDirConflictError(err error) bool {
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) {
		return false
	}
	return errors.Is(linkErr.Err, syscall.ENOTEMPTY) || errors.Is(linkErr.Err, syscall.EEXIST)
}

// isCrossDeviceError reports whether err from renameDir indicates the
// legacy and canonical roots live on different filesystems (EXDEV).
func isCrossDeviceError(err error) bool {
	var linkErr *os.LinkError
	if !errors.As(err, &linkErr) {
		return false
	}
	return errors.Is(linkErr.Err, syscall.EXDEV)
}

// manualDirMoveAndLinkCommand returns the copy-pasteable command a Skipped
// report suggests when a human needs to both move a directory by hand and
// leave the same relative symlink an automated run would have created, so a
// later automated run still finds a migrated entry there. Every path operand
// is shell-quoted: entry names come from the filesystem, not from scion, so
// they can contain spaces or other shell metacharacters.
func manualDirMoveAndLinkCommand(src, dst, relLink string) string {
	return fmt.Sprintf("mv %s %s && ln -s %s %s",
		shellQuote(src), shellQuote(dst), shellQuote(relLink), shellQuote(src))
}

// manualSymlinkCommand returns the copy-pasteable command a Skipped report
// suggests when a directory move succeeded but leaving the symlink behind
// failed.
func manualSymlinkCommand(src, relLink string) string {
	return fmt.Sprintf("ln -s %s %s", shellQuote(relLink), shellQuote(src))
}

// shellQuote wraps s in single quotes for safe inclusion in the
// copy-pasteable commands a Skipped report suggests, so a path containing a
// space or another shell metacharacter (e.g. a HOME of "/Users/Jane Doe")
// doesn't silently split into extra operands when a user copy-pastes the
// command. A literal single quote in s is escaped the standard POSIX way:
// close the quoted string, emit an escaped quote, and reopen it.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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
	v, ok := overrides[projectkeys.ConfigProjectIDKey]
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
