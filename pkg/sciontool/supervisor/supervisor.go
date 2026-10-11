/*
Copyright 2025 The Scion Authors.
*/

// Package supervisor provides process lifecycle management for sciontool init.
// It handles spawning child processes, signal forwarding, and graceful shutdown.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
	"github.com/GoogleCloudPlatform/scion/pkg/util/fsutil"
)

// ErrNoCommand is returned when no command is specified for the supervisor to run.
var ErrNoCommand = errors.New("no command specified")

// ErrAlreadyStarted is returned by Run when the Supervisor has already
// started a child process, or another Run call on it is in progress. A
// Supervisor runs at most one child; a Run that failed before starting the
// child may be retried.
var ErrAlreadyStarted = errors.New("supervisor already started")

// ErrPrivilegeDropRequired is returned by Run when Config.RequirePrivilegeDrop
// is set but Config.UID/GID do not both pass the same UID>0 && GID>0
// predicate the credential drop itself uses (see Run's Credential-setting
// block): a "no drop" decision reaching this point in enforced mode must be
// a hard, fail-closed error rather than a silent run-as-root — the same
// fail-closed principle commands.requirePrivilegeDropOrFail already applies
// one layer up, at RunInit's own call site. In practice, the equivalent
// clamp on requirePrivilegeDropOrFail already makes this unreachable in
// enforced mode (it refuses to reach Supervisor.Run at all unless UID>0 &&
// GID>0), so this is belt-and-suspenders against a future caller that
// constructs a Config directly, bypassing RunInit's own check.
var ErrPrivilegeDropRequired = errors.New("privilege drop required but UID/GID were not both set; refusing to run the child as root")

// Config holds configuration for the Supervisor.
type Config struct {
	// GracePeriod is the time to wait after SIGTERM before sending SIGKILL.
	GracePeriod time.Duration
	// UID is the target UID for the child process (0 = no change)
	UID int
	// GID is the target GID for the child process (0 = no change)
	GID int
	// Username is the target username for the child process (used to set HOME, USER, LOGNAME)
	Username string
	// Rootless indicates the container is running in a rootless user namespace
	// (e.g. rootless Podman). When true, the supervisor skips the credential
	// drop (UID 0 inside the container IS the unprivileged host user) but
	// still sets HOME/USER/LOGNAME to the Username so harnesses find their
	// config in the right place.
	Rootless bool
	// EnvOverlay are additional environment variables produced by the harness
	// pre-start provisioner (e.g. resolved API keys read from secret files).
	// Existing entries in the runtime environment win on conflict; overlay
	// values only fill keys that are not already set.
	EnvOverlay map[string]string
	// NativeTelemetryPolicy enables fail-closed validation of the generated
	// native telemetry environment before the child is launched.
	NativeTelemetryPolicy string
	// SecretOverrides are secret values fetched from the hub's
	// POST /api/v1/agent/secrets endpoint (#127, P2d). Unlike EnvOverlay,
	// these REPLACE existing entries — the runtime-provided value is a
	// placeholder by construction (P3 writes SCION_SECRET_KEYS=A,B,C with
	// empty values), so the fetched value must win.
	//
	// Applied AFTER mergeEnvOverlay. Do NOT fold these into EnvOverlay or
	// change mergeEnvOverlay's precedence rule — it is correct for its own
	// case. See the override reasoning in the P2d PR description.
	SecretOverrides map[string]string
	// WorkingDir sets the child process's working directory (exec.Cmd.Dir).
	// Empty (the zero value) leaves cmd.Dir unset, so the child inherits
	// this process's own current working directory — exactly today's
	// behaviour for every caller that does not set this field. Supervisor
	// never inspects the environment or filesystem to decide this itself;
	// the caller resolves it (see commands.InitRunOptions.WorkingDir, set
	// only by substrate-serve's InitRunner wiring).
	WorkingDir string
	// RequirePrivilegeDrop is the caller's own
	// commands.InitRunOptions.RequirePrivilegeDrop (true only for
	// substrate). It gates chownRecursive's hard-link guard: a regular file
	// with more than one hard link is skipped rather than chowned only when
	// this is true, since the guard is new, security-motivated behaviour —
	// a legitimately hard-linked file under a non-substrate container's home
	// directory would otherwise be silently left unowned by the target user
	// and break writes, with no privilege boundary at stake to justify that
	// on runtimes other than substrate. The fd-relative, no-follow walk
	// itself (see chownRecursive's doc comment) is unconditional — it is
	// behaviour-preserving and has no legitimate dependent case.
	RequirePrivilegeDrop bool
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		GracePeriod: 10 * time.Second,
	}
}

