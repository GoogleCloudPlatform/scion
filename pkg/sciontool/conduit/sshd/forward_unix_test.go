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

// TestStreamlocalRefusedWithoutDialer: without a connector for the
// container user, direct-streamlocal is refused, also for an existing
// socket; a path outside the profile is refused the same way.
func TestStreamlocalRefusedWithoutDialer(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	echoServer(t, l)
	client := serve(t, testConfig(t), "scion")
	for _, path := range []string{sock, "/etc/passwd", "relative.sock", filepath.Join(dir, "none.sock")} {
		_, err := client.Dial("unix", path)
		wantOpenRefused(t, err, ssh.Prohibited)
	}
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
