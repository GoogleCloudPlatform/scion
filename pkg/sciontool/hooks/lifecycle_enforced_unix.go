//go:build unix

/*
Copyright 2025 The Scion Authors.
*/

package hooks

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
)

// buildEnforcedCmd builds the *exec.Cmd executeScriptEnforced runs, without
// running it — split out purely so a test can assert on the constructed
// Credential/Env/Dir directly (none of which requires any privilege to
// inspect) without needing CAP_SETUID/CAP_SETGID to exercise the "dropped"
// branch, or a root-owned fixture to exercise the "as root" branch.
//
// asRoot is DecideExecAsRoot's own result for this script — the only input
// this function trusts to pick a branch; it does not re-derive or
// second-guess it. eventName picks the root branch's environment: see
// hardenedRootHookEnv's doc comment for why every event except pre-start
// gets a hardened env instead of hookEnv's workload-owned HOME.
//
// SCION_HOOK_PATH is set to the script's own path (displayPath) on both
// branches: $0, as the shebang interpreter sees it, is
// "/proc/self/fd/<n>" (execViaFd's own fexecve-equivalent construction), not
// this path — a script relying on `dirname "$0"` would otherwise silently
// break only on substrate. See §8.1 of the substrate runtime design doc.
//
// Returns an error only for the cases that must fail closed rather than
// silently fall back to running as root: the harness-provision wrapper
// (below) or, on the non-asRoot path, any caller — asked for a dropped
// credential but no valid workload uid/gid is on hand to drop to.
func (m *LifecycleManager) buildEnforcedCmd(scriptFile *os.File, path, eventName string, asRoot bool) (*exec.Cmd, error) {
	cmd, err := execViaFd(scriptFile, path)
	if err != nil {
		return nil, err
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if asRoot {
		if eventName == EventPreStart && filepath.Base(path) == HarnessProvisionHookFilename {
			// The wrapper's own root ownership only proves it is the
			// genuine, broker-delivered file — not that what it execs is
			// safe to run as root. `sciontool harness provision` and the
			// harness's own provisioner script both read and write $HOME
			// and /workspace, the same workload-controlled paths that make
			// every OTHER root-owned script in this branch dangerous to run
			// after the workload has had any chance to touch them (see the
			// "any project/hub pre-start hook" case just below, which keeps
			// running as root). So this one wrapper — identified by its
			// fixed, well-known name, not by anything the workload
			// influences — is run under the workload's own identity even
			// though DecideExecAsRoot classified it asRoot: the fd-anchored
			// open above still proves it is the untampered staged file, but
			// the process that runs from that fd never holds root. This
			// closes the same hole whether $HOME is freshly cloned or,
			// under a future resume/re-bootstrap over a persisted $HOME
			// (see .design/kubernetes/substrate-runtime.md §11), already
			// workload-written: the provisioner never gets root privilege
			// to matter either way.
			return m.buildDroppedProvisionCmd(cmd, path)
		}
		// Every other root-eligible hook — pre-start (in practice, a
		// project/hub-owned script staged alongside the provisioner) included
		// — gets the same hardened, allowlisted environment
		// hardenedRootHookEnv builds: HOME=/root, never AgentHome, which is
		// workload-writable the instant a re-bootstrap runs a pre-start hook
		// over an already-workload-touched $HOME (see
		// .design/kubernetes/substrate-runtime.md §11). A hook that
		// genuinely needs to find what was staged under AgentHome reads
		// SCION_AGENT_HOME instead of relying on HOME for it. Unlike the
		// provisioner above, there is no fixed-name carve-out here: every
		// other pre-start hook runs as root under this same hardened
		// environment regardless of whether $HOME has ever been
		// workload-touched.
		cmd.Env = m.hardenedRootHookEnv()
		// Never inherit init's cwd. Nothing in sciontool ever chdirs, so that
		// cwd is whatever the image sets (e.g. Dockerfile WORKDIR
		// /workspace) — the workload's own, workload-writable git workspace,
		// not root's. Many interpreters and tools resolve code relative to
		// cwd (python3 -c/-m puts '' first on sys.path, node -e uses
		// ./node_modules, make reads ./Makefile, dotenv loaders read ./.env):
		// left unset, a root hook that merely runs one of those tools would
		// load workload-planted content from cwd, the same escalation class
		// HOME=/root above exists to close. A hook that genuinely needs the
		// workspace must cd there explicitly.
		cmd.Dir = "/"
		cmd.Env = setEnvVar(cmd.Env, "SCION_HOOK_PATH", path)
		return cmd, nil
	}

	if m.WorkloadUID <= 0 || m.WorkloadGID <= 0 {
		return nil, fmt.Errorf("hooks: enforced mode requires a valid workload uid/gid (uid=%d gid=%d); refusing to run %s", m.WorkloadUID, m.WorkloadGID, path)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Keeps the runtime-granted nfs shared-dir groups (ptone/scion#3155),
		// so a hook writing into a shared dir behaves like the harness.
		Credential: suppgroups.Credential(uint32(m.WorkloadUID), uint32(m.WorkloadGID)),
	}
	cmd.Env = m.droppedHookEnv()
	cmd.Env = setEnvVar(cmd.Env, "SCION_HOOK_PATH", path)
	if m.WorkloadWorkingDir != "" {
		cmd.Dir = m.WorkloadWorkingDir
	}
	return cmd, nil
}

// buildDroppedProvisionCmd finishes building cmd (already opened via the
// fd-anchored, symlink-safe path buildEnforcedCmd's caller established) for
// the one root-eligible pre-start script this package runs under the
// workload's own identity instead of root: the harness-provision wrapper.
// See buildEnforcedCmd's own call site for why.
//
// Resolves the credential to drop to exactly the way the rest of this
// package's dropped branch does — m.WorkloadUID/WorkloadGID, the same
// setupHostUser-resolved target identity RunInit threads into every other
// dropped hook and into the harness child process itself — rather than
// re-deriving or looking up a uid/gid here. Supplementary groups are set
// explicitly by suppgroups.Credential: only the runtime-granted nfs
// shared-dir groups, otherwise an empty, non-nil slice, rather than left to
// Go's own default handling of a nil Groups field.
//
// Fails closed — refusing to run the script at all, never falling back to
// running it as root — when no valid (>0) workload uid is available. In
// practice this can only happen if a caller constructs a LifecycleManager
// with EnforcePrivilegeDrop set but never resolves WorkloadUID/WorkloadGID:
// RunInit's own construction always calls requirePrivilegeDropOrFail first,
// which already refuses to reach this code at all under those conditions.
// This check exists so that guarantee does not have to be re-verified by
// inspection at every call site — the one thing this function must never do
// is let a missing/zero workload identity silently degrade into running the
// provisioner as root.
func (m *LifecycleManager) buildDroppedProvisionCmd(cmd *exec.Cmd, path string) (*exec.Cmd, error) {
	if m.WorkloadUID <= 0 || m.WorkloadGID <= 0 {
		return nil, fmt.Errorf(
			"hooks: refusing to run %s as root and no valid workload uid/gid is available to drop to (uid=%d gid=%d)",
			path, m.WorkloadUID, m.WorkloadGID)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// Groups is the runtime-granted nfs shared-dir groups
		// (ptone/scion#3155), or an empty, non-nil slice when there are none.
		Credential: suppgroups.Credential(uint32(m.WorkloadUID), uint32(m.WorkloadGID)),
	}
	cmd.Env = setEnvVar(m.droppedHookEnv(), "PYTHONNOUSERSITE", "1")
	cmd.Env = setEnvVar(cmd.Env, "SCION_HOOK_PATH", path)
	return cmd, nil
}
