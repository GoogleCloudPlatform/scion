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

package control

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/tmuxdeliver"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
)

// runnerPassEnv are the variables of this process a delivery command
// inherits: what tmux needs to find the server socket. Everything else
// (notably credentials) stays out of its environment.
var runnerPassEnv = []string{"PATH", "TMUX_TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TZ"}

// errNoPrivilegeDrop: the route would run as sciontool's own identity
// while a privilege drop is required.
var errNoPrivilegeDrop = errors.New("control: privilege drop required but no agent user is set")

// NewLocalRunner returns the deliver-message Runner: it runs argv directly
// (no shell is added; the only sh is tmuxdeliver's constant readiness
// probe) as the user that owns the harness's tmux server, the same identity
// conduit PTY clients use (uid, gid and username as in conduit.PTYUser). It
// refuses to run as sciontool's own identity when requirePrivilegeDrop is
// set and no agent user is known. Standard error is discarded, so no
// command output reaches an error or a log.
func NewLocalRunner(uid, gid int, username string, requirePrivilegeDrop bool) tmuxdeliver.Runner {
	drop := uid > 0 && gid > 0
	return tmuxdeliver.RunnerFunc(func(ctx context.Context, argv []string, stdin io.Reader) (string, error) {
		if requirePrivilegeDrop && !drop {
			return "", errNoPrivilegeDrop
		}
		if len(argv) == 0 {
			return "", errors.New("control: empty argv")
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = runnerEnv(username)
		cmd.Dir = "/"
		if stdin != nil {
			cmd.Stdin = stdin
		}
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.SysProcAttr = &syscall.SysProcAttr{}
		if drop {
			cmd.SysProcAttr.Credential = suppgroups.Credential(uint32(uid), uint32(gid))
		}
		if err := procreap.RunManaged(cmd); err != nil {
			// The error names the command's exit only, never its output.
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return "", errors.New("control: delivery command failed: " + ee.String())
			}
			return "", errors.New("control: delivery command could not run")
		}
		return out.String(), nil
	})
}

func runnerEnv(username string) []string {
	var env []string
	for _, k := range runnerPassEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if username != "" {
		env = append(env, "HOME=/home/"+username, "USER="+username, "LOGNAME="+username)
	} else if home, ok := os.LookupEnv("HOME"); ok {
		env = append(env, "HOME="+home)
	}
	return env
}
