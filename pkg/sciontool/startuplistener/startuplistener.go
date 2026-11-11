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

// Package startuplistener holds the startup listener: a TCP listener that
// sciontool init opens so a platform TCP startup probe can succeed in an
// agent container that otherwise listens on no port (Cloud Run Instances
// apply a default TCP startup probe and restart an instance that never
// answers it).
//
// The runtime that wants the listener sets EnvPort on the container, and
// configures its startup probe on the same port. Every other runtime leaves
// EnvPort unset, so the listener never opens there.
package startuplistener

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

const (
	// EnvPort names the environment variable that tells sciontool init to
	// open the startup listener, and on which port.
	EnvPort = "SCION_STARTUP_LISTENER_PORT"

	// Port is the port runtimes use for the startup listener. It sits next
	// to the metadata server port (18380) in the range sciontool already
	// uses, away from the ports agents commonly give their own dev servers
	// (3000, 5000, 5173, 8000, 8080, 8888, 9000), and below the Linux
	// ephemeral range, so outbound connections never take it first.
	Port = 18381

	// acceptRetryDelay paces the accept loop after an accept error other
	// than a closed listener (for example running out of file
	// descriptors), so the loop cannot spin.
	acceptRetryDelay = 50 * time.Millisecond
)

// Start opens the startup listener when EnvPort is set (looked up through
// getenv) and returns a Closer that stops it. When EnvPort is unset or empty
// it returns nil, nil and opens nothing.
//
// The listener binds all interfaces, since the platform probe connects from
// outside the container. Each accepted connection is closed at once, in the
// single accept goroutine: nothing is read, written or logged per
// connection, and no goroutine is started per connection.
func Start(getenv func(string) string) (io.Closer, error) {
	raw := getenv(EnvPort)
	if raw == "" {
		return nil, nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("startup listener: invalid %s %q", EnvPort, raw)
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return nil, fmt.Errorf("startup listener: listen on port %d: %w", port, err)
	}
	go acceptAndClose(ln)
	return ln, nil
}

func acceptAndClose(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(acceptRetryDelay)
			continue
		}
		_ = conn.Close()
	}
}
