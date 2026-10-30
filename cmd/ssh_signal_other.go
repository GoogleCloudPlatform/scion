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
//go:build !unix

package cmd

import (
	"os"
	"syscall"
)

// sshTerminalSignals are the signals a console sends to every process
// attached to it, so ssh already receives them.
var sshTerminalSignals = []os.Signal{os.Interrupt}

// sshEndSignals are the signals that end scion ssh and its ssh process.
// On Windows, Go reports a console close, logoff or shutdown as SIGTERM;
// there is no hangup signal.
var sshEndSignals = []os.Signal{syscall.SIGTERM}

// forwardSSHSignal ends the ssh process. Windows cannot deliver a signal
// to another process, so it is killed instead.
func forwardSSHSignal(p *os.Process, _ os.Signal) {
	_ = p.Kill()
}
