//go:build linux

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

package homeprep

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// maxSentinelBytes bounds the sentinel and link record reads.
const maxSentinelBytes = 1 << 20

// Prepare runs the per-start preparation of an NFS home and returns the
// chosen mode. On failure the error is a *ClassError naming the class.
func Prepare(opts PrepareOptions) (string, error) {
	if opts.AgentID == "" || opts.StartID == "" {
		return "", classErr(ErrClassPrepare, "agent ID and start ID are required")
	}
	r, err := openRoot(opts.Home)
	if err != nil {
		return "", classErr(ErrClassUnavailable, "cannot open the agent home: %v", err)
	}
	defer r.close()

	if err := writeProbe(r, opts.StartID); err != nil {
		return "", err
	}
	read := readSentinel(r)
	names, err := r.names()
	if err != nil {
		return "", classErr(ErrClassPrepare, "cannot list the agent home: %v", err)
	}
	uid := os.Getuid()
	dec, err := Decide(read, opts.AgentID, uid, names)
	if err != nil {
		return "", err
	}

	var sentinel Sentinel
	switch dec.Mode {
	case ModeSeed:
		for _, n := range dec.RemoveReserved {
			if err := removeEntry(r.fd, n); err != nil {
				return "", classErr(ErrClassSeed, "cannot remove %s: %v", n, err)
			}
		}
		if dec.CleanInterrupted {
			opts.logf("Previous seed of the agent home did not finish; clearing it")
			if err := removeContents(r.fd, func(n string) bool { return n == SentinelName }); err != nil {
				return "", classErr(ErrClassSeed, "cannot clear an interrupted seed: %v", err)
			}
		}
		sentinel = Sentinel{
			Version: 1, AgentID: opts.AgentID, State: StateSeeding,
			StartID: opts.StartID, LaunchID: opts.LaunchID,
			UID: uid, GID: os.Getgid(), Skeleton: SkeletonNone,
		}
		if err := writeSentinel(r, &sentinel); err != nil {
			return "", classErr(ErrClassSeed, "cannot write the home sentinel: %v", err)
		}
		if opts.SkeletonSource != "" {
			res, err := copySkeleton(r, opts.SkeletonSource, opts.SkeletonMaxBytes, opts.logf)
			if err != nil {
				return "", classErr(ErrClassSeed, "cannot copy the image home: %v", err)
			}
			sentinel.Skeleton = res
		}
	case ModeSeedOver:
		sentinel = *read.Sentinel
		if err := clearHooks(r, opts.logf); err != nil {
			return "", classErr(ErrClassPrepare, "cannot clear %s: %v", hooksDir, err)
		}
	}

	results, err := placeLinks(r, opts.Links, opts.StartID, opts.logf)
	if err != nil {
		return "", err
	}

	sentinel.StartID = opts.StartID
	sentinel.LaunchID = opts.LaunchID
	if err := writeSentinel(r, &sentinel); err != nil {
		return "", classErr(ErrClassPrepare, "cannot stamp the home sentinel: %v", err)
	}

	if err := writeMemJSON(opts.MemDir, LinksResultFileName, LinksResult{Links: results}); err != nil {
		return "", classErr(ErrClassPrepare, "cannot write the link result: %v", err)
	}
	if err := writeMemJSON(opts.MemDir, ModeFileName, ModeFile{Mode: dec.Mode, StartID: opts.StartID}); err != nil {
		return "", classErr(ErrClassPrepare, "cannot write the mode file: %v", err)
	}
	return dec.Mode, nil
}

