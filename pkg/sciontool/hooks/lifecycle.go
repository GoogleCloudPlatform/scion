/*
Copyright 2025 The Scion Authors.
*/

package hooks

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
)

// harnessProvisionHookFilename must stay equal to
// pkg/harness.HarnessProvisionHookFilename, the name writeHookWrapper
// actually stages. Duplicated here, as a plain string, rather than imported:
// pkg/harness's own test package imports pkg/sciontool/hooks (to exercise
// project-hook staging against a real LifecycleManager), so the reverse
// import this package would otherwise need creates a cycle. The two are
// covered by TestHarnessProvisionHookFilenameMatchesWriter, which fails if
// they ever diverge.
const harnessProvisionHookFilename = "20-harness-provision"

// LifecycleManager handles Scion lifecycle hooks.
// These are container-level events managed by sciontool init.
type LifecycleManager struct {
	// HooksDirs is the ordered list of directories containing hook scripts.
	// Discovery walks each directory in order; per-agent staged hooks (e.g.
	// $HOME/.scion/hooks for container-script harnesses) are appended after
	// the system default and run after the system hooks.
	HooksDirs []string

	// Handlers are the registered handlers for lifecycle events.
	Handlers map[string][]Handler

	// AgentHome overrides the HOME environment variable for hook scripts.
	// Init runs as root (HOME=/root) but hook scripts (especially the
	// container-script harness provisioner) need HOME to point at the
	// scion user's home directory where the harness bundle is staged.
	AgentHome string

	// EnforcePrivilegeDrop selects privilege-drop-enforced mode (set from
	// InitRunOptions.RequirePrivilegeDrop — substrate-serve only). false
	// (the zero value) runs every hook script via the calling process's
	// own credentials, on every other runtime.
	//
	// true switches executeScript to the fstat-based root/drop decision
	// (DecideExecAsRoot): a script runs as root only if it and every
	// directory in its chain up to "/" are root-owned and not group- or
	// world-writable; otherwise it runs dropped to WorkloadUID/WorkloadGID.
	// This applies to every event, not just pre-start — see
	// DecideExecAsRoot's doc comment for why there is no per-event
	// exception.
	EnforcePrivilegeDrop bool

	// WorkloadUID and WorkloadGID are the uid/gid a dropped hook script
	// runs as in enforced mode — the same target identity RunInit resolves
	// for the harness child process itself (setupHostUser's targetUID/
	// targetGID). Ignored when EnforcePrivilegeDrop is false.
	WorkloadUID int
	WorkloadGID int

	// WorkloadUsername is the workload account name (always "scion" for
	// every current caller — the same literal harnessSupervisorConfig
	// passes to supervisor.Config.Username), used to set HOME/USER/LOGNAME
	// for a dropped hook exactly like the harness process gets them
	// (supervisor.Supervisor.Run). Ignored when EnforcePrivilegeDrop is
	// false.
	WorkloadUsername string

	// WorkloadWorkingDir is the working directory a dropped hook script
	// runs in, matching the harness child's own resolved cwd
	// (InitRunOptions.WorkingDir / ResolveWorkingDir, threaded into
	// supervisor.Config.WorkingDir). Empty leaves the dropped hook's cwd
	// unset, inheriting init's own — RunInit sets this only after the
	// working directory has actually been resolved, so a pre-start hook
	// (which runs before that resolution) will see it empty even in
	// enforced mode; every later event (post-start, pre-stop, session-end)
	// runs after it has been set. Ignored when EnforcePrivilegeDrop is
	// false.
	WorkloadWorkingDir string
}

// NewLifecycleManager creates a new lifecycle manager.
//
// The default discovery list is built from $SCION_HOOKS_DIR (colon-separated;
// each entry is honored in order) and falls back to /etc/scion/hooks. Callers
// can append per-agent hook directories with AddHooksDir.
func NewLifecycleManager() *LifecycleManager {
	return &LifecycleManager{
		HooksDirs: defaultHooksDirs(),
		Handlers:  make(map[string][]Handler),
	}
}

func defaultHooksDirs() []string {
	if envDir := os.Getenv("SCION_HOOKS_DIR"); envDir != "" {
		var dirs []string
		for _, d := range strings.Split(envDir, ":") {
			d = strings.TrimSpace(d)
			if d != "" {
				dirs = append(dirs, d)
			}
		}
		if len(dirs) > 0 {
			return dirs
		}
	}
	return []string{"/etc/scion/hooks"}
}

