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
	"sync"
	"syscall"
	"time"
)

const agySQLiteWakeWriteTimeout = 50 * time.Millisecond

var wakeUmaskMu sync.Mutex

type unixWakeListener struct {
	conn *net.UnixConn
	path string
	done chan struct{}
}

func (l *unixWakeListener) Close() error {
	err := l.conn.Close()
	<-l.done
	_ = os.Remove(l.path)
	return err
}

// startAgySQLiteWakeListener binds a Unix domain datagram socket at sockPath
// so short-lived `sciontool hook` processes (running as the agent user) can
// send a non-blocking 1-byte wake datagram on PostInvocation without polling
// the filesystem or depending on OTLP export.
func startAgySQLiteWakeListener(sockPath string, owner *OutboxOwner, onWake func()) (io.Closer, error) {
	if sockPath == "" {
		return nil, fmt.Errorf("empty wake socket path")
	}
	dir := filepath.Dir(sockPath)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("mkdir wake socket dir: %w", err)
		}
		if owner != nil && os.Geteuid() == 0 && owner.UID > 0 {
			_ = os.Lchown(dir, owner.UID, owner.GID)
		}
	}
	_ = os.Remove(sockPath)

	addr := &net.UnixAddr{Name: sockPath, Net: "unixgram"}
	// Tighten umask around bind so the socket inode is created 0600 atomically,
	// avoiding any post-bind chmod symlink race in the agent-writable .scion dir.
	wakeUmaskMu.Lock()
	oldUmask := syscall.Umask(0o177)
	conn, err := net.ListenUnixgram("unixgram", addr)
	syscall.Umask(oldUmask)
	wakeUmaskMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("listen wake unixgram %s: %w", sockPath, err)
	}
	if owner != nil && os.Geteuid() == 0 && owner.UID > 0 {
		_ = os.Lchown(sockPath, owner.UID, owner.GID)
	}

	l := &unixWakeListener{
		conn: conn,
		path: sockPath,
		done: make(chan struct{}),
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
