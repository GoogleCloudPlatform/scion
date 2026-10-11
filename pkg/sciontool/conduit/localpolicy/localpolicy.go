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

import "slices"

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
