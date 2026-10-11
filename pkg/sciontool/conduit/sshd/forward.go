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

package sshd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/crypto/ssh"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit/localpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// localhostName is the only name a direct-tcpip destination may use. It
// is never resolved: it maps to 127.0.0.1, then ::1 if that is refused.
const localhostName = "localhost"

// Loopback addresses the name localhost maps to, in dial order.
var localhostAddrs = []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()}

// directTCPIPMsg is the direct-tcpip channel's extra data (RFC 4254 §7.2).
type directTCPIPMsg struct {
	Host     string
	Port     uint32
	OrigHost string
	OrigPort uint32
}

// streamlocalMsg is the direct-streamlocal@openssh.com channel's extra
// data (OpenSSH PROTOCOL §2.4).
type streamlocalMsg struct {
	SocketPath string
	Reserved0  string
	Reserved1  uint32
}

// TCPDestination applies the direct-tcpip profile and returns the
// addresses to dial, in order. The host must be a loopback IP literal
// (127.0.0.0/8 or ::1, without a zone) or the name localhost (ASCII
// case-insensitive, no trailing dot), which is never resolved. The port
// must be 1..65535 and not on the shared deny-list.
func TCPDestination(host string, port uint32) ([]netip.AddrPort, error) {
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid port %d", port)
	}
	if localpolicy.IsReservedPort(int(port)) {
		return nil, fmt.Errorf("port %d is reserved", port)
	}
	var addrs []netip.Addr
	if isLocalhostName(host) {
		addrs = localhostAddrs
	} else {
		a, err := netip.ParseAddr(host)
		if err != nil {
			return nil, fmt.Errorf("destination %q is not a loopback IP literal or localhost", host)
		}
		if a.Zone() != "" || a.Is4In6() || !a.IsLoopback() {
			return nil, fmt.Errorf("destination %s is not loopback", host)
		}
		addrs = []netip.Addr{a}
	}
	out := make([]netip.AddrPort, len(addrs))
	for i, a := range addrs {
		out[i] = netip.AddrPortFrom(a, uint16(port))
	}
	return out, nil
}

// isLocalhostName reports whether host is localhost compared
// case-insensitively over ASCII only: exactly nine bytes, with no
// Unicode case folding and no trailing dot.
func isLocalhostName(host string) bool {
	if len(host) != len(localhostName) {
		return false
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != localhostName[i] {
			return false
		}
	}
	return true
}

// handleDirectTCPIP serves a direct-tcpip channel: profile check, dial,
// then accept.
func (c *Config) handleDirectTCPIP(ctx context.Context, nc ssh.NewChannel) {
	var m directTCPIPMsg
	if err := ssh.Unmarshal(nc.ExtraData(), &m); err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, "malformed direct-tcpip request")
		return
	}
	dests, err := TCPDestination(m.Host, m.Port)
	if err != nil {
		log.Debug("Conduit SSH: refused direct-tcpip to %s: %v", net.JoinHostPort(m.Host, strconv.Itoa(int(m.Port))), err)
		_ = nc.Reject(ssh.Prohibited, err.Error())
		return
	}
	conn, err := c.dialLoopback(ctx, dests)
	if err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, "connection failed")
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		_ = conn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	splice(ctx, ch, conn)
}

// dialLoopback dials dests in order, moving to the next only when a
// connection is refused.
func (c *Config) dialLoopback(ctx context.Context, dests []netip.AddrPort) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	var err error
	for _, d := range dests {
		var conn net.Conn
		conn, err = c.DialTCP(ctx, "tcp", d.String())
		if err == nil || !errors.Is(err, syscall.ECONNREFUSED) {
			return conn, err
		}
	}
	return nil, err
}

// SocketDestination applies the direct-streamlocal profile to path: a
// clean absolute path, not on the socket deny-list, naming an existing
// Unix socket in the container's filesystem. Permission to connect is
// decided separately, with the container user's permissions.
func SocketDestination(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) {
		return fmt.Errorf("socket path %q is not a clean absolute path", path)
	}
	if localpolicy.IsReservedSocket(path) {
		return fmt.Errorf("socket %s is reserved", path)
	}
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not an existing unix socket in the container", path)
	}
	return nil
}

// handleStreamlocal serves a direct-streamlocal@openssh.com channel:
// profile check, connect with the container user's permissions, then
// accept. Any failure refuses the channel.
func (c *Config) handleStreamlocal(ctx context.Context, nc ssh.NewChannel) {
	var m streamlocalMsg
	if err := ssh.Unmarshal(nc.ExtraData(), &m); err != nil {
		_ = nc.Reject(ssh.ConnectionFailed, "malformed direct-streamlocal request")
		return
	}
	if err := SocketDestination(m.SocketPath); err != nil {
		log.Debug("Conduit SSH: refused direct-streamlocal: %v", err)
		_ = nc.Reject(ssh.Prohibited, err.Error())
		return
	}
	if c.DialUnix == nil {
		_ = nc.Reject(ssh.Prohibited, "unix socket forwarding is not available")
		return
	}
	ctx2, cancel := context.WithTimeout(ctx, dialTimeout)
	conn, err := c.DialUnix(ctx2, m.SocketPath)
	cancel()
	if err != nil {
		log.Debug("Conduit SSH: direct-streamlocal to %s: %v", m.SocketPath, err)
		_ = nc.Reject(ssh.ConnectionFailed, "connection failed")
		return
	}
	ch, reqs, err := nc.Accept()
	if err != nil {
		_ = conn.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	splice(ctx, ch, conn)
}
