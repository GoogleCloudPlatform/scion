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

package liveness

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
)

const tmuxAttachedTimeout = 2 * time.Second

// TmuxAttachedChecker returns a CheckAttached function that queries
// `tmux list-clients -t <session>` as the agent user (uid/gid) and reports
// true when at least one terminal client is attached.
//
// sciontool init runs as PID 1 (root) while the tmux server belongs to the
// agent user, whose socket the client finds only under that user's uid. This
// mirrors captureHarnessPane (cmd/sciontool/commands/harness_pane_tail_unix.go):
// resolve tmux from trusted image directories via rootexec.Resolve, construct a
// fresh environment via rootexec.Env (forwarding TMUX_TMPDIR when set), drop to
// the agent user's credential when uid > 0 && gid > 0, and run via procreap.
func TmuxAttachedChecker(session string, uid, gid int) func() bool {
	return tmuxAttachedCheckerOnSocket(session, "", uid, gid)
}

func tmuxAttachedCheckerOnSocket(session, socket string, uid, gid int) func() bool {
	if session == "" {
		session = "scion"
	}
	return func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), tmuxAttachedTimeout)
		defer cancel()
		cmd, err := buildTmuxListClientsCmd(ctx, session, socket, uid, gid)
		if err != nil {
			return false
		}
		out, err := procreap.OutputManaged(cmd)
		if err != nil {
			return false
		}
		return strings.TrimSpace(string(out)) != ""
	}
}

func buildTmuxListClientsCmd(ctx context.Context, session, socket string, uid, gid int) (*exec.Cmd, error) {
	tmuxPath, err := rootexec.Resolve("tmux")
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, 5)
	if socket != "" {
		args = append(args, "-L", socket)
	}
	args = append(args, "list-clients", "-t", session)
	cmd := exec.CommandContext(ctx, tmuxPath, args...)
	var extra []string
	if dir, ok := os.LookupEnv("TMUX_TMPDIR"); ok {
		extra = append(extra, "TMUX_TMPDIR="+dir)
	}
	cmd.Env = rootexec.Env(extra...)
	if uid > 0 && gid > 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Credential: suppgroups.Credential(uint32(uid), uint32(gid)),
		}
	}
	return cmd, nil
}
