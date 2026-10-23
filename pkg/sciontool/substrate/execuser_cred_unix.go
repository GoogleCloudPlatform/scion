//go:build unix

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

package substrate

import (
	"fmt"
	"os"
	"strconv"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
)

// execUserCredential resolves user's passwd entry and returns the
// environment pairs (HOME, USER, LOGNAME, SHELL) and the *syscall.Credential
// runExec's child process needs to run as that user directly — a credential
// drop via SysProcAttr.Credential, never `su`: su's own login-shell
// environment reset is exactly the kind of PAM/login-shell dependency this
// mechanism is replacing, and the workload's own main process (RunInit,
// via pkg/sciontool/supervisor.Supervisor.Run) already runs under the same
// kind of direct Credential drop with no PAM involved at all — matching
// that is the requirement, not re-deriving su's own behavior.
//
// shPath is runExec's own resolved "sh" binary, used here as SHELL's value:
// os/user.User carries no shell field (the standard library does not parse
// it out of the passwd entry), and the command this credential ultimately
// runs is always invoked directly via "sh -c" regardless of the target
// user's own configured login shell, so SHELL names the interpreter
// actually in use rather than guessing (or hardcoding) the passwd-configured
// one.
//
// homeDir is the user's passwd home directory, which runExec also uses as
// the child's working directory (see checkExecHomeDir).
//
// A user that resolves to uid 0 is refused outright: /exec only ever runs
// the agent workload as an unprivileged user, so a passwd entry (or a
// test/image misconfiguration) mapping the target name to root must never
// yield a root child, whether through a credential or through the
// same-identity shortcut below. A primary gid of 0 is refused the same way
// and in the same position (before that shortcut): a non-root uid running
// with the root group as its primary group could still write any
// group-writable root-group file.
//
// cred is nil when user resolves to this process's own current identity
// (euid/egid already match) — the shape a test that asks to run as its own
// real user takes, and also a defensive no-op in case this handler is ever
// reached while already running as the target (it is not, in production:
// substrate-serve is root until RunInit's own later drop, and runExec's
// caller already refuses any user other than "scion" — see server.go). This
// is not merely an optimization: Go's exec implementation calls setgroups()
// whenever Credential is non-nil UNLESS NoSetGroups is set, which requires
// CAP_SETGID even to set an unchanged group list — a real privilege an
// unprivileged test process (asking to "become" the user it already is)
// does not have and must not need.
func execUserCredential(username, shPath string) (envPairs []string, homeDir string, cred *syscall.Credential, err error) {
	u, err := execUserLookup(username)
	if err != nil {
		return nil, "", nil, fmt.Errorf("substrate: resolve user %q: %w", username, err)
	}
	uid64, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, "", nil, fmt.Errorf("substrate: user %q has an unparseable uid %q: %w", username, u.Uid, err)
	}
	if uid64 == 0 {
		return nil, "", nil, fmt.Errorf("substrate: user %q resolves to uid 0; refusing to exec as root", username)
	}
	gid64, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, "", nil, fmt.Errorf("substrate: user %q has an unparseable gid %q: %w", username, u.Gid, err)
	}
	if gid64 == 0 {
		return nil, "", nil, fmt.Errorf("substrate: user %q has primary gid 0; refusing to exec with the root group", username)
	}
	uid, gid := uint32(uid64), uint32(gid64)

	envPairs = []string{
		"HOME=" + u.HomeDir,
		"USER=" + u.Username,
		"LOGNAME=" + u.Username,
		"SHELL=" + shPath,
	}

	if uid == uint32(os.Geteuid()) && gid == uint32(os.Getegid()) {
		return envPairs, u.HomeDir, nil, nil
	}
	// Keeps the runtime-granted nfs shared-dir groups (ptone/scion#3155);
	// an empty set when none were granted, as before.
	return envPairs, u.HomeDir, suppgroups.Credential(uid, gid), nil
}