// MarkSeeded marks the home seeded after the broker's seed transfer. The
// sentinel must belong to agentID and carry startID; after writing, it is
// read again and must still carry startID, so a start that is no longer the
// latest one fails.
func MarkSeeded(home, agentID, startID string) error {
	if agentID == "" || startID == "" {
		return classErr(ErrClassPrepare, "agent ID and start ID are required")
	}
	r, err := openRoot(home)
	if err != nil {
		return classErr(ErrClassUnavailable, "cannot open the agent home: %v", err)
	}
	defer r.close()
	read := readSentinel(r)
	if read.NotExist {
		return classErr(ErrClassPrepare, "the home has no sentinel")
	}
	if read.Err != nil {
		return classErr(ErrClassPrepare, "cannot read the home sentinel: %v", read.Err)
	}
	s := read.Sentinel
	switch {
	case s.AgentID != agentID:
		return classErr(ErrClassPrepare, "the home belongs to agent %q, not %q", s.AgentID, agentID)
	case s.UID != os.Getuid():
		return classErr(ErrClassPrepare, "the home was seeded by uid %d, not %d", s.UID, os.Getuid())
	case s.StartID != startID:
		return classErr(ErrClassPrepare, "the home sentinel names start %q, not this start %q", s.StartID, startID)
	}
	if s.State != StateSeeded {
		s.State = StateSeeded
		s.SeededAt = time.Now().UTC().Format(time.RFC3339)
		s.SeededBy = startID
		if err := writeSentinel(r, s); err != nil {
			return classErr(ErrClassPrepare, "cannot write the home sentinel: %v", err)
		}
	}
	again := readSentinel(r)
	if again.Err != nil || again.Sentinel == nil {
		return classErr(ErrClassPrepare, "cannot read the home sentinel back: %v", again.Err)
	}
	if again.Sentinel.StartID != startID || again.Sentinel.State != StateSeeded {
		return classErr(ErrClassPrepare, "the home sentinel changed to start %q while marking it seeded", again.Sentinel.StartID)
	}
	return nil
}

func randSuffix() string {
	b := make([]byte, 6)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// writeProbe creates and removes a reserved probe file in the home, so an
// export that does not let this uid write fails here with a clear message.
func writeProbe(r *root, startID string) error {
	name := probePrefix + safeName(startID) + "." + randSuffix()
	fd, err := unix.Openat(r.fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		switch {
		case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
			return classErr(ErrClassUnavailable, "the agent home is not writable by uid %d; the export must allow uid/gid %d/%d (see docs)", os.Getuid(), os.Getuid(), os.Getgid())
		case errors.Is(err, unix.EROFS), errors.Is(err, unix.EDQUOT), errors.Is(err, unix.ENOSPC):
			return classErr(ErrClassUnavailable, "the agent home is not writable: %v", err)
		}
		return classErr(ErrClassUnavailable, "cannot write in the agent home: %v", err)
	}
	_ = unix.Close(fd)
	if err := unix.Unlinkat(r.fd, name, 0); err != nil {
		return classErr(ErrClassUnavailable, "cannot remove the write probe: %v", err)
	}
	return nil
}

// safeName keeps an ID usable in a file name.
func safeName(s string) string {
	return strings.Map(func(c rune) rune {
		if c == '-' || c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			return c
		}
		return '_'
	}, s)
}

// readSentinel reads the sentinel without following links. ESTALE is
// retried once.
func readSentinel(r *root) SentinelRead {
	data, err := r.readFile(SentinelName, maxSentinelBytes)
	if errors.Is(err, unix.ESTALE) {
		data, err = r.readFile(SentinelName, maxSentinelBytes)
	}
	if errors.Is(err, unix.ENOENT) {
		return SentinelRead{NotExist: true}
	}
	if err != nil {
		return SentinelRead{Err: err}
	}
	s, err := parseSentinel(data)
	if err != nil {
		return SentinelRead{Err: err}
	}
	return SentinelRead{Sentinel: s}
}

func writeSentinel(r *root, s *Sentinel) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := sentinelTmp + safeName(s.StartID) + "." + randSuffix()
	err = r.writeFileAtomic(SentinelName, tmp, append(data, '\n'), 0o644)
	if errors.Is(err, unix.ENOENT) {
		// The rename source vanished: read back and compare.
		back := readSentinel(r)
		if back.Sentinel != nil && back.Sentinel.StartID == s.StartID && back.Sentinel.State == s.State {
			return nil
		}
	}
	return err
}

