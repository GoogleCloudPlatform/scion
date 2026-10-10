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

// This file's own user/credential resolution is independent of
// pkg/runtime.ExecAsUserCmd's equivalent for other scion runtimes
// (pkg/runtime/exec_user.go) rather than importing it: pkg/runtime
// transitively imports pkg/config, and sciontool must never import
// pkg/config (project path resolution has no business inside an agent
// container — see TestInitProjectDataIsolation in
// cmd/sciontool/commands/init_test.go, which fails the build if that
// boundary is crossed). Only the CA-bundle var names
// (substrateenv.TrustBundleVarNames) are actually shared, via a small
// dependency-free package both sides can import (see that package's doc
// comment).
package substrate

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
	"github.com/GoogleCloudPlatform/scion/pkg/substrateenv"
)

// execCandidateEnv is runExec's source of the pre-exec environment this
// process itself was started with, as a package var so a test can supply a
// synthetic environment without mutating the real process environment via
// os.Setenv. Used only to read the CA-bundle candidate values (see
// trustBundleEnvPairs) — runExec's own child environment is otherwise built
// from scratch (rootexec.Env), never from this.
var execCandidateEnv = os.Environ

// execResolve is runExec's own source of a bare command name's verified,
// absolute path — a package var (rather than calling rootexec.Resolve
// directly) so a test can substitute a deterministic stand-in for "sh"
// without needing a real recorder binary to sit under one of
// rootexec.SearchPath's fixed, non-test-injectable directories. Production
// code never reassigns it.
var execResolve = rootexec.Resolve

// SetExecResolveForTest overrides runExec's own means of resolving "sh" to
// an absolute path, for the duration of a test — including a test in
// another package that drives a real exec through this package's Server via
// NewServer (e.g. pkg/runtime's own real-control-server tests), which has
// no other way to reach this package's unexported execResolve var. Mirrors
// SetPrivateRootTmpDirForTest's shape exactly. Production code never calls
// this. Returns a cleanup function that restores the previous value.
func SetExecResolveForTest(resolve func(name string) (string, error)) func() {
	orig := execResolve
	execResolve = resolve
	return func() { execResolve = orig }
}

// execUserLookup is runExec's own source of a target user's passwd entry —
// a package var (rather than calling user.Lookup directly) so a test can
// substitute a synthetic user without depending on a real system account
// existing on whatever machine runs the test. Production code never
// reassigns it.
var execUserLookup = user.Lookup

// SetExecUserLookupForTest overrides runExec's own means of resolving a
// target username to a *user.User, for the duration of a test. Mirrors
// SetExecResolveForTest's shape exactly. Production code never calls this.
// Returns a cleanup function that restores the previous value.
func SetExecUserLookupForTest(lookup func(username string) (*user.User, error)) func() {
	orig := execUserLookup
	execUserLookup = lookup
	return func() { execUserLookup = orig }
}

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

// checkExecHomeDir verifies that homeDir, the exec user's passwd home
// directory, names an existing directory. runExec runs the child there
// (cmd.Dir) and fails the exec when it is missing rather than falling back
// to the control server's own working directory, which belongs to a root
// process and is no place to run a workload command.
func checkExecHomeDir(username, homeDir string) error {
	if homeDir == "" {
		return fmt.Errorf("substrate: user %q has no home directory; refusing to exec", username)
	}
	info, err := os.Stat(homeDir)
	if err != nil {
		return fmt.Errorf("substrate: home directory %q of user %q is not available: %w", homeDir, username, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("substrate: home directory %q of user %q is not a directory", homeDir, username)
	}
	return nil
}

// trustBundleEnvPairs returns "NAME=value" for each CA-bundle candidate
// present with a non-empty value in env, in substrateenv.TrustBundleVarNames's
// fixed order — the actual values a from-scratch child environment
// (rootexec.Env, which carries nothing over from the ambient environment by
// design) must be given explicitly: buildActorTemplate
// (pkg/runtime/substrate_template.go) sets these on the container when
// egress_trust_bundle is configured, and without passing them through here
// explicitly, a direct credential drop's child would simply never see them
// (unlike a bare cmd.Run() that happened to inherit this process's own
// environment, which runExec deliberately does not do either).
func trustBundleEnvPairs(env []string) []string {
	set := make(map[string]string, len(env))
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 {
			set[kv[:i]] = kv[i+1:]
		}
	}
	var pairs []string
	for _, name := range substrateenv.TrustBundleVarNames {
		if v := set[name]; v != "" {
			pairs = append(pairs, name+"="+v)
		}
	}
	return pairs
}