// Supervisor manages child process lifecycle including signal forwarding
// and graceful shutdown.
type Supervisor struct {
	config Config
	cmd    *exec.Cmd

	// execToken is the reaper registration handle for cmd's PID, set once in
	// Run (before waitForChild is spawned, so no synchronization is needed
	// to read it there) and consumed exactly once in waitForChild's
	// UnregisterManagedPID call. See procreap.Token for why the token
	// (rather than just the PID) must be passed back.
	execToken *procreap.Token

	// mu protects the process state
	mu sync.Mutex
	// running is set, under mu, by the Run call that owns this Supervisor,
	// and cleared again if that Run fails before starting the child, so
	// concurrent or repeated Run calls cannot both start a child.
	running   bool
	started   bool
	exited    bool
	exitCode  int
	exitError error

	// startedCh is closed once the child process has been started (and
	// Signal can reach it). It is never closed if Run fails before Start.
	startedCh chan struct{}

	// done is closed when the child process exits
	done chan struct{}
}

// New creates a new Supervisor with the given configuration.
func New(config Config) *Supervisor {
	return &Supervisor{
		config:    config,
		startedCh: make(chan struct{}),
		done:      make(chan struct{}),
	}
}

// Run starts and supervises the given command until it exits or the context
// is cancelled. It returns the exit code of the child process. A Supervisor
// runs at most one child: once a Run has started its child, or while another
// Run is in progress, Run returns ErrAlreadyStarted. A Run that fails before
// starting the child may be retried.
func (s *Supervisor) Run(ctx context.Context, args []string) (int, error) {
	s.mu.Lock()
	// running stays set after a successful start, so it alone covers both cases.
	if s.running {
		s.mu.Unlock()
		return 1, ErrAlreadyStarted
	}
	s.running = true
	s.mu.Unlock()
	// Release the claim if this Run returns before the child started, so a
	// failed start can be retried. Once started is set it stays claimed.
	defer func() {
		s.mu.Lock()
		if !s.started {
			s.running = false
		}
		s.mu.Unlock()
	}()

	if len(args) == 0 {
		return 1, ErrNoCommand
	}

	// Create the child process
	s.cmd = exec.Command(args[0], args[1:]...)
	s.cmd.Stdin = os.Stdin
	s.cmd.Stdout = os.Stdout
	s.cmd.Stderr = os.Stderr

	// Leave cmd.Dir unset (today's behaviour: the child inherits this
	// process's own cwd) unless the caller explicitly resolved one. See
	// Config.WorkingDir's doc comment.
	if s.config.WorkingDir != "" {
		s.cmd.Dir = s.config.WorkingDir
		log.Debug("Child working directory: %s", s.config.WorkingDir)
	}

	// Start in a new process group so we can signal the whole group
	s.cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	// Drop privileges if UID/GID specified (skip in rootless mode where
	// UID 0 inside the container is already the unprivileged host user).
	if s.config.UID > 0 && s.config.GID > 0 {
		// Keeps the runtime-granted nfs shared-dir groups (ptone/scion#3155).
		s.cmd.SysProcAttr.Credential = suppgroups.Credential(uint32(s.config.UID), uint32(s.config.GID))
		log.Debug("Child will run as UID=%d, GID=%d", s.config.UID, s.config.GID)
	} else if s.config.RequirePrivilegeDrop {
		return 1, ErrPrivilegeDropRequired
	}

	// Fix ownership of the home directory when dropping privileges.
	// Pre-start hooks run as root and may create files (e.g. .claude/, agent-info.json)
	// that the child process needs to write to. Without this, the child (running as
	// UID/GID from the credential drop) gets permission denied on its own home.
	if s.config.UID > 0 && s.config.GID > 0 && s.config.Username != "" {
		home := "/home/" + s.config.Username
		err := chownRecursive(home, s.config.UID, s.config.GID, s.config.RequirePrivilegeDrop)
		if err != nil {
			log.Error("Failed to chown home directory %s: %v", home, err)
		} else {
			log.Debug("Chowned %s to %d:%d", home, s.config.UID, s.config.GID)
		}
	}

	// Build the child's environment (ChildEnv: user identity,
	// SCION_EXTRA_PATH, the harness env overlay and secret overrides).
	env, err := ChildEnv(s.config, os.Environ())
	if err != nil {
		return 1, err
	}
	s.cmd.Env = env

	// Tell the child its logical cwd explicitly. exec.Cmd setting Dir does
	// not itself add PWD to the environment, so without this the child
	// would inherit this process's own PWD. sh, tmux and Node's
	// process.cwd() all prefer PWD over getcwd() when the two agree, so
	// this keeps a symlinked WorkingDir's logical path visible instead of
	// its resolved physical one — the same PWD behaviour Docker/Podman/
	// Kubernetes already get from the shell that applies the image's
	// WORKDIR. Scoped to WorkingDir != "" so every other caller, which
	// never sets it, is unaffected.
	if s.config.WorkingDir != "" {
		s.cmd.Env = setEnvVar(s.cmd.Env, "PWD", s.config.WorkingDir)
	}

	// Start and register the child's PID as a single gated step so
	// sciontool init's SIGCHLD reaper cannot observe it as
	// exited-and-unmanaged in the gap between Start() returning and
	// registration (see pkg/sciontool/procreap for why).
	if err := procreap.Gated(func() error {
		if err := s.cmd.Start(); err != nil {
			return err
		}
		s.execToken = procreap.RegisterManagedPID(s.cmd.Process.Pid)
		return nil
	}); err != nil {
		return 1, fmt.Errorf("failed to start command: %w", err)
	}
	log.Debug("Started child process %d: %v", s.cmd.Process.Pid, args)

	s.mu.Lock()
	s.started = true
	s.mu.Unlock()
	close(s.startedCh)

	// Wait for the child in a goroutine
	go s.waitForChild()

	// Wait for either context cancellation or child exit
	select {
	case <-ctx.Done():
		log.Info("Context cancelled, initiating graceful shutdown")
		return s.shutdown()
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		log.Debug("Child process %d exited naturally", s.cmd.Process.Pid)
		return s.exitCode, s.exitError
	}
}

