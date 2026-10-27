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

//go:build unix

package migrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Tree names a directory (or, for an entry move, a file) for the mover:
// Base is an absolute, operator-controlled directory (a share mount point
// or the global Scion dir) and may sit behind a symlink; Rel is a local
// path below it whose every component is walked with O_NOFOLLOW, so a
// symlink anywhere in Rel is refused rather than followed.
type Tree struct {
	Base string
	Rel  string
}

// String is the host path of t, for reports.
func (t Tree) String() string { return filepath.Join(t.Base, t.Rel) }

func (t Tree) validate() error {
	if !filepath.IsAbs(t.Base) {
		return fmt.Errorf("base %q is not absolute", t.Base)
	}
	if t.Rel == "" || t.Rel == "." || !filepath.IsLocal(t.Rel) {
		return fmt.Errorf("relative path %q is not a local path below the base", t.Rel)
	}
	return nil
}

// Spec is one move of a legacy tree into its target.
type Spec struct {
	Step   Step
	Source Tree
	Target Tree
	// NoRename forces the copy path even when the target is empty or
	// absent, for targets whose directory must be created by its owner
	// (a shared-dir leaf gets its mode and default ACL from EnsureLeaf).
	NoRename bool
	// PrepareTarget, when set, creates the target directory (if absent)
	// before a copy: the authority's leaf creation (projectfs
	// Resolved.Open(create), i.e. shareddirs.EnsureLeaf, mode 2775 and the
	// default ACL). Never called in a dry run, for a rename, or when the
	// source has nothing to move. Without it a target the copy needs is
	// created with the source directory's permission bits.
	PrepareTarget func() error
	// TargetIsLeaf: the target is a Mount leaf (mode 2775 and the default
	// ACL, as shareddirs.EnsureLeaf creates it). A tree renamed into it
	// gets that contract on its top-level directory.
	TargetIsLeaf bool
	// GitLast copies a top-level .git directory last, under
	// GitMigratingName, renaming it to .git only once complete (workspace
	// moves).
	GitLast bool
	// CopyOnly copies the files the target lacks and changes nothing in the
	// source: no rename into the target, no .migrated copy. For a source
	// that is still another component's live tree (a co-located broker's
	// ~/.scion/projects/<slug>). Such a Spec "moves" only when it copies.
	CopyOnly bool
	// Exclude lists source paths (relative to Source, slash-separated)
	// that are not copied (copy path only).
	Exclude []string
	// Normalize says how migrated entries are made usable by agents
	// (owner from Options.Owner; modes by Mount).
	Normalize Normalize
	// Entries, when set, moves only these top-level entries of Source,
	// each by the same rule, into Target (created when absent). The Source
	// directory itself and its other entries stay where they are.
	Entries []string
}