// AddHooksDir appends a hooks directory to the discovery list. Per-agent
// hook directories (e.g. $HOME/.scion/hooks for container-script harnesses)
// are appended after the system default so system hooks run first. Duplicate
// paths are ignored.
func (m *LifecycleManager) AddHooksDir(dir string) {
	if dir == "" {
		return
	}
	for _, d := range m.HooksDirs {
		if d == dir {
			return
		}
	}
	m.HooksDirs = append(m.HooksDirs, dir)
}

// RegisterHandler adds a handler for a lifecycle event.
func (m *LifecycleManager) RegisterHandler(eventName string, handler Handler) {
	m.Handlers[eventName] = append(m.Handlers[eventName], handler)
}

// RunPreStart executes pre-start lifecycle hooks.
// Called after container setup but before the child process starts.
func (m *LifecycleManager) RunPreStart() error {
	event := &Event{
		Name: EventPreStart,
	}
	return m.runHooks(event)
}

// RunPostStart executes post-start lifecycle hooks.
// Called after the child process is confirmed running.
func (m *LifecycleManager) RunPostStart() error {
	event := &Event{
		Name: EventPostStart,
	}
	return m.runHooks(event)
}

// RunPreStop executes pre-stop lifecycle hooks.
// Called when a termination signal (SIGTERM/SIGINT) is received,
// before starting the graceful shutdown process.
func (m *LifecycleManager) RunPreStop() error {
	event := &Event{
		Name: EventPreStop,
	}
	return m.runHooks(event)
}

// RunSessionEnd executes session-end lifecycle hooks.
// Called on graceful shutdown before child termination.
func (m *LifecycleManager) RunSessionEnd() error {
	event := &Event{
		Name: EventSessionEnd,
	}
	return m.runHooks(event)
}

// runHooks executes both script-based and registered handlers for an event.
func (m *LifecycleManager) runHooks(event *Event) error {
	var errs []string

	// Run script-based hooks
	if err := m.runScriptHooks(event.Name); err != nil {
		errs = append(errs, fmt.Sprintf("script hooks: %v", err))
	}

	// Run registered handlers
	if handlers, ok := m.Handlers[event.Name]; ok {
		for _, handler := range handlers {
			if err := handler(event); err != nil {
				errs = append(errs, fmt.Sprintf("handler: %v", err))
			}
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("hook errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// runScriptHooks looks for and executes script files across all configured
// hooks directories in order. Within each directory, single-file hooks
// (eventName, eventName.sh) execute before the corresponding eventName.d/
// directory entries (sorted alphabetically).
func (m *LifecycleManager) runScriptHooks(eventName string) error {
	for _, dir := range m.HooksDirs {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}

		// Single-file hooks (legacy form)
		patterns := []string{
			filepath.Join(dir, eventName),
			filepath.Join(dir, eventName+".sh"),
		}
		for _, pattern := range patterns {
			if info, err := os.Stat(pattern); err == nil && !info.IsDir() {
				if err := m.executeScript(pattern, eventName); err != nil {
					if m.skipRefusedEntry(dir, pattern, err) {
						continue
					}
					return fmt.Errorf("script %s: %w", pattern, err)
				}
			}
		}

		// .d directory with multiple scripts
		dirPath := filepath.Join(dir, eventName+".d")
		if info, err := os.Stat(dirPath); err == nil && info.IsDir() {
			entries, err := os.ReadDir(dirPath)
			if err != nil {
				return fmt.Errorf("reading hooks dir %s: %w", dirPath, err)
			}
			// os.ReadDir already returns entries sorted by name, but be
			// explicit: stable lexical order matters because scripts use the
			// numeric prefix convention (10-foo, 20-harness-provision).
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].Name() < entries[j].Name()
			})
			for _, entry := range entries {
				if entry.IsDir() {
					continue
				}
				scriptPath := filepath.Join(dirPath, entry.Name())
				if err := m.executeScript(scriptPath, eventName); err != nil {
					if m.skipRefusedEntry(dir, scriptPath, err) {
						continue
					}
					return fmt.Errorf("script %s: %w", scriptPath, err)
				}
			}
		}
	}

	return nil
}

