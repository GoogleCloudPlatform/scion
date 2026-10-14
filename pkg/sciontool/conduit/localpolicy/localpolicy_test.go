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

func TestParsePolicy(t *testing.T) {
	p, err := ParsePolicy("")
	if err != nil || p.DisableSSH || p.DisableSSHSocketForwarding || p.PortDenied(9999) {
		t.Errorf("ParsePolicy(\"\") = %+v, %v; want no restriction", p, err)
	}
	p, err = ParsePolicy(`{"disable_ssh":true,"deny_ports":[9999],"disable_ssh_socket_forwarding":true}`)
	if err != nil || !p.DisableSSH || !p.DisableSSHSocketForwarding || !p.PortDenied(9999) || p.PortDenied(8463) {
		t.Errorf("ParsePolicy = %+v, %v", p, err)
	}
}

// TestParsePolicyFailsClosed: a value from a newer hub (unknown field) too.
func TestParsePolicyFailsClosed(t *testing.T) {
	for _, raw := range []string{`{`, `[]`, `null`, `{"allow_ports":[22]}`, `{} {}`, `{"disable_ssh":true}}`} {
		p, err := ParsePolicy(raw)
		if err == nil || !strings.Contains(err.Error(), EnvPolicy) {
			t.Errorf("ParsePolicy(%q) error = %v, want an error naming %s", raw, err, EnvPolicy)
		}
		if !p.DisableSSH || !p.DisableSSHSocketForwarding || !p.PortDenied(22) {
			t.Errorf("ParsePolicy(%q) = %+v, want everything refused", raw, p)
		}
	}
}