// Options configures a run.
type Options struct {
	// DryRun reports what would move and changes nothing.
	DryRun bool
	// Now stamps the .migrated-<ts> name of a copied source (default
	// time.Now).
	Now func() time.Time
	// Context, when set, bounds the walks: a done context stops a copy or
	// an ownership walk at the next entry (the move then fails and its
	// source stays in place).
	Context context.Context
	// AdoptUnverified, when it returns true for a project slug, migrates
	// that project's slug-keyed legacy trees that carry no identity marker
	// (an operator's decision per project). A tree whose marker names
	// another project is never migrated. Nil adopts none.
	AdoptUnverified func(slug string) bool
	// Owner, when set, is the uid:gid migrated entries are chowned to
	// (Specs with a Normalize mode). Sources and their .migrated copies
	// are never chowned.
	Owner *Ownership
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// Outcome is what happened to one Spec.
type Outcome string

const (
	// OutcomeNothing: the source is absent or empty; nothing to move.
	OutcomeNothing Outcome = "nothing"
	// OutcomeRenamed: the source was renamed into an empty or absent target.
	OutcomeRenamed Outcome = "renamed"
	// OutcomeMerged: files missing from the target were copied in, and the
	// source was renamed to <name>.migrated-<ts>.
	OutcomeMerged Outcome = "merged"
	// OutcomeCopied: a CopyOnly Spec copied what the target lacked; the
	// source is unchanged.
	OutcomeCopied Outcome = "copied"
	// OutcomeWouldRename, OutcomeWouldMerge and OutcomeWouldCopy are the
	// dry-run forms.
	OutcomeWouldRename Outcome = "would-rename"
	OutcomeWouldMerge  Outcome = "would-merge"
	OutcomeWouldCopy   Outcome = "would-copy"
	// OutcomeFailed: the move stopped with Err; the source is left in place.
	OutcomeFailed Outcome = "failed"
	// OutcomeDeferred: the source was left in place on purpose this run
	// (it is in use, for example the home of a running agent) and moves on
	// a later run; it counts as something left to move.
	OutcomeDeferred Outcome = "deferred"
)

// MigratedSuffix separates a copied source's name from its timestamp.
const MigratedSuffix = ".migrated-"

// migratedTimeFormat is the <ts> of <name>.migrated-<ts> (UTC).
const migratedTimeFormat = "20060102T150405Z"

// maxDepth bounds the copy recursion (one open fd per level per tree).
const maxDepth = 256

// Result is the report of one Spec.
type Result struct {
	Step    Step
	Source  string
	Target  string
	Outcome Outcome
	// Copied counts files copied into the target (would be, in a dry run).
	Copied int
	// Identical counts source files already present in the target with
	// the same content (left in the source's .migrated copy).
	Identical int
	// Conflicts are paths, relative to the target, that exist in the
	// target with other content or another type. The target is never
	// overwritten; the source's version stays in its .migrated copy.
	Conflicts []string
	// Refused are source paths that were not copied: symlinks (never
	// followed), special files, and trees deeper than the copy limit.
	Refused []string
	// Backup is where a copied source was renamed to.
	Backup string
	// Entries holds the per-entry results of an Entries move.
	Entries []Result
	// OwnershipErr is the first failure normalising migrated entries
	// (owner, mode, ACL). It does not fail the move.
	OwnershipErr error
	Err          error
}

// Moved reports whether the Spec moved (or, in a dry run, would move)
// anything: a second run over the same trees reports false for every Spec.
func (r Result) Moved() bool {
	switch r.Outcome {
	case OutcomeRenamed, OutcomeMerged, OutcomeWouldRename, OutcomeWouldMerge, OutcomeDeferred:
		return true
	case OutcomeCopied, OutcomeWouldCopy:
		if r.Copied > 0 {
			return true
		}
	}
	for _, e := range r.Entries {
		if e.Moved() {
			return true
		}
	}
	return false
}

// Failed reports whether this Spec, or one of its entries, failed.
func (r Result) Failed() bool {
	if r.Outcome == OutcomeFailed || r.Err != nil {
		return true
	}
	for _, e := range r.Entries {
		if e.Failed() {
			return true
		}
	}
	return false
}

// Move moves spec.Source into spec.Target by the migration rule: rename when the target is absent (an existing empty
// target is copied into instead, because it may be mounted); otherwise
// copy the files the target does not have, never overwriting one, report
// conflicts, and rename the source to <name>.migrated-<ts>. Symlinks in the
// source are never followed and never copied (reported in Refused). A
// symlink at any component of either path refuses the move.
func Move(spec Spec, opts Options) Result {
	res := Result{Step: spec.Step, Source: spec.Source.String(), Target: spec.Target.String()}
	fail := func(err error) Result {
		res.Outcome = OutcomeFailed
		res.Err = err
		return res
	}
	if err := spec.Source.validate(); err != nil {
		return fail(fmt.Errorf("source: %w", err))
	}
	if err := spec.Target.validate(); err != nil {
		return fail(fmt.Errorf("target: %w", err))
	}
	if opts.Context != nil && opts.Context.Err() != nil {
		// Before any write (a rename is one syscall the per-entry check
		// would not see): a lost lock or an expired run writes nothing.
		return fail(fmt.Errorf("not started: %w", opts.Context.Err()))
	}
	if len(spec.Entries) > 0 {
		return moveEntries(spec, opts, res)
	}

	srcParent, srcName, ok, err := openParent(spec.Source, false)
	if err != nil {
		return fail(fmt.Errorf("open source: %w", err))
	}
	if !ok {
		res.Outcome = OutcomeNothing
		return res
	}
	defer closeFd(srcParent)

	var srcSt unix.Stat_t
	if err := unix.Fstatat(srcParent, srcName, &srcSt, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			res.Outcome = OutcomeNothing
			return res
		}
		return fail(fmt.Errorf("stat source: %w", err))
	}
	switch srcSt.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
	case unix.S_IFREG:
		return moveFile(spec, opts, res, srcParent, srcName, &srcSt)
	case unix.S_IFLNK:
		return fail(errors.New("source is a symlink; refusing to follow it"))
	default:
		return fail(errors.New("source is not a directory or regular file"))
	}

	srcFd, err := openDirAt(srcParent, srcName)
	if err != nil {
		return fail(fmt.Errorf("open source: %w", err))
	}
	defer closeFd(srcFd)
	names, err := readDirNames(srcFd)
	if err != nil {
		return fail(fmt.Errorf("read source: %w", err))
	}
	if len(names) == 0 {
		res.Outcome = OutcomeNothing
		return res
	}
	dstParent, dstName, dstParentOK, err := openParent(spec.Target, !opts.DryRun)
	if err != nil {
		return fail(fmt.Errorf("open target parent: %w", err))
	}
	if dstParentOK {
		defer closeFd(dstParent)
	}
	targetKind := kindAbsent
	if dstParentOK {
		if targetKind, err = kindAt(dstParent, dstName); err != nil {
			return fail(fmt.Errorf("stat target: %w", err))
		}
	}

	// Rename only into an ABSENT target. An existing directory, even an
	// empty one, may be the source of a running agent's bind or subPath
	// mount, or held open by an upload, which a rename onto it would leave
	// on the replaced, unlinked inode; it is copied into instead.
	if !spec.NoRename && !spec.CopyOnly && targetKind == kindAbsent && (!dstParentOK || sameDevice(srcFd, dstParent)) {
		if opts.DryRun {
			res.Outcome = OutcomeWouldRename
			return res
		}
		// Atomic no-replace rename (renameat2 RENAME_NOREPLACE): a target
		// created concurrently by a broker, an agent start or an upload is
		// never replaced. Where the filesystem has no atomic no-replace
		// rename (NFS), the tree is copied instead.
		renamed := false
		err := renameDirNoReplace(srcParent, srcName, dstParent, dstName)
		switch {
		case err == nil:
			renamed = true
		case errors.Is(err, errNoAtomicRename), errors.Is(err, unix.EEXIST), errors.Is(err, unix.ENOTEMPTY),
			errors.Is(err, unix.EXDEV), errors.Is(err, unix.EBUSY):
			// copy below
		default:
			return fail(fmt.Errorf("rename source into target: %w", err))
		}
		if renamed {
			res.Outcome = OutcomeRenamed
			n := normalizer{mode: spec.Normalize, owner: opts.Owner, res: &res, ctx: opts.Context}
			if spec.TargetIsLeaf {
				// The renamed directory replaces the leaf the owner would
				// have created: give it the leaf's own contract.
				n.leaf(dstParent, dstName)
			}
			n.tree(dstParent, dstName)
			return res
		}
	}

	if spec.PrepareTarget != nil && !opts.DryRun {
		// The target's owner creates it if absent (mode, default ACL).
		if err := spec.PrepareTarget(); err != nil {
			return fail(fmt.Errorf("prepare target: %w", err))
		}
	}

	// Target state for the copy: absent (dry run only) or a directory.
	dstFd := -1
	if dstParentOK {
		var dstSt unix.Stat_t
		err := unix.Fstatat(dstParent, dstName, &dstSt, unix.AT_SYMLINK_NOFOLLOW)
		switch {
		case errors.Is(err, unix.ENOENT):
		case err != nil:
			return fail(fmt.Errorf("stat target: %w", err))
		case dstSt.Mode&unix.S_IFMT != unix.S_IFDIR:
			return fail(errors.New("target exists and is not a directory (a symlink is never followed)"))
		default:
			if dstFd, err = openDirAt(dstParent, dstName); err != nil {
				return fail(fmt.Errorf("open target: %w", err))
			}
			defer func() { closeFd(dstFd) }()
		}
	}

	n := &normalizer{mode: spec.Normalize, owner: opts.Owner, res: &res, ctx: opts.Context}
	if dstFd < 0 && !opts.DryRun {
		if dstFd, err = createTarget(dstParent, dstName, &srcSt); err != nil {
			return fail(err)
		}
		defer closeFd(dstFd)
		var st unix.Stat_t
		if err := unix.Fstat(dstFd, &st); err == nil {
			n.entry(dstParent, dstName, &st)
		}
	}

	m := merger{dryRun: opts.DryRun, res: &res, norm: n, exclude: excludeSet(spec.Exclude), ctx: opts.Context, gitLast: spec.GitLast}
	m.mergeDir(srcFd, dstFd, "", 0)
	if len(m.errs) > 0 {
		return fail(fmt.Errorf("copy into target: %d error(s), first: %w", len(m.errs), m.errs[0]))
	}
	if spec.CopyOnly {
		res.Outcome = OutcomeCopied
		if opts.DryRun {
			res.Outcome = OutcomeWouldCopy
		}
		return res
	}
	if opts.DryRun {
		res.Outcome = OutcomeWouldMerge
		return res
	}
	// The copies are durable before the source is put aside: the source's
	// .migrated copy must not outlive placements a power loss could drop.
	if err := syncFS(dstFd); err != nil {
		return fail(fmt.Errorf("sync target: %w", err))
	}
	backup, err := renameToBackup(srcParent, srcName, opts)
	if err != nil {
		return fail(err)
	}
	res.Backup = filepath.Join(filepath.Dir(res.Source), backup)
	res.Outcome = OutcomeMerged
	return res
}