// Signal sends a signal to the child process.
func (s *Supervisor) Signal(sig os.Signal) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started || s.exited || s.cmd.Process == nil {
		return nil
	}

	return s.cmd.Process.Signal(sig)
}

// waitForChild waits for the child process to exit and records its exit status.
func (s *Supervisor) waitForChild() {
	err := s.cmd.Wait()
	procreap.UnregisterManagedPID(s.cmd.Process.Pid, s.execToken)

	s.mu.Lock()
	s.exited = true
	s.exitError = err

	if err == nil {
		s.exitCode = 0
	} else {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			s.exitCode = exitErr.ExitCode()
			s.exitError = nil // Exit with non-zero is not an error condition
		} else {
			s.exitCode = 1
		}
	}
	s.mu.Unlock()

	close(s.done)
}

// shutdown performs a graceful shutdown of the child process.
func (s *Supervisor) shutdown() (int, error) {
	s.mu.Lock()
	if s.exited {
		exitCode := s.exitCode
		exitErr := s.exitError
		s.mu.Unlock()
		return exitCode, exitErr
	}
	s.mu.Unlock()

	log.Info("Sending SIGTERM to child process group")
	// Send SIGTERM first
	if err := s.Signal(syscall.SIGTERM); err != nil {
		// If we can't signal, try to get exit status anyway
		s.mu.Lock()
		if s.exited {
			exitCode := s.exitCode
			exitErr := s.exitError
			s.mu.Unlock()
			return exitCode, exitErr
		}
		s.mu.Unlock()
		return 1, fmt.Errorf("failed to send SIGTERM: %w", err)
	}

	// Wait for graceful exit or timeout
	select {
	case <-s.done:
		log.Info("Child process exited gracefully after SIGTERM")
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.exitCode, s.exitError
	case <-time.After(s.config.GracePeriod):
		log.Info("Grace period %s expired, sending SIGKILL to child process group", s.config.GracePeriod)
		// Grace period expired, force kill
		if err := s.Signal(syscall.SIGKILL); err != nil {
			s.mu.Lock()
			if s.exited {
				exitCode := s.exitCode
				exitErr := s.exitError
				s.mu.Unlock()
				return exitCode, exitErr
			}
			s.mu.Unlock()
		}
		// Wait for process to actually exit after SIGKILL
		<-s.done
		log.Info("Child process terminated with SIGKILL")
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.exitCode, s.exitError
	}
}

// Started returns a channel that is closed once the child process has been
// started, i.e. from the point at which Signal reaches it rather than being
// a no-op. It is never closed if Run fails before starting the child, so
// callers waiting on it should also select on Done or a deadline. It is
// closed at most once: only the Run that starts the child closes it, and any
// later Run returns ErrAlreadyStarted.
func (s *Supervisor) Started() <-chan struct{} {
	return s.startedCh
}

