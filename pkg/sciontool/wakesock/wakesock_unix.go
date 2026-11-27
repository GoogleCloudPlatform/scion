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

package wakesock

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const notifyWriteTimeout = 50 * time.Millisecond

type unixListener struct {
	conn     *net.UnixConn
	parentFd int
	sockBase string
	done     chan struct{}
}

func (l *unixListener) Close() error {
	err := l.conn.Close()
	<-l.done
	if l.parentFd >= 0 {
		_ = unix.Unlinkat(l.parentFd, l.sockBase, 0)
		_ = unix.Close(l.parentFd)
		l.parentFd = -1
	}
	return err
}

func randomStageName() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return ".w" + hex.EncodeToString(b[:]), nil
}

// Listen binds a Unix domain datagram socket at sockPath and invokes onByte
// with the first byte of every received datagram.
//
// All staging, permission/ownership changes, rename, and Close cleanup are
// performed relative to pinned directory file descriptors (`O_NOFOLLOW |
// O_DIRECTORY` + `mkdirat` + `/proc/self/fd/<stageFd>/s` + `fchmodat` +
// `fchownat(AT_SYMLINK_NOFOLLOW)` + `renameat` + `unlinkat`) so path swaps in
// the agent-owned parent directory cannot redirect root operations. Fails
// closed if `/proc/self/fd` is unavailable.
func Listen(sockPath string, opts Options, onByte func(byte)) (io.Closer, error) {
	if sockPath == "" {
		return nil, fmt.Errorf("empty wake socket path")
	}
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		return nil, fmt.Errorf("wake socket listener requires /proc/self/fd: %w", err)
	}

	owner := opts.Owner
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

	parentFd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open wake parent dir %s: %w", dir, err)
	}
	keepParentFd := false
	defer func() {
		if !keepParentFd {
			_ = unix.Close(parentFd)
		}
	}()

	var parentStat unix.Stat_t
	if err := unix.Fstat(parentFd, &parentStat); err != nil {
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
		if merr := unix.Mkdirat(parentFd, cand, 0o700); merr != nil {
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

	stageFd, err := unix.Openat(parentFd, stage, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = unix.Unlinkat(parentFd, stage, unix.AT_REMOVEDIR)
		return nil, fmt.Errorf("openat wake staging dir: %w", err)
	}
	// Register close before cleanup so LIFO defers unlink before closing stageFd.
	defer func() { _ = unix.Close(stageFd) }()
	defer func() {
		_ = unix.Unlinkat(stageFd, "s", 0)
		_ = unix.Unlinkat(parentFd, stage, unix.AT_REMOVEDIR)
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

	if opts.AfterBindHook != nil {
		opts.AfterBindHook(dir, stage)
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
	if err := unix.Renameat(stageFd, "s", parentFd, sockBase); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("renameat wake socket %s: %w", sockPath, err)
	}

	keepParentFd = true
	l := &unixListener{
		conn:     conn,
		parentFd: parentFd,
		sockBase: sockBase,
		done:     make(chan struct{}),
	}
	go func() {
		defer close(l.done)
		buf := make([]byte, 16)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			if n > 0 && onByte != nil {
				onByte(buf[0])
			}
		}
	}()
	return l, nil
}

// Notify sends a best-effort, non-blocking 1-byte wake datagram to sockPath.
// Safe to call from short-lived hook processes; never blocks or fails if the
// listener is absent.
func Notify(sockPath string, b byte) {
	if sockPath == "" {
		return
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetWriteDeadline(time.Now().Add(notifyWriteTimeout))
	_, _ = conn.Write([]byte{b})
}