// moveEntries moves the listed top-level entries of spec.Source one by one.
func moveEntries(spec Spec, opts Options, res Result) Result {
	res.Outcome = OutcomeNothing
	for _, name := range spec.Entries {
		if name == "" || name == "." || name == ".." || strings.ContainsRune(name, '/') {
			res.Outcome = OutcomeFailed
			res.Err = fmt.Errorf("invalid entry name %q", name)
			return res
		}
		// Every field carries over. PrepareTarget and TargetIsLeaf describe
		// spec.Target itself (the entries' parent), not an entry, so they
		// do not apply to the per-entry moves; Exclude paths under the
		// entry are re-rooted at it.
		sub := Spec{
			Step:      spec.Step,
			Source:    Tree{Base: spec.Source.Base, Rel: filepath.Join(spec.Source.Rel, name)},
			Target:    Tree{Base: spec.Target.Base, Rel: filepath.Join(spec.Target.Rel, name)},
			NoRename:  spec.NoRename,
			CopyOnly:  spec.CopyOnly,
			Exclude:   excludeUnder(spec.Exclude, name),
			Normalize: spec.Normalize,
		}
		r := Move(sub, opts)
		res.Entries = append(res.Entries, r)
		switch {
		case r.Failed():
			res.Outcome = OutcomeFailed
			if res.Err == nil {
				res.Err = fmt.Errorf("entry %q: %w", name, r.Err)
			}
		case r.Moved() && res.Outcome == OutcomeNothing:
			res.Outcome = r.Outcome
		}
	}
	return res
}

