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

// Package sshd is the agent's embedded SSH server (design §3.8). It is
// built directly on golang.org/x/crypto/ssh and serves exactly one SSH
// connection per conduit SSH stream: the caller has already verified the
// stream's grant, so the connection needs no client authentication, and
// every session runs as the container user whatever user name the client
// sends. The package never opens a listening socket.
//
// The forwarding profile (A3) is enforced here: direct-tcpip only to
// loopback IP literals or the name localhost, direct-streamlocal only to
// Unix sockets in the container (connected with the container user's
// permissions), the shared port deny-list, and a per-connection channel
// cap. Remote forwarding, agent forwarding and X11 are refused.
package sshd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// DefaultMaxChannels is the default per-connection channel cap. It counts
// every open channel, sessions included (design §3.8).
const DefaultMaxChannels = 32

// Channel types this server accepts.
const (
	chanSession     = "session"
	chanDirectTCPIP = "direct-tcpip"
	chanStreamlocal = "direct-streamlocal@openssh.com"
)

// User is who every session, exec, sftp server and streamlocal connection
// runs as: the container user. A zero UID or GID means this process's
// identity.
type User struct {
	UID, GID int
	// Username sets HOME (/home/<name>), USER and LOGNAME.
	Username string
	// RequirePrivilegeDrop refuses to run anything as this process's
	// identity (UID or GID not positive).
	RequirePrivilegeDrop bool
}

// drops reports whether children switch to u's credentials.
func (u User) drops() bool { return u.UID > 0 && u.GID > 0 }

// Home is u's home directory, or "" for this process's own HOME.
func (u User) Home() string {
	if u.Username == "" {
		return ""
	}
	return "/home/" + u.Username
}

// Config configures the server. HostKey is required; everything else has
// a default.
type Config struct {
	// HostKey is this agent incarnation's host key.
	HostKey ssh.Signer
	// User is the container user.
	User User
	// MaxChannels caps the open channels of one connection
	// (DefaultMaxChannels when 0 or less).
	MaxChannels int
	// Env returns the environment of sessions, exec commands and helpers:
	// the agent's normal container environment, as the harness process
	// gets it (sciontool passes the supervisor's ChildEnv). An error
	// refuses the request. Default: DefaultEnv(User).
	Env func() ([]string, error)
	// Shell is the login shell (default: User's shell in /etc/passwd,
	// else /bin/bash if present, else /bin/sh).
	Shell string
	// HelperPath is the sciontool binary that runs the sftp server and
	// the streamlocal connector as User. Empty: the sftp subsystem and
	// direct-streamlocal are refused.
	HelperPath string
	// DialTCP dials a loopback destination (test seam; default
	// net.Dialer). It is only ever called with a loopback IP literal.
	DialTCP func(ctx context.Context, network, addr string) (net.Conn, error)
	// DialUnix connects to a Unix socket with User's permissions (test
	// seam; default: the helper at HelperPath). It is only ever called
	// with a path that passed the profile.
	DialUnix func(ctx context.Context, path string) (net.Conn, error)
}

// NewHostKey generates an ed25519 host key.
func NewHostKey() (ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sshd: generate host key: %w", err)
	}
	return ssh.NewSignerFromKey(priv)
}

// AuthorizedKey returns s's public key as an authorized_keys line without
// the trailing newline (the form served as the agent's sshHostKey).
func AuthorizedKey(s ssh.Signer) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
}

// Validate checks c and fills in defaults.
func (c *Config) Validate() error {
	if c.HostKey == nil {
		return errors.New("sshd: a host key is required")
	}
	if c.MaxChannels <= 0 {
		c.MaxChannels = DefaultMaxChannels
	}
	if c.Shell == "" {
		c.Shell = loginShell(passwdFile, c.User)
	}
	if c.Env == nil {
		u := c.User
		c.Env = func() ([]string, error) { return DefaultEnv(u), nil }
	}
	if c.DialTCP == nil {
		d := &net.Dialer{Timeout: dialTimeout}
		c.DialTCP = d.DialContext
	}
	if c.DialUnix == nil && c.HelperPath != "" {
		c.DialUnix = helperDialer(c.HelperPath, c.User, c.Env)
	}
	return nil
}

