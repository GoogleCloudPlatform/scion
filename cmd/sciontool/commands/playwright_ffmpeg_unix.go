//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// defaultPlaywrightFfmpegSeedDir is the immutable 0755 root:root directory
// populated by `install-core-toolchain.sh playwright-ffmpeg` in core-base and
// thick-prep. Keeping the image-baked files here and symlinking them into the
// agent's persistent ~/.cache/ms-playwright registry at container start avoids
// setting an image-wide PLAYWRIGHT_BROWSERS_PATH (which would move runtime
// browser downloads out of the persistent home into the ephemeral container
// writable layer).
const defaultPlaywrightFfmpegSeedDir = "/usr/local/share/scion-playwright-ffmpeg"

// playwrightFfmpegSeedDir and runSeedPlaywrightFfmpegCache are package vars so
// TestMain can isolate tests from any host/container seed directory and RunInit
// tests can point at a synthetic fixture.
var (
	playwrightFfmpegSeedDir      = defaultPlaywrightFfmpegSeedDir
	runSeedPlaywrightFfmpegCache = seedPlaywrightFfmpegCache
	playwrightFfmpegFchown       = dirfd.Fchown
	playwrightFfmpegLchownAt     = func(dirFd int, name string, uid, gid int) error {
		return unix.Fchownat(dirFd, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW)
	}
)

// seedPlaywrightFfmpegCache seeds symlinks from seedDir into Playwright's
// persistent home-based registry (<XDG_CACHE_HOME || ~/.cache>/ms-playwright)
// when XDG_CACHE_HOME resolves inside agentHome or is unset.
//
// Invariants:
//   - Strictly non-fatal: any error logs at debug level and returns cleanly.
//   - Silent no-op when seedDir does not exist (single ENOENT directory open).
//   - Never follows or replaces anything the user owns: existing real
//     directories (e.g. a previously downloaded ffmpeg-<rev>) and symlinks
//     targeting paths outside seedDir are left untouched; any symlink planted
//     along .cache/ms-playwright/.links or at a .links leaf file is refused via
//     dirfd/*at no-follow operations.
func seedPlaywrightFfmpegCache(agentHome, seedDir string, uid, gid int) {
	if agentHome == "" || seedDir == "" {
		return
	}
	cleanSeed := filepath.Clean(seedDir)
	if !filepath.IsAbs(cleanSeed) || cleanSeed == string(filepath.Separator) {
		return
	}

	entries, err := os.ReadDir(cleanSeed)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Debug("Playwright ffmpeg seed: skipping %s: %v", cleanSeed, err)
		}
		return
	}

	revNames := make([]string, 0, len(entries))
	seededSet := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() && isPlaywrightFfmpegDirName(name) {
			revNames = append(revNames, name)
			seededSet[name] = struct{}{}
		}
	}
	if len(revNames) == 0 {
		return
	}

	if xdg := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(xdg) {
		cand := filepath.Join(xdg, "ms-playwright")
		if fd, underRoot, err := dirfd.EnsureDirNoFollowUnderRoot(agentHome, cand, 0o755, uid, gid); underRoot {
			if err != nil {
				log.Debug("Playwright ffmpeg seed: skipping %s: %v", cand, err)
				return
			}
			regFile := os.NewFile(uintptr(fd), cand)
			defer func() { _ = regFile.Close() }()
			seedPlaywrightFfmpegIntoRegistry(regFile, cand, cleanSeed, revNames, seededSet, uid, gid)
			return
		}
	}

	registryPath := filepath.Join(agentHome, ".cache", "ms-playwright")
	fd, underRoot, err := dirfd.EnsureDirNoFollowUnderRoot(agentHome, registryPath, 0o755, uid, gid)
	if !underRoot || err != nil {
		if err != nil {
			log.Debug("Playwright ffmpeg seed: skipping %s: %v", registryPath, err)
		}
		return
	}
	regFile := os.NewFile(uintptr(fd), registryPath)
	defer func() { _ = regFile.Close() }()
	seedPlaywrightFfmpegIntoRegistry(regFile, registryPath, cleanSeed, revNames, seededSet, uid, gid)
}