// moveFile moves a regular-file source: rename when the target is absent
// and on the same device; otherwise copy when absent, and in every case
// other than the rename keep the source as <name>.migrated-<ts>.
func moveFile(spec Spec, opts Options, res Result, srcParent int, srcName string, srcSt *unix.Stat_t) Result {
	fail := func(err error) Result {
		res.Outcome = OutcomeFailed
		res.Err = err
		return res
	}
	dstParent, dstName, dstParentOK, err := openParent(spec.Target, !opts.DryRun)
	if err != nil {
		return fail(fmt.Errorf("open target parent: %w", err))
	}
	if dstParentOK {
		defer closeFd(dstParent)
	}
	n := normalizer{mode: spec.Normalize, owner: opts.Owner, res: &res, ctx: opts.Context}
	// compareExisting records an existing target as identical or a conflict.
	compareExisting := func() error {
		var dstSt unix.Stat_t
		if err := unix.Fstatat(dstParent, dstName, &dstSt, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("stat target: %w", err)
		}
		if dstSt.Mode&unix.S_IFMT != unix.S_IFREG {
			res.Conflicts = append(res.Conflicts, ".")
			return nil
		}
		same, err := sameContent(srcParent, srcName, dstParent, dstName)
		if err != nil {
			return err
		}
		if same {
			res.Identical++
		} else {
			res.Conflicts = append(res.Conflicts, ".")
		}
		return nil
	}
	exists := false
	if dstParentOK {
		k, err := kindAt(dstParent, dstName)
		if err != nil {
			return fail(fmt.Errorf("stat target: %w", err))
		}
		if k != kindAbsent {
			exists = true
			if err := compareExisting(); err != nil {
				return fail(err)
			}
		}
	}
	if opts.DryRun {
		switch {
		case exists && spec.CopyOnly:
			res.Outcome = OutcomeWouldCopy
		case exists:
			res.Outcome = OutcomeWouldMerge // the source would be kept aside
		case spec.CopyOnly:
			res.Copied++
			res.Outcome = OutcomeWouldCopy
		case !dstParentOK || sameDeviceStat(srcSt, dstParent):
			res.Outcome = OutcomeWouldRename
		default:
			res.Copied++
			res.Outcome = OutcomeWouldMerge
		}
		return res
	}
	if !exists && !spec.CopyOnly {
		// Atomic no-replace placement: a target created concurrently is
		// never overwritten (EEXIST), on NFS too (linkat).
		err := renameFileNoReplace(srcParent, srcName, dstParent, dstName)
		switch {
		case err == nil:
			res.Outcome = OutcomeRenamed
			n.tree(dstParent, dstName)
			return res
		case errors.Is(err, unix.EEXIST):
			exists = true
			if err := compareExisting(); err != nil {
				return fail(err)
			}
		case errors.Is(err, unix.EXDEV):
			// another device: copy below
		default:
			return fail(fmt.Errorf("place source file in target: %w", err))
		}
	}
	if !exists {
		removeCopyTemps(dstParent) // a killed earlier run's leftovers
		conflict, err := copyFile(srcParent, srcName, dstParent, dstName, srcSt, n.entryAt)
		if err != nil {
			return fail(err)
		}
		if conflict {
			res.Conflicts = append(res.Conflicts, ".")
		} else {
			res.Copied++
			n.entryAt(dstParent, dstName)
		}
	}
	if spec.CopyOnly {
		res.Outcome = OutcomeCopied
		return res
	}
	if err := syncFS(dstParent); err != nil {
		return fail(fmt.Errorf("sync target: %w", err))
	}
	backup, err := renameToBackup(srcParent, srcName, opts)
	if err != nil {
		return fail(err)
	}
	res.Backup = filepath.Join(filepath.Dir(res.Source), backup)
	res.Outcome = OutcomeMerged
	return res
}

// createTarget creates the absent target directory before a copy, with
// the source directory's permission bits.
func createTarget(dstParent int, dstName string, srcSt *unix.Stat_t) (int, error) {
	if err := unix.Mkdirat(dstParent, dstName, uint32(srcSt.Mode&0o777)); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, fmt.Errorf("create target: %w", err)
	}
	fd, err := openDirAt(dstParent, dstName)
	if err != nil {
		return -1, fmt.Errorf("open target: %w", err)
	}
	return fd, nil
}