// skipRefusedEntry reports whether a script's error should be logged and
// skipped — letting runScriptHooks move on to the next entry — rather than
// aborting every remaining hook for the event.
//
// This applies only in enforced mode, only to ErrScriptRefused (the leaf
// itself is a symlink or not a regular file — never a directory-chain
// problem, a permission error, or anything else, all of which still hard
// fail), and never to hooksDir == EnforcedHooksDir: content there is
// broker-delivered and root-owned, so a symlink or device node appearing
// there is a real anomaly worth aborting over, not workload nuisance. Every
// OTHER hooks dir (most notably $HOME/.scion/hooks) can have a workload-
// planted symlink or non-regular entry at any time — DecideExecAsRoot would
// drop it anyway, so refusing to run it is already correct; the only
// question is whether that refusal should also silently DoS every later
// hook for the same event. It should not: a workload can otherwise trivially
// disable its own remaining post-start/pre-stop/session-end hooks (or any
// hub-scoped hook sharing the same directory) by planting one broken entry
// ahead of them. hooksDir and EnforcedHooksDir are compared after
// filepath.Clean on both sides, so a spelling difference such as a trailing
// slash cannot turn the EnforcedHooksDir hard-fail into a skip.
func (m *LifecycleManager) skipRefusedEntry(hooksDir, scriptPath string, err error) bool {
	if !m.EnforcePrivilegeDrop || filepath.Clean(hooksDir) == filepath.Clean(EnforcedHooksDir) {
		return false
	}
	if !errors.Is(err, ErrScriptRefused) {
		return false
	}
	fmt.Fprintf(os.Stderr, "[sciontool] Warning: hook script %s refused (symlink or not a regular file); skipping\n", scriptPath)
	return true
}

// executeScript runs a hook script for eventName. When EnforcePrivilegeDrop
// is false (the zero value), every runtime other than substrate-serve runs
// every hook script via the calling process's own credentials, with no
// ownership check at all (eventName is unused on this path). See
// executeScriptEnforced for the privilege-drop-enforced path.
func (m *LifecycleManager) executeScript(path, eventName string) error {
	if m.EnforcePrivilegeDrop {
		return m.executeScriptEnforced(path, eventName)
	}

	// Check if executable
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode()&0111 == 0 {
		// Not executable, skip with warning
		fmt.Fprintf(os.Stderr, "[sciontool] Warning: hook script %s is not executable, skipping\n", path)
		return nil
	}

	cmd := exec.Command(path)
	cmd.Stdout = os.Stderr // Redirect hook output to stderr
	cmd.Stderr = os.Stderr
	cmd.Env = m.hookEnv()

	// Hook scripts run inside sciontool init's PID-1 process while its
	// zombie reaper is active; RunManaged registers this child's PID so the
	// reaper leaves it for this Wait call to reap instead of racing it.
	if err := procreap.RunManaged(cmd); err != nil {
		return fmt.Errorf("execution failed: %w", err)
	}
	return nil
}

// enforcedExecPrep is prepareEnforcedExec's result: everything
// executeScriptEnforced needs to build and run the command, plus enough for
// a test to assert the root/drop decision against real on-disk state
// without running anything or needing any privilege. file is the script's
// own open file (caller must close it) if err is nil.
type enforcedExecPrep struct {
	file       *os.File
	executable bool
	asRoot     bool
}

// prepareEnforcedExec opens the script's directory chain and the script
// itself, both with O_NOFOLLOW at every component (openChainNoFollow,
// openScriptNoFollow) — never re-resolved by path — and decides root-vs-drop
// from the real fstat results via DecideExecAsRoot. Split out from
// executeScriptEnforced's actual exec so a test can assert the DECISION
// (asRoot) against a real, unprivileged filesystem fixture (e.g. a script
// under a world-writable temp dir, which DecideExecAsRoot must always drop)
// without needing to run the script or hold any privilege at all.
func prepareEnforcedExec(path string) (enforcedExecPrep, error) {
	dir := filepath.Dir(path)
	name := filepath.Base(path)

	dirFd, chain, err := openChainNoFollow(dir)
	if err != nil {
		return enforcedExecPrep{}, fmt.Errorf("hooks: %s: %w", path, err)
	}
	scriptFile, scriptOwnership, err := openScriptNoFollow(dirFd, name)
	_ = closeFd(dirFd)
	if err != nil {
		return enforcedExecPrep{}, fmt.Errorf("hooks: %s: %w", path, err)
	}

	executable, err := fdIsExecutable(scriptFile)
	if err != nil {
		_ = scriptFile.Close()
		return enforcedExecPrep{}, fmt.Errorf("hooks: %s: %w", path, err)
	}

	return enforcedExecPrep{
		file:       scriptFile,
		executable: executable,
		asRoot:     DecideExecAsRoot(scriptOwnership, chain),
	}, nil
}

