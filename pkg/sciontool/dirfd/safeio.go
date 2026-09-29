/*
Copyright 2026 The Scion Authors.
*/

package dirfd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ErrNotSingleLinkRegular is returned by ReadAtNoFollow (and everything
// built on it) when the opened entry is not a regular file with exactly one
// hard link: a FIFO, a device, a directory, or a hardlink to an unrelated
// (possibly root-owned) file. A symlink at the leaf never reaches this
// check at all — O_NOFOLLOW on the open refuses it first (ELOOP) — but the
// same sentinel is used for both so callers have one thing to test for
// "this wasn't a plain file I can safely read", whichever way the refusal
// happened.
//
// Callers running in a root context are expected to treat this the way
// readServicesYAML's enforced branch already does: log the path and skip —
// not crash or fail hard — since a workload that owns the containing
// directory can always produce one of these instead of the file root
// expects, and root must not let that choice affect its own control flow.
var ErrNotSingleLinkRegular = errors.New("dirfd: not a single-link regular file")

// ErrTooLarge is returned when a read's content would exceed the caller-
// supplied byte limit. Detected via io.LimitReader(f, max+1): if that many
// bytes come back, the file is at least max+1 bytes long, so the read never
// buffers more than one byte past the limit before refusing it.
var ErrTooLarge = errors.New("dirfd: content exceeds size limit")

// ErrPathEscapesRoot is returned by ReadUnderRootNoFollow when path, once
// normalized, does not resolve to somewhere inside root — either because it
// requires a ".." to get there, or because filepath.Rel could not express
// it as a descendant of root at all. Refused before any filesystem call is
// made, since no amount of walking changes that answer. It is also returned
// once the walk is underway if a symlink encountered along the way (at any
// component, including the leaf) targets something this walk cannot prove
// stays under root: an absolute path, or a relative path containing "..".
var ErrPathEscapesRoot = errors.New("dirfd: path escapes root")

// ErrTooManySymlinksUnderRoot is returned by ReadUnderRootNoFollow when
// resolving path would require following more symlink hops than
// maxSymlinkResolutionsUnderRoot. This guards against a symlink loop
// planted by whatever owns the containing directory (e.g. "a" -> "b", "b"
// -> "a") spinning the walk forever — the same failure mode a kernel path
// lookup itself reports as ELOOP.
var ErrTooManySymlinksUnderRoot = errors.New("dirfd: too many symlinks resolving path under root")

// maxSymlinkResolutionsUnderRoot bounds how many symlink targets
// ReadUnderRootNoFollow will splice into its remaining path components
// while resolving path under root. A real Kubernetes projected-secret
// volume needs at most two hops to read one key: the key's own entry is a
// symlink through "..data" (e.g. "key" -> "..data/key"), and "..data"
// itself is a symlink to a timestamped directory (e.g. "..data" ->
// "..2026_09_28_12_00_00.123456789"). This is generously beyond that, the
// same order of headroom VerifyRootOwnedExecutable's maxSymlinkHops gives a
// root-owned binary's own alternatives chain, while still refusing a
// symlink loop instead of spinning forever.
const maxSymlinkResolutionsUnderRoot = 20

// ReadAtNoFollow opens name relative to dirFd for reading with
// O_NOFOLLOW|O_NONBLOCK — refusing (ELOOP), not following, a symlink
// planted at name, and never blocking the open even if name is a FIFO with
// no writer waiting (O_NONBLOCK makes open(2) return immediately instead of
// waiting for a writer to connect, which matters because this can run in
// root's own PID-1 process and must never hang it). It then fstats the
// result and requires a single-link regular file — ErrNotSingleLinkRegular
// otherwise, which also covers the FIFO case: a FIFO fails the S_IFREG
// check right here, before any read is ever attempted, so O_NONBLOCK's
// read-side semantics (which only prevent a blocking read, not guarantee
// one byte of data) never come into play. Finally it reads at most max+1
// bytes via io.LimitReader and reports ErrTooLarge if that bound is
// exceeded, the same "read one byte past the limit and check" pattern
// readServicesYAML uses.
//
// This is the shared core behind every bounded, no-follow read in this
// package: callers that already hold an open, symlink-safe directory fd —
// a root anchor from OpenDirNoFollow, or a caller doing its own component
// walk — call this directly; ReadFileNoFollow wraps it for the common
// "read one absolute path" case.
func ReadAtNoFollow(dirFd int, name string, max int64) ([]byte, error) {
	f, err := OpenAt(dirFd, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("dirfd: open %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()

	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return nil, fmt.Errorf("dirfd: stat %s: %w", name, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 {
		return nil, fmt.Errorf("dirfd: %s: %w", name, ErrNotSingleLinkRegular)
	}

	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, fmt.Errorf("dirfd: read %s: %w", name, err)
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("dirfd: %s: %w", name, ErrTooLarge)
	}
	return data, nil
}

// ReadFileNoFollow reads path the way readServicesYAML's enforced branch
// does: OpenParentNoFollow walks every directory component from "/" down
// with O_NOFOLLOW, so a symlinked intermediate directory is refused rather
// than followed, then ReadAtNoFollow opens and bounds-checks the leaf.
// path does not need to exist; a missing leaf or a missing intermediate
// directory both come back satisfying errors.Is(err, os.ErrNotExist), so
// callers that treat "absent" as "nothing to read" keep that behaviour.
func ReadFileNoFollow(path string, max int64) ([]byte, error) {
	dirFd, leaf, err := OpenParentNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = syscall.Close(dirFd) }()

	return ReadAtNoFollow(dirFd, leaf, max)
}

// writeNoFollowPreChmodTestHook, when non-nil, fires in WriteFileNoFollow
// right after the temp file has been written and before it is chmod'd (and
// chowned, if requested). It receives the temp file's leaf name — purely
// informational, so a test can simulate "the temp file's directory entry
// was unlinked and replaced with a symlink to some unrelated file in
// exactly this window" and then prove the subsequent Chmod/Chown still
// land on the original temp file's inode, never on whatever the swapped-in
// symlink points to, because both calls are fd-based (fchmod/fchown on the
// already-open descriptor) rather than a path-based lookup of the name.
// Always nil in production; unexported — this package's own tests are the
// only thing that may set it. Same purpose and fd-immutability guarantee
// as walk.go's chownWalkTestHook family; see its doc comment.
var writeNoFollowPreChmodTestHook func(tmpName string)

// LeafPolicy controls what WriteFileNoFollow does when path's own leaf
// entry ALREADY exists as something other than a plain regular file it is
// about to atomically replace.
//
// The zero value, LeafPolicyUnset, is deliberately not a usable choice:
// WriteFileNoFollow refuses it outright rather than silently behaving as
// either policy, so every caller must say which one it means. This exists
// because the two policies protect different things, and picking the wrong
// one by omission — inheriting whichever happens to be zero — would be a
// silent security decision, not a default worth having.
type LeafPolicy int

const (
	// LeafPolicyUnset is LeafPolicy's zero value. WriteFileNoFollow returns
	// an error if it is ever passed; it has no meaning of its own.
	LeafPolicyUnset LeafPolicy = iota

	// ReplaceLeaf renames the new content over path's leaf unconditionally,
	// whatever currently sits there — a symlink, a FIFO, a regular file, or
	// nothing. RenameAt (renameat(2)) replaces a directory ENTRY without
	// ever dereferencing it, so a symlink at the leaf is replaced outright,
	// never followed or written through, the same guarantee either policy
	// gives; the two differ only in whether that pre-existing entry is worth
	// refusing on sight.
	//
	// Use this for installing content into a location inside a directory
	// the WORKLOAD owns outright (e.g. $HOME/.gitconfig, $HOME's
	// agent-info.json): the workload can always recreate that same
	// substitution the instant after a refusal would have run anyway, so
	// refusing buys nothing, and a legitimate stale leaf (yesterday's
	// gitconfig, yesterday's status file) is exactly what this call means to
	// overwrite.
	ReplaceLeaf

	// RefuseSymlink checks path's leaf BEFORE doing any work — via
	// RefuseSymlinkOrNonRegularAt, against the same already-open parent
	// dirFd this call holds — and fails closed if it is already a symlink or
	// any non-regular file (FIFO, device, directory), instead of silently
	// replacing it.
	//
	// Use this for credential and state files whose directory is NOT
	// necessarily under the same party's control as the content being
	// written (an auth token, scion-env): a planted link at the destination
	// there means tampering worth refusing loudly, not a stale leaf to
	// overwrite quietly.
	RefuseSymlink
)

// WriteFileNoFollow atomically replaces path's content the way
// writeLimitsState does: it resolves path's parent directory once via
// OpenParentNoFollow, optionally refuses an existing non-regular leaf (see
// LeafPolicy), creates a temp file in that directory with CreateExclAt
// (O_CREAT|O_EXCL|O_NOFOLLOW, so the temp name itself cannot already be a
// pre-planted symlink), writes data to it, sets its permission bits via
// Chmod on the open *os.File (fchmod on the fd — never a path-based
// os.Chmod, which a symlink swapped in at the temp path between create and
// chmod could redirect to an arbitrary target's permissions) and, when
// uid > 0, its ownership the same fd-based way, fsyncs it, then renames it
// into place with a single fd-relative RenameAt. The temp file is removed
// on any error path.
//
// This is the one fd-anchored writer every atomic-install caller in this
// codebase shares; hub.WriteFileNoFollowChown is a thin wrapper over
// WriteFileNoFollowWithChown (this function's own core, see its doc
// comment) rather than a second, independent implementation of the same
// create-write-chmod-chown-fsync-rename sequence.
func WriteFileNoFollow(path string, data []byte, mode os.FileMode, uid, gid int, policy LeafPolicy) error {
	return WriteFileNoFollowWithChown(path, data, mode, uid, gid, policy, syscall.Fchown)
}

// WriteFileNoFollowWithChown is WriteFileNoFollow's core, parameterized on
// the fd-based chown call itself. Production code always gets there through
// WriteFileNoFollow, which passes syscall.Fchown; a caller that needs its
// own test seam for the chown step — hub.WriteFileNoFollowChown does, so
// its existing tests can intercept the fchown call without actually needing
// CAP_CHOWN — calls this directly with its own chown function instead.
func WriteFileNoFollowWithChown(path string, data []byte, mode os.FileMode, uid, gid int, policy LeafPolicy, chown func(fd, uid, gid int) error) (err error) {
	if policy != ReplaceLeaf && policy != RefuseSymlink {
		return fmt.Errorf("dirfd: WriteFileNoFollow %s: invalid LeafPolicy %d (every caller must choose ReplaceLeaf or RefuseSymlink)", path, policy)
	}

	dirFd, leaf, err := OpenParentNoFollow(path)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(dirFd) }()

	if policy == RefuseSymlink {
		if rerr := RefuseSymlinkOrNonRegularAt(dirFd, leaf); rerr != nil {
			return fmt.Errorf("dirfd: refusing to write %s: %w", path, rerr)
		}
	}

	// PID + nanosecond timestamp is unique enough that CreateExclAt's
	// O_EXCL is only ever a defense against a pre-planted entry at this
	// name, not a collision this process itself needs to retry.
	tmpName := fmt.Sprintf(".%s.tmp-%d-%d", leaf, os.Getpid(), time.Now().UnixNano())
	tmpFile, err := CreateExclAt(dirFd, tmpName, 0o600)
	if err != nil {
		return fmt.Errorf("dirfd: create temp for %s: %w", path, err)
	}
	return writeTempAndRename(dirFd, tmpFile, tmpName, leaf, path, data, mode, uid, gid, chown)
}

func writeTempAndRename(dirFd int, tmpFile *os.File, tmpName, leaf, path string, data []byte, mode os.FileMode, uid, gid int, chown func(fd, uid, gid int) error) (err error) {
	defer func() {
		if err != nil {
			_ = UnlinkAt(dirFd, tmpName)
		}
	}()

	if _, werr := tmpFile.Write(data); werr != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("dirfd: write temp for %s: %w", path, werr)
	}

	if writeNoFollowPreChmodTestHook != nil {
		writeNoFollowPreChmodTestHook(tmpName)
	}

	if cerr := tmpFile.Chmod(mode); cerr != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("dirfd: chmod temp for %s: %w", path, cerr)
	}
	if uid > 0 {
		if cerr := chown(int(tmpFile.Fd()), uid, gid); cerr != nil {
			_ = tmpFile.Close()
			return fmt.Errorf("dirfd: chown temp for %s: %w", path, cerr)
		}
	}
	// fsync after every metadata change and before the rename that makes
	// this content visible at path: a crash between rename and a later
	// fsync could otherwise leave path pointing at a temp file whose
	// content, mode, or ownership never made it to disk, even though
	// renameat(2) itself already completed.
	if serr := tmpFile.Sync(); serr != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("dirfd: fsync temp for %s: %w", path, serr)
	}
	if cerr := tmpFile.Close(); cerr != nil {
		return fmt.Errorf("dirfd: close temp for %s: %w", path, cerr)
	}

	if rerr := RenameAt(dirFd, tmpName, leaf); rerr != nil {
		return fmt.Errorf("dirfd: rename into %s: %w", path, rerr)
	}
	return nil
}