// renameToBackup renames srcName in srcParent to <name>.migrated-<ts>.
func renameToBackup(srcParent int, srcName string, opts Options) (string, error) {
	backup := srcName + MigratedSuffix + opts.now().UTC().Format(migratedTimeFormat)
	var st unix.Stat_t
	if err := unix.Fstatat(srcParent, backup, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return "", fmt.Errorf("rename source to %s: already exists", backup)
	}
	if err := renameBackup(srcParent, srcName, backup); err != nil {
		return "", fmt.Errorf("rename source to %s: %w", backup, err)
	}
	return backup, nil
}

// copyEntryHook runs before each entry of a copy walk with the entry's
// path relative to the source; a test seam.
var copyEntryHook = func(rel string) {}

// GitMigratingName is the temporary name a source .git directory is copied
// under inside the target: it
// is renamed to .git only once complete, so a provisioner sees either no
// .git (and refuses a non-empty workspace) or a complete one, never a
// partial one. An interrupted run resumes into it.
const GitMigratingName = ".git.scion-migrating"

// merger copies a source tree into a target without overwriting.
type merger struct {
	ctx     context.Context
	gitLast bool
	dryRun  bool
	res     *Result
	norm    *normalizer
	exclude map[string]bool
	errs    []error
}

// excludeUnder re-roots the Exclude paths below entry at it.
func excludeUnder(paths []string, entry string) []string {
	var out []string
	for _, p := range paths {
		p = filepath.ToSlash(filepath.Clean(filepath.FromSlash(p)))
		if rest, ok := strings.CutPrefix(p, entry+"/"); ok && rest != "" {
			out = append(out, rest)
		}
	}
	return out
}

func excludeSet(paths []string) map[string]bool {
	set := map[string]bool{}
	for _, p := range paths {
		set[filepath.Clean(filepath.FromSlash(p))] = true
	}
	return set
}

// mergeDir copies src's entries into dst. dst is -1 when the target
// directory does not exist (dry run only): everything below would be copied.
func (m *merger) mergeDir(src, dst int, rel string, depth int) {
	names, err := readDirNames(src)
	if err != nil {
		m.errs = append(m.errs, fmt.Errorf("read %q: %w", relOrDot(rel), err))
		return
	}
	if dst >= 0 && !m.dryRun {
		removeCopyTemps(dst)
	}
	gitLast := false
	for _, name := range names {
		p := filepath.Join(rel, name)
		copyEntryHook(p)
		if m.ctx != nil && m.ctx.Err() != nil {
			m.errs = append(m.errs, fmt.Errorf("stopped at %q: %w", p, m.ctx.Err()))
			return
		}
		if m.exclude[p] {
			continue
		}
		if m.gitLast && rel == "" && name == ".git" {
			if k, err := kindAt(src, name); err == nil && k == kindDir {
				gitLast = true // copied after everything else, under a temp name
				continue
			}
		}
		var st unix.Stat_t
		if err := unix.Fstatat(src, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			m.errs = append(m.errs, fmt.Errorf("stat %q: %w", p, err))
			continue
		}
		switch st.Mode & unix.S_IFMT {
		case unix.S_IFDIR:
			m.mergeSubdir(src, dst, name, p, &st, depth)
		case unix.S_IFREG:
			m.mergeFile(src, dst, name, p, &st)
		case unix.S_IFLNK:
			m.res.Refused = append(m.res.Refused, p+" (symlink)")
		default:
			m.res.Refused = append(m.res.Refused, p+" (special file)")
		}
	}
	if gitLast {
		m.mergeGit(src, dst)
	}
}

