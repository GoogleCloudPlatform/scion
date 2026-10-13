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
package localpolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
// hub sets or removes it on every dispatch, so absence means no restriction.
const EnvPolicy = "SCION_CONDUIT_POLICY"

// PolicyDoc is the JSON form of EnvPolicy.
type PolicyDoc struct {
	DisableSSH  bool     `json:"disable_ssh,omitempty"`
	DenyPorts   []int    `json:"deny_ports,omitempty"`
	DenySockets []string `json:"deny_sockets,omitempty"`
}

// Policy is the operator's conduit restriction; the zero value adds nothing.
type Policy struct {
	SSHDisabled bool
	denyPorts   []int
	denySockets []string
	denyAll     bool
}

// ParsePolicy reads EnvPolicy. An invalid value, including one with unknown
// fields, fails closed: ssh is disabled and every destination denied.
func ParsePolicy(raw string) (Policy, error) {
	if strings.TrimSpace(raw) == "" {
		return Policy{}, nil
	}
	var docp *PolicyDoc
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	err := dec.Decode(&docp)
	if err == nil && docp == nil {
		err = errors.New("not an object")
	}
	if err == nil {
		if _, terr := dec.Token(); terr != io.EOF {
			err = errors.New("trailing data")
		}
	}
	var doc PolicyDoc
	if err == nil {
		doc = *docp
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

// SocketDenied reports whether path is reserved, or equal to or below a
// denied entry, comparing paths as written.
func (p Policy) SocketDenied(path string) bool {
	if p.denyAll || IsReservedSocket(path) {
		return true
	}
	return underAny(path, p.denySockets)
}

// SocketDeniedOnDisk is SocketDenied with symbolic links resolved in both
// path and entries; a path that fails to resolve, other than not existing,
// is denied. A link swapped in after the check is not seen.
func (p Policy) SocketDeniedOnDisk(path string) bool {
	if p.SocketDenied(path) {
		return true
	}
	if len(p.denySockets) == 0 {
		return false
	}
	paths := []string{path}
	switch resolved, err := filepath.EvalSymlinks(path); {
	case err == nil:
		paths = append(paths, resolved)
	case !errors.Is(err, fs.ErrNotExist):
		return true
	}
	entries := slices.Clone(p.denySockets)
	for _, e := range p.denySockets {
		if r, err := filepath.EvalSymlinks(e); err == nil && r != e {
			entries = append(entries, r)
		}
	}
	for _, q := range paths {
		if underAny(q, entries) {
			return true
		}
	}
	return false
}

// underAny reports whether path equals one of entries or lies below it.
func underAny(path string, entries []string) bool {
	for _, e := range entries {
		if e == "/" || path == e || strings.HasPrefix(path, e+"/") {
			return true
		}
	}
	return false
}