// relUnderRoot resolves path and root to absolute form and reports the
// path relative to root, refusing (ErrPathEscapesRoot) anything that
// requires a ".." to get there, is root itself, or that filepath.Rel could
// not express as a descendant of root at all. This is a string-level
// pre-check only — ReadUnderRootNoFollow does not trust it for safety, only
// for turning path into the list of component names it walks one openat
// per component; containment itself is enforced by that walk never
// resolving a name outside the fd chain it holds, not by this check.
func relUnderRoot(root, path string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("dirfd: resolve root %s: %w", root, err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("dirfd: resolve %s: %w", path, err)
	}
	rel, err := filepath.Rel(absRoot, absPath)
	escapes := err != nil || rel == "." || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)
	if !escapes {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			if part == "" || part == "." || part == ".." {
				escapes = true
				break
			}
		}
	}
	if escapes {
		return "", fmt.Errorf("dirfd: %s: %w", path, ErrPathEscapesRoot)
	}
	return rel, nil
}

// readUnderRootIntermediateCloseTestHook, when non-nil, fires in
// ReadUnderRootNoFollow immediately after an owned intermediate directory
// fd is closed — whether the next component's openat succeeded or not. It
// receives the exact fd number that was just closed, purely informational,
// so a test can immediately dup a sentinel onto that number (claiming it
// deterministically, rather than hoping the kernel's normal allocator
// happens to reuse it for something else first) and later prove this
// package's own deferred closer never touches that number again. Always
// nil in production; unexported — this package's own tests are the only
// thing that may set it. Same purpose as walk.go's chownWalkTestHook
// family; see its doc comment.
var readUnderRootIntermediateCloseTestHook func(closedFd int)