// mergeGit copies the source's top-level .git directory last: into
// GitMigratingName inside the target, then, once that copy is complete and
// workspace/.git is still absent, renames it to .git (the caller holds the
// project's provisioning locks, so no provisioner clones concurrently). A
// target .git that already exists (a broker cloned first) is a conflict:
// the temp dir is not created or renamed.
func (m *merger) mergeGit(src, dst int) {
	childSrc, err := openDirAt(src, ".git")
	if err != nil {
		m.errs = append(m.errs, fmt.Errorf("open %q: %w", ".git", err))
		return
	}
	defer closeFd(childSrc)
	if dst < 0 {
		m.mergeDir(childSrc, -1, ".git", 1)
		return
	}
	k, err := kindAt(dst, ".git")
	if err != nil {
		m.errs = append(m.errs, fmt.Errorf("stat target .git: %w", err))
		return
	}
	if k != kindAbsent {
		m.res.Conflicts = append(m.res.Conflicts, ".git")
		return
	}
	tk, err := kindAt(dst, GitMigratingName)
	if err != nil {
		m.errs = append(m.errs, fmt.Errorf("stat %s: %w", GitMigratingName, err))
		return
	}
	switch tk {
	case kindAbsent:
		if m.dryRun {
			m.mergeDir(childSrc, -1, ".git", 1)
			return
		}
		var st unix.Stat_t
		if err := unix.Fstat(childSrc, &st); err != nil {
			m.errs = append(m.errs, err)
			return
		}
		// Exclusive: a temp dir that appears between the check and here is
		// not ours to merge into (resuming happens by name, below).
		if err := unix.Mkdirat(dst, GitMigratingName, uint32(st.Mode&0o777)); err != nil {
			m.errs = append(m.errs, fmt.Errorf("create %s: %w", GitMigratingName, err))
			return
		}
		m.norm.entryAt(dst, GitMigratingName)
	case kindOther:
		m.res.Conflicts = append(m.res.Conflicts, GitMigratingName)
		return
	}
	tmp, err := openDirAt(dst, GitMigratingName)
	if err != nil {
		m.errs = append(m.errs, fmt.Errorf("open %s: %w", GitMigratingName, err))
		return
	}
	defer closeFd(tmp)
	before, beforeConflicts := len(m.errs), len(m.res.Conflicts)
	m.mergeDir(childSrc, tmp, ".git", 1)
	if len(m.errs) > before || m.dryRun || (m.ctx != nil && m.ctx.Err() != nil) {
		return // incomplete: a re-run resumes into the temp dir
	}
	if n := len(m.res.Conflicts) - beforeConflicts; n > 0 {
		// The temp copy differs from the source: never rename an
		// inconsistent .git into place.
		m.errs = append(m.errs, fmt.Errorf(".git copy incomplete: %d entries in %s differ from the source; not renamed into place (inspect or remove %s, then run again)", n, GitMigratingName, GitMigratingName))
		return
	}
	if k, err := kindAt(dst, ".git"); err != nil || k != kindAbsent {
		m.res.Conflicts = append(m.res.Conflicts, ".git")
		return
	}
	err = renameNoReplace(dst, GitMigratingName, dst, ".git")
	if errors.Is(err, errNoAtomicRename) {
		// NFS has no RENAME_NOREPLACE. A plain rename(2) is atomic; it
		// fails on a non-empty .git (ENOTEMPTY/EEXIST: a conflict below)
		// and replaces only an EMPTY .git directory that a writer taking no
		// lock created after the absent check above. Nothing is lost then:
		// the result is the complete repository. Provisioners are excluded
		// by the locks the caller holds.
		err = unix.Renameat(dst, GitMigratingName, dst, ".git")
	}
	if err != nil {
		if errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOTEMPTY) {
			m.res.Conflicts = append(m.res.Conflicts, ".git")
			return
		}
		m.errs = append(m.errs, fmt.Errorf("rename %s to .git: %w", GitMigratingName, err))
	}
}

func (m *merger) mergeSubdir(src, dst int, name, p string, st *unix.Stat_t, depth int) {
	if depth+1 >= maxDepth {
		m.res.Refused = append(m.res.Refused, p+" (too deep)")
		return
	}
	childSrc, err := openDirAt(src, name)
	if err != nil {
		m.errs = append(m.errs, fmt.Errorf("open %q: %w", p, err))
		return
	}
	defer closeFd(childSrc)
	if dst < 0 {
		m.mergeDir(childSrc, -1, p, depth+1)
		return
	}
	var dstSt unix.Stat_t
	err = unix.Fstatat(dst, name, &dstSt, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case errors.Is(err, unix.ENOENT):
		if m.dryRun {
			m.mergeDir(childSrc, -1, p, depth+1)
			return
		}
		err := unix.Mkdirat(dst, name, uint32(st.Mode&0o777))
		if err != nil && !errors.Is(err, unix.EEXIST) {
			m.errs = append(m.errs, fmt.Errorf("create %q: %w", p, err))
			return
		}
		if err == nil {
			var newSt unix.Stat_t
			if err := unix.Fstatat(dst, name, &newSt, unix.AT_SYMLINK_NOFOLLOW); err == nil {
				m.norm.entry(dst, name, &newSt)
			}
		}
	case err != nil:
		m.errs = append(m.errs, fmt.Errorf("stat target %q: %w", p, err))
		return
	case dstSt.Mode&unix.S_IFMT != unix.S_IFDIR:
		m.res.Conflicts = append(m.res.Conflicts, p)
		return
	}
	childDst, err := openDirAt(dst, name)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			m.res.Conflicts = append(m.res.Conflicts, p)
			return
		}
		m.errs = append(m.errs, fmt.Errorf("open target %q: %w", p, err))
		return
	}
	defer closeFd(childDst)
	m.mergeDir(childSrc, childDst, p, depth+1)
}

func (m *merger) mergeFile(src, dst int, name, p string, st *unix.Stat_t) {
	if dst < 0 {
		m.res.Copied++
		return
	}
	var dstSt unix.Stat_t
	err := unix.Fstatat(dst, name, &dstSt, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case errors.Is(err, unix.ENOENT):
		if m.dryRun {
			m.res.Copied++
			return
		}
		conflict, err := copyFile(src, name, dst, name, st, m.norm.entryAt)
		switch {
		case err != nil:
			m.errs = append(m.errs, fmt.Errorf("copy %q: %w", p, err))
		case conflict:
			m.res.Conflicts = append(m.res.Conflicts, p)
		default:
			m.res.Copied++
			m.norm.entryAt(dst, name) // the new file's own stat (never hard-linked)
		}
	case err != nil:
		m.errs = append(m.errs, fmt.Errorf("stat target %q: %w", p, err))
	case dstSt.Mode&unix.S_IFMT == unix.S_IFREG:
		same, err := sameContent(src, name, dst, name)
		switch {
		case err != nil:
			m.errs = append(m.errs, fmt.Errorf("compare %q: %w", p, err))
		case same:
			m.res.Identical++
		default:
			m.res.Conflicts = append(m.res.Conflicts, p)
		}
	default:
		m.res.Conflicts = append(m.res.Conflicts, p)
	}
}

