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
//go:build unix

package cmd

import (
	"os"
	"syscall"
)

// sshTerminalSignals are the signals a terminal sends to its whole
// foreground process group, so ssh already receives them; scion ssh only
// keeps them from ending scion before ssh does.
var sshTerminalSignals = []os.Signal{os.Interrupt, syscall.SIGQUIT}

// sshEndSignals are the signals passed on to ssh: a hangup from a closed
// terminal, or a termination request sent to scion ssh alone, as a wrapper
// or service manager does. ssh stays in scion's process group, which keeps
// it in the terminal's foreground group so it can read the terminal; the
// signal goes to the ssh process itself, and ssh stops its ProxyCommand.
var sshEndSignals = []os.Signal{syscall.SIGHUP, syscall.SIGTERM}

// forwardSSHSignal passes sig on to the ssh process.
func forwardSSHSignal(p *os.Process, sig os.Signal) {
	_ = p.Signal(sig)
}
