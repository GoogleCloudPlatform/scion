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
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

// echoServer accepts connections on l and echoes them (test fixture).
func echoServer(t *testing.T, l net.Listener) {
	t.Helper()
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				_, _ = io.Copy(c, c)
			}()
		}
	}()
}

// roundTrip writes msg on c and reads it back.
func roundTrip(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg {
		t.Fatalf("echo %q, want %q", buf, msg)
	}
}

// wantStreamlocalRefusal checks that err is the one streamlocal refusal
// used for every failure after the lexical check.
func wantStreamlocalRefusal(t *testing.T, err error) {
	t.Helper()
	var oe *ssh.OpenChannelError
	if !errors.As(err, &oe) || oe.Reason != ssh.ConnectionFailed || oe.Message != streamlocalRefusal {
		t.Fatalf("open = %v, want %v %q", err, ssh.ConnectionFailed, streamlocalRefusal)
	}
}

// wantOpenRefused checks that err is a channel open failure for reason.
func wantOpenRefused(t *testing.T, err error, reason ssh.RejectionReason) {
	t.Helper()
	var oe *ssh.OpenChannelError
	if !errors.As(err, &oe) || oe.Reason != reason {
		t.Fatalf("open = %v, want refusal %v", err, reason)
	}
}

// recordingDialer records every address DialTCP is called with.
type recordingDialer struct {
	mu    sync.Mutex
	addrs []string
}

func (d *recordingDialer) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	d.mu.Unlock()
	var nd net.Dialer
	return nd.DialContext(ctx, network, addr)
}