// CopyTempPrefix starts the name of a file being copied: copyFile writes, fsyncs and only then places the file under
// its final name, so a killed run never leaves a partial file under a
// final name. Leftover temp files are removed by the next run's copy walk
// and are never workspace content.
const CopyTempPrefix = ".scion-migrate-tmp-"

// copyCrashHook runs after the temp file is written, before it is placed;
// a test seam that can simulate a killed run (errSimulatedKill: return at
// once, leaving the temp file, as a killed process would).
var copyCrashHook = func(dstName string) error { return nil }

var errSimulatedKill = errors.New("simulated kill")

// copyFile copies the regular file srcName to a NEW file dstName. It writes
// a temp sibling (CopyTempPrefix, O_EXCL, no-follow), fsyncs it, and places
// it with no-replace semantics (renameat2(RENAME_NOREPLACE), or linkat +
// unlink where that is unavailable): an existing target, even one created
// concurrently, is never touched and is reported as a conflict, and the
// final name never holds a partial file. Permission bits (no
// setuid/setgid/sticky) and the modification time are kept.
func copyFile(srcDir int, srcName string, dstDir int, dstName string, st *unix.Stat_t, pre func(dir int, name string)) (conflict bool, err error) {
	sfd, err := unix.Openat(srcDir, srcName, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return false, fmt.Errorf("open source file: %w", err)
	}
	sf := os.NewFile(uintptr(sfd), srcName)
	defer func() { _ = sf.Close() }()
	var fst unix.Stat_t
	if err := unix.Fstat(sfd, &fst); err != nil {
		return false, fmt.Errorf("stat source file: %w", err)
	}
	if fst.Mode&unix.S_IFMT != unix.S_IFREG || fst.Ino != st.Ino || fst.Dev != st.Dev {
		return false, errors.New("source file changed while copying")
	}
	perm := uint32(fst.Mode & 0o777)
	var tmpName string
	var dfd int
	for attempt := 0; ; attempt++ {
		var rnd [8]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return false, err
		}
		tmpName = CopyTempPrefix + hex.EncodeToString(rnd[:])
		dfd, err = unix.Openat(dstDir, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, perm)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EEXIST) || attempt > 3 {
			return false, fmt.Errorf("create temp file: %w", err)
		}
	}
	df := os.NewFile(uintptr(dfd), tmpName)
	_, cerr := io.Copy(df, sf)
	if cerr == nil {
		cerr = unix.Fchmod(dfd, perm)
	}
	if cerr == nil {
		cerr = df.Sync()
	}
	if closeErr := df.Close(); cerr == nil {
		cerr = closeErr
	}
	if cerr == nil {
		ts := []unix.Timespec{fst.Atim, fst.Mtim}
		cerr = unix.UtimesNanoAt(dstDir, tmpName, ts, unix.AT_SYMLINK_NOFOLLOW)
	}
	if cerr == nil && pre != nil {
		// Owner and mode are applied to the temp file before it is placed,
		// so a kill right after placement cannot leave a final-named file
		// with the migrator's owner that later runs see as identical.
		pre(dstDir, tmpName)
	}
	if cerr == nil {
		if herr := copyCrashHook(dstName); errors.Is(herr, errSimulatedKill) {
			return false, herr // as if killed: the temp file stays
		}
	}
	if cerr != nil {
		_ = unix.Unlinkat(dstDir, tmpName, 0)
		return false, fmt.Errorf("copy file: %w", cerr)
	}
	if err := renameFileNoReplace(dstDir, tmpName, dstDir, dstName); err != nil {
		_ = unix.Unlinkat(dstDir, tmpName, 0)
		if errors.Is(err, unix.EEXIST) {
			return true, nil
		}
		return false, fmt.Errorf("place copied file: %w", err)
	}
	return false, nil
}