// dialTimeout bounds a forwarded channel's local connection.
const dialTimeout = 10 * time.Second

// Serve runs one SSH connection over conn until the client disconnects or
// ctx ends, then closes conn. Every channel's processes get their hangup
// before Serve returns. Validate must have succeeded.
func (c *Config) Serve(ctx context.Context, conn net.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	defer func() { _ = conn.Close() }()

	sc := &ssh.ServerConfig{NoClientAuth: true, ServerVersion: "SSH-2.0-scion"}
	sc.AddHostKey(c.HostKey)
	sconn, chans, reqs, err := ssh.NewServerConn(conn, sc)
	if err != nil {
		return fmt.Errorf("sshd: handshake: %w", err)
	}
	// The user name is not used: everything runs as the container user.
	log.Debug("Conduit SSH: connection established (client user %q runs as the container user)", sconn.User())
	go refuseGlobalRequests(reqs)

	cs := &connState{cfg: c}
	var wg sync.WaitGroup
	for nc := range chans {
		h, reason, msg := cs.route(nc)
		if h == nil {
			log.Debug("Conduit SSH: refused %s channel: %s", nc.ChannelType(), msg)
			_ = nc.Reject(reason, msg)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cs.release()
			h(ctx, nc)
		}()
	}
	cancel()
	wg.Wait()
	return nil
}

// refuseGlobalRequests answers every global request with failure:
// tcpip-forward, streamlocal-forward@openssh.com and their cancels (no
// remote forwarding), and anything else.
func refuseGlobalRequests(reqs <-chan *ssh.Request) {
	for r := range reqs {
		if r.Type != "keepalive@openssh.com" {
			log.Debug("Conduit SSH: refused global request %q", r.Type)
		}
		if r.WantReply {
			_ = r.Reply(false, nil)
		}
	}
}

// connState tracks one connection's open channels against the cap.
type connState struct {
	cfg  *Config
	mu   sync.Mutex
	open int
}

type channelHandler func(ctx context.Context, nc ssh.NewChannel)

// route picks the handler for nc and reserves a channel slot, or returns
// the refusal. Channel types other than session, direct-tcpip and
// direct-streamlocal@openssh.com (x11, auth-agent@openssh.com, ...) are
// refused before the cap is consulted.
func (cs *connState) route(nc ssh.NewChannel) (channelHandler, ssh.RejectionReason, string) {
	var h channelHandler
	switch nc.ChannelType() {
	case chanSession:
		h = cs.cfg.handleSession
	case chanDirectTCPIP:
		h = cs.cfg.handleDirectTCPIP
	case chanStreamlocal:
		h = cs.cfg.handleStreamlocal
	default:
		return nil, ssh.Prohibited, fmt.Sprintf("channel type %q is not served", nc.ChannelType())
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.open >= cs.cfg.MaxChannels {
		return nil, ssh.ResourceShortage, fmt.Sprintf("channel limit (%d) reached", cs.cfg.MaxChannels)
	}
	cs.open++
	return h, 0, ""
}

func (cs *connState) release() {
	cs.mu.Lock()
	cs.open--
	cs.mu.Unlock()
}

// splice copies both ways between an accepted channel and a local
// connection, propagating half-closes, until both directions finish or
// ctx ends.
func splice(ctx context.Context, ch ssh.Channel, conn net.Conn) {
	var once sync.Once
	closeBoth := func() {
		once.Do(func() {
			_ = conn.Close()
			_ = ch.Close()
		})
	}
	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ch, conn)
		_ = ch.CloseWrite()
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, ch)
		if hc, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = hc.CloseWrite()
		}
	}()
	wg.Wait()
	closeBoth()
}
