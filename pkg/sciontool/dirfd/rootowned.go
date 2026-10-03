/*
Copyright 2026 The Scion Authors.
*/

package dirfd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// trustedChainModeBits are the permission bits chainIsTrusted refuses on any
// component: group-write and other-write. There is deliberately no
// sticky-bit exemption. A sticky bit only makes a world-writable directory
// (e.g. a hardened "/tmp") resistant to one workload renaming or unlinking
// another workload's entries — it does nothing to stop a workload from
// creating a brand new entry there in the first place, which is all the
// directories this check protects need to resist.
const trustedChainModeBits = 0o022

// chainIsTrusted reports whether a single already-fstat'd component (an
// arbitrary node along a directory chain) is safe to treat as under this
// process's exclusive control: owned by uid 0, or — when this process is not
// itself running as root (a rootless/keep-id deployment, where PID 1 starts
// as the workload identity and there is no separate root/workload boundary
// to protect at all) — owned by selfUID, the caller's own effective uid.
// Either way, it must carry neither the group- nor the other-write bit.
//
// It says nothing about any OTHER component in the chain — callers combine
// every component's result themselves by requiring it of all of them. Kept
// as a pure function of already-fetched values (not one that stats anything
// itself), so it can be exercised by a test with fabricated ownership/mode
// values, the same way pkg/sciontool/hooks.NodeOwnership.rootProtected is
// tested, without requiring the test process itself to be root or to own a
// real root-owned directory.
func chainIsTrusted(uid uint32, mode uint32, selfUID uint32) bool {
	return (uid == 0 || uid == selfUID) && mode&trustedChainModeBits == 0
}

// OpenParentNoFollowRootOwned resolves path's parent directory exactly like
// OpenParentNoFollow — walking every component from "/" down with
// openat(2) O_DIRECTORY|O_NOFOLLOW, so a symlink planted at any component is
// refused rather than followed — but in addition fstats every already-open
// component, including "/" itself, and fails closed (see chainIsTrusted) the
// instant any of them is not safe to treat as under this process's exclusive
// control.
//
// This exists for a directory that must be root-only along its ENTIRE
// chain, not just at its own leaf: OpenParentNoFollow alone refuses a
// symlink swapped into an ancestor, but says nothing about an ancestor that
// is simply writable by everyone (e.g. an unhardened, non-sticky "/tmp"),
// which on its own is enough for a workload to rename an entry out of it and
// plant a symlink in its place. No component's mode is ever assumed from
// its name — not even "/" or "/run", both of which can legitimately vary
// across images and runtimes — every one is checked by fstat on its own
// already-open descriptor as the walk proceeds, which also closes the
// TOCTOU window a separate stat-by-path-then-open-by-path sequence would
// leave open.
func OpenParentNoFollowRootOwned(path string) (dirFd int, leaf string, err error) {
	selfUID := uint32(os.Geteuid())

	abs, err := filepath.Abs(path)
	if err != nil {
		return -1, "", fmt.Errorf("dirfd: resolve %s: %w", path, err)
	}
	abs = filepath.Clean(abs)
	if abs == string(filepath.Separator) {
		return -1, "", fmt.Errorf("dirfd: refusing to resolve the root directory itself")
	}

	trimmed := strings.TrimPrefix(abs, string(filepath.Separator))
	parts := strings.Split(trimmed, string(filepath.Separator))
	leaf = parts[len(parts)-1]
	dirs := parts[:len(parts)-1]

	fd, err := syscall.Open(string(filepath.Separator), syscall.O_DIRECTORY|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", fmt.Errorf("dirfd: open /: %w", err)
	}
	if verr := fstatRequireTrusted(fd, string(filepath.Separator), selfUID); verr != nil {
		_ = syscall.Close(fd)
		return -1, "", verr
	}

	walked := string(filepath.Separator)
	for _, name := range dirs {
		child, oerr := unix.Openat(fd, name, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		_ = syscall.Close(fd)
		if oerr != nil {
			return -1, "", fmt.Errorf("dirfd: open %s: %w", name, oerr)
		}
		walked = filepath.Join(walked, name)
		if verr := fstatRequireTrusted(child, walked, selfUID); verr != nil {
			_ = syscall.Close(child)
			return -1, "", verr
		}
		fd = child
	}
	return fd, leaf, nil
}

// fstatRequireTrusted fstats fd and fails closed via chainIsTrusted.
// displayPath is used only for the returned error's message.
func fstatRequireTrusted(fd int, displayPath string, selfUID uint32) error {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return fmt.Errorf("dirfd: fstat %s: %w", displayPath, err)
	}
	if !chainIsTrusted(st.Uid, uint32(st.Mode), selfUID) {
		return fmt.Errorf("dirfd: %s is not root-owned (or self-owned) and free of group/other write (uid=%d mode=%#o)", displayPath, st.Uid, st.Mode&0o7777)
	}
	return nil
}

