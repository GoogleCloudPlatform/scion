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

package delegation

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// listenSocket creates the delegation socket at cfg.path, mode 0600 and
// owned by cfg.uid:cfg.gid, and returns a listener that accepts only peers
// running as cfg.uid or as this process's effective user.
//
// sciontool init runs as root while the socket's directory belongs to the
// agent user, so nothing here follows a symlink: the directory is opened
// without following symlinks, the socket is bound through that directory's
// descriptor, and its mode and owner are set through an O_PATH descriptor
// of the socket itself, after checking that it is a socket.
func listenSocket(cfg socketConfig) (net.Listener, error) {
	if cfg.path == "" {
		return nil, errors.New("no socket path configured")
	}
	d, err := dirfd.EnsureDirNoFollow(filepath.Dir(cfg.path), 0o700)
	if err != nil {
		return nil, fmt.Errorf("open socket directory: %w", err)
	}
	defer func() { _ = d.Close() }()
	dfd := int(d.Fd())
	name := filepath.Base(cfg.path)

	if err := removeSocketAt(dfd, name, nil); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", "/proc/self/fd/"+strconv.Itoa(dfd)+"/"+name)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", cfg.path, err)
	}
	// The bound name refers to a descriptor closed below; Close removes
	// the socket by inode instead (removeSocket).
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	st, err := secureSocketAt(dfd, name, cfg.uid, cfg.gid)
	if err != nil {
		_ = ln.Close()
		_ = removeSocketAt(dfd, name, nil)
		return nil, fmt.Errorf("secure %s: %w", cfg.path, err)
	}
	return &peerListener{
		Listener: ln,
		allowed:  allowedPeers(cfg.uid),
		dev:      st.Dev,
		ino:      st.Ino,
	}, nil
}

// secureSocketAt sets the socket name in dfd to mode 0600 and, when
// uid > 0, to owner uid:gid, through an O_PATH descriptor of the socket,
// and returns its final status.
func secureSocketAt(dfd int, name string, uid, gid int) (unix.Stat_t, error) {
	var st unix.Stat_t
	fd, err := unix.Openat(dfd, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return st, err
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Fstat(fd, &st); err != nil {
		return st, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return st, errors.New("not a socket")
	}
	// chmod through the descriptor's /proc entry: chmod has no
	// descriptor form for an O_PATH descriptor.
	if err := unix.Chmod("/proc/self/fd/"+strconv.Itoa(fd), socketMode); err != nil {
		return st, fmt.Errorf("chmod: %w", err)
	}
	if uid > 0 {
		if err := unix.Fchownat(fd, "", uid, gid, unix.AT_EMPTY_PATH); err != nil {
			return st, fmt.Errorf("chown: %w", err)
		}
	}
	if err := unix.Fstat(fd, &st); err != nil {
		return st, err
	}
	if st.Mode&0o777 != socketMode {
		return st, fmt.Errorf("mode is %o, not %o", st.Mode&0o777, socketMode)
	}
	if uid > 0 && int(st.Uid) != uid {
		return st, fmt.Errorf("owner is %d, not %d", st.Uid, uid)
	}
	return st, nil
}

// removeSocketAt removes the socket name in dfd. It refuses an entry that
// is not a socket, and with want set removes only that socket.
func removeSocketAt(dfd int, name string, want *peerListener) error {
	var st unix.Stat_t
	err := unix.Fstatat(dfd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", name, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return fmt.Errorf("%s exists and is not a socket", name)
	}
	if want != nil && (st.Dev != want.dev || st.Ino != want.ino) {
		// Replaced by another socket since; not ours to remove.
		return nil
	}
	if err := unix.Unlinkat(dfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove %s: %w", name, err)
	}
	return nil
}

// removeSocket removes the socket ln was created as, if it is still there.
func removeSocket(cfg socketConfig, ln net.Listener) error {
	pl, ok := ln.(*peerListener)
	if !ok {
		return nil
	}
	dfd, leaf, err := dirfd.OpenParentNoFollow(cfg.path)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dfd) }()
	return removeSocketAt(dfd, leaf, pl)
}

// peerListener accepts only connections whose peer runs as an allowed user
// (SO_PEERCRED).
type peerListener struct {
	net.Listener
	allowed  map[uint32]bool
	dev, ino uint64
}

// allowedPeers is this process's effective user and, when uid > 0, uid.
func allowedPeers(uid int) map[uint32]bool {
	allowed := map[uint32]bool{uint32(os.Geteuid()): true}
	if uid > 0 {
		allowed[uint32(uid)] = true
	}
	return allowed
}

// Accept returns the next connection from an allowed peer, closing any
// other.
func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(c)
		if err == nil && l.allowed[uid] {
			return c, nil
		}
		if err != nil {
			log.Info("Delegation helper: refused a connection whose peer could not be identified: %v", err)
		} else {
			log.Info("Delegation helper: refused a connection from uid %d", uid)
		}
		_ = c.Close()
	}
}

// peerUID returns the user ID of the process at the other end of c.
func peerUID(c net.Conn) (uint32, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errors.New("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var (
		cred *unix.Ucred
		cerr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if cerr != nil {
		return 0, cerr
	}
	return cred.Uid, nil
}
