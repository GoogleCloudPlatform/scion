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

package localpolicy

import (
	"strings"
	"testing"
)

// TestReservedPortsList pins the deny-list contents (design §3.8).
func TestReservedPortsList(t *testing.T) {
	for _, p := range []int{9810, 18380} {
		if !IsReservedPort(p) {
			t.Errorf("port %d must be reserved", p)
		}
	}
	for _, p := range []int{0, 22, 80, 8080, 9811, 18381, 65535} {
		if IsReservedPort(p) {
			t.Errorf("port %d must not be reserved", p)
		}
	}
	if len(ReservedPorts) != 2 {
		t.Errorf("ReservedPorts = %v, want exactly 9810 and 18380", ReservedPorts)
	}
}

func TestReservedSocketsEmpty(t *testing.T) {
	if IsReservedSocket("/tmp/x.sock") {
		t.Error("no socket is reserved today")
	}
}

// TestParsePolicy: an empty value restricts nothing; a valid document is
// applied as given; anything else disables ssh and denies every
// destination, and is reported.
func TestParsePolicy(t *testing.T) {
	for _, raw := range []string{"", "  "} {
		p, err := ParsePolicy(raw)
		if err != nil || p.SSHDisabled || p.PortDenied(8462) || p.SocketDenied("/run/x.sock") {
			t.Errorf("ParsePolicy(%q) = %+v, %v; want no restriction", raw, p, err)
		}
	}
	p, err := ParsePolicy(`{"disable_ssh":true,"deny_ports":[8462],"deny_sockets":["/run/x.sock","/srv/private"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !p.SSHDisabled {
		t.Error("ssh not disabled")
	}
	for _, port := range []int{8462, 9810, 18380} {
		if !p.PortDenied(port) {
			t.Errorf("port %d allowed", port)
		}
	}
	if p.PortDenied(8463) {
		t.Error("port 8463 denied")
	}
	for _, path := range []string{"/run/x.sock", "/srv/private/a.sock", "/srv/private/d/b.sock"} {
		if !p.SocketDenied(path) {
			t.Errorf("socket %s allowed", path)
		}
	}
	for _, path := range []string{"/run/x.sock2", "/run/y.sock", "/srv/private2/a.sock", "/srv"} {
		if p.SocketDenied(path) {
			t.Errorf("socket %s denied", path)
		}
	}
	all, err := ParsePolicy(`{"deny_sockets":["/"]}`)
	if err != nil || !all.SocketDenied("/tmp/a.sock") || all.PortDenied(22) {
		t.Errorf("deny_sockets [/] = %+v, %v; want every socket and no port denied", all, err)
	}

	for _, raw := range []string{
		`{`,
		`[]`,
		`{"deny_ports":[0]}`,
		`{"deny_ports":[65536]}`,
		`{"deny_sockets":["relative.sock"]}`,
		`{"deny_sockets":["/a/../b"]}`,
		`{"allow_ports":[22]}`,
		`{"disable_ssh":false} {}`,
		`null`,
		`{"disable_ssh":true}}`,
		`{"disable_ssh":true}]`,
	} {
		p, err := ParsePolicy(raw)
		if err == nil || !strings.Contains(err.Error(), EnvPolicy) {
			t.Errorf("ParsePolicy(%q) error = %v, want an error naming %s", raw, err, EnvPolicy)
		}
		if !p.SSHDisabled || !p.PortDenied(22) || !p.SocketDenied("/tmp/a.sock") {
			t.Errorf("ParsePolicy(%q) = %+v, want ssh disabled and everything denied", raw, p)
		}
	}
}
