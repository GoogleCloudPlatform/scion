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

package startuplistener

import (
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// freePort returns a TCP port that was free a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func envWith(port string) func(string) string {
	return func(k string) string {
		if k == EnvPort {
			return port
		}
		return ""
	}
}

func TestStart_UnsetOpensNothing(t *testing.T) {
	c, err := Start(envWith(""))
	if err != nil || c != nil {
		t.Fatalf("Start with %s unset = %v, %v; want nil, nil", EnvPort, c, err)
	}
}

func TestStart_AcceptsAndClosesConnections(t *testing.T) {
	port := freePort(t)
	c, err := Start(envWith(strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if c == nil {
		t.Fatal("Start returned a nil Closer with the port set")
	}
	defer func() { _ = c.Close() }()

	// More dials than any per-connection state could absorb unnoticed;
	// each must be accepted and then closed by the listener.
	for i := 0; i < 5; i++ {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), 2*time.Second)
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := conn.Read(make([]byte, 1))
		_ = conn.Close()
		if n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("read %d = %d, %v; want 0, EOF (the listener closes at once and writes nothing)", i, n, err)
		}
	}
}

func TestStart_CloseStopsListening(t *testing.T) {
	port := freePort(t)
	c, err := Start(envWith(strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("dial succeeded after Close; want the port closed")
	}
}

func TestStart_InvalidPort(t *testing.T) {
	for _, v := range []string{"abc", "0", "-1", "65536"} {
		c, err := Start(envWith(v))
		if err == nil {
			_ = c.Close()
			t.Errorf("Start(%q) succeeded; want an error", v)
			continue
		}
		if !strings.Contains(err.Error(), EnvPort) {
			t.Errorf("Start(%q) error %q does not name %s", v, err, EnvPort)
		}
	}
}

func TestStart_BindFailureNamesPort(t *testing.T) {
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = busy.Close() }()
	port := strconv.Itoa(busy.Addr().(*net.TCPAddr).Port)

	c, err := Start(envWith(port))
	if err == nil {
		_ = c.Close()
		t.Fatal("Start on a port in use succeeded; want an error")
	}
	if !strings.Contains(err.Error(), "port "+port) {
		t.Errorf("error %q does not name port %s", err, port)
	}
}
