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

package conduit

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	core "github.com/GoogleCloudPlatform/scion/pkg/conduit"
)

// requireIPv6Loopback skips the test when the host has no ::1.
func requireIPv6Loopback(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback on this host: %v", err)
	}
	_ = l.Close()
}

// tagListener serves on l: each connection gets tag, then the listener
// closes the connection.
func tagListener(t *testing.T, l net.Listener, tag string) {
	t.Helper()
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte(tag))
			_ = c.Close()
		}
	}()
}

// dualStackListeners listens on 127.0.0.1 and ::1 on the same port.
func dualStackListeners(t *testing.T) (v4, v6 net.Listener) {
	t.Helper()
	for range 20 {
		l4, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := strconv.Itoa(l4.Addr().(*net.TCPAddr).Port)
		l6, err := net.Listen("tcp6", net.JoinHostPort("::1", port))
		if err == nil {
			return l4, l6
		}
		_ = l4.Close()
	}
	t.Fatal("no port free on both 127.0.0.1 and ::1")
	return nil, nil
}

// TestAgentTCPLoopbackFallback: with the grant's logical host 127.0.0.1,
// the target reaches a v4-only, a v6-only and a dual-stack listener; the
// dual-stack one is reached on 127.0.0.1.
func TestAgentTCPLoopbackFallback(t *testing.T) {
	tests := []struct {
		name   string
		listen func(t *testing.T) int
		want   string
	}{
		{"v4 only", func(t *testing.T) int {
			l, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			tagListener(t, l, "v4")
			return l.Addr().(*net.TCPAddr).Port
		}, "v4"},
		{"v6 only", func(t *testing.T) int {
			requireIPv6Loopback(t)
			l, err := net.Listen("tcp6", "[::1]:0")
			if err != nil {
				t.Fatal(err)
			}
			tagListener(t, l, "v6")
			return l.Addr().(*net.TCPAddr).Port
		}, "v6"},
		{"dual stack", func(t *testing.T) int {
			requireIPv6Loopback(t)
			l4, l6 := dualStackListeners(t)
			tagListener(t, l4, "v4")
			tagListener(t, l6, "v6")
			return l4.Addr().(*net.TCPAddr).Port
		}, "v4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			port := tt.listen(t)
			key := newTestKey(t, "k1")
			h := newFakeHub(t, key.public)
			startAgent(t, h, nil)
			s := h.nextSession(t)
			st, err := s.OpenStream(context.Background(), tcpOpen(t, key, s.Info(), "launch-1", port))
			if err != nil {
				t.Fatalf("OpenStream: %v", err)
			}
			defer func() { _ = st.Close() }()
			got, err := io.ReadAll(st)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("reached %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAgentTCPRefusedOnBoth: a port refused on 127.0.0.1 and on ::1 gives
// the existing 4504 upstream_unreachable.
func TestAgentTCPRefusedOnBoth(t *testing.T) {
	l4, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l4.Addr().(*net.TCPAddr).Port
	_ = l4.Close()

	key := newTestKey(t, "k1")
	h := newFakeHub(t, key.public)
	startAgent(t, h, nil)
	s := h.nextSession(t)
	_, err = s.OpenStream(context.Background(), tcpOpen(t, key, s.Info(), "launch-1", port))
	if core.CodeOf(err, 0) != core.CloseRelayTimeout {
		t.Fatalf("OpenStream = %v, want 4504 upstream_unreachable", err)
	}
}

// dialErr builds the error net.Dialer returns for errno.
func dialErr(err error) error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", err)}
}

// TestAgentTCPFallbackRule pins the dial sequence: only a refused
// 127.0.0.1 dial is retried, once, on [::1]; timeouts and other errors are
// not retried.
func TestAgentTCPFallbackRule(t *testing.T) {
	refused := dialErr(syscall.ECONNREFUSED)
	tests := []struct {
		name     string
		v4Err    error
		v6Err    error
		wantDial []string
	}{
		{"refused then v6 refused", refused, refused, []string{"tcp 127.0.0.1:8080", "tcp [::1]:8080"}},
		{"refused then v6 unreachable", refused, dialErr(syscall.EADDRNOTAVAIL), []string{"tcp 127.0.0.1:8080", "tcp [::1]:8080"}},
		{"timeout", &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}, nil, []string{"tcp 127.0.0.1:8080"}},
		{"context deadline", context.DeadlineExceeded, nil, []string{"tcp 127.0.0.1:8080"}},
		{"other error", dialErr(syscall.ENETUNREACH), nil, []string{"tcp 127.0.0.1:8080"}},
		{"plain refused text", errors.New("connection refused"), nil, []string{"tcp 127.0.0.1:8080"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := newTestKey(t, "k1")
			h := newFakeHub(t, key.public)
			var mu sync.Mutex
			var dialed []string
			var deadlines []time.Time
			startAgent(t, h, func(o *Options) {
				o.DialLocal = func(ctx context.Context, network, addr string) (net.Conn, error) {
					mu.Lock()
					defer mu.Unlock()
					dialed = append(dialed, network+" "+addr)
					d, ok := ctx.Deadline()
					if !ok {
						t.Errorf("dial %s: no deadline on ctx", addr)
					}
					deadlines = append(deadlines, d)
					if len(dialed) == 1 {
						return nil, tt.v4Err
					}
					return nil, tt.v6Err
				}
			})
			s := h.nextSession(t)
			_, err := s.OpenStream(context.Background(), tcpOpen(t, key, s.Info(), "launch-1", 8080))
			if core.CodeOf(err, 0) != core.CloseRelayTimeout {
				t.Fatalf("OpenStream = %v, want 4504 upstream_unreachable", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(dialed, tt.wantDial) {
				t.Fatalf("dialed %q, want %q", dialed, tt.wantDial)
			}
			// Both attempts share one dial deadline (the retry gets no
			// fresh budget).
			if len(deadlines) == 2 && !deadlines[0].Equal(deadlines[1]) {
				t.Fatalf("dial deadlines differ: %v then %v", deadlines[0], deadlines[1])
			}
		})
	}
}

// TestAgentTCPReservedPortsBothFamilies: a deny-listed port is refused
// before any dial, so neither 127.0.0.1 nor ::1 is tried, even when the v4
// dial would be refused.
func TestAgentTCPReservedPortsBothFamilies(t *testing.T) {
	for _, port := range ReservedPorts {
		t.Run(strconv.Itoa(port), func(t *testing.T) {
			key := newTestKey(t, "k1")
			h := newFakeHub(t, key.public)
			startAgent(t, h, func(o *Options) {
				o.DialLocal = func(_ context.Context, _, addr string) (net.Conn, error) {
					t.Errorf("DialLocal(%s) called for a reserved port", addr)
					return nil, dialErr(syscall.ECONNREFUSED)
				}
			})
			s := h.nextSession(t)
			_, err := s.OpenStream(context.Background(), tcpOpen(t, key, s.Info(), "launch-1", port))
			if core.CodeOf(err, 0) != core.CloseForbidden {
				t.Fatalf("OpenStream = %v, want 4403", err)
			}
		})
	}
}