// executeScriptEnforced is executeScript's privilege-drop-enforced path: it
// calls prepareEnforcedExec (see its own doc comment for the no-TOCTOU
// open/decide construction) and then either skips a non-executable script or
// runs it via buildEnforcedCmd, as root or dropped to
// WorkloadUID/WorkloadGID per the prepared decision.
func (m *LifecycleManager) executeScriptEnforced(path, eventName string) error {
	prep, err := prepareEnforcedExec(path)
	if err != nil {
		return err
	}
	defer func() { _ = prep.file.Close() }()

	if !prep.executable {
		fmt.Fprintf(os.Stderr, "[sciontool] Warning: hook script %s is not executable, skipping\n", path)
		return nil
	}
	if !prep.asRoot {
		fmt.Fprintf(os.Stderr,
			"[sciontool] hook script %s is not root-protected (owner/mode); running as the workload uid=%d gid=%d instead of root\n",
			path, m.WorkloadUID, m.WorkloadGID)
	}
	cmd, err := m.buildEnforcedCmd(prep.file, path, eventName, prep.asRoot)
	if err != nil {
		return err
	}
	return runEnforcedCmd(cmd)
}

// runEnforcedCmd runs an *exec.Cmd buildEnforcedCmd built, split out from
// executeScriptEnforced purely so a test can drive the exact same run step
// against a cmd it built directly via buildEnforcedCmd (asRoot passed
// explicitly), without needing prepareEnforcedExec's real on-disk
// root-ownership check — the same reason buildEnforcedCmd itself is split
// from executeScriptEnforced.
//
// Enforced hook scripts run inside sciontool init's PID-1 process while its
// zombie reaper is active, the same as executeScript's unenforced path
// above: RunManaged registers this child's PID so the reaper leaves it for
// this Wait call to reap instead of racing it.
func runEnforcedCmd(cmd *exec.Cmd) error {
	if err := procreap.RunManaged(cmd); err != nil {
		return fmt.Errorf("execution failed: %w", err)
	}
	return nil
}

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
		if eventName == EventPreStart && filepath.Base(path) == harnessProvisionHookFilename {
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
		Credential: &syscall.Credential{
			Uid: uint32(m.WorkloadUID),
			Gid: uint32(m.WorkloadGID),
		},
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
// re-deriving or looking up a uid/gid here. Supplementary groups are
// cleared explicitly (Groups set to an empty, non-nil slice) rather than
// left to Go's own default handling of a nil Groups field, so the intent
// reads directly off the Credential literal instead of depending on
// documented-but-unstated zero-value behavior.
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
		Credential: &syscall.Credential{
			Uid:    uint32(m.WorkloadUID),
			Gid:    uint32(m.WorkloadGID),
			Groups: []uint32{},
		},
	}
	cmd.Env = setEnvVar(m.droppedHookEnv(), "PYTHONNOUSERSITE", "1")
	cmd.Env = setEnvVar(cmd.Env, "SCION_HOOK_PATH", path)
	return cmd, nil
}

// hookEnv builds the environment for hook scripts. When AgentHome is set,
// HOME is overridden so that $HOME in hook scripts resolves to the scion
// user's home directory rather than root's. PYTHONDONTWRITEBYTECODE is set
// to prevent root-owned __pycache__ artifacts from being created during
// pre-start hooks (which run before privilege drop).
func (m *LifecycleManager) hookEnv() []string {
	env := os.Environ()
	env = append(env, "PYTHONDONTWRITEBYTECODE=1")
	if m.AgentHome == "" {
		return env
	}
	override := "HOME=" + m.AgentHome
	for i, e := range env {
		if strings.HasPrefix(e, "HOME=") {
			env[i] = override
			return env
		}
	}
	return append(env, override)
}

// droppedHookEnv builds the environment for a hook script that
// executeScriptEnforced has decided to run dropped: the same base env
// hookEnv builds (AgentHome-overridden HOME, PYTHONDONTWRITEBYTECODE), plus
// USER/LOGNAME set to WorkloadUsername — matching how
// supervisor.Supervisor.Run sets HOME/USER/LOGNAME for the harness child
// process itself when it drops privileges, so a dropped hook sees the same
// identity-derived environment the harness does. A no-op for USER/LOGNAME
// when WorkloadUsername is empty.
func (m *LifecycleManager) droppedHookEnv() []string {
	env := m.hookEnv()
	if m.WorkloadUsername == "" {
		return env
	}
	env = setEnvVar(env, "USER", m.WorkloadUsername)
	env = setEnvVar(env, "LOGNAME", m.WorkloadUsername)
	return env
}

