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

//go:build !linux

package delegation

import (
	"errors"
	"fmt"
	"net"
	"os"
)

// listenSocket creates the delegation socket at cfg.path, mode 0600 and
// owned by cfg.uid:cfg.gid. Agent containers run Linux (socket_linux.go);
// this form, for local development builds, has no peer-user check.
func listenSocket(cfg socketConfig) (net.Listener, error) {
	if cfg.path == "" {
		return nil, errors.New("no socket path configured")
	}
	if fi, err := os.Lstat(cfg.path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", cfg.path)
		}
		if err := os.Remove(cfg.path); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", cfg.path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.path, socketMode); err != nil {
		_ = ln.Close()
		return nil, err
	}
	if cfg.uid > 0 {
		if err := os.Lchown(cfg.path, cfg.uid, cfg.gid); err != nil {
			_ = ln.Close()
			return nil, err
		}
	}
	return ln, nil
}

// removeSocket is a no-op: the listener removes its socket when closed.
func removeSocket(socketConfig, net.Listener) error { return nil }