// TestDirectTCPIPChannels: loopback literals and localhost are
// forwarded; a non-loopback literal, a hostname and a reserved port are
// refused as administratively prohibited without any dial.
func TestDirectTCPIPChannels(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	echoServer(t, l)
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	d := &recordingDialer{}
	cfg := testConfig(t)
	cfg.DialTCP = d.dial
	client := serve(t, cfg, "scion")

	for _, host := range []string{"127.0.0.1", "localhost", "LOCALHOST"} {
		c, err := client.Dial("tcp", net.JoinHostPort(host, port))
		if err != nil {
			t.Fatalf("direct-tcpip to %s refused: %v", host, err)
		}
		roundTrip(t, c, "hello "+host)
		_ = c.Close()
	}
	for _, addr := range []string{"172.17.0.10:" + port, "10.1.2.3:80", "example.com:80", "localhost.:" + port, "foo.localhost:" + port, "127.0.0.1:9810", "127.0.0.1:18380", "localhost:9810"} {
		_, err := client.Dial("tcp", addr)
		wantOpenRefused(t, err, ssh.Prohibited)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.addrs {
		if !strings.HasPrefix(a, "127.0.0.1:") && !strings.HasPrefix(a, "[::1]:") {
			t.Fatalf("dialed %q: only loopback literals may be dialed", a)
		}
	}
}

// shortTempDir is a temp dir with a path short enough for a Unix socket.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sshd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestStreamlocalChannels: a Unix socket in the container is forwarded
// through the helper. A path that is not clean and absolute is refused as
// prohibited; a missing path, a path that is not a socket and a socket
// without permission all get the same one refusal text.
func TestStreamlocalChannels(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	echoServer(t, l)
	client := serve(t, testConfig(t), "scion")

	c, err := client.Dial("unix", sock)
	if err != nil {
		t.Fatalf("direct-streamlocal refused: %v", err)
	}
	roundTrip(t, c, "via helper")
	_ = c.Close()

	for _, path := range []string{"relative.sock", dir + "/../" + filepath.Base(dir) + "/s.sock", dir + "//s.sock"} {
		_, err := client.Dial("unix", path)
		wantOpenRefused(t, err, ssh.Prohibited)
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/etc/passwd", plain, filepath.Join(dir, "none.sock"), filepath.Join(dir, "sub", "none.sock")} {
		_, err := client.Dial("unix", path)
		wantStreamlocalRefusal(t, err)
	}
	if os.Geteuid() != 0 {
		denied := filepath.Join(dir, "denied.sock")
		dl, err := net.Listen("unix", denied)
		if err != nil {
			t.Fatal(err)
		}
		echoServer(t, dl)
		if err := os.Chmod(denied, 0); err != nil {
			t.Fatal(err)
		}
		_, err = client.Dial("unix", denied)
		wantStreamlocalRefusal(t, err)
	}
}

// TestStreamlocalUsesContainerUserPermissions: a socket the container
// user cannot open is refused. Running as root, the container user is
// nobody (65534): a socket in a root-only directory is refused and one in
// an open directory is forwarded. Otherwise the container user is this
// process's user and a socket without permissions is refused.
func TestStreamlocalUsesContainerUserPermissions(t *testing.T) {
	closed := shortTempDir(t)
	sock := filepath.Join(closed, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	echoServer(t, l)
	cfg := testConfig(t)
	var openSock string
	if os.Geteuid() == 0 {
		cfg.User = User{UID: 65534, GID: 65534}
		cfg.HelperPath = worldExecutableCopy(t, os.Args[0])
		openDir := shortTempDir(t)
		openSock = filepath.Join(openDir, "o.sock")
		ol, err := net.Listen("unix", openSock)
		if err != nil {
			t.Fatal(err)
		}
		echoServer(t, ol)
		for path, mode := range map[string]os.FileMode{closed: 0o700, openDir: 0o755, openSock: 0o777} {
			if err := os.Chmod(path, mode); err != nil {
				t.Fatal(err)
			}
		}
	} else if err := os.Chmod(sock, 0); err != nil {
		t.Fatal(err)
	}
	client := serve(t, cfg, "scion")
	_, err = client.Dial("unix", sock)
	wantStreamlocalRefusal(t, err)
	if openSock != "" {
		c, err := client.Dial("unix", openSock)
		if err != nil {
			t.Fatalf("socket open to the container user refused: %v", err)
		}
		roundTrip(t, c, "as nobody")
		_ = c.Close()
	}
}

// worldExecutableCopy copies the binary at src into a directory every
// user can reach and returns the copy's path.
func worldExecutableCopy(t *testing.T, src string) string {
	t.Helper()
	dir := shortTempDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "helper")
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

// TestStreamlocalHelperFailureRefuses: when the helper cannot run, the
// channel is refused; no connection is made by the server itself.
func TestStreamlocalHelperFailureRefuses(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	echoServer(t, l)
	cfg := testConfig(t)
	cfg.HelperPath = filepath.Join(dir, "no-such-helper")
	client := serve(t, cfg, "scion")
	_, err = client.Dial("unix", sock)
	wantStreamlocalRefusal(t, err)

	// A required privilege drop without a container user refuses too.
	cfg3 := testConfig(t)
	cfg3.User = User{RequirePrivilegeDrop: true}
	client3 := serve(t, cfg3, "scion")
	_, err = client3.Dial("unix", sock)
	wantStreamlocalRefusal(t, err)

	// Without a helper at all, streamlocal is refused outright.
	cfg2 := testConfig(t)
	cfg2.HelperPath = ""
	client2 := serve(t, cfg2, "scion")
	_, err = client2.Dial("unix", sock)
	wantOpenRefused(t, err, ssh.Prohibited)
}

// TestRemoteForwardingRefused: tcpip-forward (-R) and
// streamlocal-forward@openssh.com are refused.
func TestRemoteForwardingRefused(t *testing.T) {
	client := serve(t, testConfig(t), "scion")
	if l, err := client.Listen("tcp", "127.0.0.1:17900"); err == nil {
		_ = l.Close()
		t.Fatal("tcpip-forward accepted")
	}
	if l, err := client.ListenUnix(filepath.Join(shortTempDir(t), "r.sock")); err == nil {
		_ = l.Close()
		t.Fatal("streamlocal-forward accepted")
	}
	ok, _, err := client.SendRequest("cancel-tcpip-forward", true, nil)
	if err != nil || ok {
		t.Fatalf("cancel-tcpip-forward = %v, %v; want refused", ok, err)
	}
}

// TestAgentAndX11ForwardingRefused: auth-agent-req@openssh.com and
// x11-req are refused, and the auth-agent@openssh.com and x11 channel
// types are not served.
func TestAgentAndX11ForwardingRefused(t *testing.T) {
	client := serve(t, testConfig(t), "scion")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	for _, req := range []string{"auth-agent-req@openssh.com", "x11-req", "env"} {
		ok, err := sess.SendRequest(req, true, nil)
		if err != nil || ok {
			t.Fatalf("%s = %v, %v; want refused", req, ok, err)
		}
	}
	for _, typ := range []string{"auth-agent@openssh.com", "x11", "forwarded-tcpip", "forwarded-streamlocal@openssh.com"} {
		_, _, err := client.OpenChannel(typ, nil)
		wantOpenRefused(t, err, ssh.Prohibited)
	}
}

// TestChannelCap: the cap counts every open channel, sessions included;
// the channel over the cap is refused with resource shortage, and a
// closed channel frees its slot.
func TestChannelCap(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	echoServer(t, l)
	for _, tc := range []struct {
		name string
		max  int
		want int
	}{
		{"default", 0, 32},
		{"configured", 3, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			cfg.MaxChannels = tc.max
			client := serve(t, cfg, "scion")
			var open []io.Closer
			for i := 0; i < tc.want; i++ {
				if i%2 == 0 {
					ch, reqs, err := client.OpenChannel("session", nil)
					if err != nil {
						t.Fatalf("channel %d refused: %v", i+1, err)
					}
					go ssh.DiscardRequests(reqs)
					open = append(open, ch)
				} else {
					c, err := client.Dial("tcp", l.Addr().String())
					if err != nil {
						t.Fatalf("channel %d refused: %v", i+1, err)
					}
					open = append(open, c)
				}
			}
			_, _, err := client.OpenChannel("session", nil)
			wantOpenRefused(t, err, ssh.ResourceShortage)
			_, err = client.Dial("tcp", l.Addr().String())
			wantOpenRefused(t, err, ssh.ResourceShortage)

			_ = open[0].Close()
			waitFor(t, "a freed channel slot", func() bool {
				ch, reqs, err := client.OpenChannel("session", nil)
				if err != nil {
					return false
				}
				go ssh.DiscardRequests(reqs)
				open[0] = ch
				return true
			})
			for _, c := range open {
				_ = c.Close()
			}
		})
	}
}
