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

// Package tunnelclient is the client side of user tunnels over a conduit
// session: it dials the hub's conduit endpoint as a user, asks for each
// tunnel with POST /v1/tunnels, accepts the stream the hub opens back and
// copies bytes. It holds the exit-code table shared by the tunnel, ssh and
// ssh-config commands.
package tunnelclient

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Exit codes of scion tunnel, scion tunnel status and scion ssh-config.
// This is the one definition of the table; other commands import it.
const (
	// ExitOK is success or a clean close.
	ExitOK = 0
	// ExitError is any other error.
	ExitError = 1
	// ExitUsage is a usage error (bad flags or arguments).
	ExitUsage = 2
	// ExitCredential is no usable credential, a refused credential, or a
	// changed identity.
	ExitCredential = 3
	// ExitAgentNotFound is an agent (or project) that does not exist or
	// was deleted.
	ExitAgentNotFound = 4
	// ExitForbidden is a refusal by policy, including token scope.
	ExitForbidden = 5
	// ExitHubUnavailable is a hub that cannot be reached or is
	// unavailable: a dial failure or timeout, a hub-level 503 at the
	// dial, or a transient fault that persisted after its one retry.
	ExitHubUnavailable = 6
	// ExitPolicyClosed is a tunnel closed by policy (authorization
	// expired).
	ExitPolicyClosed = 7
	// ExitCapReached is a session or stream cap reached.
	ExitCapReached = 8
	// ExitAgentNotRunning is an agent that is stopped or suspended.
	ExitAgentNotRunning = 9
	// ExitTargetUnavailable is a target that cannot be reached while the
	// hub can: its broker or its session is not connected.
	ExitTargetUnavailable = 10
	// ExitNoTunnelSupport is a hub without tunnels: a 404 without
	// agent_not_found, an unknown-RPC response, or the conduit endpoint
	// refused or absent.
	ExitNoTunnelSupport = 11
	// ExitInterrupted is an interrupt (SIGINT).
	ExitInterrupted = 130
	// ExitTerminated is a termination request (SIGTERM).
	ExitTerminated = 143
)

// Error is a failure with its exit code. Msg is one line.
type Error struct {
	Code int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// ExitCode returns the process exit status for the failure.
func (e *Error) ExitCode() int { return e.Code }

// Errorf returns an *Error with code and a formatted message.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// CodeOf returns the exit code for err: 0 for nil, the code of an *Error
// anywhere in its chain, otherwise ExitError.
func CodeOf(err error) int {
	if err == nil {
		return ExitOK
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ExitError
}

// SignalExit returns the exit code for a process ended by sig: 130 for an
// interrupt, 143 for a termination request, otherwise ExitError.
func SignalExit(sig os.Signal) int {
	switch sig {
	case os.Interrupt:
		return ExitInterrupted
	case syscall.SIGTERM:
		return ExitTerminated
	}
	return ExitError
}