// ReadUnderRootNoFollow reads path, which must resolve to somewhere inside
// root, by walking from root down to path one component at a time with
// openat(..., O_NOFOLLOW): root itself is opened once via OpenDirNoFollow
// (refusing a symlink at root's own leaf too), and every remaining
// directory component is opened relative to the previous component's own
// already-open fd — never by re-parsing a path string — with
// O_DIRECTORY|O_NOFOLLOW, before the final component is read via
// ReadAtNoFollow's own O_NOFOLLOW|O_NONBLOCK open and bounded read.
//
// Containment is checked by walking, not by comparing path strings, which a
// string-prefix or filepath.Rel check alone cannot make symlink-safe: a
// symlink whose own *name* sits inside root but whose target does not
// (root/link -> /etc/shadow) must be refused even though the string form
// "root/link" passes any textual containment check. relUnderRoot's job is
// only to turn path into the list of component names this function walks;
// every actual safety guarantee comes from resolving each of those names
// against an fd already anchored inside root, with no step that ever looks
// a name up starting from "/" again.
//
// Unlike a bare O_NOFOLLOW walk, a symlink encountered at ANY component —
// intermediate or leaf — is not refused outright: this function follows it
// exactly as far as it can prove, by continuing the same fd-anchored walk,
// that the target stays under root. When the no-follow open of a component
// fails, isSymlinkAt fstats that exact name (AT_SYMLINK_NOFOLLOW, against
// the same dirFd, so still no path re-lookup) to tell "this failed because
// it's a symlink" apart from every other reason an open can fail — the two
// look identical in errno terms for an intermediate component: an
// O_DIRECTORY|O_NOFOLLOW open of a symlink fails ENOTDIR on Linux, the same
// code a plain file in that position would also produce, not the ELOOP a
// non-O_DIRECTORY open of a symlink gives. Only a confirmed symlink is
// resolved; every other failure (missing, wrong type, permission) is
// returned as-is. Once confirmed, the target is read with unix.Readlinkat
// against the fd this walk already holds for its containing directory —
// never os.Readlink or filepath.EvalSymlinks, both of which re-parse a path
// string from scratch, reopening the TOCTOU window a workload that owns the
// directory could use to swap an ancestor between the read and the reopen —
// and its components are spliced in front of the remaining path and walked
// the same way. An absolute target, or one containing "..", is refused
// (ErrPathEscapesRoot): this walk has no way to prove either stays under
// root without trusting the target's own text, which is exactly what it
// must not do. A chain longer than maxSymlinkResolutionsUnderRoot hops is
// refused (ErrTooManySymlinksUnderRoot) rather than followed forever.
//
// This is what makes a real Kubernetes projected-secret volume readable:
// kubelet lays each key out as "key" -> "..data/key", and "..data" itself
// as a symlink to a timestamped directory holding the actual files — two
// ordinary, root-relative symlink hops that never leave the volume's own
// directory, which a blanket symlink refusal broke unconditionally.
//
// There is no separate stat anywhere in this path beyond what resolving a
// symlink requires: the file content itself is read exactly once, from the
// fd the walk finally verifies, by ReadAtNoFollow.
func ReadUnderRootNoFollow(root, path string, max int64) ([]byte, error) {
	rel, err := relUnderRoot(root, path)
	if err != nil {
		return nil, err
	}

	rootFile, err := OpenDirNoFollow(root)
	if err != nil {
		return nil, fmt.Errorf("dirfd: open root %s: %w", root, err)
	}
	defer func() { _ = rootFile.Close() }()

	queue := strings.Split(rel, string(filepath.Separator))
	curFd := int(rootFile.Fd())
	ownsCur := false
	defer func() {
		if ownsCur {
			_ = syscall.Close(curFd)
		}
	}()

	symlinksLeft := maxSymlinkResolutionsUnderRoot
	for {
		name := queue[0]
		queue = queue[1:]
		last := len(queue) == 0

		if last {
			data, rerr := ReadAtNoFollow(curFd, name, max)
			if rerr == nil {
				return data, nil
			}
			if !isSymlinkAt(curFd, name) {
				return nil, rerr
			}
		} else {
			child, operr := syscall.Openat(curFd, name, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
			if operr == nil {
				if ownsCur {
					_ = syscall.Close(curFd)
					if readUnderRootIntermediateCloseTestHook != nil {
						readUnderRootIntermediateCloseTestHook(curFd)
					}
					// Mark curFd's ownership released the instant it's
					// closed, not after the branch below: in a
					// multi-threaded process — this walk can run in root's
					// own PID-1 — a second goroutine can have already been
					// handed that number back by the kernel by the time
					// anything later in this function looks at ownsCur, so
					// leaving it true a moment longer risks a second close
					// of somebody else's fd.
					ownsCur = false
				}
				curFd = child
				ownsCur = true
				continue
			}
			if !isSymlinkAt(curFd, name) {
				if ownsCur {
					_ = syscall.Close(curFd)
					if readUnderRootIntermediateCloseTestHook != nil {
						readUnderRootIntermediateCloseTestHook(curFd)
					}
					ownsCur = false
				}
				return nil, fmt.Errorf("dirfd: open %s: %w", name, operr)
			}
			// name is a confirmed symlink. curFd (the directory name was
			// resolved against) is deliberately left open here — the
			// symlink resolution below reads its target through this same
			// fd — rather than closed the way the success and hard-failure
			// branches above close it.
		}

		// Both branches above only fall through here for a confirmed
		// symlink: name is a symlink in the directory curFd is open on.
		// Resolve it through curFd itself, never by re-parsing path or
		// root as strings and looking name up again from scratch.
		symlinksLeft--
		if symlinksLeft < 0 {
			return nil, fmt.Errorf("dirfd: %s: %w", name, ErrTooManySymlinksUnderRoot)
		}
		target, terr := readlinkAt(curFd, name)
		if terr != nil {
			return nil, fmt.Errorf("dirfd: readlink %s: %w", name, terr)
		}
		if filepath.IsAbs(target) {
			return nil, fmt.Errorf("dirfd: symlink %s targets absolute path %q: %w", name, target, ErrPathEscapesRoot)
		}
		spliced := make([]string, 0, len(queue)+2)
		for _, part := range strings.Split(target, string(filepath.Separator)) {
			switch part {
			case "", ".":
				continue
			case "..":
				return nil, fmt.Errorf("dirfd: symlink %s targets %q, which climbs above its own directory: %w", name, target, ErrPathEscapesRoot)
			default:
				spliced = append(spliced, part)
			}
		}
		if len(spliced) == 0 {
			return nil, fmt.Errorf("dirfd: symlink %s resolved to an empty target: %w", name, ErrPathEscapesRoot)
		}
		queue = append(spliced, queue...)
	}
}

// isSymlinkAt reports whether name, relative to dirFd, is currently a
// symlink, via unix.Fstatat with AT_SYMLINK_NOFOLLOW — the fd-relative
// equivalent of Lstat, so this is still resolved against the fd the caller's
// walk already holds, never by re-parsing a path string. Used only to
// classify why a preceding no-follow open of the same name just failed: a
// missing name, a permission error, or (for an O_DIRECTORY open) a
// non-symlink non-directory all report false here, so the caller's original
// error is what gets returned for those. It never fires on a name that
// resolved successfully — this function is not part of the "is name safe to
// use" decision, only "was that failure a symlink."
func isSymlinkAt(dirFd int, name string) bool {
	var st unix.Stat_t
	if err := unix.Fstatat(dirFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return false
	}
	return st.Mode&unix.S_IFMT == unix.S_IFLNK
}

// readlinkAt reads the symlink at name relative to dirFd via
// unix.Readlinkat, not os.Readlink or filepath.EvalSymlinks — both of
// which take a path string and re-resolve it from scratch — so the read
// happens against the same already-open, already-verified directory fd the
// caller's walk holds, with no second name lookup for a concurrent
// rename/symlink-swap of an ancestor to land in.
func readlinkAt(dirFd int, name string) (string, error) {
	buf := make([]byte, 4096)
	n, err := unix.Readlinkat(dirFd, name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}