// Done returns a channel that is closed when the child process exits.
func (s *Supervisor) Done() <-chan struct{} {
	return s.done
}

// ExitCode returns the exit code of the child process.
// Only valid after Done() is closed.
func (s *Supervisor) ExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode
}

// setEnvVar sets or replaces an environment variable in a list of KEY=VALUE strings.
// ChildEnv builds the harness child's environment from base (the init
// process's environment). It is the one definition of "the agent's
// normal container environment"; SSHSessionEnv derives the conduit SSH
// server's session environment from it:
//   - HOME, USER and LOGNAME for cfg.Username when dropping privileges
//     or in rootless mode;
//   - SCION_EXTRA_PATH prepended to PATH, then removed;
//   - the harness env overlay merged in (runtime env wins), with the
//     native telemetry policy check when one is set;
//   - fetched secret overrides replacing existing entries;
//   - the native telemetry policy key removed.
//
// It does not set PWD: callers that pick a working directory set it.
func ChildEnv(cfg Config, base []string) ([]string, error) {
	return childEnv(cfg, base, true)
}

// SSHSessionEnv is the environment of the conduit SSH server's sessions:
// SSH sessions do not receive secret overrides; other environment
// matches the harness child (ChildEnv). The secret overrides still take
// part in the native telemetry policy check, as for the harness.
func SSHSessionEnv(cfg Config, base []string) ([]string, error) {
	return childEnv(cfg, base, false)
}

// childEnv is ChildEnv, applying the secret overrides only when
// applySecrets is set.
func childEnv(cfg Config, base []string, applySecrets bool) ([]string, error) {
	env := append([]string(nil), base...)

	// Set the child's user environment when dropping privileges OR in
	// rootless mode. In rootless containers, init runs as UID 0 (which
	// maps to the unprivileged host user), so no Credential is needed,
	// but HOME/USER/LOGNAME must still point to the scion user's home
	// so harnesses find their configuration.
	if cfg.Username != "" && (cfg.UID > 0 || cfg.Rootless) {
		home := "/home/" + cfg.Username
		env = setEnvVar(env, "HOME", home)
		env = setEnvVar(env, "USER", cfg.Username)
		env = setEnvVar(env, "LOGNAME", cfg.Username)
		log.Debug("Child env: HOME=%s, USER=%s, LOGNAME=%s", home, cfg.Username, cfg.Username)
	}

	// Apply SCION_EXTRA_PATH: prepend its value to PATH, then remove it from env.
	if extraPath := getEnvVar(env, "SCION_EXTRA_PATH"); extraPath != "" {
		newPath := extraPath
		if currentPath := getEnvVar(env, "PATH"); currentPath != "" {
			newPath = extraPath + ":" + currentPath
		}
		env = setEnvVar(env, "PATH", newPath)
		env = removeEnvVar(env, "SCION_EXTRA_PATH")
		log.Debug("Applied SCION_EXTRA_PATH: PATH=%s", newPath)
	}

	// Merge harness-generated env overlay. Runtime env wins on conflict so a
	// container-script harness cannot mask a value set by the broker/CLI.
	if len(cfg.EnvOverlay) > 0 {
		before := len(env)
		if cfg.NativeTelemetryPolicy != "" {
			merged, err := hooks.MergeEnvOverlayWithNativeTelemetryPolicy(cfg.NativeTelemetryPolicy, env, cfg.EnvOverlay, cfg.SecretOverrides)
			if err != nil {
				return nil, err
			}
			env = merged
		} else {
			env = mergeEnvOverlay(env, cfg.EnvOverlay)
		}
		log.Debug("Applied harness env overlay: %d entries (added %d)", len(cfg.EnvOverlay), len(env)-before)
	} else if cfg.NativeTelemetryPolicy != "" {
		if err := hooks.ValidateNativeTelemetryEnv(cfg.NativeTelemetryPolicy, env, nil, cfg.SecretOverrides); err != nil {
			return nil, err
		}
	}

	// Apply fetched secret overrides. These REPLACE existing entries —
	// the runtime-provided value is a placeholder (P3 writes empty values
	// in SCION_SECRET_KEYS), so the fetched value must win. This is
	// deliberately separate from mergeEnvOverlay, which is additive-only.
	// (#127, P2d) The telemetry receiver never sees these values, so a
	// secret named SCION_USAGE_SOURCE is unsupported: the child would get
	// it while the receiver's usage gating would not.
	if applySecrets && len(cfg.SecretOverrides) > 0 {
		for k, v := range cfg.SecretOverrides {
			env = setEnvVar(env, k, v)
		}
		log.Debug("Applied %d fetched secret override(s)", len(cfg.SecretOverrides))
	}
	if cfg.NativeTelemetryPolicy != "" {
		env = removeEnvVar(env, hooks.NativeTelemetryPolicyKey)
	}
	return env, nil
}

