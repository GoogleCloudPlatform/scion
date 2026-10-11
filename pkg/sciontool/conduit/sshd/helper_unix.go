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

package sshd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/sys/unix"
)

// HelperCommand is the hidden sciontool subcommand that runs the helpers
// below as the container user: `sciontool <HelperCommand> unix-connect
// <path>` and `sciontool <HelperCommand> sftp-server`.
const HelperCommand = "conduit-ssh-helper"

const (
	helperUnixConnect = "unix-connect"
	helperSFTP        = "sftp-server"
)

// helperStatusFD is the descriptor the unix-connect helper reports on:
// the child end of a socketpair, passed as the first ExtraFiles entry.
const helperStatusFD = 3

// helperStatusMax bounds the helper's status message.
const helperStatusMax = 512

// helperDialer returns a DialUnix that connects with the container
// user's permissions: it runs the unix-connect helper as u, which applies
// the socket profile again, connects, and passes the connected
// descriptor back over a socketpair. Any helper failure is an error; the
// connection is never made by this process itself.
func helperDialer(exe string, u User, env func() ([]string, error)) func(ctx context.Context, path string) (net.Conn, error) {
	return func(ctx context.Context, path string) (net.Conn, error) {
		attr, err := sysProcAttr(u, false)
		if err != nil {
			return nil, err
		}
		environ, err := env()
		if err != nil {
			return nil, fmt.Errorf("environment: %w", err)
		}
		fds, err := cloexecSocketpair()
		if err != nil {
			return nil, err
		}
		parent := os.NewFile(uintptr(fds[0]), "conduit-ssh-helper-parent")
		child := os.NewFile(uintptr(fds[1]), "conduit-ssh-helper-child")
		defer func() { _ = parent.Close() }()

		cmd := exec.CommandContext(ctx, exe, HelperCommand, helperUnixConnect, path)
		cmd.Env = environ
		cmd.Dir = "/"
		cmd.SysProcAttr = attr
		cmd.ExtraFiles = []*os.File{child}
		wait, err := startManaged(cmd)
		_ = child.Close()
		if err != nil {
			return nil, fmt.Errorf("start helper: %w", err)
		}
		conn, rerr := receiveConn(ctx, parent)
		werr := wait()
		if rerr != nil {
			return nil, rerr
		}
		if werr != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("helper: %w", werr)
		}
		return conn, nil
	}
}

// cloexecSocketpair creates a connected AF_UNIX stream pair with
// close-on-exec set on both ends. SOCK_CLOEXEC is not portable, so the
// flag is set under syscall.ForkLock, which keeps a concurrent fork from
// inheriting the descriptors in between.
func cloexecSocketpair() ([2]int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return fds, fmt.Errorf("socketpair: %w", err)
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return fds, nil
}

// receiveConn reads the helper's status byte, message and passed
// descriptor from the socketpair end f.
func receiveConn(ctx context.Context, f *os.File) (net.Conn, error) {
	fc, err := net.FileConn(f)
	if err != nil {
		return nil, err
	}
	defer func() { _ = fc.Close() }()
	uc, ok := fc.(*net.UnixConn)
	if !ok {
		return nil, errors.New("helper channel is not a unix socket")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(dialTimeout)
	}
	_ = uc.SetReadDeadline(deadline)
	buf := make([]byte, helperStatusMax)
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, fmt.Errorf("helper: %w", err)
	}
	var fds []int
	if oobn > 0 {
		msgs, perr := unix.ParseSocketControlMessage(oob[:oobn])
		if perr == nil {
			for i := range msgs {
				got, rerr := unix.ParseUnixRights(&msgs[i])
				if rerr == nil {
					fds = append(fds, got...)
				}
			}
		}
	}
	if n < 1 || buf[0] != 0 || len(fds) != 1 {
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
		msg := "no connection"
		if n > 1 {
			msg = string(buf[1:n])
		}
		return nil, fmt.Errorf("helper: %s", msg)
	}
	sf := os.NewFile(uintptr(fds[0]), "conduit-ssh-streamlocal")
	defer func() { _ = sf.Close() }()
	return net.FileConn(sf)
}

// RunHelper runs a helper named by args (the arguments after
// HelperCommand) and returns the process exit code. It runs as the
// container user, started by this package.
func RunHelper(args []string, stdin io.Reader, stdout io.WriteCloser) int {
	switch {
	case len(args) == 2 && args[0] == helperUnixConnect:
		return runUnixConnect(args[1])
	case len(args) == 1 && args[0] == helperSFTP:
		return runSFTP(stdin, stdout)
	}
	fmt.Fprintln(os.Stderr, "usage: "+HelperCommand+" unix-connect <path> | sftp-server")
	return 2
}

// runUnixConnect applies the socket profile to path, connects with this
// process's (the container user's) permissions, and passes the connected
// descriptor back on helperStatusFD.
func runUnixConnect(path string) int {
	status := os.NewFile(helperStatusFD, "status")
	fail := func(err error) int {
		msg := []byte{1}
		msg = append(msg, err.Error()...)
		if len(msg) > helperStatusMax {
			msg = msg[:helperStatusMax]
		}
		_ = unix.Sendmsg(int(status.Fd()), msg, nil, nil, 0)
		return 1
	}
	if err := SocketDestination(path); err != nil {
		return fail(err)
	}
	conn, err := net.DialTimeout("unix", path, dialTimeout)
	if err != nil {
		return fail(err)
	}
	f, err := conn.(*net.UnixConn).File()
	_ = conn.Close()
	if err != nil {
		return fail(err)
	}
	defer func() { _ = f.Close() }()
	if err := unix.Sendmsg(int(status.Fd()), []byte{0}, unix.UnixRights(int(f.Fd())), nil, 0); err != nil {
		return 1
	}
	return 0
}

// runSFTP serves the sftp protocol on stdin and stdout until the client
// ends the session.
func runSFTP(stdin io.Reader, stdout io.WriteCloser) int {
	srv, err := sftp.NewServer(struct {
		io.Reader
		io.WriteCloser
	}{stdin, stdout})
	if err != nil {
		fmt.Fprintln(os.Stderr, "sftp-server:", err)
		return 1
	}
	if err := srv.Serve(); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "sftp-server:", err)
		return 1
	}
	return 0
}
