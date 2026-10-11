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

package cmd

// This file is a temporary stand-in for the exit-code table that
// pkg/tunnelclient defines for scion tunnel. It mirrors the names and values
// the ssh and ssh-config commands use, plus the endpoint normalisation, so moving to that package is a rename
// at each call site. Remove this file once pkg/tunnelclient is available.

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Exit codes shared by scion tunnel, scion tunnel status and scion ssh-config.
const (
	ExitOK             = 0
	ExitError          = 1
	ExitUsage          = 2
	ExitCredential     = 3
	ExitAgentNotFound  = 4
	ExitForbidden      = 5
	ExitHubUnavailable = 6
)

// Error is an error carrying the process exit code it maps to.
type Error struct {
	Code int
	Msg  string
}

// Error returns the one-line message.
func (e *Error) Error() string { return e.Msg }

// ExitCode returns the exit code for the error.
func (e *Error) ExitCode() int { return e.Code }

// Errorf returns an *Error with the given exit code and formatted message.
func Errorf(code int, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// CodeOf returns the exit code for err: 0 for nil, the code of an *Error in
// the chain, and 1 for any other error.
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

// NormalizeEndpoint returns the canonical form of a hub endpoint: trimmed,
// with a lower-case scheme and host, no default port and no trailing slash.
func NormalizeEndpoint(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("invalid hub endpoint %q: %w", raw, err)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	switch {
	case port != "":
		u.Host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		u.Host = "[" + host + "]"
	default:
		u.Host = host
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String(), nil
}