// rootHookEnvAllowlist is the complete, closed set of variable NAMES
// hardenedRootHookEnv keeps from the inherited process environment —
// everything else is dropped outright, not merely overridden. init's own
// environment carries every key substrate-serve's bootstrap applied via
// os.Setenv (harness env, operator template/cfg env, resolved auth and
// secret env — pkg/runtime/substrate_bootstrap.go's buildBootstrapEnv), any
// of which could in principle be an interpreter or loader redirector —
// PYTHONPATH, PYTHONSTARTUP, BASH_ENV, ENV, LD_PRELOAD, LD_LIBRARY_PATH,
// NODE_OPTIONS, PERL5LIB/PERL5OPT, RUBYLIB/RUBYOPT, XDG_CONFIG_HOME (git's
// global config, e.g. core.fsmonitor), GIT_CONFIG_* — pointed at a
// workload-writable location. HOME=/root and a fixed PATH alone do not stop
// a tool that reads one of those directly instead of resolving through HOME
// or PATH: the same escalation class DecideExecAsRoot and the HOME override
// exist to close, reopened through the environment.
//
// This is a closed list of exact names, not a set of name/prefix patterns:
// no root-eligible hook shipped today (there is none after pre-start —
// broker delivery is pre-start only, and no image ships anything in
// /etc/scion/hooks) reads any SCION_* variable, so none is allowlisted by
// name or by a blanket "SCION_" prefix; a future hook that genuinely needs
// one adds it here, by name, with its own justification, rather than
// inheriting the whole namespace on the assumption that SCION_* values are
// never a workload-controlled path. LANG and TERM are the only two kept:
// display/locale hints a hook's own output formatting might consult, never
// a code or config search path.
var rootHookEnvAllowlist = map[string]bool{
	"LANG": true,
	"TERM": true,
}

// hardenedRootHookEnv builds the environment for every root-eligible hook
// script this package execs, at any event including pre-start. hookEnv's own
// HOME=AgentHome (the workload's own, workload-owned home directory) would
// let such a root-run hook — if it happens to invoke python, bash, git, pip,
// or anything else that consults its HOME for rc/site/config files — load
// workload-planted content (~/.bashrc, a PYTHONPATH-adjacent site
// customization, ~/.gitconfig, a pip user config) and execute it as root:
// exactly the escalation class DecideExecAsRoot exists to close, reintroduced
// through the environment instead of the exec path. So a root hook gets
// HOME=/root (never workload-owned), PYTHONNOUSERSITE=1 (disables Python's
// per-user site-packages lookup, which HOME would otherwise influence), and
// a fixed, minimal PATH that never includes anything workload-writable —
// and, unlike hookEnv, does NOT otherwise inherit the process environment at
// all: only the exact names in rootHookEnvAllowlist survive from it, so an
// interpreter/loader redirector variable (see that var's own doc comment) —
// including NODE_OPTIONS, PERL5OPT, RUBYOPT, and XDG_CONFIG_HOME — never
// reaches a root-eligible hook regardless of where it came from. The dropped
// branch (droppedHookEnv) is unaffected by any of this — a dropped hook gets
// the same environment the harness child process itself gets, unfiltered.
//
// SCION_AGENT_HOME is set explicitly (never via HOME) so a pre-start
// project/hub hook — which runs once, before the workload exists at all,
// and needs to find whatever was staged under AgentHome — can still do so
// without HOME itself ever pointing at a workload-writable location, even
// across a future resume/re-bootstrap over a $HOME the workload has already
// touched. The harness-provision wrapper is also classified root-eligible at
// pre-start, but never reaches this function at all: buildEnforcedCmd
// recognizes it by name and routes it to buildDroppedProvisionCmd instead,
// since — unlike a project/hub hook — it goes on to read and write $HOME and
// /workspace itself.
func (m *LifecycleManager) hardenedRootHookEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		key, _, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		if rootHookEnvAllowlist[key] {
			env = append(env, e)
		}
	}
	env = append(env, "PYTHONDONTWRITEBYTECODE=1")
	env = setEnvVar(env, "HOME", "/root")
	env = setEnvVar(env, "PYTHONNOUSERSITE", "1")
	env = setEnvVar(env, "PATH", strings.Join(rootexec.SearchPath, ":"))
	env = setEnvVar(env, "SCION_AGENT_HOME", m.AgentHome)
	return env
}

// setEnvVar sets key=value in a KEY=VALUE environment slice, replacing an
// existing entry for key in place or appending a new one — the same
// behaviour as pkg/sciontool/supervisor's own unexported setEnvVar, kept as
// a separate copy here rather than an import so this leaf package does not
// need to depend on the supervisor package purely for a five-line string
// helper.
func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}
