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

package telemetry

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const agySQLiteWakeWriteTimeout = 50 * time.Millisecond

// agyWakeAfterBindHook is a test-only seam invoked immediately after binding
// the staged Unix socket and before fchmodat/fchownat/renameat. Always nil in
// production.
var agyWakeAfterBindHook func(parentDir, stageName string)

type unixWakeListener struct {
	conn     *net.UnixConn
	scionFd  int
	sockBase string
	done     chan struct{}
}

func (l *unixWakeListener) Close() error {
	err := l.conn.Close()
	<-l.done
	if l.scionFd >= 0 {
		_ = unix.Unlinkat(l.scionFd, l.sockBase, 0)
		_ = unix.Close(l.scionFd)
		l.scionFd = -1
	}
	return err
}

// verifyAgyParentDir checks via Lstat that dir is a real directory (never a
// symlink) and, when running as root with an expected agent owner, is owned by
// owner.UID.
func verifyAgyParentDir(dir string, owner *OutboxOwner) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("antigravity parent dir %s is a symlink", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("antigravity parent dir %s is not a directory", dir)
	}
	if owner != nil && os.Geteuid() == 0 && owner.UID > 0 {
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("antigravity parent dir %s: cannot inspect owner", dir)
		}
		if int(st.Uid) != owner.UID {
			return fmt.Errorf("antigravity parent dir %s owned by uid %d, want %d", dir, st.Uid, owner.UID)
		}
	}
	return nil
}

func randomStageName() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return ".w" + hex.EncodeToString(b[:]), nil
}

// startAgySQLiteWakeListener binds a Unix domain datagram socket at sockPath
// so short-lived `sciontool hook` processes (running as the agent user) can
// send a non-blocking 1-byte wake datagram on PostInvocation without polling
// the filesystem or depending on OTLP export.
//
// All staging, permission/ownership changes, rename, and Close cleanup are
// performed relative to pinned directory file descriptors (`O_NOFOLLOW |
// O_DIRECTORY` + `mkdirat` + `/proc/self/fd/<stageFd>/s` + `fchmodat` +
// `fchownat(AT_SYMLINK_NOFOLLOW)` + `renameat` + `unlinkat`) so path swaps in
// the agent-owned parent directory cannot redirect root operations. Fails closed
// if `/proc/self/fd` is unavailable (the tailer's 15s fallback ticker still
// runs).
func startAgySQLiteWakeListener(sockPath string, owner *OutboxOwner, onWake func()) (io.Closer, error) {
	if sockPath == "" {
		return nil, fmt.Errorf("empty wake socket path")
	}
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		return nil, fmt.Errorf("wake socket listener requires /proc/self/fd: %w", err)
	}

	dir := filepath.Dir(sockPath)
	sockBase := filepath.Base(sockPath)
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("mkdir wake socket dir: %w", err)
		}
		if owner != nil && os.Geteuid() == 0 && owner.UID > 0 {
			_ = os.Lchown(dir, owner.UID, owner.GID)
		}
	}

	scionFd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open wake parent dir %s: %w", dir, err)
	}
	keepScionFd := false
	defer func() {
		if !keepScionFd {
			_ = unix.Close(scionFd)
		}
	}()

	var parentStat unix.Stat_t
	if err := unix.Fstat(scionFd, &parentStat); err != nil {
		return nil, fmt.Errorf("fstat wake parent dir %s: %w", dir, err)
	}
	if parentStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, fmt.Errorf("wake socket parent %s is not a directory", dir)
	}
	if owner != nil && os.Geteuid() == 0 && owner.UID > 0 && int(parentStat.Uid) != owner.UID {
		return nil, fmt.Errorf("wake socket parent %s owned by uid %d, want %d", dir, parentStat.Uid, owner.UID)
	}

	var stage string
	for attempt := 0; attempt < 8; attempt++ {
		cand, rerr := randomStageName()
		if rerr != nil {
			return nil, fmt.Errorf("generate wake staging name: %w", rerr)
		}
		if merr := unix.Mkdirat(scionFd, cand, 0o700); merr != nil {
			if errors.Is(merr, unix.EEXIST) {
				continue
			}
			return nil, fmt.Errorf("mkdirat wake staging dir: %w", merr)
		}
		stage = cand
		break
	}
	if stage == "" {
		return nil, fmt.Errorf("mkdirat wake staging dir: exhausted name attempts")
	}

	stageFd, err := unix.Openat(scionFd, stage, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Unlinkat(scionFd, stage, unix.AT_REMOVEDIR)
		return nil, fmt.Errorf("openat wake staging dir: %w", err)
	}
	// Register close before cleanup so LIFO defers unlink before closing stageFd.
	defer func() { _ = unix.Close(stageFd) }()
	defer func() {
		_ = unix.Unlinkat(stageFd, "s", 0)
		_ = unix.Unlinkat(scionFd, stage, unix.AT_REMOVEDIR)
	}()

	var stageStat unix.Stat_t
	if err := unix.Fstat(stageFd, &stageStat); err != nil {
		return nil, fmt.Errorf("fstat wake staging dir: %w", err)
	}
	if int(stageStat.Uid) != os.Geteuid() || stageStat.Mode&0o777 != 0o700 {
		return nil, fmt.Errorf("wake staging dir unexpected owner/mode (uid=%d mode=%#o)", stageStat.Uid, stageStat.Mode&0o777)
	}

	bindPath := fmt.Sprintf("/proc/self/fd/%d/s", stageFd)
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: bindPath, Net: "unixgram"})
	if err != nil {
		return nil, fmt.Errorf("listen wake unixgram %s: %w", sockPath, err)
	}

	if agyWakeAfterBindHook != nil {
		agyWakeAfterBindHook(dir, stage)
	}

	if err := unix.Fchmodat(stageFd, "s", 0o600, 0); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("fchmodat staged wake socket: %w", err)
	}
	if owner != nil && os.Geteuid() == 0 && owner.UID > 0 {
		if err := unix.Fchownat(stageFd, "s", owner.UID, owner.GID, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("fchownat staged wake socket: %w", err)
		}
	}
	if err := unix.Renameat(stageFd, "s", scionFd, sockBase); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("renameat wake socket %s: %w", sockPath, err)
	}

	keepScionFd = true
	l := &unixWakeListener{
		conn:     conn,
		scionFd:  scionFd,
		sockBase: sockBase,
		done:     make(chan struct{}),
	}
	go func() {
		defer close(l.done)
		buf := make([]byte, 16)
		for {
			_, err := conn.Read(buf)
			if err != nil {
				return
			}
			if onWake != nil {
				onWake()
			}
		}
	}()
	return l, nil
}

// NotifyAgySQLiteWake sends a best-effort, non-blocking 1-byte datagram to the
// live Antigravity SQLite tailer's Unix domain socket. Safe to call from
// short-lived `sciontool hook` processes; never blocks or fails the hook if
// the daemon is not listening.
func NotifyAgySQLiteWake(agentHome string) {
	NotifyAgySQLiteWakePath(AgySQLiteWakePath(agentHome))
}

// NotifyAgySQLiteWakePath sends a best-effort 1-byte wake datagram to sockPath.
func NotifyAgySQLiteWakePath(sockPath string) {
	if sockPath == "" {
		return
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetWriteDeadline(time.Now().Add(agySQLiteWakeWriteTimeout))
	_, _ = conn.Write([]byte{1})
}
