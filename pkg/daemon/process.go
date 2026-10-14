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

package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// ProcessArgs returns the command line of the process with the given PID.
// On Linux it reads /proc/<pid>/cmdline, which gives the exact argv. On other
// systems (macOS) it runs "ps -ww -o args= -p <pid>" and splits the output on
// whitespace, so an argument containing spaces comes back split. It returns
// an error when the command line cannot be read or is empty (for example a
// process that has exited, or a kernel thread).
func ProcessArgs(pid int) ([]string, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("invalid PID %d", pid)
	}
	var args []string
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		if err != nil {
			return nil, fmt.Errorf("failed to read command line of PID %d: %w", pid, err)
		}
		for _, arg := range bytes.Split(bytes.TrimRight(data, "\x00"), []byte{0}) {
			args = append(args, string(arg))
		}
	} else {
		out, err := exec.Command("ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
		if err != nil {
			return nil, fmt.Errorf("failed to read command line of PID %d: %w", pid, err)
		}
		args = strings.Fields(string(out))
	}
	if len(args) == 0 || args[0] == "" {
		return nil, fmt.Errorf("empty command line for PID %d", pid)
	}
	return args, nil
}

// MatchesCommand reports whether args is a run of the given executable with
// the given leading arguments: the base name of args[0] must equal the base
// name of executable, and args[1:] must start with wantArgs. The base name is
// compared, not the full path, so a binary reached through a different path
// (for example after a package upgrade) still matches.
func MatchesCommand(args []string, executable string, wantArgs ...string) bool {
	if len(args) < 1+len(wantArgs) || executable == "" {
		return false
	}
	if filepath.Base(args[0]) != filepath.Base(executable) {
		return false
	}
	for i, want := range wantArgs {
		if args[1+i] != want {
			return false
		}
	}
	return true
}

// IsProcessCommand reports whether the process with the given PID is running
// executable with the given leading arguments (see MatchesCommand). It
// returns false and an error when the command line cannot be read, so callers
// can refuse to act on a process they cannot identify.
func IsProcessCommand(pid int, executable string, wantArgs ...string) (bool, error) {
	if executable == "" {
		return false, errors.New("no executable to compare against")
	}
	args, err := ProcessArgs(pid)
	if err != nil {
		return false, err
	}
	return MatchesCommand(args, executable, wantArgs...), nil
}
