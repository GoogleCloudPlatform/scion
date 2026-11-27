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

// Package wakesock provides a security-hardened Unix domain datagram wake
// socket for short-lived agent-UID hook processes to notify long-lived
// root-run sciontool init loops without blocking on locks or network I/O.
package wakesock

// Owner identifies the non-root agent UID/GID that should own the wake socket
// when the listener is created by root (euid == 0).
type Owner struct {
	UID int
	GID int
}

// Options configures optional listener behavior.
type Options struct {
	// Owner, when non-nil and UID > 0 while running as root, enforces parent
	// directory ownership and chowns the staged socket before publishing it.
	Owner *Owner

	// AfterBindHook is a test-only seam invoked immediately after binding the
	// staged Unix socket and before fchmodat/fchownat/renameat.
	AfterBindHook func(parentDir, stageName string)
}
