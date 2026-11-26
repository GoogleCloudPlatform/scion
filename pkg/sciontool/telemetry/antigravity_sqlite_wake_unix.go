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
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const agySQLiteWakeWriteTimeout = 50 * time.Millisecond

type unixWakeListener struct {
	conn  *net.UnixConn
	path  string
	owner *OutboxOwner
	done  chan struct{}
}

func (l *unixWakeListener) Close() error {
	err := l.conn.Close()
	<-l.done
	// Re-verify parent dir is still a non-symlink directory owned by the
	// expected user before unlinking so root never unlinks through a swapped
	// symlinked .scion directory.
	if verifyWakeParentDir(filepath.Dir(l.path), l.owner) == nil {
		_ = os.Remove(l.path)
	}
	return err
}

// verifyWakeParentDir checks via Lstat that dir is a real directory (never a
// symlink) and, when running as root with an expected agent owner, is owned by
// owner.UID. Refuses symlinks and unexpected owners so root never binds,
// chowns, or unlinks inside an attacker-redirected directory.
func verifyWakeParentDir(dir string, owner *OutboxOwner) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("wake socket parent %s is a symlink", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("wake socket parent %s is not a directory", dir)
	}
	if owner != nil && os.Geteuid() == 0 && owner.UID > 0 {
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("wake socket parent %s: cannot inspect owner", dir)
		}
		if int(st.Uid) != owner.UID {
			return fmt.Errorf("wake socket parent %s owned by uid %d, want %d", dir, st.Uid, owner.UID)
		}
	}
	return nil
}

// startAgySQLiteWakeListener binds a Unix domain datagram socket at sockPath
// so short-lived `sciontool hook` processes (running as the agent user) can
// send a non-blocking 1-byte wake datagram on PostInvocation without polling
// the filesystem or depending on OTLP export.
//
// Security: validates that the parent `.scion` directory is a real directory
// (not a symlink) owned by the expected agent UID, then binds the socket inside
// a fresh root-owned 0700 staging directory, sets 0600 + Lchown inside that
// private staging directory (no process-wide umask mutation and no post-bind
// chmod symlink race), and atomically renames the socket into place.
func startAgySQLiteWakeListener(sockPath string, owner *OutboxOwner, onWake func()) (io.Closer, error) {
	if sockPath == "" {
		return nil, fmt.Errorf("empty wake socket path")
	}
	dir := filepath.Dir(sockPath)
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("mkdir wake socket dir: %w", err)
		}
		if owner != nil && os.Geteuid() == 0 && owner.UID > 0 {
			_ = os.Lchown(dir, owner.UID, owner.GID)
		}
	}
	if err := verifyWakeParentDir(dir, owner); err != nil {
		return nil, err
	}

	// Stage the socket inside a fresh 0700 directory owned by this process (root
	// in sciontool init) so the agent user cannot traverse or swap anything
	// between ListenUnixgram, Chmod, and Lchown, and without touching the
	// process-wide umask.
	// Use a short prefix so <dir>/.w<digits>/s stays shorter than sockPath
	// itself and well within Linux's 108-byte sun_path limit.
	stageDir, err := os.MkdirTemp(dir, ".w")
	if err != nil {
		return nil, fmt.Errorf("create wake staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(stageDir) }()

	stagedSock := filepath.Join(stageDir, "s")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: stagedSock, Net: "unixgram"})
	if err != nil {
		return nil, fmt.Errorf("listen wake unixgram %s: %w", sockPath, err)
	}
	if err := os.Chmod(stagedSock, 0o600); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("chmod staged wake socket: %w", err)
	}
	if owner != nil && os.Geteuid() == 0 && owner.UID > 0 {
		if err := os.Lchown(stagedSock, owner.UID, owner.GID); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("lchown staged wake socket: %w", err)
		}
	}
	if err := os.Rename(stagedSock, sockPath); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("rename wake socket %s: %w", sockPath, err)
	}

	l := &unixWakeListener{
		conn:  conn,
		path:  sockPath,
		owner: owner,
		done:  make(chan struct{}),
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
