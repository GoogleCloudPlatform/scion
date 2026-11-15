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

package commands

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
)

// captureHarnessPane runs tmux capture-pane on the harness window. init
// runs as root while the tmux server belongs to the agent user, whose
// socket the client finds only under that user's uid, so the client runs
// as the agent user when uid and gid are set.
func captureHarnessPane(ctx context.Context, uid, gid int) ([]byte, error) {
	// init runs as root: resolve tmux from the trusted image directories,
	// never the ambient PATH, and give it a fresh environment.
	tmuxPath, err := rootexec.Resolve("tmux")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, tmuxPath, "capture-pane", "-p", "-J",
		"-t", harnessPaneTarget, "-S", "-"+strconv.Itoa(harnessPaneCaptureLines))
	var extra []string
	if dir, ok := os.LookupEnv("TMUX_TMPDIR"); ok {
		extra = append(extra, "TMUX_TMPDIR="+dir)
	}
	cmd.Env = rootexec.Env(extra...)
	if uid > 0 && gid > 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: suppgroups.Credential(uint32(uid), uint32(gid))}
	}
	// init is PID 1 with procreap's SIGCHLD reaper running: the child must
	// be registered with it, or the reaper can steal its exit status.
	return procreap.OutputManaged(cmd)
}
