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
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// TestTCPDestinationProfile: direct-tcpip goes only to loopback IP
// literals or the name localhost (never resolved), and never to a port on
// the shared deny-list.
func TestTCPDestinationProfile(t *testing.T) {
	allowed := []struct {
		host string
		want []string
	}{
		{"127.0.0.1", []string{"127.0.0.1:8000"}},
		{"127.0.0.2", []string{"127.0.0.2:8000"}},
		{"127.255.255.254", []string{"127.255.255.254:8000"}},
		{"::1", []string{"[::1]:8000"}},
		{"localhost", []string{"127.0.0.1:8000", "[::1]:8000"}},
		{"LOCALHOST", []string{"127.0.0.1:8000", "[::1]:8000"}},
		{"LocalHost", []string{"127.0.0.1:8000", "[::1]:8000"}},
	}
	for _, tt := range allowed {
		got, err := TCPDestination(tt.host, 8000)
		if err != nil {
			t.Errorf("%s: refused: %v", tt.host, err)
			continue
		}
		if len(got) != len(tt.want) {
			t.Errorf("%s: dial %v, want %v", tt.host, got, tt.want)
			continue
		}
		for i := range got {
			if got[i].String() != tt.want[i] {
				t.Errorf("%s: dial %v, want %v", tt.host, got, tt.want)
			}
		}
	}
	refused := []string{
		"10.0.0.1", "172.17.0.10", "192.168.1.1", "0.0.0.0", "::", "2001:db8::1",
		"::ffff:127.0.0.1", "fe80::1%lo",
		"example.com", "localhost.", "localhost.localdomain", "foo.localhost", "ip6-localhost",
		"127.1", "0x7f000001", "", " localhost",
	}
	for _, host := range refused {
		if got, err := TCPDestination(host, 8000); err == nil {
			t.Errorf("%q: allowed (%v), want refused", host, got)
		}
	}
}

// TestTCPDestinationReservedPorts: the shared deny-list applies to IP
// literals and to localhost after mapping; ports outside 1..65535 are
// refused.
func TestTCPDestinationReservedPorts(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
		for _, port := range []uint32{9810, 18380, 0, 65536} {
			if _, err := TCPDestination(host, port); err == nil {
				t.Errorf("%s:%d allowed, want refused", host, port)
			}
		}
		if _, err := TCPDestination(host, 9811); err != nil {
			t.Errorf("%s:9811 refused: %v", host, err)
		}
	}
}

// TestTCPDestinationNeverAName: every allowed destination is an IP
// address, so nothing is ever resolved.
func TestTCPDestinationNeverAName(t *testing.T) {
	got, err := TCPDestination("localhost", 22)
	if err != nil {
		t.Fatal(err)
	}
	for _, ap := range got {
		if !ap.Addr().IsLoopback() || ap.Addr() == (netip.Addr{}) {
			t.Fatalf("destination %v is not a loopback address", ap)
		}
	}
}

// TestSocketDestinationProfile: direct-streamlocal goes only to a clean
// absolute path naming an existing Unix socket.
func TestSocketDestinationProfile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"", "relative.sock", "/etc/passwd", file, dir, filepath.Join(dir, "missing.sock"),
		dir + "/../" + filepath.Base(dir) + "/plain", dir + "//plain", "/tmp/a\x00b",
	} {
		if err := SocketDestination(path); err == nil {
			t.Errorf("%q allowed, want refused", path)
		}
	}
}

// TestHostKeyPerCall: each NewHostKey is a distinct ed25519 key, and
// AuthorizedKey is a single authorized_keys line.
func TestHostKeyPerCall(t *testing.T) {
	a, err := NewHostKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewHostKey()
	if err != nil {
		t.Fatal(err)
	}
	ka, kb := AuthorizedKey(a), AuthorizedKey(b)
	if ka == kb {
		t.Fatal("two host keys are equal")
	}
	if len(ka) < 20 || ka[:12] != "ssh-ed25519 " {
		t.Fatalf("AuthorizedKey = %q, want an ssh-ed25519 line", ka)
	}
	for _, c := range ka {
		if c == '\n' {
			t.Fatalf("AuthorizedKey %q contains a newline", ka)
		}
	}
}

// TestValidateDefaults: a host key is required; the cap defaults to 32.
func TestValidateDefaults(t *testing.T) {
	if err := (&Config{}).Validate(); err == nil {
		t.Fatal("Validate without a host key succeeded")
	}
	key, err := NewHostKey()
	if err != nil {
		t.Fatal(err)
	}
	c := &Config{HostKey: key}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.MaxChannels != DefaultMaxChannels || DefaultMaxChannels != 32 {
		t.Fatalf("MaxChannels = %d, want 32", c.MaxChannels)
	}
	if c.DialUnix != nil {
		t.Fatal("DialUnix set without a helper: streamlocal must be refused")
	}
}