func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

// getEnvVar returns the value of an environment variable from a list of KEY=VALUE strings.
// Returns empty string if the key is not found.
func getEnvVar(env []string, key string) string {
	prefix := key + "="
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			return e[len(prefix):]
		}
	}
	return ""
}

// removeEnvVar removes an environment variable from a list of KEY=VALUE strings.
func removeEnvVar(env []string, key string) []string {
	prefix := key + "="
	result := env[:0:0]
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			continue
		}
		result = append(result, e)
	}
	return result
}

// mergeEnvOverlay folds overlay key/value pairs into env. Existing entries
// in env take precedence so runtime/CLI-provided env vars are never masked
// by harness output.
func mergeEnvOverlay(env []string, overlay map[string]string) []string {
	if len(overlay) == 0 {
		return env
	}
	taken := make(map[string]struct{}, len(env))
	for _, e := range env {
		if i := indexByte(e, '='); i > 0 {
			taken[e[:i]] = struct{}{}
		}
	}
	// Deterministic order for reproducibility.
	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	for _, k := range keys {
		if _, ok := taken[k]; ok {
			continue
		}
		env = append(env, k+"="+overlay[k])
	}
	return env
}

// indexByte avoids the strings import for a tight inner loop.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// chownRecursive changes ownership of a directory and all its contents to
// uid:gid, unconditionally (every entry, not just root-owned ones — the
// home directory this is called on belongs entirely to the workload user
// both before and after the drop, so there is no "leave root-owned entries
// alone" distinction to make here, unlike chownTreeRootOwned).
//
// fsutil.CheckRoot refuses root outright on its own path/name alone when it
// is a known critical system path or looks like a filesystem root by
// content; fsutil.CheckMountSource additionally refuses root when it is
// itself a mount point whose bind source names a critical system directory
// — see that function's doc comment for exactly what it does and does not
// detect. Checking CheckRoot first means an already-invalid root is never
// checked against the mount table at all.
//
// It walks via dirfd.ChownTreeNoFollow: every entry is resolved to a file
// descriptor exactly once (openat(O_DIRECTORY|O_NOFOLLOW) for a directory,
// openat(O_PATH|O_NOFOLLOW) otherwise), and every chown is
// fchownat(fd, "", uid, gid, AT_EMPTY_PATH) issued against that same fd —
// never a full-path os.Lchown, which re-resolves every intermediate path
// component on every call and can be redirected by a symlink a scion-uid
// process (a sidecar service, or a process a pre-start hook spawned) swaps
// into one of them between this walk visiting that component and the
// Lchown call for something beneath it. sup.Run calls this while such
// processes may already be alive, so that window is real. This part is
// unconditional on every runtime: it is behaviour-preserving (every entry
// still ends up chowned exactly as before) and has no legitimate case that
// depends on the old, re-resolving behaviour.
//
// requirePrivilegeDrop gates the walk's hard-link guard only — see
// Config.RequirePrivilegeDrop's doc comment for why that one part of this
// is new behaviour that must not change non-substrate runtimes.
//
// Per-entry chown failures and hard-link-guard skips are logged (entry name
// only) rather than silently discarded.
func chownRecursive(root string, uid, gid int, requirePrivilegeDrop bool) error {
	if err := fsutil.CheckRoot(root); err != nil {
		return err
	}
	if err := checkMountSource(root); err != nil {
		return err
	}
	_, _, err := dirfd.ChownTreeNoFollow(root, uid, gid, func(uint32) bool { return true }, requirePrivilegeDrop, func(name string, cerr error) {
		if errors.Is(cerr, dirfd.ErrHardlinkedRegularFile) {
			log.Warn("chownRecursive: skipping %s: %v", name, cerr)
			return
		}
		log.Error("chownRecursive: failed to chown %s: %v", name, cerr)
	})
	return err
}

// checkMountSource is fsutil.CheckMountSource, held behind a package
// variable so a test can stub it (to prove chownRecursive actually calls
// it) without needing a real mount to exercise. The production value is
// fixed; only tests reassign it, and always restore it afterward.
var checkMountSource = fsutil.CheckMountSource