// removeCopyTemps removes leftover copy temp files (CopyTempPrefix,
// regular files owned by this process's effective uid) from a target
// directory: a killed earlier run's partial copies. Nothing else is ever
// removed.
func removeCopyTemps(dir int) {
	names, err := readDirNames(dir)
	if err != nil {
		return
	}
	for _, n := range names {
		if !strings.HasPrefix(n, CopyTempPrefix) {
			continue
		}
		var st unix.Stat_t
		// Only regular files this process's identity created (temp files
		// are chowned only as they are placed).
		if err := unix.Fstatat(dir, n, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil && st.Mode&unix.S_IFMT == unix.S_IFREG && int(st.Uid) == unix.Geteuid() {
			_ = unix.Unlinkat(dir, n, 0)
		}
	}
}

// sameContent reports whether two regular files have the same bytes.
func sameContent(aDir int, aName string, bDir int, bName string) (bool, error) {
	open := func(dir int, name string) (*os.File, int64, error) {
		fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, 0, err
		}
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
			_ = unix.Close(fd)
			if err == nil {
				err = errors.New("not a regular file")
			}
			return nil, 0, err
		}
		return os.NewFile(uintptr(fd), name), st.Size, nil
	}
	a, aSize, err := open(aDir, aName)
	if err != nil {
		return false, fmt.Errorf("open for compare: %w", err)
	}
	defer func() { _ = a.Close() }()
	b, bSize, err := open(bDir, bName)
	if err != nil {
		return false, fmt.Errorf("open for compare: %w", err)
	}
	defer func() { _ = b.Close() }()
	if aSize != bSize {
		return false, nil
	}
	bufA, bufB := make([]byte, 64<<10), make([]byte, 64<<10)
	for {
		na, errA := io.ReadFull(a, bufA)
		nb, errB := io.ReadFull(b, bufB)
		if na != nb || !bytes.Equal(bufA[:na], bufB[:nb]) {
			return false, nil
		}
		endA := errors.Is(errA, io.EOF) || errors.Is(errA, io.ErrUnexpectedEOF)
		endB := errors.Is(errB, io.EOF) || errors.Is(errB, io.ErrUnexpectedEOF)
		if errA != nil && !endA {
			return false, errA
		}
		if errB != nil && !endB {
			return false, errB
		}
		if endA || endB {
			return endA && endB, nil
		}
	}
}

// intermediateMode is the mode of a directory openParent creates.
const intermediateMode = 0o2755

// openParent opens the parent directory of t.Rel below t.Base, walking
// every component with O_NOFOLLOW. With create, missing components are
// created at intermediateMode. ok is false when a component is missing and create is
// off.
func openParent(t Tree, create bool) (fd int, name string, ok bool, err error) {
	baseFd, err := unix.Open(t.Base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) && !create {
			return -1, "", false, nil
		}
		return -1, "", false, fmt.Errorf("open base %q: %w", t.Base, err)
	}
	cur := baseFd
	parent, name := filepath.Split(filepath.Clean(t.Rel))
	for _, comp := range strings.Split(filepath.ToSlash(filepath.Clean(parent)), "/") {
		if comp == "" || comp == "." {
			continue
		}
		next, err := openDirAt(cur, comp)
		if errors.Is(err, unix.ENOENT) && create {
			mkErr := unix.Mkdirat(cur, comp, 0o755)
			if mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
				closeFd(cur)
				return -1, "", false, fmt.Errorf("create %q: %w", comp, mkErr)
			}
			next, err = openDirAt(cur, comp)
			if err == nil && mkErr == nil {
				// Same intermediate mode as shareddirs.EnsureLeaf: setgid,
				// no group write.
				if chErr := unix.Fchmod(next, intermediateMode); chErr != nil {
					closeFd(next)
					closeFd(cur)
					return -1, "", false, fmt.Errorf("chmod %q: %w", comp, chErr)
				}
			}
		}
		closeFd(cur)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				return -1, "", false, nil
			}
			return -1, "", false, fmt.Errorf("open %q: %w", comp, err)
		}
		cur = next
	}
	return cur, name, true, nil
}

// kindAt reports what name in dir is, without following a symlink.
func kindAt(dir int, name string) (kind, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return kindAbsent, nil
		}
		return kindAbsent, err
	}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		return kindDir, nil
	}
	return kindOther, nil
}

// openDirAt opens the directory name under dir without following a
// symlink at name (ELOOP or ENOTDIR for a symlink or non-directory).
func openDirAt(dir int, name string) (int, error) {
	return unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// readDirNames lists a directory fd's entries (sorted, without . and ..).
func readDirNames(dirFd int) ([]string, error) {
	dup, err := unix.Dup(dirFd)
	if err != nil {
		return nil, err
	}
	// A dup shares the directory offset: rewind, so a directory can be
	// listed more than once through the same fd.
	if _, err := unix.Seek(dup, 0, 0); err != nil {
		_ = unix.Close(dup)
		return nil, err
	}
	f := os.NewFile(uintptr(dup), "dir")
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

func sameDevice(aFd, bFd int) bool {
	var a unix.Stat_t
	if err := unix.Fstat(aFd, &a); err != nil {
		return false
	}
	return sameDeviceStat(&a, bFd)
}

func sameDeviceStat(a *unix.Stat_t, bFd int) bool {
	var b unix.Stat_t
	if err := unix.Fstat(bFd, &b); err != nil {
		return false
	}
	return a.Dev == b.Dev
}

func closeFd(fd int) {
	if fd >= 0 {
		_ = unix.Close(fd)
	}
}

func relOrDot(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}