// OpenNoFollowRootOwnedFile opens path as a regular file, applying the same
// root-owned-or-self-owned, not-group/other-writable check to its entire
// parent chain (OpenParentNoFollowRootOwned) and to the leaf itself.
//
// path must already be free of symlink components by the time this is
// called — e.g. the output of filepath.EvalSymlinks — so this can safely use
// O_NOFOLLOW at every step without refusing a perfectly ordinary system
// layout where "/bin" or "/sbin" are themselves symlinks into "/usr" (Debian
// and most other modern distributions' merged-/usr layout): a caller that
// wants to allow a legitimate root-installed symlink chain (e.g. Debian's
// iptables, which resolves through /etc/alternatives) resolves it first,
// then verifies the real destination with this function — the destination,
// and every real directory leading to it, is what must be root-owned (or
// self-owned, on a runtime with no separate root/workload identity to
// protect against), never the symlink names along the way.
//
// Used by cmd/sciontool/commands' substrate rootfs-fixup and serve paths to
// verify a destination file is safe to open (never a workload-planted
// symlink, and never reached through a workload-writable or
// non-root-owned parent directory) before root reads or writes through
// it. This does not check the file's link count: a hardlink to an
// otherwise-legitimate root-owned file, planted before this process ever
// reaches it, is not detected or refused here.
func OpenNoFollowRootOwnedFile(path string) (*os.File, error) {
	dirFd, leaf, err := OpenParentNoFollowRootOwned(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = syscall.Close(dirFd) }()

	f, err := OpenAt(dirFd, leaf, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("dirfd: open %s: %w", path, err)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("dirfd: fstat %s: %w", path, err)
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		_ = f.Close()
		return nil, fmt.Errorf("dirfd: %s is not a regular file (mode %#o)", path, st.Mode&syscall.S_IFMT)
	}
	if verr := fstatRequireTrusted(int(f.Fd()), path, uint32(os.Geteuid())); verr != nil {
		_ = f.Close()
		return nil, verr
	}
	return f, nil
}

// maxSymlinkHops bounds VerifyRootOwnedExecutable's manual symlink-chain
// walk, generously beyond any real installation this codebase's search
// paths are expected to encounter (Debian's update-alternatives chains are
// two hops).
const maxSymlinkHops = 20

// VerifyRootOwnedExecutable verifies that candidate — and, if it is a
// symlink, every hop of its symlink chain down to the final destination —
// are each root-owned (or self-owned, on a runtime with no separate root/
// workload identity to protect against at all) and free of the group- and
// other-write bits, including each hop's own containing-directory chain
// (walked from "/" via OpenParentNoFollowRootOwned, exactly like a
// standalone file's). This is what makes it safe for a caller to go on to
// exec candidate itself, rather than the fully-resolved destination: once
// every hop and its own directory chain are proven under root's (or self's)
// exclusive control, the workload has no write access anywhere along the
// path a later, real execve() would re-walk, so there is no window between
// this check and that exec for anything to change.
//
// A legitimate root-installed symlink chain (Debian's "iptables" ->
// "/etc/alternatives/iptables" -> "xtables-nft-multi", a multi-call binary
// that dispatches on argv[0]'s own basename) is followed and verified hop
// by hop, never refused outright the way a single O_NOFOLLOW open would
// refuse any symlink at its leaf. A caller that needs
// a multi-call binary's dispatch to work needs candidate's OWN basename to
// survive into argv[0], which only holds if it goes on to exec candidate
// itself, not whatever this walk resolves it to — so this function never
// returns the resolved path, only whether exec'ing candidate is safe.
func VerifyRootOwnedExecutable(candidate string) error {
	selfUID := uint32(os.Geteuid())
	current := candidate
	visited := make(map[string]bool, maxSymlinkHops)

	for hop := 0; hop < maxSymlinkHops; hop++ {
		if visited[current] {
			return fmt.Errorf("dirfd: symlink loop resolving %s", candidate)
		}
		visited[current] = true

		dirFd, leaf, err := OpenParentNoFollowRootOwned(current)
		if err != nil {
			return fmt.Errorf("dirfd: %s: %w", candidate, err)
		}

		fd, openErr := unix.Openat(dirFd, leaf, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if openErr == nil {
			var st syscall.Stat_t
			statErr := syscall.Fstat(fd, &st)
			_ = syscall.Close(fd)
			_ = syscall.Close(dirFd)
			if statErr != nil {
				return fmt.Errorf("dirfd: fstat %s: %w", current, statErr)
			}
			if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
				return fmt.Errorf("dirfd: %s is not a regular file (mode %#o)", current, st.Mode&syscall.S_IFMT)
			}
			if !chainIsTrusted(st.Uid, uint32(st.Mode), selfUID) {
				return fmt.Errorf("dirfd: %s is not root-owned (or self-owned) and free of group/other write (uid=%d mode=%#o)", current, st.Uid, st.Mode&0o7777)
			}
			return nil
		}
		if !errors.Is(openErr, syscall.ELOOP) {
			_ = syscall.Close(dirFd)
			return fmt.Errorf("dirfd: open %s: %w", current, openErr)
		}

		// current's leaf is a symlink: verify the symlink directory entry's
		// own OWNER (unix.Fstatat with AT_SYMLINK_NOFOLLOW is the
		// fd-relative equivalent of Lstat — a fresh workload-planted
		// symlink here would show the workload's own uid). A symlink's own
		// permission bits are not meaningful on Linux — the kernel reports
		// (and largely ignores) a fixed mode for every symlink regardless
		// of anything resembling a chmod — so only ownership is checked
		// here, unlike chainIsTrusted's combined uid+mode rule for a real
		// file or directory. Then read the target and continue the walk.
		var lst unix.Stat_t
		if err := unix.Fstatat(dirFd, leaf, &lst, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			_ = syscall.Close(dirFd)
			return fmt.Errorf("dirfd: lstat %s: %w", current, err)
		}
		if lst.Uid != 0 && lst.Uid != selfUID {
			_ = syscall.Close(dirFd)
			return fmt.Errorf("dirfd: symlink %s is not owned by root (or self) (uid=%d)", current, lst.Uid)
		}
		buf := make([]byte, 4096)
		n, rlErr := unix.Readlinkat(dirFd, leaf, buf)
		_ = syscall.Close(dirFd)
		if rlErr != nil {
			return fmt.Errorf("dirfd: readlink %s: %w", current, rlErr)
		}
		target := string(buf[:n])
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
		current = filepath.Clean(target)
	}
	return fmt.Errorf("dirfd: too many symlink hops resolving %s (> %d)", candidate, maxSymlinkHops)
}
