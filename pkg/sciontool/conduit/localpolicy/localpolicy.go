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

// Package localpolicy holds the agent-local destination rules that every
// conduit stream kind applies at the sciontool end (design §3.5, §3.8):
// one port deny-list shared by the TCP stream kind and the SSH server's
// direct-tcpip channels, and a socket deny-list beside it for the SSH
// server's direct-streamlocal channels. Keeping them in one place means
// the stream kinds cannot apply different lists.
//
// Policy adds the operator's restrictions, which the hub sends to the
// agent in EnvPolicy, on top of the fixed reserved lists.
package localpolicy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// ReservedPorts are never a destination, whatever the grant or the SSH
// client asks for: the hub API (9810) and the metadata server (18380).
var ReservedPorts = []int{9810, 18380}

// ReservedSockets are Unix socket paths that are never a
// direct-streamlocal destination. sciontool serves no Unix socket today,
// so the list is empty; a future sciontool socket is added here.
var ReservedSockets = []string{}

// IsReservedPort reports whether port is on the deny-list.
func IsReservedPort(port int) bool { return slices.Contains(ReservedPorts, port) }

// IsReservedSocket reports whether path (already clean and absolute) is
// on the socket deny-list.
func IsReservedSocket(path string) bool { return slices.Contains(ReservedSockets, path) }

// EnvPolicy carries the operator's conduit policy as JSON (PolicyDoc). The
// hub owns the variable: it sets it on dispatch when the policy restricts
// anything and removes any other value, so its absence means no operator
// restriction.
const EnvPolicy = "SCION_CONDUIT_POLICY"

// PolicyDoc is the JSON form of EnvPolicy.
type PolicyDoc struct {
	DisableSSH  bool     `json:"disable_ssh,omitempty"`
	DenyPorts   []int    `json:"deny_ports,omitempty"`
	DenySockets []string `json:"deny_sockets,omitempty"`
}

// Policy is the operator's restriction of what conduit streams may reach
// in this agent. The zero value restricts nothing beyond the reserved
// lists.
type Policy struct {
	// SSHDisabled means the agent serves no ssh streams.
	SSHDisabled bool
	denyPorts   []int
	denySockets []string
	// denyAll refuses every port and socket destination. It is set when
	// EnvPolicy cannot be understood.
	denyAll bool
}

// ParsePolicy reads the value of EnvPolicy. An empty value is the zero
// Policy. A value that is not a valid PolicyDoc (including one with
// fields this sciontool does not know, which a newer hub may send) yields
// a Policy that disables ssh and denies every destination, together with
// the error: an operator restriction is never dropped silently.
func ParsePolicy(raw string) (Policy, error) {
	if strings.TrimSpace(raw) == "" {
		return Policy{}, nil
	}
	var doc PolicyDoc
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	err := dec.Decode(&doc)
	if err == nil && dec.More() {
		err = fmt.Errorf("trailing data")
	}
	if err == nil {
		err = doc.validate()
	}
	if err != nil {
		return Policy{SSHDisabled: true, denyAll: true}, fmt.Errorf("invalid %s: %w", EnvPolicy, err)
	}
	return Policy{
		SSHDisabled: doc.DisableSSH,
		denyPorts:   slices.Clone(doc.DenyPorts),
		denySockets: slices.Clone(doc.DenySockets),
	}, nil
}

func (d PolicyDoc) validate() error {
	for _, p := range d.DenyPorts {
		if p < 1 || p > 65535 {
			return fmt.Errorf("deny_ports: port %d is outside 1-65535", p)
		}
	}
	for _, s := range d.DenySockets {
		if !filepath.IsAbs(s) || filepath.Clean(s) != s || strings.ContainsRune(s, 0) {
			return fmt.Errorf("deny_sockets: %q is not a clean absolute path", s)
		}
	}
	return nil
}

// PortDenied reports whether port is reserved or denied by p.
func (p Policy) PortDenied(port int) bool {
	return p.denyAll || IsReservedPort(port) || slices.Contains(p.denyPorts, port)
}

// SocketDenied reports whether path (clean and absolute) is reserved or
// denied by p: equal to a denied entry or below it.
func (p Policy) SocketDenied(path string) bool {
	if p.denyAll || IsReservedSocket(path) {
		return true
	}
	for _, e := range p.denySockets {
		if e == "/" || path == e || strings.HasPrefix(path, e+"/") {
			return true
		}
	}
	return false
}