func seedPlaywrightFfmpegIntoRegistry(regFile *os.File, registryPath, cleanSeed string, revNames []string, seededSet map[string]struct{}, uid, gid int) {
	regFd := int(regFile.Fd())
	linksPath := filepath.Join(registryPath, ".links")
	linksFd, err := ensureChildDirNoFollow(regFd, ".links", 0o755, uid, gid)
	if err != nil {
		log.Debug("Playwright ffmpeg seed: skipping %s: %v", linksPath, err)
		return
	}
	linksFile := os.NewFile(uintptr(linksFd), linksPath)
	defer func() { _ = linksFile.Close() }()

	// Prune retired seed symlinks left by an older base image (only when the
	// entry is a symlink provably targeting cleanSeed and no longer present in
	// seededSet). User directories and foreign symlinks are never touched.
	if existingNames, rerr := regFile.Readdirnames(-1); rerr == nil {
		for _, name := range existingNames {
			if !isPlaywrightFfmpegDirName(name) {
				continue
			}
			if _, keep := seededSet[name]; keep {
				continue
			}
			var st unix.Stat_t
			if err := unix.Fstatat(regFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				continue
			}
			if st.Mode&unix.S_IFMT != unix.S_IFLNK {
				continue
			}
			target, err := readPlaywrightSymlinkAt(regFd, name)
			if err != nil || !isOwnedSeedTarget(target, cleanSeed) {
				continue
			}
			if err := dirfd.UnlinkAt(regFd, name); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Debug("Playwright ffmpeg seed: failed to prune retired symlink %s: %v", name, err)
				continue
			}
			linkName := "scion-" + name
			if raw, rerr := dirfd.ReadAtNoFollow(int(linksFile.Fd()), linkName, 4096); rerr == nil && isOwnedSeedTarget(string(raw), cleanSeed) {
				_ = dirfd.UnlinkAt(int(linksFile.Fd()), linkName)
			}
		}
	}

	for _, revName := range revNames {
		wantTarget := filepath.Join(cleanSeed, revName)
		linkName := "scion-" + revName

		var st unix.Stat_t
		err := unix.Fstatat(regFd, revName, &st, unix.AT_SYMLINK_NOFOLLOW)
		switch {
		case errors.Is(err, syscall.ENOENT):
			if serr := unix.Symlinkat(wantTarget, regFd, revName); serr != nil {
				log.Debug("Playwright ffmpeg seed: symlink %s: %v", revName, serr)
				continue
			}
			if uid > 0 {
				if cerr := playwrightFfmpegLchownAt(regFd, revName, uid, gid); cerr != nil {
					log.Debug("Playwright ffmpeg seed: lchown %s: %v", revName, cerr)
				}
			}
			ensurePlaywrightLinkFile(int(linksFile.Fd()), linksPath, linkName, []byte(wantTarget), uid, gid)

		case err != nil:
			log.Debug("Playwright ffmpeg seed: stat %s: %v", revName, err)

		case st.Mode&unix.S_IFMT == unix.S_IFLNK:
			existingTarget, rerr := readPlaywrightSymlinkAt(regFd, revName)
			if rerr != nil {
				log.Debug("Playwright ffmpeg seed: readlink %s: %v", revName, rerr)
				continue
			}
			if !isOwnedSeedTarget(existingTarget, cleanSeed) {
				// Foreign symlink created by the user: leave untouched.
				continue
			}
			if existingTarget != wantTarget {
				if rerr := replacePlaywrightSymlinkAt(regFd, revName, wantTarget, uid, gid); rerr != nil {
					log.Debug("Playwright ffmpeg seed: refresh symlink %s: %v", revName, rerr)
					continue
				}
			}
			ensurePlaywrightLinkFile(int(linksFile.Fd()), linksPath, linkName, []byte(wantTarget), uid, gid)

		default:
			// Existing real directory (e.g. previously downloaded ffmpeg-<rev>)
			// or user file: never follow, overwrite, or delete.
		}
	}
}

func ensureChildDirNoFollow(parentFd int, name string, mode os.FileMode, uid, gid int) (int, error) {
	child, err := unix.Openat(parentFd, name, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err == nil {
		return child, nil
	}
	if !errors.Is(err, syscall.ENOENT) {
		return -1, fmt.Errorf("open %s: %w", name, err)
	}
	if merr := unix.Mkdirat(parentFd, name, uint32(mode)); merr != nil && !errors.Is(merr, syscall.EEXIST) {
		return -1, fmt.Errorf("mkdir %s: %w", name, merr)
	}
	child, err = unix.Openat(parentFd, name, syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open %s after create: %w", name, err)
	}
	if uid > 0 {
		if cerr := playwrightFfmpegFchown(child, uid, gid); cerr != nil {
			_ = syscall.Close(child)
			return -1, fmt.Errorf("chown %s: %w", name, cerr)
		}
	}
	return child, nil
}

func replacePlaywrightSymlinkAt(dirFd int, name, wantTarget string, uid, gid int) error {
	tmpName := fmt.Sprintf(".%s.tmp-%d-%d", name, os.Getpid(), time.Now().UnixNano())
	if err := unix.Symlinkat(wantTarget, dirFd, tmpName); err != nil {
		return err
	}
	if uid > 0 {
		if cerr := playwrightFfmpegLchownAt(dirFd, tmpName, uid, gid); cerr != nil {
			_ = dirfd.UnlinkAt(dirFd, tmpName)
			return cerr
		}
	}
	if err := dirfd.RenameAt(dirFd, tmpName, name); err != nil {
		_ = dirfd.UnlinkAt(dirFd, tmpName)
		return err
	}
	return nil
}

func ensurePlaywrightLinkFile(linksFd int, linksPath, linkName string, want []byte, uid, gid int) {
	got, err := dirfd.ReadAtNoFollow(linksFd, linkName, 4096)
	if err == nil && bytes.Equal(got, want) {
		return
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Debug("Playwright ffmpeg seed: refusing non-regular or symlinked %s/%s: %v", linksPath, linkName, err)
		return
	}
	if werr := dirfd.WriteAtNoFollowWithChown(
		linksFd,
		linkName,
		filepath.Join(linksPath, linkName),
		want,
		0o644,
		uid,
		gid,
		dirfd.RefuseSymlink,
		playwrightFfmpegFchown,
	); werr != nil {
		log.Debug("Playwright ffmpeg seed: write %s/%s: %v", linksPath, linkName, werr)
	}
}

func readPlaywrightSymlinkAt(dirFd int, name string) (string, error) {
	buf := make([]byte, 4096)
	n, err := unix.Readlinkat(dirFd, name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

func isPlaywrightFfmpegDirName(name string) bool {
	const prefix = "ffmpeg-"
	if !strings.HasPrefix(name, prefix) || len(name) == len(prefix) {
		return false
	}
	for i := len(prefix); i < len(name); i++ {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return true
}

func isOwnedSeedTarget(rawTarget, cleanSeed string) bool {
	if !filepath.IsAbs(rawTarget) {
		return false
	}
	for _, part := range strings.Split(rawTarget, string(filepath.Separator)) {
		if part == ".." {
			return false
		}
	}
	cleaned := filepath.Clean(rawTarget)
	return strings.HasPrefix(cleaned, cleanSeed+string(filepath.Separator))
}
