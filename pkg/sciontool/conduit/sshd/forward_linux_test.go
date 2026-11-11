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

//go:build linux

package sshd

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/pkg/sftp"
	"golang.org/x/sys/unix"
)

// peerPIDs accepts connections on l, records each connecting process's
// pid (SO_PEERCRED) on the returned channel, and echoes.
func peerPIDs(t *testing.T, l *net.UnixListener) <-chan int32 {
	t.Helper()
	t.Cleanup(func() { _ = l.Close() })
	pids := make(chan int32, 8)
	go func() {
		for {
			c, err := l.AcceptUnix()
			if err != nil {
				return
			}
			raw, err := c.SyscallConn()
			if err == nil {
				_ = raw.Control(func(fd uintptr) {
					if cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); err == nil {
						pids <- cred.Pid
					}
				})
			}
			go func() {
				defer func() { _ = c.Close() }()
				buf := make([]byte, 64)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					if _, err := c.Write(buf[:n]); err != nil {
						return
					}
				}
			}()
		}
	}()
	return pids
}

// TestStreamlocalConnectionMadeByHelper: a forwarded Unix socket
// connection is made by the helper process (with the container user's
// permissions), never by the server process itself.
func TestStreamlocalConnectionMadeByHelper(t *testing.T) {
	sock := filepath.Join(shortTempDir(t), "s.sock")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	pids := peerPIDs(t, l)
	client := serve(t, testConfig(t), "scion")
	c, err := client.Dial("unix", sock)
	if err != nil {
		t.Fatalf("direct-streamlocal refused: %v", err)
	}
	defer func() { _ = c.Close() }()
	roundTrip(t, c, "peer")
	pid := <-pids
	if int(pid) == os.Getpid() {
		t.Fatalf("the connection was made by the server process (pid %d)", pid)
	}
	select {
	case extra := <-pids:
		t.Fatalf("a second connection (pid %d) reached the socket", extra)
	default:
	}
}

// TestSFTPRunsOutsideServerProcess: the sftp server is a separate process
// (it reads its own pid through /proc/self).
func TestSFTPRunsOutsideServerProcess(t *testing.T) {
	client := serve(t, testConfig(t), "scion")
	sc, err := sftp.NewClient(client)
	if err != nil {
		t.Fatalf("sftp: %v", err)
	}
	defer func() { _ = sc.Close() }()
	self, err := sc.ReadLink("/proc/self")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(filepath.Base(self))
	if err != nil {
		t.Fatalf("readlink /proc/self = %q", self)
	}
	if pid == os.Getpid() {
		t.Fatal("the sftp server runs in the server process")
	}
}