// writeMemJSON writes v to name in the memory directory.
func writeMemJSON(memDir, name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := filepath.Join(memDir, "."+name+"."+randSuffix())
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(memDir, name))
}

// removeEntry removes one entry of dirFd, emptying it first when it is a
// directory. ENOENT is not an error.
func removeEntry(dirFd int, name string) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(dirFd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
		return nil
	}
	child, err := unix.Openat(dirFd, name, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	// Make the directory writable before emptying it, so read-only trees
	// can be removed.
	if err := unix.Fchmod(child, (st.Mode&0o7777)|0o700); err != nil {
		_ = unix.Close(child)
		return err
	}
	err = removeContents(child, nil)
	_ = unix.Close(child)
	if err != nil {
		return err
	}
	if err := unix.Unlinkat(dirFd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// maxRemoveDepth bounds the recursion of removeContents.
var maxRemoveDepth = 256

// removeContents removes every entry of the directory dirFd except those
// keep accepts, without following symbolic links. It does not close dirFd.
func removeContents(dirFd int, keep func(string) bool) error {
	return removeContentsDepth(dirFd, keep, 0)
}

func removeContentsDepth(dirFd int, keep func(string) bool, depth int) error {
	if depth > maxRemoveDepth {
		return fmt.Errorf("directory tree deeper than %d levels", maxRemoveDepth)
	}
	dup, err := unix.Openat(dirFd, ".", unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(dup), ".")
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return err
	}
	for _, name := range names {
		if keep != nil && keep(name) {
			continue
		}
		var st unix.Stat_t
		if err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("%s: %w", name, err)
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			if err := unix.Unlinkat(dirFd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return fmt.Errorf("%s: %w", name, err)
			}
			continue
		}
		child, err := unix.Openat(dirFd, name, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return fmt.Errorf("%s: %w", name, err)
		}
		if err := unix.Fchmod(child, (st.Mode&0o7777)|0o700); err != nil {
			_ = unix.Close(child)
			return fmt.Errorf("%s: %w", name, err)
		}
		err = removeContentsDepth(child, nil, depth+1)
		_ = unix.Close(child)
		if err != nil {
			return fmt.Errorf("%s/%w", name, err)
		}
		if err := unix.Unlinkat(dirFd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

// clearHooks removes the contents of .scion/hooks before a seed-over
// transfer. A missing directory is fine; a symbolic link or non-directory
// in its place is left alone with a warning.
func clearHooks(r *root, logf func(string, ...any)) error {
	fd, err := r.open(hooksDir, unix.O_DIRECTORY|unix.O_RDONLY, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.EXDEV), errors.Is(err, unix.ENOTDIR):
		logf("Warning: %s is not a plain directory in the agent home; not clearing it", hooksDir)
		return nil
	case err != nil:
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return removeContents(fd, nil)
}

// copySkeleton copies the image's home tree into a new home when the
// regular files in it total at most max bytes: directories and regular
// files with their permission bits, and symbolic links as links. Other
// files and entries this user cannot read are skipped and logged. Over the
// cap nothing is copied.
func copySkeleton(r *root, src string, max int64, logf func(string, ...any)) (string, error) {
	srcInfo, err := os.Lstat(src)
	if errors.Is(err, os.ErrNotExist) {
		return SkeletonNone, nil
	}
	if err != nil {
		return "", err
	}
	if !srcInfo.IsDir() {
		return SkeletonNone, nil
	}
	var rootSt unix.Stat_t
	if err := unix.Fstat(r.fd, &rootSt); err != nil {
		return "", err
	}
	if st, ok := srcInfo.Sys().(*unix.Stat_t); ok && st.Dev == rootSt.Dev && st.Ino == rootSt.Ino {
		return "", fmt.Errorf("the image home %s is the agent home itself", src)
	}

	var total int64
	err = filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			logf("Warning: skipping %s in the image home: %v", p, err)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type().IsRegular() {
			if info, ierr := d.Info(); ierr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if total > max {
		logf("Warning: the image home is %d bytes, over the %d byte limit; it is not copied into the new home", total, max)
		return SkeletonSkipped, nil
	}

	err = filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(strings.SplitN(rel, "/", 2)[0], ReservedPrefix) {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			logf("Warning: skipping %s in the image home: %v", rel, ierr)
			return nil
		}
		switch {
		case d.IsDir():
			if err := mkdirBeneath(r, rel, uint32(info.Mode().Perm())); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
		case d.Type()&os.ModeSymlink != 0:
			target, lerr := os.Readlink(p)
			if lerr != nil {
				logf("Warning: skipping %s in the image home: %v", rel, lerr)
				return nil
			}
			dir, leaf, perr := r.parent(rel)
			if perr != nil {
				return fmt.Errorf("%s: %w", rel, perr)
			}
			serr := unix.Symlinkat(target, dir, leaf)
			closeUnlessRoot(r, dir)
			if serr != nil {
				return fmt.Errorf("%s: %w", rel, serr)
			}
		case d.Type().IsRegular():
			if err := copyFileBeneath(r, p, rel, uint32(info.Mode().Perm())); err != nil {
				if errors.Is(err, os.ErrPermission) {
					logf("Warning: skipping %s in the image home: %v", rel, err)
					return nil
				}
				return fmt.Errorf("%s: %w", rel, err)
			}
		default:
			logf("Warning: skipping special file %s in the image home", rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return SkeletonCopied, nil
}

func mkdirBeneath(r *root, rel string, mode uint32) error {
	dir, leaf, err := r.parent(rel)
	if err != nil {
		return err
	}
	defer closeUnlessRoot(r, dir)
	if err := unix.Mkdirat(dir, leaf, mode|0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return err
	}
	fd, err := unix.Openat(dir, leaf, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return unix.Fchmod(fd, mode|0o700)
}

func copyFileBeneath(r *root, src, rel string, mode uint32) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	fd, err := r.open(rel, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, mode|0o600)
	if err != nil {
		return err
	}
	out := os.NewFile(uintptr(fd), rel)
	_, cerr := io.Copy(out, in)
	if cerr == nil {
		cerr = unix.Fchmod(fd, mode)
	}
	if err := out.Close(); cerr == nil {
		cerr = err
	}
	return cerr
}

// placeLinks places the requested links and returns one result per link.
//
// At each target:
//   - absent: the link is created (missing parent directories are created
//     as this user);
//   - a symbolic link the previous start recorded: it is replaced;
//   - a regular file where the previous start recorded a link (a tool
//     replaced the link): the file is removed and the link placed again;
//   - anything else (a user's file or link): it is kept and the target is
//     reported as skipped, with a warning;
//   - a directory, or a parent that is not a directory or is a symbolic
//     link: preparation fails, naming the target.
//
// Links the previous start recorded that are no longer requested are
// removed if they are still symbolic links. The new set is recorded in
// LinksRecordPath.
func placeLinks(r *root, links []Link, startID string, logf func(string, ...any)) ([]LinkResult, error) {
	prev := map[string]bool{}
	if data, err := r.readFile(LinksRecordPath, maxSentinelBytes); err == nil {
		var rec linksRecord
		if json.Unmarshal(data, &rec) == nil {
			for _, t := range rec.Links {
				prev[t] = true
			}
		} else {
			logf("Warning: ignoring an unreadable %s", LinksRecordPath)
		}
	} else if !errors.Is(err, unix.ENOENT) {
		logf("Warning: ignoring %s: %v", LinksRecordPath, err)
	}

	results := make([]LinkResult, 0, len(links))
	var placed []string
	requested := map[string]bool{}
	for _, l := range links {
		requested[l.Target] = true
		state, err := placeLink(r, l, prev[l.Target], startID, logf)
		if err != nil {
			return nil, err
		}
		results = append(results, LinkResult{Target: l.Target, State: state})
		if state == LinkLinked {
			placed = append(placed, l.Target)
		}
	}

	for t := range prev {
		if requested[t] {
			continue
		}
		st, err := r.lstat(t)
		if err != nil || st.Mode&unix.S_IFMT != unix.S_IFLNK {
			continue
		}
		dir, leaf, err := r.parent(t)
		if err != nil {
			continue
		}
		if err := unix.Unlinkat(dir, leaf, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			logf("Warning: cannot remove the old link %s: %v", t, err)
		}
		closeUnlessRoot(r, dir)
	}

	slices.Sort(placed)
	data, err := json.Marshal(linksRecord{Links: placed})
	if err != nil {
		return nil, err
	}
	if err := r.mkdirAll(".scion", 0o755); err != nil {
		return nil, classErr(ErrClassPrepare, "cannot create .scion in the agent home: %v", err)
	}
	if err := r.writeFileAtomic(LinksRecordPath, ".home-links."+randSuffix(), append(data, '\n'), 0o644); err != nil {
		return nil, classErr(ErrClassPrepare, "cannot record the placed links: %v", err)
	}
	return results, nil
}

func placeLink(r *root, l Link, wasLink bool, startID string, logf func(string, ...any)) (string, error) {
	if parent := filepath.ToSlash(filepath.Dir(l.Target)); parent != "." {
		if err := r.mkdirAll(parent, 0o755); err != nil {
			return "", classErr(ErrClassPrepare, "cannot place the link %s: its parent directory is not a plain directory in the agent home (%v)", l.Target, err)
		}
	}
	dir, leaf, err := r.parent(l.Target)
	if err != nil {
		return "", classErr(ErrClassPrepare, "cannot place the link %s: %v", l.Target, err)
	}
	defer closeUnlessRoot(r, dir)

	var st unix.Stat_t
	err = unix.Fstatat(dir, leaf, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case errors.Is(err, unix.ENOENT):
		if err := unix.Symlinkat(l.Source, dir, leaf); err != nil {
			return "", classErr(ErrClassPrepare, "cannot place the link %s: %v", l.Target, err)
		}
		return LinkLinked, nil
	case err != nil:
		return "", classErr(ErrClassPrepare, "cannot check %s: %v", l.Target, err)
	}

	switch st.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		if !wasLink {
			logf("Warning: %s is a symbolic link that scion did not place; keeping it", l.Target)
			return LinkSkipped, nil
		}
		tmp := ".scion-home-link." + safeName(startID) + "." + randSuffix()
		if err := unix.Symlinkat(l.Source, dir, tmp); err != nil {
			return "", classErr(ErrClassPrepare, "cannot place the link %s: %v", l.Target, err)
		}
		if err := unix.Renameat(dir, tmp, dir, leaf); err != nil {
			_ = unix.Unlinkat(dir, tmp, 0)
			return "", classErr(ErrClassPrepare, "cannot place the link %s: %v", l.Target, err)
		}
		return LinkLinked, nil
	case unix.S_IFREG:
		if !wasLink {
			logf("Warning: %s is a file that scion did not place; keeping it, so the staged file is not linked there", l.Target)
			return LinkSkipped, nil
		}
		// A tool replaced the link with a file: its changes are discarded,
		// as they were with the read-only mount the link replaces.
		if err := unix.Unlinkat(dir, leaf, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return "", classErr(ErrClassPrepare, "cannot replace %s: %v", l.Target, err)
		}
		if err := unix.Symlinkat(l.Source, dir, leaf); err != nil {
			return "", classErr(ErrClassPrepare, "cannot place the link %s: %v", l.Target, err)
		}
		return LinkLinked, nil
	case unix.S_IFDIR:
		return "", classErr(ErrClassPrepare, "cannot place the link %s: a directory is in its place", l.Target)
	default:
		logf("Warning: %s is a special file; keeping it", l.Target)
		return LinkSkipped, nil
	}
}
