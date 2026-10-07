/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/autoexpose"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/metadata"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/services"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/supervisor"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/fsutil"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	gracePeriod time.Duration
)

// telemetryStopBudget permits one safe 15-second metric write interval plus
// five seconds for the final export after the child has exited. The bound does
// not guarantee Cloud delivery when the backend is slower.
const telemetryStopBudget = 20 * time.Second

func stopTelemetryWithTimeout(stop func(context.Context) error, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return stop(ctx)
}

// initCmd represents the init command
var initCmd = &cobra.Command{
	Use:   "init [--] <command> [args...]",
	Short: "Run as container init (PID 1) and supervise child processes",
	Long: `The init command runs sciontool as the container's init process (PID 1).

It provides:
  - Zombie process reaping (critical for PID 1)
  - Signal forwarding to child processes
  - Graceful shutdown with configurable grace period
  - Child process exit code propagation

The command after -- is executed as the child process. If no command is
specified, sciontool will exit with an error.

Examples:
  sciontool init -- gemini
  sciontool init -- tmux new-session -A -s main
  sciontool init --grace-period=30s -- claude`,
	DisableFlagParsing: false,
	Run: func(cmd *cobra.Command, args []string) {
		exitCode := RunInit(args, InitRunOptions{})
		os.Exit(exitCode)
	},
}

// InitRunOptions configures a single invocation of RunInit. The zero value
// matches `sciontool init`'s historical CLI behaviour except where noted.
// These fields exist as a seam for an in-process caller that embeds RunInit
// instead of going through the `sciontool init` CLI path; substrate-serve
// (pkg/sciontool/substrate, cmd/sciontool/commands/substrate_serve.go) is
// the current example of such a caller and the only one that sets any of
// them today.
type InitRunOptions struct {
	// DisableTermSignalForwarding controls whether RunInit skips installing
	// its own SIGTERM/SIGINT handler that runs pre-stop hooks and gracefully
	// shuts down the child process. The zero value (false) matches
	// `sciontool init`'s (the CLI command's) historical behaviour — signal
	// forwarding on — without that command needing to set anything. A caller
	// that is itself PID 1 and owns SIGTERM handling for the whole process
	// sets this to true so the two handlers don't race on the same signal.
	DisableTermSignalForwarding bool

	// RequirePrivilegeDrop fails RunInit closed — refusing to start the
	// harness — when setupHostUser could not actually drop from root to
	// the scion user (e.g. the container's capability set lacks
	// CAP_SETUID/CAP_SETGID). It does not change setupHostUser's own
	// rootless fallback for any other caller; it only adds a check of that
	// fallback's result, for a caller whose runtime has no legitimate
	// "still UID 0" outcome.
	RequirePrivilegeDrop bool

	// DisablePortForwarding skips starting the hub port-forward tunnel
	// manager and the auto-expose port scanner. `sciontool init` (the CLI
	// command) always leaves this false — that behaviour is unchanged. A
	// caller whose network path can't route that traffic sets this to true.
	DisablePortForwarding bool

	// DisableConduit keeps the legacy port-forward tunnel even when the hub
	// advertises conduit (hub.conduit in SCION_HUB_EXPERIMENTS). The zero
	// value dials the conduit endpoint when, and only when, the hub
	// advertises it.
	DisableConduit bool

	// DisableReExec skips RunInit's environ-purge re-exec (see
	// reExecWithCleanEnv). That re-exec exists only to purge
	// /proc/1/environ, which the kernel fills from the execve(2)
	// environment and never updates afterward. An embedding caller that
	// receives its secrets in-process — substrate-serve gets them in the
	// bootstrap request body and applies them with os.Setenv — never had
	// them in the kernel's copy, so there is nothing to purge; re-execing
	// would instead replace that caller's own PID 1 with a fresh process
	// that has none of its in-memory state. A caller that set this after
	// receiving secrets through its OWN execve environment would leave them
	// readable in /proc/1/environ, so only an in-process caller may set it.
	// Staging still runs either way: the transport token file is written,
	// SCION_TRANSPORT_TOKEN is cleared and SCION_TRANSPORT_TOKEN_FILE is
	// set; only the re-exec is skipped. The zero value (false) keeps
	// `sciontool init`'s behaviour unchanged.
	DisableReExec bool

	// WorkingDir sets the harness child's working directory (threaded into
	// supervisor.Config.WorkingDir, which sets exec.Cmd.Dir — see that
	// field's doc comment). Empty (the zero value) leaves cmd.Dir unset, so
	// the child inherits this process's own current working directory —
	// RunInit's unconditional behaviour for `sciontool init`.
	//
	// Superseded outright by ResolveWorkingDir below when both are set: its
	// result is what reaches the harness, and this field is ignored. No
	// caller sets both today, so this precedence is a documented default for
	// a future caller rather than a path any current one exercises.
	WorkingDir string

	// ResolveWorkingDir, when non-nil, is called once by RunInit — directly
	// after gitCloneWorkspace and the post-pre-start-hook ownership fixup
	// have both run, and before anything else that starts a long-running
	// component on the harness's behalf (sidecar services, the metadata
	// server, the hub secret fetch) or before harnessSupervisorConfig builds
	// the supervisor.Config — to compute the harness child's working
	// directory in place of the static WorkingDir field above.
	//
	// That placement is not incidental: a resolver that needs to know
	// whether a directory is actually usable (searchable by the scion
	// uid/gid) has to run after every step that can change that, and before
	// any step whose work would be wasted (and, on the fail-closed exit
	// path, left running) if the resolver then errors. A resolver called
	// before both has seen the workspace in whatever state the broker's
	// bind mount left it in, which can resolve to the wrong directory.
	//
	// nil (the zero value) for every caller that doesn't need dynamic
	// resolution: RunInit's behaviour is then exactly WorkingDir's own
	// zero-value contract above, unchanged.
	//
	// An error from ResolveWorkingDir fails RunInit closed with
	// exitCodeNoUsableHarnessCwd: the harness is never started, sidecar
	// services/the metadata server/the hub secret fetch never start either,
	// and RunInit never falls back to WorkingDir's own zero-value "inherit
	// this process's cwd" behaviour or to "/".
	//
	// See pkg/sciontool/substrate / cmd/sciontool/commands/substrate_serve.go
	// for why substrate needs all four fields above and how it resolves
	// this one (also .design/kubernetes/substrate-runtime.md §§1, 5.6-5.7, 8).
	ResolveWorkingDir func() (string, error)
}

// forwardsTermSignal reports whether RunInit should install its own
// SIGTERM/SIGINT handler, given DisableTermSignalForwarding. Kept as its own
// tiny method (rather than inlining the negation at the one call site) so
// the "zero-value InitRunOptions matches sciontool init's historical
// CLI behaviour" property has a single, directly unit-testable home.
func (opts InitRunOptions) forwardsTermSignal() bool {
	return !opts.DisableTermSignalForwarding
}

// errPrivilegeDropRequired is returned when RequirePrivilegeDrop is set and
// setupHostUser did not actually drop privileges. It is deliberately
// generic and secret-free: setupHostUser's own log lines (CAP_SETUID
// absent, SCION_HOST_UID/GID not set, etc.) carry the specific reason.
var errPrivilegeDropRequired = errors.New("privilege drop to the scion user did not happen; refusing to start the harness as root")

// requirePrivilegeDropOrFail implements RequirePrivilegeDrop's fail-closed
// check: substrate must never run the harness as root — see
// RequirePrivilegeDrop's own doc comment, and pkg/runtime/substrate_bootstrap.go's
// buildBootstrapEnv for why targetGID's clamp is still fail-closed even
// though only one UID/GID pairing can occur in practice today.
func requirePrivilegeDropOrFail(targetUID, targetGID int, requirePrivilegeDrop bool) error {
	if requirePrivilegeDrop && (targetUID <= 0 || targetGID <= 0) {
		return errPrivilegeDropRequired
	}
	return nil
}

// exitCodePrivilegeDropRequired is the exit code RunInit returns when
// requirePrivilegeDropOrFail trips — never returned for any other reason.
// It stays a distinct value (rather than a plain 1) purely so an operator
// reading a caller's own logged exit code can tell which failure this was.
// RunInit reports PhaseError to the Hub and to the local agent-info state
// before returning it, the same way the git-clone failure path does — see
// reportInitFailure's doc comment. See pkg/sciontool/substrate's
// StateInitFailed for why a caller embedding RunInit in a process that
// doesn't exit on failure needs this signal at all.
const exitCodePrivilegeDropRequired = 17

// exitCodeNoUsableHarnessCwd is the exit code RunInit returns when
// InitRunOptions.ResolveWorkingDir is set and returns an error — never for
// any other reason. It stays a distinct value, the same reasoning as
// exitCodePrivilegeDropRequired above. RunInit reports PhaseError the same
// way — via reportInitFailure — before returning it; see
// ResolveWorkingDir's doc comment for the fail-closed contract this
// enforces.
const exitCodeNoUsableHarnessCwd = 18

func init() {
	rootCmd.AddCommand(initCmd)

	initCmd.Flags().DurationVar(&gracePeriod, "grace-period", 10*time.Second,
		"Time to wait after SIGTERM before sending SIGKILL")

	// Override the default SCION_GRACE_PERIOD env var if set
	if envGrace := os.Getenv("SCION_GRACE_PERIOD"); envGrace != "" {
		if d, err := time.ParseDuration(envGrace); err == nil {
			gracePeriod = d
		}
	}
}

// resolveAgentHome resolves the scion user's home directory for agent state
// files (agent-info.json, hooks, etc.). Init runs as root (HOME=/root), but
// this must point at the scion user's home whenever a drop actually
// happened (or would have, in rootless mode) so status/hook files land
// where the agent process that reads them expects.
//
// Extracted from RunInit's body so the privilege-drop fail-closed path
// (which returns before RunInit's own agentHome resolution) and RunInit's
// normal continuation share one implementation instead of two copies that
// could drift apart.
func resolveAgentHome(targetUID int, rootless bool) string {
	agentHome := os.Getenv("HOME")
	if targetUID != 0 {
		if scionUser, err := lookupUserByID(strconv.Itoa(targetUID)); err == nil {
			agentHome = scionUser.HomeDir
		} else {
			log.Debug("Could not look up user for UID %d: %v", targetUID, err)
		}
	} else if rootless {
		if scionUser, err := scionUserLookup("scion"); err == nil {
			agentHome = scionUser.HomeDir
		} else {
			log.Debug("Could not look up scion user in rootless mode: %v", err)
		}
	}
	return agentHome
}

// reportInitFailure reports a RunInit failure the same way the git-clone
// failure path does: local agent-info state to PhaseError with a
// message, plus a best-effort direct Hub report. For runtimes whose broker
// reads the container's agent-info.json as part of its own status
// heartbeat, that local write is a second, independent path to the same
// result if the direct Hub call fails or the Hub isn't configured; for a
// runtime whose broker has no such fallback, the direct Hub call is the
// only failure signal that reaches the Hub at all — see
// pkg/sciontool/substrate's StateInitFailed doc comment for why substrate
// is one. cause's message ends up in the Hub-visible message and possibly
// a caller's own exposed response, so callers must only pass fixed,
// secret-free errors (as errPrivilegeDropRequired and every caller below
// do) — never one built from raw command output or file contents.
//
// Shared by every RunInit failure path that needs to report before
// returning, rather than each constructing its own StatusHandler: every
// early-return path reports through this one function, so none can
// silently skip it. It is its own function, taking
// agentHome directly, so it's testable with a plain temp directory,
// independent of setupHostUser's real-environment-dependent agentHome
// resolution.
func reportInitFailure(agentHome string, cause error) {
	statusHandler := handlers.NewStatusHandler()
	statusHandler.StatusPath = filepath.Join(agentHome, "agent-info.json")
	errMsg := cause.Error()
	_ = statusHandler.UpdatePhase(state.PhaseError, "", "")
	_ = statusHandler.SetMessage(errMsg)
	if hubClient := hub.NewClient(); hubClient != nil && hubClient.IsConfigured() {
		hubCtx, hubCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if hubErr := hubClient.ReportState(hubCtx, state.PhaseError, "", errMsg); hubErr != nil {
			log.Error("Failed to report init failure to Hub: %v", hubErr)
		}
		hubCancel()
	} else {
		log.Info("Hub client not configured, init failure will be relayed via broker heartbeat")
	}
}

// resolveProjectHookPath returns the path RunInit checks to decide whether a
// project pre-start hook was staged (30-project-custom) and so must abort
// startup on failure. Extracted as its own pure function of agentHome and
// requirePrivilegeDrop — rather than left inline — so this join between
// InitRunOptions.RequirePrivilegeDrop and where bootstrap actually delivers
// the file can be pinned by a table-driven unit test without invoking
// RunInit itself (which, under RequirePrivilegeDrop: true, cannot reach this
// far in a test environment lacking a real CAP_SETUID/CAP_SETGID privilege
// drop — see requirePrivilegeDropOrFail).
//
// In privilege-drop-enforced mode, a broker-delivered $HOME/.scion/hooks/
// tree is redirected to the root-owned hooks.EnforcedHooksDir instead of
// being chowned to the workload like the rest of the composed home — see
// EnforcedHooksDir's own doc comment (pkg/sciontool/hooks) — so the staged
// file, if any, lives there instead of under agentHome.
func resolveProjectHookPath(agentHome string, requirePrivilegeDrop bool) string {
	dir := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d")
	if requirePrivilegeDrop {
		dir = filepath.Join(hooks.EnforcedHooksDir, "pre-start.d")
	}
	return filepath.Join(dir, "30-project-custom")
}

// harnessSupervisorConfig builds the supervisor.Config for the harness
// child process from RunInit's inputs. It is a pure function of its
// arguments — it reads no globals and has no side effects — so the join
// between InitRunOptions.WorkingDir and supervisor.Config.WorkingDir can be
// pinned by a table-driven unit test without invoking RunInit itself.
func harnessSupervisorConfig(opts InitRunOptions, gracePeriod time.Duration, targetUID, targetGID int, rootless bool, envOverlay map[string]string, nativeTelemetryPolicy string, secretOverrides map[string]string) supervisor.Config {
	return supervisor.Config{
		GracePeriod:           gracePeriod,
		UID:                   targetUID,
		GID:                   targetGID,
		Username:              "scion",
		Rootless:              rootless,
		EnvOverlay:            envOverlay,
		NativeTelemetryPolicy: nativeTelemetryPolicy,
		SecretOverrides:       secretOverrides,
		WorkingDir:            opts.WorkingDir,
		RequirePrivilegeDrop:  opts.RequirePrivilegeDrop,
	}
}

// newLifecycleManager builds and wires the *hooks.LifecycleManager RunInit
// uses, from its own resolved workload identity and options, and reports
// the value the caller should pass to hub.EnforceTokenFileOwnerChecks —
// both derive from the identical requirePrivilegeDrop input, returned
// together so a test can assert every part of "turn on enforced mode" in
// one place instead of trusting several independent call sites to agree.
//
// Privilege-drop-enforced mode (see InitRunOptions.RequirePrivilegeDrop)
// makes hook script execution a function of each script's own fstat'd
// ownership (DecideExecAsRoot) instead of always running as the calling
// process's credentials. targetUID/targetGID are the same identity
// setupHostUser resolved for the harness child process itself, and "scion"
// is the same literal harnessSupervisorConfig passes as
// supervisor.Config.Username.
func newLifecycleManager(agentHome string, targetUID, targetGID int, requirePrivilegeDrop bool) (*hooks.LifecycleManager, bool) {
	lifecycleManager := hooks.NewLifecycleManager()
	lifecycleManager.AgentHome = agentHome
	lifecycleManager.EnforcePrivilegeDrop = requirePrivilegeDrop
	lifecycleManager.WorkloadUID = targetUID
	lifecycleManager.WorkloadGID = targetGID
	lifecycleManager.WorkloadUsername = "scion"
	if requirePrivilegeDrop {
		// The dedicated, root-owned directory broker-delivered hook content
		// is redirected into instead of being chowned to the workload — see
		// EnforcedHooksDir's own doc comment (pkg/sciontool/hooks).
		// Registered before $HOME/.scion/hooks below so trusted,
		// broker-delivered content still runs before anything staged
		// per-agent, matching the system-then-per-agent ordering
		// AddHooksDir's own doc comment describes.
		lifecycleManager.AddHooksDir(hooks.EnforcedHooksDir)
	}
	// Register the per-agent hooks directory so container-script harnesses
	// (whose pre-start wrapper is staged at $HOME/.scion/hooks/pre-start.d/)
	// participate in the standard hook discovery alongside system hooks. In
	// enforced mode this directory does not carry broker-delivered
	// content (redirected above), but stays registered and subject to the
	// same ownership check: anything the workload itself later plants here
	// (e.g. a session-end script) is workload-owned by construction — the
	// home directory chown (supervisor.Supervisor.Run) — and so always
	// runs dropped, never as root.
	lifecycleManager.AddHooksDir(filepath.Join(agentHome, ".scion", "hooks"))
	return lifecycleManager, requirePrivilegeDrop
}

// runNewLifecycleManager and runEnforceTokenFileOwnerChecks are RunInit's own
// seams onto newLifecycleManager and hub.EnforceTokenFileOwnerChecks: a test
// substitutes both to pin the exact arguments RunInit's call site passes —
// agentHome, targetUID, targetGID, and opts.RequirePrivilegeDrop into the
// former, and the FORMER's OWN SECOND RETURN VALUE (not a separately
// re-derived RequirePrivilegeDrop) into the latter — without needing a real
// enforced-mode call site's own argument-threading with a non-root test
// process. Production code always leaves both at their default; only a test
// replaces them.
var runNewLifecycleManager = newLifecycleManager
var runEnforceTokenFileOwnerChecks = hub.EnforceTokenFileOwnerChecks

// RunInit runs the sciontool init logic: it sets up the container user,
// clones the workspace, runs lifecycle hooks, launches the child process
// under supervision, and reports status/heartbeats to the Hub until the
// child exits. It returns the process's intended exit code and never calls
// os.Exit itself, so it is safe to call in-process from other entry points
// (see InitRunOptions.DisableTermSignalForwarding for the substrate-serve case).
//
// This is the exact logic `sciontool init -- <cmd>` runs; it is exported
// so other subcommands can reuse it instead of forking a copy.
func RunInit(args []string, opts InitRunOptions) int {
	// initStart times sciontool init's own startup sequence, for start-time
	// attribution (dispatch -> pod ready -> init -> harness). See the
	// elapsed_ms log lines below.
	initStart := time.Now()

	// Start the reaper goroutine for zombie process cleanup.
	// This is critical when running as PID 1 in a container.
	startReaper()

	// Extract the child command (everything after --)
	childArgs := extractChildCommand(args)
	if len(childArgs) == 0 {
		fmt.Fprintln(os.Stderr, "Error: no command specified after --")
		fmt.Fprintln(os.Stderr, "Usage: sciontool init [--] <command> [args...]")
		return 1
	}

	// Log startup
	log.Info("sciontool init starting as PID %d (uid=%d, gid=%d, euid=%d, egid=%d)", os.Getpid(), os.Getuid(), os.Getgid(), os.Geteuid(), os.Getegid())
	log.Info("Child command: %v", childArgs)
	log.Info("Grace period: %s", gracePeriod)

	// Log operating mode for diagnostics
	mode := hub.OperatingMode()
	switch mode {
	case hub.ModeLocal:
		log.Info("Operating mode: local (no hub configured)")
	case hub.ModeHubConnected:
		log.Info("Operating mode: hub-connected (endpoint: %s)", os.Getenv(hub.EnvHubEndpoint))
	case hub.ModeHosted:
		log.Info("Operating mode: hosted (endpoint: %s)", os.Getenv(hub.EnvHubEndpoint))
	}

	// nfs shared-dir writers: clear the umask's group bits (022 -> 002) when
	// the runtime granted shared-dir groups (SCION_SUPPLEMENTAL_GIDS checked
	// against this process's own groups), so new files stay group-writable
	// where the export cannot hold the leaf's default ACL (ptone/scion#3155).
	// Every child started below inherits it: harness, services, lifecycle
	// hooks, the provision wrapper and the substrate exec endpoint. This
	// must run BEFORE setupHostUser: its rootless keep-id early drop calls
	// setgroups([scion]), after which the granted groups (and so this
	// decision) are no longer visible.
	if applied, previous, current := suppgroups.ApplySharedDirUmask(); applied {
		log.Info("nfs shared-dir groups granted: umask %04o (was %04o)", current, previous)
	}

	// Set up scion user UID/GID to match host user
	targetUID, targetGID, rootless := runSetupHostUser(opts.RequirePrivilegeDrop)
	log.Info("setupHostUser result: targetUID=%d, targetGID=%d, rootless=%v (now euid=%d, egid=%d)", targetUID, targetGID, rootless, os.Geteuid(), os.Getegid())

	// Fail closed rather than start the harness as root (see
	// InitRunOptions.RequirePrivilegeDrop's doc comment). No secrets in this
	// error: setupHostUser's own preceding log lines carry the specific
	// reason (missing capability, unmapped UID, etc.).
	if err := requirePrivilegeDropOrFail(targetUID, targetGID, opts.RequirePrivilegeDrop); err != nil {
		log.Error("%v", err)
		// Report the failure the same way the git-clone failure path below
		// does (local agent-info state to PhaseError, plus a best-effort
		// direct Hub report), instead of only logging it — see
		// exitCodePrivilegeDropRequired's doc comment for why this is
		// defence in depth rather than the primary fail-closed mechanism.
		reportInitFailure(resolveAgentHome(targetUID, rootless), err)
		return exitCodePrivilegeDropRequired
	}

	// Chown the log file so the scion user can write to it even if it was created by root
	if targetUID != 0 {
		if err := log.Chown(targetUID, targetGID); err != nil {
			log.Error("Failed to chown log file: %v", err)
		}
	}

	// Resolve the scion user's home directory early. Init runs as root
	// (HOME=/root), but agent-info.json and other agent state files live
	// in the scion user's home directory. This must happen before the
	// StatusHandler is created so it writes to the correct path.
	agentHome := resolveAgentHome(targetUID, rootless)

	// Move the bootstrap transport credential out of the environment and
	// into the transport token file before anything else can inherit it.
	// Child processes read the file (via SCION_TRANSPORT_TOKEN_FILE), which
	// the refresh loop keeps current; the env value is only valid for about
	// an hour. When staged secrets are also present, the single re-exec
	// below restarts init with the cleaned environment for both.
	transportCleared := stageTransportToken(targetUID, targetGID)

	// Stage secrets from the SCION_STAGED_SECRETS env var. The broker
	// serializes file and variable secrets into this single base64 blob
	// instead of bind-mounting them from the host filesystem. We decode and
	// write them before anything else so they are available to hooks and
	// the harness. This must happen before telemetry pipeline initialization
	// because the GCP credentials file (pointed to by SCION_OTEL_GCP_CREDENTIALS)
	// must exist on disk when the telemetry pipeline starts.
	if encoded := os.Getenv(stagedsecrets.EnvVar); encoded != "" {
		staged, err := stagedsecrets.Decode(encoded)
		if err != nil {
			log.Error("Failed to decode staged secrets: %v", err)
			// Structural (base64/JSON) decode errors, never secret content —
			// same reasoning as the git-clone failure message below.
			reportInitFailure(agentHome, fmt.Errorf("failed to decode staged secrets: %w", err))
			return 1
		}
		if err := stagedsecrets.Write(agentHome, staged); err != nil {
			log.Error("Failed to write staged secrets: %v", err)
			reportInitFailure(agentHome, fmt.Errorf("failed to write staged secrets: %w", err))
			return 1
		}
		_ = os.Unsetenv(stagedsecrets.EnvVar)
		log.Info("Staged %d file secret(s) and %d variable secret(s)",
			len(staged.FileSecrets), len(staged.VariableSecrets))

		// Re-exec to purge SCION_STAGED_SECRETS from /proc/1/environ.
		//
		// os.Unsetenv removes the variable from Go's in-process copy, but
		// /proc/<pid>/environ is populated by the kernel from the execve(2)
		// environment and is never updated afterward. In rootless and keep-id
		// modes the child process shares a UID with PID 1 and can read
		// /proc/1/environ, exposing the raw secret blob for the lifetime of
		// the container.
		//
		// Re-execing with the cleaned os.Environ() causes a fresh execve(2),
		// which replaces the kernel's copy. On the second exec the env var is
		// absent, so the if-block above is skipped and init continues normally.
		//
		// All setup that ran before this point (StartReaper, setupHostUser,
		// log.Chown) is either superseded by the new process image or
		// idempotent on the second pass.
		//
		// See: miller79/scion#7
		//
		// Skipped for an embedded caller (see InitRunOptions.DisableReExec).
		if !opts.DisableReExec {
			if err := reExecWithCleanEnv(); err != nil {
				log.Error("Re-exec to clear /proc environ failed: %v (secret remains in /proc)", err)
				// Fall through — child-process inheritance is still blocked by
				// os.Unsetenv, so this degrades to the pre-fix behavior.
			}
		} else {
			log.Info("RunInit embedded: skipping environ-purge re-exec")
		}
	} else if transportCleared {
		// Restart init with the cleaned environment, as above. On the
		// second pass the env var is absent and the file already exists,
		// so this branch is not taken again. Skipped for an embedded
		// caller (see InitRunOptions.DisableReExec).
		if !opts.DisableReExec {
			if err := reExecWithCleanEnv(); err != nil {
				log.Error("Re-exec with cleaned environment failed: %v", err)
			}
		} else {
			log.Info("RunInit embedded: skipping environ-purge re-exec")
		}
	}

	// Start telemetry pipeline if configured. This must happen after
	// staged secrets are written so that the GCP credentials file
	// referenced by SCION_OTEL_GCP_CREDENTIALS exists on disk.
	var telemetryPipeline *telemetry.Pipeline
	telemetryCtx := context.Background()
	if pipeline := telemetry.New(); pipeline != nil {
		var telemetryCancel context.CancelFunc
		telemetryCtx, telemetryCancel = context.WithCancel(context.Background())
		if err := pipeline.Start(telemetryCtx); err != nil {
			log.Error("Failed to start telemetry: %v", err)
			telemetryCancel()
			// Continue anyway - telemetry failure shouldn't block agent
		} else {
			telemetryPipeline = pipeline
			log.Info("Telemetry pipeline started")
			defer func() {
				if err := stopTelemetryWithTimeout(telemetryPipeline.Stop, telemetryStopBudget); err != nil {
					log.Error("Failed to stop telemetry: %v", err)
				}
				telemetryCancel()
			}()
		}
	}

	// Initialize lifecycle hooks manager. newLifecycleManager also reports
	// the value this run wants hub.EnforceTokenFileOwnerChecks called with —
	// both come from the identical requirePrivilegeDrop input, bundled into
	// one seam so a test can assert every part of "turn on enforced mode"
	// together, rather than trusting two independent call sites to stay in
	// sync. Still runs before any ReadTokenFile/ChownTokenFile call in the
	// rest of RunInit — see EnforceTokenFileOwnerChecks's own doc comment
	// for why every other caller leaves this at its default.
	lifecycleManager, enforceTokenOwnerChecks := runNewLifecycleManager(agentHome, targetUID, targetGID, opts.RequirePrivilegeDrop)
	runEnforceTokenFileOwnerChecks(enforceTokenOwnerChecks)

	// Register status and logging handlers for lifecycle events
	// These handlers update agent-info.json and agent.log on container lifecycle events
	statusHandler := handlers.NewStatusHandler()
	statusHandler.StatusPath = filepath.Join(agentHome, "agent-info.json")
	loggingHandler := handlers.NewLoggingHandler()

	for _, eventName := range []string{hooks.EventPreStart, hooks.EventPostStart, hooks.EventPreStop, hooks.EventSessionEnd} {
		lifecycleManager.RegisterHandler(eventName, statusHandler.Handle)
		lifecycleManager.RegisterHandler(eventName, loggingHandler.Handle)
	}

	// Create telemetry handler for hook-to-span conversion
	// Note: The hook command is invoked separately by harnesses, so telemetry
	// handler registration happens in hook.go. This handler is for lifecycle events.
	var telemetryHandler *handlers.TelemetryHandler
	var lifecycleProviders *telemetry.Providers
	if telemetryPipeline != nil && telemetryPipeline.Config() != nil {
		redactor := telemetry.NewRedactor(telemetryPipeline.Config().Redaction)

		// Create real providers for span + log export (batch mode for long-lived init)
		provCtx := context.Background()
		var provErr error
		lifecycleProviders, provErr = telemetry.NewProviders(provCtx, telemetryPipeline.Config(), true)
		if provErr != nil {
			log.Error("Failed to create lifecycle telemetry providers: %v", provErr)
		}

		telemetryHandler = registerLifecycleTelemetryHandler(lifecycleManager, lifecycleProviders, redactor)
		log.Info("Telemetry handler initialized for hook-to-span conversion")
	}
	if lifecycleProviders != nil {
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := lifecycleProviders.Shutdown(shutdownCtx); err != nil {
				log.Error("Failed to shutdown lifecycle telemetry providers: %v", err)
			}
		}()
	}

	// Detect whether a container-script harness has staged a manifest that
	// requires pre-start provisioning. When required, pre-start hook failures
	// must abort startup rather than silently launching a misconfigured child.
	harnessReq, harnessReqErr := hooks.LoadHarnessManifestRequirement(agentHome)
	if harnessReqErr != nil {
		log.Error("Failed to load harness manifest: %v", harnessReqErr)
		// Fatal: the staged manifest cannot be interpreted, or names a
		// provisioner that cannot run (legacy "builtin").
		reportInitFailure(agentHome, fmt.Errorf("failed to load harness manifest: %w", harnessReqErr))
		return 1
	}

	// Detect whether a project pre-start hook was staged by the broker.
	// The presence of this file means the operator explicitly configured a hook
	// and startup should be aborted if it fails (abort-on-failure policy).
	projectHookPath := resolveProjectHookPath(agentHome, opts.RequirePrivilegeDrop)
	_, projectHookStatErr := os.Stat(projectHookPath)
	projectHookStaged := projectHookStatErr == nil

	// Clone git workspace BEFORE pre-start hooks so that provisioners
	// (e.g. antigravity) see the populated workspace rather than an empty
	// one.  The clone depends only on environment variables and agent
	// config — not on anything produced by pre-start hooks.  Running it
	// first also prevents provisioner-created files (e.g. .agents/) from
	// causing isWorkspaceEmpty to return false and skipping the clone.
	// See: https://github.com/ptone/scion/issues/739
	cloneStepStart := time.Now()
	cloneErr := runGitCloneWorkspace(targetUID, targetGID, agentHome, opts.RequirePrivilegeDrop)
	slog.Info("sciontool init: clone step complete", "elapsed_ms", time.Since(cloneStepStart).Milliseconds(), "ok", cloneErr == nil)
	if err := cloneErr; err != nil {
		log.Error("Git clone failed: %v", err)

		// Update local agent-info.json to error state so local status readers
		// and the broker heartbeat see the failure and error message.
		errMsg := fmt.Sprintf("git clone failed: %v", err)
		_ = statusHandler.UpdatePhase(state.PhaseError, "", "")
		_ = statusHandler.SetMessage(errMsg)

		// Report error to Hub directly so the agent doesn't stay stuck in "cloning" state.
		// This is best-effort; the broker heartbeat will also pick up the error from
		// agent-info.json as a fallback if this call fails (e.g. network unreachable).
		if hubClient := hub.NewClient(); hubClient != nil && hubClient.IsConfigured() {
			hubCtx, hubCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if hubErr := hubClient.ReportState(hubCtx, state.PhaseError, "", errMsg); hubErr != nil {
				log.Error("Failed to report clone error to Hub: %v", hubErr)
			}
			hubCancel()
		} else {
			log.Info("Hub client not configured, clone error will be relayed via broker heartbeat")
		}
		return 1
	}

	// Run pre-start hooks (after setup, before child process)
	log.Info("Running pre-start hooks...")
	preStartStart := time.Now()
	preStartErr := lifecycleManager.RunPreStart()
	slog.Info("sciontool init: pre-start hooks complete", "elapsed_ms", time.Since(preStartStart).Milliseconds(), "ok", preStartErr == nil)
	if err := preStartErr; err != nil {
		log.Error("Pre-start hooks failed: %v", err)
		if harnessReq.Required || projectHookStaged {
			// On restart, check for an existing env overlay from a previous
			// successful run. If it exists, we can fall through and let
			// LoadEnvOverlay use the existing file instead of aborting.
			// This makes restarts resilient to transient provisioner failures
			// while still requiring success on first creation.
			var fallbackExists bool
			if harnessReq.EnvOverlayPath != "" {
				existingOverlay, _, pathErr := harnessReq.ResolveEnvOverlay(agentHome)
				if pathErr != nil {
					log.Error("Cannot locate previous env overlay: %v", pathErr)
				} else if _, statErr := os.Stat(existingOverlay); statErr == nil {
					log.Info("WARNING: Pre-start provisioning failed but previous env overlay exists at %s, using fallback", existingOverlay)
					fallbackExists = true
				}
			}
			if !fallbackExists {
				log.Error("Pre-start provisioning is required; aborting startup")
				_ = statusHandler.UpdatePhase(state.PhaseError, "", "")
				_ = statusHandler.SetMessage(fmt.Sprintf("pre-start hook failed: %v", err))
				return 1
			}
		}
		// Continue anyway — non-required harness hooks failing shouldn't prevent startup
	}

	// After pre-start hooks, fix up ownership of any files provisioners
	// created as root. Provisioning runs before privilege drop, so scripts
	// may write files owned by root:root into the bind-mounted workspace
	// or the agent home directory. The non-root broker cannot delete
	// root-owned files later, so we chown them now.
	runPostPreStartOwnershipFixup(targetUID, targetGID, agentHome, opts.RequirePrivilegeDrop)

	// Resolve the harness working directory now — after runGitCloneWorkspace
	// and the post-pre-start-hook ownership fixup above have both run, and
	// before anything that starts a long-running component on the harness's
	// behalf (sidecar services, the metadata server, the hub secret fetch)
	// or builds the supervisor.Config — so a resolver that depends on the
	// workspace being present and searchable by the scion uid
	// (substrate-serve's, in particular) sees it in its final state rather
	// than whatever the broker's bind mount left it in, and a resolver
	// failure exits before any of those start. See
	// InitRunOptions.ResolveWorkingDir's doc comment for why this placement
	// matters and what nil means for every other caller. ResolveWorkingDir's
	// result supersedes opts.WorkingDir outright when both are set — see
	// that field's own doc comment for why no caller does today.
	if opts.ResolveWorkingDir != nil {
		workingDir, err := opts.ResolveWorkingDir()
		if err != nil {
			log.Error("%v", err)
			// Wrapped with a fixed prefix, like every other reportInitFailure
			// call site below, rather than passed through directly: err's
			// own text is bounded (ResolveWorkingDir's implementations only
			// ever name candidate paths and uids tried — see
			// resolveSubstrateHarnessCwd's doc comment), but reportInitFailure's
			// own contract is "callers only pass fixed, secret-free errors",
			// and passing a resolver's own err straight through reads like an
			// exception to that rule rather than an instance of it.
			reportInitFailure(agentHome, fmt.Errorf("failed to resolve harness working directory: %w", err))
			return exitCodeNoUsableHarnessCwd
		}
		opts.WorkingDir = workingDir
	}
	// Mirror the harness child's own resolved cwd into the lifecycle
	// manager now that it is final, so a dropped post-start/pre-stop/
	// session-end hook (executeScriptEnforced) gets the same cwd the
	// harness process itself runs in — "env and cwd handled the same way as
	// the harness process". A pre-start hook runs before this line, so a
	// dropped pre-start hook's cwd is whatever init's own cwd already is,
	// same as this field's zero-value contract.
	lifecycleManager.WorkloadWorkingDir = opts.WorkingDir

	// Load and validate the env overlay produced by the pre-start
	// provisioner, and hand its usage-source selection to the
	// already-running telemetry pipeline.
	harnessEnvOverlay, nativeTelemetryPolicy, overlayErr := loadHarnessEnvOverlay(telemetryCtx, harnessReq, agentHome, telemetryPipeline)
	if overlayErr != nil {
		reportInitFailure(agentHome, overlayErr)
		return 1
	}

	// Configure git credentials for shared-workspace projects (git-workspace hybrid).
	// The workspace is pre-cloned on the host; agents need credentials to push/pull.
	if resolveIsSharedGitWorkspace() {
		if err := configureSharedWorkspaceGit(agentHome, targetUID, targetGID, opts.RequirePrivilegeDrop); err != nil {
			log.Error("Refusing shared workspace git configuration: %v", err)
		}
	}

	// Write critical environment variables to a shell-sourceable file so that
	// processes launched by harnesses (which may re-exec with a filtered env)
	// can recover the full SCION environment. The file is sourced by .bashrc/.zshrc.
	writeEnvFile(agentHome, targetUID, targetGID)

	// Read and start sidecar services
	var svcManager *services.Manager
	// Workaround: Claude Code creates a dangling symlink at
	// ~/.claude/debug/latest that causes apple-container removal to hang.
	// Pre-create the directory as read-only (0555) so no symlinks can be
	// created inside it. We use chmod rather than chown because chown is
	// silently a no-op on VirtioFS mounts used by the Apple VZ runtime.
	if isClaude(childArgs) {
		debugDir := filepath.Join(agentHome, ".claude", "debug")
		runBlockClaudeDebugSymlink(debugDir, opts.RequirePrivilegeDrop)
	}

	servicesPath := filepath.Join(agentHome, ".scion", "scion-services.yaml")
	log.Debug("Looking for services config at: %s", servicesPath)
	if data, err := runReadServicesYAML(servicesPath, opts.RequirePrivilegeDrop); err == nil {
		var specs []api.ServiceSpec
		if err := yaml.Unmarshal(data, &specs); err != nil {
			log.Error("Failed to parse scion-services.yaml: %v", err)
		} else {
			// Authoritative gate for the service-Name-as-path-component
			// content-trust class: reject anything that isn't safe to use
			// as a single path component BEFORE any consumer of Name
			// (openLogs' log-path builder, but also every log tag this
			// package emits) ever sees it. See ValidateServiceName's own
			// doc comment for the exact rule and why this runs
			// unconditionally, on every runtime. An invalid Name drops
			// only that one service (logged, name quoted/capped/escaped —
			// never emit a raw workload-chosen string into the log) —
			// every other valid service still gets its own chance to
			// start, consistent with the per-service drop behaviour
			// openLogs failures already have.
			specs = validateServiceSpecs(specs)
			if len(specs) > 0 {
				log.Info("Starting %d sidecar service(s)...", len(specs))
				svcManager = services.New(gracePeriod)
				svcCtx := context.Background()
				servicesStart := time.Now()
				svcErr := runServicesStart(svcCtx, svcManager, specs, targetUID, targetGID, "scion", opts.RequirePrivilegeDrop)
				slog.Info("sciontool init: sidecar services started", "elapsed_ms", time.Since(servicesStart).Milliseconds(),
					"count", len(specs), "ok", svcErr == nil)
				if err := svcErr; err != nil {
					log.Error("Failed to start services: %v", err)
					// Continue — service failure shouldn't block harness
				}
			}
		}
	}

	// Initialize hubClient early so the metadata server's fetch callbacks
	// can use it without data races or startup race conditions.
	hubClient := hub.NewClient()

	// Wire the OnSessionEnd callback so the aggregator sends finalized
	// session metrics to the Hub when a session completes. The closure
	// captures hubClient, which is already initialized above.
	if telemetryHandler != nil && hubClient != nil && hubClient.IsConfigured() {
		telemetryHandler.OnSessionEnd = func(summary telemetry.SessionSummary) {
			payload := hub.SummaryToMetricsPayload(summary)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := hubClient.ReportMetrics(ctx, payload); err != nil {
				log.Error("Failed to report session metrics to hub: %v", err)
			} else {
				log.Info("Session metrics reported to hub for session %s", summary.SessionID)
			}
		}
	}

	// Start GCP metadata server if configured
	var metadataServer *metadata.Server
	if metaCfg := metadata.ConfigFromEnv(); metaCfg != nil {
		// Remove pre-existing gcloud configuration state so that gcloud
		// re-initializes and discovers the emulated metadata server via
		// GCE_METADATA_ROOT. gcloud only checks for the metadata server
		// during its first-run configuration detection.
		// We preserve application_default_credentials.json which may be
		// bind-mounted as a secret (gcloud-adc).
		runCleanGcloudConfigForMetadata(filepath.Join(agentHome, ".config", "gcloud"), opts.RequirePrivilegeDrop)
		// Wire up dynamic token retrieval so the metadata server always
		// uses the latest agent token after refresh, not the startup value.
		metaCfg.TokenFunc = func() string {
			return hub.ReadTokenFile()
		}
		// Delegate GCP token fetching to the hub client so the metadata
		// server uses the correct auth headers (X-Scion-Agent-Token) and
		// OIDC transport layer. The hub client is created after the metadata
		// server starts, so the closures capture the hubClient variable
		// which is set later. Token requests only arrive after the child
		// process has started, so the hub client is always available by then.
		metaCfg.FetchGCPToken = func(ctx context.Context, scopes []string) (*metadata.GCPAccessTokenResponse, error) {
			hc := hubClient
			if hc == nil || !hc.IsConfigured() {
				return nil, fmt.Errorf("hub client not initialized")
			}
			hubResp, err := hc.FetchGCPToken(ctx, scopes)
			if err != nil {
				return nil, err
			}
			return &metadata.GCPAccessTokenResponse{
				AccessToken: hubResp.AccessToken,
				ExpiresIn:   hubResp.ExpiresIn,
				TokenType:   hubResp.TokenType,
			}, nil
		}
		metaCfg.FetchGCPIdentityToken = func(ctx context.Context, audience string) (string, error) {
			hc := hubClient
			if hc == nil || !hc.IsConfigured() {
				return "", fmt.Errorf("hub client not initialized")
			}
			return hc.FetchGCPIdentityToken(ctx, audience)
		}
		metadataServer = metadata.New(*metaCfg)
		metaCtx := context.Background()
		if err := runMetadataServerStart(metaCtx, metadataServer); err != nil {
			log.Error("Failed to start metadata server: %v", err)
			// Continue — metadata failure shouldn't block harness
		} else {
			log.Info("GCP metadata server started (mode=%s, port=%d)", metaCfg.Mode, metaCfg.Port)
		}
	}

	// Pre-flight checks: verify key paths are accessible before launching child
	if rootless {
		if _, err := os.Stat(agentHome); err != nil {
			log.Error("Pre-flight: agent home %s is not accessible: %v", agentHome, err)
		} else if f, err := os.CreateTemp(agentHome, ".scion-preflight-*"); err != nil {
			log.Error("Pre-flight: cannot write to agent home %s: %v (uid=%d)", agentHome, err, os.Geteuid())
		} else {
			_ = os.Remove(f.Name())
			_ = f.Close()
			log.Debug("Pre-flight: agent home %s is writable (uid=%d)", agentHome, os.Geteuid())
		}
		if _, err := exec.LookPath("tmux"); err != nil {
			log.Error("Pre-flight: tmux not found on PATH: %v", err)
		}
	}

	// Fetch secrets from the hub if SCION_SECRET_KEYS is set (#127, P2d).
	// Absent or empty is the normal case today and is a clean no-op.
	var secretOverrides map[string]string
	if secretKeysRaw := os.Getenv("SCION_SECRET_KEYS"); secretKeysRaw != "" {
		keys := splitSecretKeys(secretKeysRaw)
		if len(keys) > 0 && hubClient != nil && hubClient.IsConfigured() {
			secretFetchStart := time.Now()
			secretOverrides = runFetchSecretOverrides(hubClient, keys)
			slog.Info("sciontool init: secret fetch complete", "elapsed_ms", time.Since(secretFetchStart).Milliseconds(), "count", len(keys))
		} else if len(keys) > 0 {
			log.Error("SCION_SECRET_KEYS is set but hub client is not configured — cannot fetch secrets")
		}
	}

	// Create supervisor with configuration
	config := harnessSupervisorConfig(opts, gracePeriod, targetUID, targetGID, rootless, harnessEnvOverlay, nativeTelemetryPolicy, secretOverrides)
	sup := supervisor.New(config)

	// Create a cancellable context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up signal handling with pre-stop hook for graceful shutdown.
	// requestedShutdown tracks whether the process received an intentional
	// SIGTERM/SIGINT so classifyExit can distinguish a clean stop from a crash.
	var requestedShutdown atomic.Bool
	if opts.forwardsTermSignal() {
		sigHandler := supervisor.NewSignalHandler(sup, cancel).
			WithPreStopHook(func() error {
				requestedShutdown.Store(true)
				log.Info("Running pre-stop hooks...")
				return lifecycleManager.RunPreStop()
			})
		sigHandler.Start()
		defer sigHandler.Stop()
	} else {
		// DisableTermSignalForwarding (substrate-serve): the caller owns
		// SIGTERM handling for the whole process, so RunInit must not also
		// listen for it here — doing so would shut down the child out from
		// under the caller's own signal handling. See
		// InitRunOptions.DisableTermSignalForwarding.
		log.Info("Termination-signal forwarding disabled for this init run; the child will not be stopped on SIGTERM/SIGINT by this code path")
	}

	// Run the child process under supervision
	// We use a goroutine to allow post-start hooks to run after process starts
	exitChan := make(chan struct {
		code int
		err  error
	}, 1)

	// since_init_ms (not elapsed_ms, unlike the other sciontool init: lines
	// above): this is the time since process start, the same quantity as
	// startup_ms below, not the duration of a single step. Naming it
	// differently keeps a step-line summation from silently double-counting
	// it.
	slog.Info("sciontool init: launching harness", "since_init_ms", time.Since(initStart).Milliseconds())
	go func() {
		code, err := sup.Run(ctx, childArgs)
		exitChan <- struct {
			code int
			err  error
		}{code, err}
	}()

	// Heartbeat and token refresh control variables - declared here so they're accessible during shutdown and auth reset
	var heartbeatCancel context.CancelFunc
	var heartbeatDone <-chan struct{}
	var tokenRefreshCancel context.CancelFunc
	var tokenRefreshDone <-chan struct{}
	var ghTokenRefreshCancel context.CancelFunc
	var ghTokenRefreshDone <-chan struct{}

	// Wait a moment for process to start, then run post-start hooks
	// Use a short timeout to detect immediate startup failures
	log.Debug("Waiting for child process startup (100ms check)...")
	select {
	case result := <-exitChan:
		// Child exited immediately - likely a startup error
		if result.err != nil {
			log.Error("Child exited immediately with error: %v (uid=%d, gid=%d)", result.err, os.Geteuid(), os.Getegid())
			reportInitFailure(agentHome, fmt.Errorf("child process failed to start: %w", result.err))
			return 1
		}
		log.Info("Child exited immediately with code %d (uid=%d, gid=%d)", result.code, os.Geteuid(), os.Getegid())
		return result.code
	case <-time.After(100 * time.Millisecond):
		// Process appears to be running, execute post-start hooks
		log.Info("Running post-start hooks...")
		if err := lifecycleManager.RunPostStart(); err != nil {
			log.Error("Post-start hooks failed: %v", err)
			// Continue anyway
		}

		// Report running status to Hub if in hosted mode
		log.Debug("Hub client check: client=%v, configured=%v", hubClient != nil, hubClient != nil && hubClient.IsConfigured())
		log.Debug("Hub env: SCION_HUB_ENDPOINT=%q, SCION_HUB_URL=%q, token_file=%v, SCION_AGENT_ID=%q",
			os.Getenv("SCION_HUB_ENDPOINT"), os.Getenv("SCION_HUB_URL"), hub.ReadTokenFile() != "", os.Getenv("SCION_AGENT_ID"))
		if hubClient != nil && hubClient.IsConfigured() {
			hubCtx, hubCancel := context.WithTimeout(context.Background(), 10*time.Second)
			startedAtStr := time.Now().UTC().Format(time.RFC3339)
			zeroCount := 0
			s := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityWorking}
			// startup_ms (elapsed since sciontool init started) rides in
			// Metadata rather than a new StatusUpdate field: it is
			// observability only, read by the hub purely for a log line,
			// never persisted to a column (see updateAgentStatus).
			startupMs := strconv.FormatInt(time.Since(initStart).Milliseconds(), 10)
			if err := hubClient.UpdateStatus(hubCtx, hub.StatusUpdate{
				Phase:             state.PhaseRunning,
				Activity:          state.ActivityWorking,
				Status:            s.DisplayStatus(),
				Message:           "Agent started",
				StartedAt:         startedAtStr,
				CurrentTurns:      &zeroCount,
				CurrentModelCalls: &zeroCount,
				Metadata:          map[string]string{"startup_ms": startupMs},
			}); err != nil {
				log.Error("Failed to report running status to Hub: %v", err)
			} else {
				log.Info("Reported running status to Hub (startedAt=%s)", startedAtStr)
			}
			hubCancel()

			// Start heartbeat loop in background
			var heartbeatCtx context.Context
			heartbeatCtx, heartbeatCancel = context.WithCancel(context.Background())
			heartbeatDone = hubClient.StartHeartbeat(heartbeatCtx, &hub.HeartbeatConfig{
				Interval: hub.DefaultHeartbeatInterval,
				Timeout:  hub.DefaultHeartbeatTimeout,
				OnError: func(err error) {
					log.Error("Heartbeat failed: %v", err)
				},
				OnSuccess: func() {
					log.Debug("Heartbeat sent successfully")
				},
			})
			log.Info("Started Hub heartbeat loop (interval: %s)", hub.DefaultHeartbeatInterval)

			// Skip both the port-forward tunnel manager and auto-expose when
			// the caller sets InitRunOptions.DisablePortForwarding — see that
			// field's doc comment for why substrate-serve is the one caller
			// that does.
			if opts.DisablePortForwarding {
				log.Info("port forwarding disabled: skipping port-forward tunnel manager and auto-expose")
			} else {
				go newPortForwarding(hubClient, opts.DisableConduit, os.Getenv).run(ctx)

				// Auto-expose: detect and register listening ports
				if autoExposeCfg := autoexpose.ConfigFromEnv(); autoExposeCfg.Enabled && hubClient != nil {
					reconciler := autoexpose.NewReconciler(hubClient, autoExposeCfg)
					reconciler.SetMessageClient(&hubMessageAdapter{client: hubClient})
					go reconciler.Run(ctx)
					log.Info("Started auto-expose port scanner (interval: %s, mode: %s)", autoExposeCfg.Interval, autoExposeCfg.FilterMode)
				}
			}

			// Read the agent token from the canonical token file (written by
			// the host-side agent manager before the container started).
			// Init runs as root — chown the file so the scion user can read
			// it. ChownTokenFile resolves the file via a symlink-safe fd
			// chain and fchowns the open fd, rather than a path-based chown
			// that a symlink swapped in after this point (this runs after
			// sup.Run has started, so the workload is already alive) could
			// redirect to an arbitrary file.
			token := hub.ReadTokenFile()
			if token != "" && targetUID > 0 {
				if err := hub.ChownTokenFile(targetUID, targetGID); err != nil {
					log.Error("Failed to chown token file to UID=%d: %v", targetUID, err)
				}
			}

			// Start token refresh loop if token has an expiry
			if tokenExpiry, err := hub.ParseTokenExpiry(token); err != nil {
				log.Debug("Could not parse token expiry, skipping token refresh: %v", err)
			} else {
				// Schedule refresh 2 hours before expiry
				refreshAt := tokenExpiry.Add(-2 * time.Hour)
				if refreshAt.Before(time.Now()) {
					// Token is already within the refresh window or expired —
					// refresh immediately in both cases. On resume the persisted
					// token may have expired while the agent was stopped; always
					// starting the refresh loop lets StartTokenRefresh retry with
					// backoff and fire OnAuthLost if recovery fails, instead of
					// silently giving up.
					refreshAt = time.Now()
					if time.Now().Before(tokenExpiry) {
						log.Info("Token within refresh window, refreshing immediately (expires: %s)", tokenExpiry.Format(time.RFC3339))
					} else {
						log.Error("AUTH_EXPIRED: Agent token has expired at %s - attempting refresh", tokenExpiry.Format(time.RFC3339))
					}
				} else {
					log.Info("Token refresh scheduled at %s (token expires: %s)",
						refreshAt.Format(time.RFC3339), tokenExpiry.Format(time.RFC3339))
				}

				var tokenRefreshCtx context.Context
				tokenRefreshCtx, tokenRefreshCancel = context.WithCancel(context.Background())
				tokenRefreshDone = hubClient.StartTokenRefresh(tokenRefreshCtx, &hub.TokenRefreshConfig{
					RefreshAt: refreshAt,
					ChownUID:  targetUID,
					ChownGID:  targetGID,
					OnRefreshed: func(newExpiry time.Time) {
						log.Info("Token refreshed successfully, new expiry: %s", newExpiry.Format(time.RFC3339))
					},
					OnError: func(err error) {
						log.Error("Token refresh failed: %v", err)
					},
					OnAuthLost: func() {
						log.Error("AUTH_LOST: Agent token has expired and could not be refreshed - hub communication is no longer possible")
						log.Error("AUTH_LOST: Agent limits (max-duration, max-turns, max-model-calls) are enforced locally and remain active")
					},
				})
			}
		} else {
			log.Debug("Hub client not configured - skipping status report")
		}

		// Warn if user-provided GITHUB_TOKEN overlaps with GitHub App
		if util.ParseBoolEnv(hub.EnvUserGitHubToken, false) {
			log.Info("User-provided GITHUB_TOKEN detected alongside GitHub App installation")
			log.Info("The user's GITHUB_TOKEN will be used for gh CLI; GitHub App tokens will be used for git credential helper")
		}

		// Start GitHub App token refresh loop if enabled
		if hub.IsGitHubAppEnabled() && hubClient != nil && hubClient.IsConfigured() {
			tokenPath := hub.GitHubTokenPath()

			// Write the initial token to the token file so consumers can read it.
			// Init runs as root, so chown the file to the scion user so the
			// credential helper (which runs as the scion user) can read it.
			initialToken := os.Getenv("GITHUB_TOKEN")
			if initialToken != "" {
				// Ownership is applied by WriteGitHubTokenFile itself
				// (fchown on the open fd, before the rename onto the final
				// path), not by a separate path-based os.Chown afterwards.
				if err := hub.WriteGitHubTokenFile(tokenPath, initialToken, targetUID, targetGID); err != nil {
					log.Error("Failed to write initial GitHub token file: %v", err)
				} else {
					log.Info("Wrote initial GitHub token to %s", tokenPath)
				}
			}

			// Parse initial token expiry to schedule first refresh
			expiryStr := os.Getenv(hub.EnvGitHubTokenExpiry)
			if expiryStr != "" {
				ghTokenExpiry, err := time.Parse("2006-01-02T15:04:05Z", expiryStr)
				if err != nil {
					ghTokenExpiry, err = time.Parse(time.RFC3339, expiryStr)
				}
				if err != nil {
					log.Error("Failed to parse GitHub token expiry %q: %v", expiryStr, err)
				} else {
					// Write the initial expiry so the credential helper can
					// detect stale tokens even before the first refresh
					// cycle. Ownership is applied on the fd, as above.
					if err := hub.WriteGitHubTokenExpiry(tokenPath, ghTokenExpiry, targetUID, targetGID); err != nil {
						log.Error("Failed to write initial GitHub token expiry file: %v", err)
					}
					// Schedule first refresh 10 minutes before expiry (tokens last 1 hour)
					ghRefreshAt := ghTokenExpiry.Add(-10 * time.Minute)
					if ghRefreshAt.Before(time.Now()) {
						if time.Now().Before(ghTokenExpiry) {
							ghRefreshAt = time.Now()
							log.Info("GitHub token within refresh window, refreshing immediately (expires: %s)", ghTokenExpiry.Format(time.RFC3339))
						} else {
							log.Error("GitHub token already expired at %s", ghTokenExpiry.Format(time.RFC3339))
							ghRefreshAt = time.Time{}
						}
					} else {
						log.Info("GitHub token refresh scheduled at %s (expires: %s)",
							ghRefreshAt.Format(time.RFC3339), ghTokenExpiry.Format(time.RFC3339))
					}

					if !ghRefreshAt.IsZero() {
						var ghTokenRefreshCtx context.Context
						ghTokenRefreshCtx, ghTokenRefreshCancel = context.WithCancel(context.Background())
						ghTokenRefreshDone = hubClient.StartGitHubTokenRefresh(ghTokenRefreshCtx, &hub.GitHubTokenRefreshConfig{
							RefreshAt: ghRefreshAt,
							TokenPath: tokenPath,
							ChownUID:  targetUID,
							ChownGID:  targetGID,
							OnRefreshed: func(newToken string, newExpiry time.Time) {
								log.Info("GitHub token refreshed, new expiry: %s", newExpiry.Format(time.RFC3339))
								writeEnvFile(agentHome, targetUID, targetGID)
							},
							OnError: func(err error) {
								log.Error("GitHub token refresh failed: %v", err)
							},
						})
					}
				}
			} else {
				log.Debug("No GitHub token expiry set, skipping GitHub token refresh loop")
			}
		}
	}

	// Set up SIGUSR1 handler for limits-exceeded signaling from hook processes.
	// When a hook handler detects a limit is exceeded, it sends SIGUSR1 to PID 1.
	usr1Chan := make(chan os.Signal, 1)
	signal.Notify(usr1Chan, syscall.SIGUSR1)
	defer signal.Stop(usr1Chan)

	// Set up SIGUSR2 handler for auth reset. When the broker writes a fresh
	// token to ~/.scion/scion-token and sends SIGUSR2, init re-reads the
	// token, updates the hub client, and restarts the token refresh loop.
	usr2Chan := make(chan os.Signal, 1)
	signal.Notify(usr2Chan, syscall.SIGUSR2)
	defer signal.Stop(usr2Chan)

	// Set up duration timer if max_duration is configured
	var durationTimer <-chan time.Time
	maxDurStr := os.Getenv("SCION_MAX_DURATION")
	if maxDurStr != "" {
		maxDur := api.ParseDuration(maxDurStr)
		if maxDur > 0 {
			t := time.NewTimer(maxDur)
			defer t.Stop()
			durationTimer = t.C
			log.Info("Duration limit set: %s", maxDur)
		}
	}

	// Initialize agent-limits.json for turn and model call tracking
	maxTurns := handlers.ParseEnvInt("SCION_MAX_TURNS")
	maxModelCalls := handlers.ParseEnvInt("SCION_MAX_MODEL_CALLS")
	if maxTurns > 0 || maxModelCalls > 0 {
		limitsPath := filepath.Join(agentHome, "agent-limits.json")
		// InitLimitsFile chowns the file itself (fchown on the temp file's
		// fd, before the rename) when targetUID > 0, so hook processes
		// running as the dropped-privilege scion user can read/write it.
		if err := handlers.InitLimitsFile(limitsPath, maxTurns, maxModelCalls, targetUID, targetGID); err != nil {
			log.Error("Failed to initialize agent-limits.json: %v", err)
		} else {
			log.Info("Limits initialized: max_turns=%d, max_model_calls=%d", maxTurns, maxModelCalls)
		}
		// Remove stale trigger file from a previous run
		_ = os.Remove(handlers.LimitsTriggerFile)
	}

	// Watch for limits-exceeded trigger file (works across UID boundaries).
	// This supplements SIGUSR1 which may fail when hooks run as non-root.
	triggerChan := make(chan struct{}, 1)
	triggerCtx, triggerCancel := context.WithCancel(context.Background())
	defer triggerCancel()
	if maxTurns > 0 || maxModelCalls > 0 {
		go watchLimitsTriggerFile(triggerCtx, triggerChan)
	}

	// Wait for child to exit, duration limit, SIGUSR1, SIGUSR2, or trigger file.
	// The loop allows SIGUSR2 (auth reset) to be handled without terminating.
	var result struct {
		code int
		err  error
	}
	limitsExceeded := false

waitLoop:
	for {
		select {
		case r := <-exitChan:
			result = r
			break waitLoop
		case <-durationTimer:
			limitsExceeded = true
			handleLimitsExceeded(sup, "duration", fmt.Sprintf("max_duration of %s exceeded", maxDurStr))
			result = <-exitChan
			break waitLoop
		case <-usr1Chan:
			// SIGUSR1 received from hook handler - limits already set in agent-info.json
			limitsExceeded = true
			log.TaggedInfo("LIMITS_EXCEEDED", "Received SIGUSR1: limit exceeded, initiating shutdown")
			if err := sup.Signal(syscall.SIGTERM); err != nil {
				log.Error("Failed to send SIGTERM to child: %v", err)
			}
			result = <-exitChan
			break waitLoop
		case <-usr2Chan:
			// SIGUSR2: auth reset — re-read token file and restart refresh loop.
			handleAuthReset(hubClient, &tokenRefreshCancel, &tokenRefreshDone, statusHandler, targetUID, targetGID)
			// Continue waiting — this is non-terminal.
		case <-triggerChan:
			// Trigger file detected from hook handler - limits already set in agent-info.json
			limitsExceeded = true
			log.TaggedInfo("LIMITS_EXCEEDED", "Trigger file detected: limit exceeded, initiating shutdown")
			if err := sup.Signal(syscall.SIGTERM); err != nil {
				log.Error("Failed to send SIGTERM to child: %v", err)
			}
			result = <-exitChan
			break waitLoop
		}
	}

	// Stop token refresh loops and heartbeat before reporting shutdown status to prevent races
	if ghTokenRefreshCancel != nil {
		ghTokenRefreshCancel()
		<-ghTokenRefreshDone
		log.Debug("GitHub token refresh loop stopped")
	}
	if tokenRefreshCancel != nil {
		tokenRefreshCancel()
		<-tokenRefreshDone
		log.Debug("Token refresh loop stopped")
	}
	if heartbeatCancel != nil {
		heartbeatCancel()
		<-heartbeatDone
		log.Debug("Heartbeat loop stopped")
	}

	// Clean up the GitHub token file on exit
	if hub.IsGitHubAppEnabled() {
		tokenPath := hub.GitHubTokenPath()
		if err := os.Remove(tokenPath); err != nil && !os.IsNotExist(err) {
			log.Error("Failed to clean up GitHub token file: %v", err)
		} else {
			log.Debug("Cleaned up GitHub token file: %s", tokenPath)
		}
	}

	// Report shutting down to Hub if in hosted mode
	if hubClient := hub.NewClient(); hubClient != nil && hubClient.IsConfigured() {
		hubCtx, hubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := hubClient.ReportState(hubCtx, state.PhaseStopping, "", "Agent shutting down"); err != nil {
			log.Error("Failed to report shutdown status to Hub: %v", err)
		}
		hubCancel()
	}

	// Stop metadata server
	if metadataServer != nil {
		metadataServer.Stop()
		log.Info("GCP metadata server stopped")
	}

	// Stop sidecar services before session-end hooks
	if svcManager != nil {
		log.Info("Stopping sidecar services...")
		svcShutdownCtx, svcShutdownCancel := context.WithTimeout(context.Background(), gracePeriod)
		if err := svcManager.Shutdown(svcShutdownCtx); err != nil {
			log.Error("Failed to stop services: %v", err)
		}
		svcShutdownCancel()
	}

	// Run session-end hooks (graceful shutdown)
	log.Info("Running session-end hooks...")
	if err := lifecycleManager.RunSessionEnd(); err != nil {
		log.Error("Session-end hooks failed: %v", err)
	}

	// Determine the final exit code and whether this was a crash.
	// Also recognize ExitCodeLimitsExceeded from the child process itself
	// (e.g., the harness detected limits before the supervisor signal).
	if !limitsExceeded && result.code == handlers.ExitCodeLimitsExceeded {
		limitsExceeded = true
	}

	// The harness runs as a tmux grandchild, so the supervised child's exit
	// code (result.code) reflects sh/tmux, not the harness itself. The tmux
	// agent-window wrapper records the harness's real exit code to a fixed
	// file; prefer it when present. If absent (e.g. the container was SIGKILLed
	// or OOM-killed before the harness could write), fall back to result.code.
	harnessCode := readHarnessExitCode()
	if harnessCode != nil {
		log.Info("Recovered harness exit code %d from %s", *harnessCode, state.HarnessExitCodeFile)
	}

	outcome := classifyExit(result.code, result.err, harnessCode, limitsExceeded, requestedShutdown.Load())
	finalCode := outcome.exitCode
	limitsExceeded = outcome.limitsExceeded

	// Update local agent-info.json BEFORE the Hub report so the broker
	// heartbeat can relay crash/limits state even if the Hub call is slow
	// or fails entirely.
	if outcome.isCrash {
		// HYBRID mapping: an unexpected non-zero exit becomes PhaseError with
		// the activity cleared (crash detail lives in the message + exitCode).
		// `crashed` activity is only valid on PhaseStopped per state validation.
		_ = statusHandler.UpdatePhase(state.PhaseError, "", "")
		_ = statusHandler.SetMessage(outcome.message)
	} else if limitsExceeded {
		_ = statusHandler.UpdatePhase(state.PhaseStopped, state.ActivityLimitsExceeded, "")
		_ = statusHandler.SetMessage("limits exceeded")
	}

	// Report final status to Hub, distinguishing clean stop from crash.
	if hubClient := hub.NewClient(); hubClient != nil && hubClient.IsConfigured() {
		hubCtx, hubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		var hubErr error
		if outcome.isCrash {
			s := state.AgentState{Phase: state.PhaseError}
			hubErr = hubClient.UpdateStatus(hubCtx, hub.StatusUpdate{
				Phase:    state.PhaseError,
				Activity: "",
				Status:   s.DisplayStatus(),
				Message:  outcome.message,
				ExitCode: &finalCode,
			})
		} else if limitsExceeded {
			s := state.AgentState{Phase: state.PhaseStopped, Activity: state.ActivityLimitsExceeded}
			hubErr = hubClient.UpdateStatus(hubCtx, hub.StatusUpdate{
				Phase:    state.PhaseStopped,
				Activity: state.ActivityLimitsExceeded,
				Status:   s.DisplayStatus(),
				Message:  "Agent stopped: limits exceeded",
				ExitCode: &finalCode,
			})
		} else {
			hubErr = hubClient.ReportState(hubCtx, state.PhaseStopped, "", "Agent stopped")
		}
		if hubErr != nil {
			log.Error("Failed to report final status to Hub: %v", hubErr)
		} else {
			log.Info("Reported final status to Hub (exitCode=%d, crash=%v)", finalCode, outcome.isCrash)
		}
		hubCancel()
	}

	if limitsExceeded {
		log.Info("Exiting with code %d (limits exceeded)", handlers.ExitCodeLimitsExceeded)
		return handlers.ExitCodeLimitsExceeded
	}

	if outcome.isCrash {
		// Propagate the authoritative crash code (which may have come from the
		// harness exit-code file rather than the supervised child) so the
		// container's exit status reflects the real failure.
		log.Error("Agent crashed with exit code %d", finalCode)
		return finalCode
	}

	if result.err != nil {
		log.Error("Supervisor error: %v", result.err)
		reportInitFailure(agentHome, fmt.Errorf("supervisor error: %w", result.err))
		return 1
	}

	log.Info("Child exited with code %d", result.code)
	return result.code
}

// loadHarnessEnvOverlay loads the env overlay produced by the pre-start
// provisioner, validates and strips the native telemetry policy marker, and
// hands the overlay's usage-source selection to the already-running telemetry
// pipeline. It returns the overlay for the harness child and the policy.
//
// from_file references are resolved to in-memory values so secrets are not
// written back to logs or persistent JSON. A non-nil error is fatal and is
// secret-free, so RunInit passes it to reportInitFailure: either the overlay
// is malformed or references missing files for a required container-script
// harness (the child would otherwise launch without its credentials), or the
// policy marker is invalid. A load failure for a non-required harness is
// logged and the child launches without an overlay.
func loadHarnessEnvOverlay(ctx context.Context, harnessReq hooks.HarnessManifestRequirement, agentHome string, pipeline *telemetry.Pipeline) (map[string]string, string, error) {
	if harnessReq.EnvOverlayPath == "" {
		return nil, "", nil
	}
	overlayPath, allowedRoots, err := harnessReq.ResolveEnvOverlay(agentHome)
	var overlay map[string]string
	if err == nil {
		overlay, err = hooks.LoadEnvOverlay(overlayPath, allowedRoots)
	}
	if err != nil {
		log.Error("Failed to load harness env overlay %s: %v", overlayPath, err)
		if harnessReq.Required {
			return nil, "", fmt.Errorf("invalid harness env overlay: %w", err)
		}
		return nil, "", nil
	}
	if len(overlay) == 0 {
		return nil, "", nil
	}
	var nativeTelemetryPolicy string
	if policy, ok := overlay[hooks.NativeTelemetryPolicyKey]; ok {
		if policy != "enabled" && policy != "disabled" {
			log.Error("Invalid native telemetry policy marker")
			return nil, "", errors.New("invalid native telemetry policy marker in harness env overlay")
		}
		nativeTelemetryPolicy = policy
		delete(overlay, hooks.NativeTelemetryPolicyKey)
	}
	log.Info("Loaded %d env overlay entries from %s", len(overlay), overlayPath)
	activateOverlayUsageSource(ctx, pipeline, overlay, nativeTelemetryPolicy)
	return overlay, nativeTelemetryPolicy, nil
}

// activateOverlayUsageSource hands the provisioner's usage-source selection
// to the already-running telemetry pipeline (ptone/scion#3391). The pipeline
// starts before the pre-start provisioner runs, so Start cannot see a
// SCION_USAGE_SOURCE that only the generated env overlay declares. Only the
// single SCION_USAGE_SOURCE value is passed, never the rest of the overlay,
// and Pipeline.ActivateUsageSource accepts only "native" and keeps runtime
// environment precedence. A disabled native telemetry policy never activates
// native usage. When the overlay selects native usage but it is not
// activated, the reason is logged so operators can tell why usage is absent.
func activateOverlayUsageSource(ctx context.Context, pipeline *telemetry.Pipeline, overlay map[string]string, nativeTelemetryPolicy string) {
	source, ok := overlay["SCION_USAGE_SOURCE"]
	if !ok || source != telemetry.UsageSourceNative {
		return
	}
	if nativeTelemetryPolicy == "disabled" {
		log.Info("Native usage derivation not activated: native telemetry policy is disabled")
		return
	}
	if pipeline == nil {
		log.Info("Native usage derivation not activated: telemetry is not running")
		return
	}
	outcome, err := pipeline.ActivateUsageSource(ctx, source)
	if err != nil {
		log.Error("Failed to activate usage source from harness env overlay: %v", err)
		return
	}
	if outcome == telemetry.UsageActivated {
		log.Info("Activated native usage derivation from harness env overlay")
		return
	}
	log.Info("Native usage derivation not activated: %s", outcome)
}

func registerLifecycleTelemetryHandler(manager *hooks.LifecycleManager, providers *telemetry.Providers, redactor *telemetry.Redactor) *handlers.TelemetryHandler {
	var tp trace.TracerProvider
	var lp otellog.LoggerProvider
	var mp metric.MeterProvider
	if providers != nil {
		tp = providers.TracerProvider
		lp = providers.LoggerProvider
		mp = providers.MeterProvider
	}
	handler := handlers.NewLifecycleTelemetryHandler(tp, lp, redactor, mp)
	for _, eventName := range []string{hooks.EventPreStart, hooks.EventPostStart, hooks.EventPreStop, hooks.EventSessionEnd} {
		manager.RegisterHandler(eventName, handler.Handle)
	}
	return handler
}

// harnessExitCodeMaxBytes bounds the read in readHarnessExitCode: the file
// only ever holds a small decimal exit code, so any read this long has
// already found something other than what the harness wrapper writes.
const harnessExitCodeMaxBytes = 32

// readHarnessExitCode reads and parses the harness exit-code file written by
// the tmux agent-window wrapper. Returns nil if the file is missing or
// unparseable (e.g. the container was SIGKILLed/OOM-killed before the
// harness could write), and also — without blocking and without reading —
// if any component of the path is a symlink or the leaf isn't a regular
// file: this process runs as root, and the workload owns the directory
// this path lives in, so a FIFO planted here must not be able to make
// root's shutdown path hang forever, and a symlink at the leaf or at an
// intermediate directory component must not be able to make it parse an
// unrelated file's contents as an exit code. O_NONBLOCK is what keeps a
// FIFO's open() itself from blocking on a reader when there is no writer.
func readHarnessExitCode() *int {
	dirFd, leaf, err := dirfd.OpenParentNoFollow(state.HarnessExitCodeFile)
	if err != nil {
		return nil
	}
	defer func() { _ = syscall.Close(dirFd) }()

	f, err := dirfd.OpenAt(dirFd, leaf, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return nil
	}

	data, err := io.ReadAll(io.LimitReader(f, harnessExitCodeMaxBytes))
	if err != nil {
		return nil
	}
	code, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return nil
	}
	return &code
}

// exitOutcome captures the classified result of a supervised agent exit.
type exitOutcome struct {
	exitCode       int
	limitsExceeded bool
	isCrash        bool
	message        string
}

// classifyExit applies the HYBRID exit mapping. It is a pure function so it can
// be unit-tested independently of the supervisor/hub machinery.
//
//   - limitsExceeded                  → stopped + limits_exceeded (handled by caller)
//   - clean exit (code 0, no error)   → stopped
//   - requestedShutdown + code -1     → stopped (signal-killed by intentional SIGTERM)
//   - unexpected non-zero exit/error  → error (crash), restartable
//
// harnessCode, when non-nil, is the authoritative harness exit code recovered
// from the exit-code file and overrides the supervised child's code for the
// crash decision. supervisorErr is the supervisor's own error (a synthetic
// failure not reflected in supervisedCode). requestedShutdown is true when
// init received SIGTERM/SIGINT, indicating the container was intentionally
// stopped — a signal-killed child (exit code -1) is expected, not a crash.
func classifyExit(supervisedCode int, supervisorErr error, harnessCode *int, limitsExceeded bool, requestedShutdown bool) exitOutcome {
	if !limitsExceeded && supervisedCode == handlers.ExitCodeLimitsExceeded {
		limitsExceeded = true
	}

	// Choose the authoritative exit code: prefer the harness file, then the
	// supervised child code.
	finalCode := supervisedCode
	if harnessCode != nil {
		finalCode = *harnessCode
	}

	if limitsExceeded {
		return exitOutcome{exitCode: handlers.ExitCodeLimitsExceeded, limitsExceeded: true}
	}

	// When init was told to shut down (SIGTERM/SIGINT), the child is killed by
	// signal and Go reports exit code -1. This is expected, not a crash.
	if requestedShutdown && finalCode == -1 {
		return exitOutcome{exitCode: 0}
	}

	// A supervisor error with a zero exit code is itself a failure.
	supervisorFailed := supervisorErr != nil && finalCode == 0
	if supervisorFailed {
		finalCode = 1
	}

	isCrash := finalCode != 0
	if !isCrash {
		return exitOutcome{exitCode: 0}
	}

	var msg string
	if supervisorFailed {
		msg = fmt.Sprintf("Agent crashed (supervisor error: %v)", supervisorErr)
	} else {
		msg = fmt.Sprintf("Agent crashed with exit code %d", finalCode)
	}
	return exitOutcome{exitCode: finalCode, isCrash: true, message: msg}
}

// handleLimitsExceeded is called when a limit is exceeded (duration timer or SIGUSR1).
// It updates the agent status, logs the event, reports to the Hub, and sends SIGTERM
// to the child process to initiate graceful shutdown.
func handleLimitsExceeded(sup *supervisor.Supervisor, limitType, message string) {
	// 1. Update agent-info.json to LIMITS_EXCEEDED (sticky)
	statusHandler := handlers.NewStatusHandler()
	if err := statusHandler.UpdateActivity(state.ActivityLimitsExceeded, ""); err != nil {
		log.Error("Failed to set limits_exceeded status: %v", err)
	}

	// 2. Log the event
	log.TaggedInfo("LIMITS_EXCEEDED", "Agent stopped: %s", message)

	// 3. Report to Hub if configured
	hubHandler := handlers.NewHubHandler()
	if hubHandler != nil {
		if err := hubHandler.ReportLimitsExceeded(message); err != nil {
			log.Error("Failed to report limits_exceeded to Hub: %v", err)
		}
	}

	// 4. Send SIGTERM to child process
	if err := sup.Signal(syscall.SIGTERM); err != nil {
		log.Error("Failed to send SIGTERM to child: %v", err)
	}
}

// handleAuthReset re-reads the token file, updates the hub client, and
// restarts the token refresh loop. Called when SIGUSR2 is received from the
// broker's reset-auth handler.
func handleAuthReset(hubClient *hub.Client, tokenRefreshCancel *context.CancelFunc, tokenRefreshDone *<-chan struct{}, statusHandler *handlers.StatusHandler, targetUID, targetGID int) {
	log.TaggedInfo("AUTH_RESET", "Received SIGUSR2: auth reset requested")

	if hubClient == nil {
		log.Error("AUTH_RESET: Hub client is not configured, cannot reset auth")
		return
	}

	newToken := hub.ReadTokenFile()
	if newToken == "" {
		log.Error("AUTH_RESET: Token file is empty after SIGUSR2, cannot reset auth")
		return
	}

	tokenExpiry, err := hub.ParseTokenExpiry(newToken)
	if err != nil {
		log.Error("AUTH_RESET: Cannot parse new token expiry: %v", err)
		return
	}

	// Cancel the existing token refresh loop if running.
	if *tokenRefreshCancel != nil {
		(*tokenRefreshCancel)()
		if *tokenRefreshDone != nil {
			<-*tokenRefreshDone
		}
	}

	// Update the hub client's in-memory token.
	if hubClient != nil {
		hubClient.SetToken(newToken)
	}

	// Reset-auth may also have written a fresh transport token. Adopt it so
	// this process uses it immediately and the file is owned by the scion
	// user for every other hub client in the container.
	if adopted, err := hubClient.AdoptTransportTokenFile(targetUID, targetGID); err != nil {
		log.Error("AUTH_RESET: Failed to adopt transport token file: %v", err)
	} else if adopted {
		log.TaggedInfo("AUTH_RESET", "Transport token reloaded")
	}

	// Clear any AUTH_LOST message from agent-info.json.
	_ = statusHandler.SetMessage("")

	// Schedule refresh 2 hours before the new token's expiry.
	refreshAt := tokenExpiry.Add(-2 * time.Hour)
	if refreshAt.Before(time.Now()) {
		if time.Now().Before(tokenExpiry) {
			refreshAt = time.Now().Add(1 * time.Minute)
		} else {
			log.Error("AUTH_RESET: New token is already expired at %s", tokenExpiry.Format(time.RFC3339))
			return
		}
	}

	// Start a new token refresh loop.
	var tokenRefreshCtx context.Context
	var cancel context.CancelFunc
	tokenRefreshCtx, cancel = context.WithCancel(context.Background())
	*tokenRefreshCancel = cancel
	*tokenRefreshDone = hubClient.StartTokenRefresh(tokenRefreshCtx, &hub.TokenRefreshConfig{
		RefreshAt: refreshAt,
		ChownUID:  targetUID,
		ChownGID:  targetGID,
		OnRefreshed: func(newExpiry time.Time) {
			log.Info("Token refreshed successfully, new expiry: %s", newExpiry.Format(time.RFC3339))
		},
		OnError: func(err error) {
			log.Error("Token refresh failed: %v", err)
		},
		OnAuthLost: func() {
			log.Error("AUTH_LOST: Agent token has expired and could not be refreshed - hub communication is no longer possible")
			log.Error("AUTH_LOST: Agent limits (max-duration, max-turns, max-model-calls) are enforced locally and remain active")
			_ = statusHandler.SetMessage("AUTH_LOST: Hub token expired and could not be refreshed")
		},
	})

	log.TaggedInfo("AUTH_RESET", "Auth reset complete — new token expires %s, refresh at %s",
		tokenExpiry.Format(time.RFC3339), refreshAt.Format(time.RFC3339))

	// Send an immediate heartbeat with the new token.
	if hubClient != nil && hubClient.IsConfigured() {
		hubCtx, hubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := hubClient.Heartbeat(hubCtx); err != nil {
			log.Error("AUTH_RESET: Post-reset heartbeat failed: %v", err)
		} else {
			log.Info("AUTH_RESET: Post-reset heartbeat sent successfully")
		}
		hubCancel()
	}
}

// extractChildCommand extracts the command arguments.
// Cobra handles -- separator, so args contains everything after --.
func extractChildCommand(args []string) []string {
	return args
}

// reExecWithCleanEnv re-execs the current process so that the kernel's
// /proc/<pid>/environ reflects the current (cleaned) environment. This is
// the only reliable way to remove a variable from /proc/<pid>/environ
// because the kernel populates that file from the execve(2) arguments and
// never updates it afterward.
//
// Uses rootexec.SelfExe() ("/proc/self/exe"), never os.Executable(): this
// runs as root, and os.Executable() re-reads this binary's path from disk —
// if that path sits somewhere workload-writable on a given runtime, a
// binary planted there between process start and this call would be the
// one re-executed as root. "/proc/self/exe" is the kernel's own magic
// symlink to the already-running inode, so it always re-execs the exact
// image already in memory, regardless of what (if anything) now sits at
// its on-disk path — the same reasoning pkg/sciontool/substrate's runExec
// gives for resolving "sh" up front via rootexec.Resolve rather than
// leaving it for a shell to look up later.
//
// On success this function does not return (the process image is replaced).
// On failure it returns an error and the caller should continue — the
// in-process environment is already clean, only /proc exposure remains.
//
// There is no separate, skippable symlink-resolution step here —
// filepath.EvalSymlinks would be redundant: syscall.Exec passes the literal
// string "/proc/self/exe", and it is the KERNEL, not this process, that resolves
// that magic symlink to the running inode at the moment of the execve
// syscall itself — standard behavior on Linux >= 2.6, under any runtime that
// runs a real Linux kernel beneath its own hypervisor/sandbox layer.
//
// A package-level func var only so a test can observe whether RunInit
// reaches it without replacing the test binary's own process image; only
// tests reassign it (saving and restoring the original).
var reExecWithCleanEnv = func() error {
	log.Info("Re-execing init (pid %d) with cleaned environment", os.Getpid())
	return syscall.Exec(rootexec.SelfExe(), os.Args, os.Environ())
}

// stageTransportToken persists the bootstrap transport credential
// (SCION_TRANSPORT_TOKEN) to the transport token file, owned by uid:gid,
// then removes it from this process's environment and points children at
// the file via SCION_TRANSPORT_TOKEN_FILE. Child processes therefore no
// longer inherit the bootstrap-only value; they read the file, which the
// refresh loop keeps current.
//
// Returns true when the env var was removed. If the file cannot be
// written the env var is left in place so hub access keeps working until
// it expires.
func stageTransportToken(uid, gid int) bool {
	tok := os.Getenv(transportauth.EnvTransportToken)
	if tok == "" {
		// No hub-provided transport token this start. If none was staged
		// earlier in this process either (SCION_TRANSPORT_TOKEN_FILE survives
		// the re-exec), remove a file left in a persisted home by an earlier
		// configuration so it is never used.
		if os.Getenv(transportauth.EnvTransportTokenFile) == "" {
			if removed, err := hub.RemoveTransportTokenFile(); err != nil {
				log.Error("Failed to remove stale transport token file: %v", err)
			} else if removed {
				log.Info("Removed stale transport token file (no transport token provided)")
			}
		}
		return false
	}
	path, err := hub.SeedTransportTokenFile(tok, uid, gid)
	if err != nil {
		log.Error("Failed to write transport token file; leaving bootstrap value in the environment: %v", err)
		return false
	}
	_ = os.Setenv(transportauth.EnvTransportTokenFile, path)
	_ = os.Unsetenv(transportauth.EnvTransportToken)
	// The injected expiry describes only the bootstrap value and goes stale
	// after the first refresh; nothing in the agent reads it.
	_ = os.Unsetenv(transportauth.EnvTransportTokenExpiry)
	log.Info("Transport credential moved to %s", path)
	return true
}

// setupHostUser modifies the scion user's UID/GID to match the host user.
// This is only done when running as root and SCION_HOST_UID/GID are set.
// Returns the target UID/GID for the child process (0 = no change) and a
// rootless flag. When rootless is true, the container is running in a rootless
// user namespace where UID 0 is the host user; the caller should set the
// child's environment (HOME, USER) to the scion user but skip privilege drop.
// watchLimitsTriggerFile polls for the limits-exceeded trigger file created by
// hook handlers. This works across UID boundaries (hooks run as scion user,
// init runs as root) where SIGUSR1 would fail with EPERM.
func watchLimitsTriggerFile(ctx context.Context, ch chan<- struct{}) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(handlers.LimitsTriggerFile); err == nil {
				ch <- struct{}{}
				return
			}
		}
	}
}

// errRealUserLookupDisabledUnderTest is defaultScionUserLookup/
// defaultLookupUserByID's own error — see their doc comment for why this
// second, independent defense exists.
var errRealUserLookupDisabledUnderTest = errors.New("scionUserLookup/lookupUserByID: real user lookups are disabled under go test; a test that needs a resolved user must override the var itself (scoped with t.Cleanup)")

// defaultScionUserLookup is scionUserLookup's real, production value — but
// even this refuses to run under `go test` (testing.Testing()), the same
// pattern pkg/sciontool/hub's NewClient uses for its own non-localhost-hub
// guard. TestMain overriding the scionUserLookup var to a stub is the
// intended, primary defense (see scionUserLookup's own doc comment); this
// is what still catches a test-driven lookup if that override is ever
// accidentally removed, since nothing about a var default silently
// reverting to this function requires a test to have opted back into real
// lookups.
func defaultScionUserLookup(username string) (*user.User, error) {
	if testing.Testing() {
		return nil, errRealUserLookupDisabledUnderTest
	}
	return user.Lookup(username)
}

// defaultLookupUserByID is lookupUserByID's real, production value. Same
// two-layer reasoning as defaultScionUserLookup.
func defaultLookupUserByID(uid string) (*user.User, error) {
	if testing.Testing() {
		return nil, errRealUserLookupDisabledUnderTest
	}
	return user.LookupId(uid)
}

// scionUserLookup resolves the "scion" system user. It is a package var
// (rather than calling user.Lookup directly) both so adjustScionUser's
// requirePrivilegeDrop-gated fail-closed checks can be exercised in a unit
// test without needing a real "scion" user on the machine running the
// test — the same reasoning as requirePrivilegeDropOrFail's separation from
// setupHostUser — and, just as importantly, so a test binary that overrides
// this once (see the package's TestMain) can guarantee that no test-driven
// codepath ever resolves the *real* "scion" account's home directory. On a
// machine where "scion" happens to be a real system user (an actor
// container's own base image, or a dev container running as that user),
// letting resolveAgentHome/setupHostUser fall through to a real
// user.Lookup("scion") means a test can end up writing agent-info.json (or
// reading the Hub token file) at the real, live path — silently changing
// this agent's own reported status. Every "scion" lookup in this file goes
// through this var for that reason, not just the ones adjustScionUser uses.
//
// Its default value, defaultScionUserLookup, is itself gated on
// testing.Testing() — a second, independent defense for when TestMain's own
// override of this var is the thing that's missing (see that function's
// doc comment).
var scionUserLookup = defaultScionUserLookup

// lookupUserByID resolves a user by numeric UID. Same reasoning as
// scionUserLookup: resolveAgentHome's targetUID != 0 branch must be
// overridable so a test can never resolve a real account's home directory.
var lookupUserByID = defaultLookupUserByID

// runDirectSetUID is directSetUID's call site as a package var, for the same
// reason as scionUserLookup: adjustScionUser's control flow around a failed
// rewrite is unit-tested without ever touching real system files.
var runDirectSetUID = directSetUID

// startReaper is procreap.StartReaper's call site as a package var.
// StartReaper installs a process-wide SIGCHLD handler that Wait4(-1, ...)s
// any reapable child — including one a later exec.Command in the *same*
// test binary is still waiting on itself, which races os/exec's own
// wait() and fails it with ECHILD. RunInit (and substrate-serve's own
// startup) call this unconditionally because a real PID 1 needs it, but a
// test driving RunInit directly does not, and starting it there corrupts
// every other test in the same binary that shells out — stubbed to a
// no-op by TestMain for exactly that reason.
var startReaper = procreap.StartReaper

// runSetupHostUser is setupHostUser's own call site as a package var. A
// test driving RunInit end to end with RequirePrivilegeDrop: true cannot
// reach any of the enforced-mode-gated call sites downstream (writeEnvFile,
// blockClaudeDebugSymlink, runServicesStart, cleanGcloudConfigForMetadata,
// readServicesYAML) without first getting past requirePrivilegeDropOrFail's
// fail-closed check just below — which requires a non-zero targetUID, and
// the real setupHostUser only ever returns that as an actual root process
// (it returns 0 for a non-root test process; see its own doc comment).
// This lets a test return (1000, 1000, false) — "as if" a real
// privilege-drop had already succeeded — so it can drive every downstream
// enforced-mode call site's own argument-threading with a non-root test
// process. Production code always leaves this at its default; only a test
// replaces it.
var runSetupHostUser = setupHostUser

// runGitCloneWorkspace is gitCloneWorkspace's own call site as a package
// var, the same reason as startReaper above: a test driving RunInit needs to
// observe (and, for the ordering RunInit's InitRunOptions.ResolveWorkingDir
// depends on, control) when the workspace clone step runs, without shelling
// out to a real git process or depending on SCION_GIT_CLONE_URL pointing at
// a reachable remote. Production code always leaves this at its default;
// only a test replaces it.
var runGitCloneWorkspace = gitCloneWorkspace

// runPostPreStartOwnershipFixup is postPreStartOwnershipFixup's call site as
// a package var, for the same reason as runGitCloneWorkspace above: the
// ordering contract InitRunOptions.ResolveWorkingDir depends on must observe
// that the resolver runs after this step, and the real step only does
// anything when the calling process is root. Production code always leaves
// this at its default; only a test replaces it.
var runPostPreStartOwnershipFixup = postPreStartOwnershipFixup

// postPreStartGeteuid is os.Geteuid's call site as a package var: it is a
// seam, because postPreStartOwnershipFixup's real euid check (below) makes
// the body — including the requirePrivilegeDrop value forwarded to
// chownTreeRootOwned — unreachable from a non-root test process, since every
// unit test runs as whatever non-root UID the test binary itself has.
// Production code always leaves this at its default; only a test replaces
// it to simulate euid 0 without actually running as root.
var postPreStartGeteuid = os.Geteuid

// runChownTreeRootOwned is chownTreeRootOwned's call site as a package var:
// this seam lets a test (with postPreStartGeteuid stubbed to report euid 0)
// capture the requirePrivilegeDrop value postPreStartOwnershipFixup forwards,
// without the test actually performing a real recursive chown. Production
// code always leaves this at its default; only a test replaces it.
var runChownTreeRootOwned = chownTreeRootOwned

// postPreStartOwnershipFixup chowns root-owned files that pre-start hooks
// (which run before the privilege drop) left in the workspace or agent home.
func postPreStartOwnershipFixup(targetUID, targetGID int, agentHome string, requirePrivilegeDrop bool) {
	if targetUID == 0 || postPreStartGeteuid() != 0 {
		return
	}
	workspacePath := os.Getenv("SCION_WORKSPACE_PATH")
	if workspacePath == "" {
		workspacePath = "/workspace"
	}
	for _, dir := range []string{workspacePath, agentHome} {
		if dir == "" {
			continue
		}
		if _, _, err := runChownTreeRootOwned(dir, targetUID, targetGID, requirePrivilegeDrop); err != nil {
			log.Error("Failed to chown %s after pre-start hooks: %v", dir, err)
		}
	}
}

// runServicesStart is (*services.Manager).Start's call site as a package
// var, the same reason as runGitCloneWorkspace above: the ordering contract
// InitRunOptions.ResolveWorkingDir depends on must observe (from a test) that
// the resolver runs before sidecar services start, without a test having to
// spawn a real sidecar process. Production code always leaves this at its
// default; only a test replaces it.
var runServicesStart = func(ctx context.Context, m *services.Manager, specs []api.ServiceSpec, uid, gid int, username string, requirePrivilegeDrop bool) error {
	return m.Start(ctx, specs, uid, gid, username, requirePrivilegeDrop)
}

// runBlockClaudeDebugSymlink, runCleanGcloudConfigForMetadata and
// runReadServicesYAML are blockClaudeDebugSymlink's, cleanGcloudConfigForMetadata's
// and readServicesYAML's own call sites as package vars, the same reason as
// runServicesStart above: a test driving RunInit needs to observe that each
// one receives the correct requirePrivilegeDrop argument (see
// runSetupHostUser's doc comment for why a non-root test can reach these
// call sites with RequirePrivilegeDrop: true at all). Production code
// always leaves these at their defaults; only a test replaces them.
var runBlockClaudeDebugSymlink = blockClaudeDebugSymlink
var runCleanGcloudConfigForMetadata = cleanGcloudConfigForMetadata
var runReadServicesYAML = readServicesYAML

// runMetadataServerStart is (*metadata.Server).Start's call site as a
// package var, the same reason as runServicesStart above: a test must be
// able to observe that the resolver runs before the metadata server starts
// without a test binding a real listener socket. Production code always
// leaves this at its default; only a test replaces it.
var runMetadataServerStart = func(ctx context.Context, s *metadata.Server) error {
	return s.Start(ctx)
}

// runFetchSecretOverrides is fetchSecretOverrides's call site as a package
// var, the same reason as runServicesStart above: a test must be able to
// observe that the resolver runs before the hub secret fetch without a test
// making a real hub request. Production code always leaves this at its
// default; only a test replaces it.
var runFetchSecretOverrides = fetchSecretOverrides

// setupHostUserGetuid, setupHostUserHasCapSetUID and setupHostUserIsUIDMapped
// are os.Getuid's, hasCapSetUID's and isUIDMapped's call sites inside
// setupHostUser, as package vars: these are seams, because setupHostUser's
// real early-return checks (a non-root euid, an absent CAP_SETUID, an
// unmapped UID) make its adjustScionUser call unreachable from a non-root
// test process: a unit test binary is never root, never holds CAP_SETUID,
// and runs in a user namespace where the test's own arbitrary target UID is
// not mapped. Stubbing all three lets a test simulate "as if root, with the
// capability, with a mapped UID" and reach runAdjustScionUser below to
// capture the requirePrivilegeDrop value setupHostUser forwards to it.
// Production code always leaves these at their defaults; only a test
// replaces them.
var setupHostUserGetuid = os.Getuid
var setupHostUserHasCapSetUID = hasCapSetUID
var setupHostUserIsUIDMapped = isUIDMapped

// runAdjustScionUser is adjustScionUser's call site inside setupHostUser, as
// a package var: this seam is paired with the three vars above so a test
// can reach this call and observe the requirePrivilegeDrop argument
// setupHostUser forwards to it without performing a real usermod/groupmod or
// /etc/passwd edit. Production code always leaves this at its default; only
// a test replaces it.
var runAdjustScionUser = adjustScionUser

// setupHostUser realigns the container's "scion" user to SCION_HOST_UID/GID
// so the harness (and, for substrate, runExec's own credential drop) can
// drop privileges from root to it. requirePrivilegeDrop is RunInit's own
// InitRunOptions.RequirePrivilegeDrop, threaded through so the stricter
// fail-closed checks in adjustScionUser only apply under it — see
// adjustScionUser's doc comment for why this must not change any other
// runtime's return value.
func setupHostUser(requirePrivilegeDrop bool) (int, int, bool) {
	// Only run privilege operations if we're root. When running under
	// --userns=keep-id (rootless Podman), PID 1 starts as the scion user
	// (UID 1000) rather than root. In that case, no usermod/groupmod/chown
	// is needed — the UID already matches the scion user and bind-mounted
	// files have correct host ownership via the keep-id mapping. We return
	// rootless=true so the supervisor sets HOME/USER/LOGNAME without
	// attempting a credential drop.
	if setupHostUserGetuid() != 0 {
		if scionUser, err := scionUserLookup("scion"); err == nil {
			scionUID, _ := strconv.Atoi(scionUser.Uid)
			if os.Getuid() == scionUID {
				log.Info("Already running as scion user (UID %d) in rootless mode, skipping privilege operations", scionUID)
				return 0, 0, true
			}
		}
		log.Debug("Not running as root, skipping user setup")
		return 0, 0, false
	}

	// Safety net: detect restricted capability environments (e.g. gVisor
	// sandboxes on Cloud Run) where CAP_SETUID is absent. In these
	// environments, setuid/setgid syscalls return EPERM. Fall back to
	// rootless-equivalent mode: no privilege drop, no usermod.
	if !setupHostUserHasCapSetUID() {
		log.Info("Running as root but CAP_SETUID is absent (restricted sandbox); " +
			"skipping privilege operations — process will remain UID 0")
		return 0, 0, true
	}

	hostUID := os.Getenv("SCION_HOST_UID")
	hostGID := os.Getenv("SCION_HOST_GID")

	if hostUID == "" || hostGID == "" {
		log.Debug("SCION_HOST_UID/GID not set, skipping user setup")
		return 0, 0, false // Continue as root
	}

	// refuseZero == requirePrivilegeDrop: outside RequirePrivilegeDrop, a
	// SCION_HOST_UID/GID of 0 keeps the pre-existing behavior of proceeding
	// as root rather than refusing outright; under RequirePrivilegeDrop it
	// is refused, since the whole point of the mode is to never regain root.
	// Either way, an out-of-range or non-numeric value is refused.
	validUID, err := rootexec.ValidWorkloadID(hostUID, requirePrivilegeDrop)
	if err != nil {
		log.Error("Invalid SCION_HOST_UID: %v", err)
		return 0, 0, false
	}
	validGID, err := rootexec.ValidWorkloadID(hostGID, requirePrivilegeDrop)
	if err != nil {
		log.Error("Invalid SCION_HOST_GID: %v", err)
		return 0, 0, false
	}
	uid, gid := int(validUID), int(validGID)

	// Check if the runtime signaled a keep-id user namespace mapping via
	// SCION_KEEPID_UID (e.g. --userns=keep-id:uid=1000,gid=1000). In this
	// case, container UID 1000 (scion) already maps to the host user's UID,
	// so bind-mount ownership is correct without remapping.
	// However, PID 1 is still UID 0 (from the Dockerfile), which maps to a
	// subordinate UID in the nested namespace — NOT the host user. Container
	// UID 0 therefore cannot write to the bind-mounted /home/scion (owned by
	// the host user). We must drop privileges to the scion user early so
	// that init's own writes (agent-info.json, scion-env, etc.) succeed.
	//
	// Note: We cannot derive this from /proc/self/uid_map because rootless
	// Podman uses nested namespaces — the uid_map shows the mapping to the
	// immediate parent namespace, not the host.
	if keepIDStr := os.Getenv("SCION_KEEPID_UID"); keepIDStr != "" {
		log.Debug("Keep-id env detected: SCION_KEEPID_UID=%s, current euid=%d, egid=%d", keepIDStr, os.Geteuid(), os.Getegid())
		keepIDUID, parseErr := strconv.Atoi(keepIDStr)
		if parseErr == nil {
			if scionUser, err := scionUserLookup("scion"); err == nil {
				scionUID, _ := strconv.Atoi(scionUser.Uid)
				scionGID, _ := strconv.Atoi(scionUser.Gid)
				log.Debug("Keep-id: scion user lookup: UID=%d, GID=%d, keepIDUID=%d", scionUID, scionGID, keepIDUID)
				if keepIDUID == scionUID {
					log.Info("Keep-id mode: host user mapped to scion (container UID %d); performing early privilege drop", scionUID)
					if err := syscall.Setgroups([]int{scionGID}); err != nil {
						log.Error("Failed to setgroups([%d]): %v", scionGID, err)
					}
					if err := syscall.Setgid(scionGID); err != nil {
						log.Error("Failed to setgid(%d): %v — continuing as root, writes to /home/scion may fail", scionGID, err)
					}
					if err := syscall.Setuid(scionUID); err != nil {
						log.Error("Failed to setuid(%d): %v — continuing as root, writes to /home/scion may fail", scionUID, err)
					}
					log.Info("Keep-id privilege drop complete: now euid=%d, egid=%d", os.Geteuid(), os.Getegid())
					return 0, 0, true
				}
			} else {
				log.Error("Keep-id: failed to look up scion user: %v", err)
			}
		} else {
			log.Error("Keep-id: failed to parse SCION_KEEPID_UID=%q: %v", keepIDStr, parseErr)
		}
	}

	// Check if the target UID is mapped in the current user namespace.
	// In rootless Podman without keep-id, the host user's UID is mapped to
	// container UID 0, and only a limited range of subordinate UIDs are
	// available. If the target UID falls outside any mapped range, chown
	// and credential-based exec would fail with EINVAL. In this case, skip
	// remapping and run as container root (which IS the host user).
	if !setupHostUserIsUIDMapped(uid) {
		log.Info("UID %d is not mapped in the container user namespace (rootless container); skipping user remapping", uid)
		return 0, 0, true
	}

	return runAdjustScionUser(uid, gid, hostUID, hostGID, requirePrivilegeDrop)
}

// adjustScionUser realigns the "scion" user to (uid, gid) — matching an
// existing entry, or editing /etc/passwd and /etc/group via usermod/groupmod
// or a direct sed fallback — and returns setupHostUser's (targetUID,
// targetGID, rootless) result.
//
// Split out from setupHostUser so it's testable without needing to be root
// or pass the capability/env preconditions above it in setupHostUser (same
// reasoning as requirePrivilegeDropOrFail).
//
// requirePrivilegeDrop gates three checks in this function: a missing
// "scion" user, a directSetUID rewrite that matches nothing, and a
// post-adjust verify that cannot confirm the target UID/GID. When true,
// each of these returns (0, 0, false), which requirePrivilegeDropOrFail
// turns into a fail-closed refusal to start the harness, reported through
// RunInit's own failure path. When false (every runtime except
// substrate-serve), this function's return value is byte-identical to the
// path that logs and continues past all three: every other caller depends
// on that lenient fallback and must see it unchanged.
func adjustScionUser(uid, gid int, hostUID, hostGID string, requirePrivilegeDrop bool) (int, int, bool) {
	// Skip if UID/GID already match (1001 is the default)
	currentInfo, lookupErr := scionUserLookup("scion")
	if currentInfo != nil {
		currentUID, _ := strconv.Atoi(currentInfo.Uid)
		currentGID, _ := strconv.Atoi(currentInfo.Gid)
		log.Debug("Current scion user: UID=%d, GID=%d (Target: UID=%d, GID=%d)", currentUID, currentGID, uid, gid)
		if currentUID == uid && currentGID == gid {
			log.Debug("scion user already has correct UID/GID")
			return uid, gid, false
		}
	} else {
		log.Error("scion user not found in system: %v", lookupErr)
		if requirePrivilegeDrop {
			return 0, 0, false
		}
	}

	log.Info("Adjusting scion user to UID=%d, GID=%d", uid, gid)

	if useDirectPasswdEdit() {
		log.Info("Using direct /etc/passwd edit (avoiding slow usermod on this runtime)")
		if err := runDirectSetUID("scion", hostUID, hostGID, requirePrivilegeDrop); err != nil {
			log.Error("Direct passwd/group edit failed: %v", err)
			if requirePrivilegeDrop || !errors.Is(err, errPasswdEntryNotRewritten) {
				return 0, 0, false
			}
			// requirePrivilegeDrop is false and the only problem was the
			// "nothing to rewrite" detection: fall through to the verify
			// step below, which (also gated on requirePrivilegeDrop) just
			// logs.
		}
	} else {
		// Modify group first (if different from current)
		if err := procreap.RunManaged(exec.Command("groupmod", "-o", "-g", hostGID, "scion")); err != nil {
			log.Error("Failed to modify scion group to %s: %v", hostGID, err)
		}

		// Modify user UID and primary group
		if err := procreap.RunManaged(exec.Command("usermod", "-o", "-u", hostUID, "-g", hostGID, "scion")); err != nil {
			// usermod can fail with exit code 12 on runtimes where the home
			// directory is a mount point (e.g. Apple Virtualization / VirtioFS)
			// because it tries a recursive chown that the filesystem rejects.
			// Fall back to direct /etc/passwd editing which skips recursive chown.
			log.Info("usermod failed (exit: %v), falling back to direct passwd edit", err)
			if err := runDirectSetUID("scion", hostUID, hostGID, requirePrivilegeDrop); err != nil {
				log.Error("Direct passwd/group fallback also failed: %v", err)
				if requirePrivilegeDrop || !errors.Is(err, errPasswdEntryNotRewritten) {
					return 0, 0, false
				}
			}
		}
	}

	// Verify the change actually landed.
	updatedInfo, verifyErr := scionUserLookup("scion")
	if verifyErr == nil {
		log.Info("Successfully adjusted scion user: UID=%s, GID=%s", updatedInfo.Uid, updatedInfo.Gid)
	} else {
		log.Error("Failed to verify scion user after adjustment: %v", verifyErr)
	}
	if requirePrivilegeDrop && !verifiedUserMatches(updatedInfo, verifyErr, uid, gid) {
		log.Error("Post-adjust verify failed: scion user does not show UID=%d GID=%d", uid, gid)
		return 0, 0, false
	}

	return uid, gid, false
}

// verifiedUserMatches reports whether a scionUserLookup("scion") result
// (post-adjustment) actually shows the target uid/gid — used by
// adjustScionUser's requirePrivilegeDrop-gated verify step.
func verifiedUserMatches(u *user.User, lookupErr error, uid, gid int) bool {
	if lookupErr != nil || u == nil {
		return false
	}
	gotUID, errU := strconv.Atoi(u.Uid)
	gotGID, errG := strconv.Atoi(u.Gid)
	return errU == nil && errG == nil && gotUID == uid && gotGID == gid
}

// useDirectPasswdEdit returns true when usermod should be avoided in favor of
// direct /etc/passwd and /etc/group editing. This is needed on runtimes like
// Podman where usermod's recursive chown is extremely slow due to fuse-overlayfs.
func useDirectPasswdEdit() bool {
	// Podman sets container=podman in the environment
	if os.Getenv("container") == "podman" {
		log.Debug("Detected Podman runtime (container=podman), using direct passwd edit")
		return true
	}
	// Allow explicit opt-in via SCION_ALT_USERMOD
	if os.Getenv("SCION_ALT_USERMOD") != "" {
		log.Debug("SCION_ALT_USERMOD set, using direct passwd edit")
		return true
	}
	return false
}

// errPasswdEntryNotRewritten is returned by directSetUID when username has
// no entry in the target file at all: sed's substitute command exits 0
// whether or not any line matched, so without an explicit pre-check, a
// missing user silently produces no error, and the (uid, gid) the caller
// believes it just set were never written anywhere on disk — the harness
// would then run under a raw UID with no passwd entry at all. Wrapped with
// the file path via %w so errors.Is still matches it after fmt.Errorf.
var errPasswdEntryNotRewritten = errors.New("no matching entry found to rewrite")

// directSetUID modifies /etc/passwd and /etc/group directly to change a user's
// UID and GID without the recursive chown that usermod performs. This also
// chowns the user's home directory and its immediate contents so ownership is
// correct. The home directory should only contain skeleton files from useradd,
// so this is fast even on fuse-overlayfs.
func directSetUID(username, newUID, newGID string, requirePrivilegeDrop bool) error {
	return directSetUIDAt(username, newUID, newGID, "/etc/group", "/etc/passwd", fmt.Sprintf("/home/%s", username), requirePrivilegeDrop)
}

// directSetUIDAtChownAt performs every real chown directSetUIDAt issues —
// Fchownat(dirFd, name, uid, gid, flags) — including the home directory fd
// itself (name="." and flags=0, an ordinary, portable relative-path lookup
// that resolves back to the same already-open directory, used identically
// on every platform) and, via chownHomeEntryNoFollow, each entry inside it:
// on GOOS=linux relative to that entry's own already-open O_PATH fd with
// name="" and flags=AT_EMPTY_PATH, elsewhere relative to the home
// directory's fd with the entry's name and flags=AT_SYMLINK_NOFOLLOW (see
// init_setuid_linux.go and init_setuid_other.go for why they differ — in
// short, AT_EMPTY_PATH is a Linux-specific kernel feature golang.org/x/sys/
// unix does not expose on every platform this binary builds for).
// Indirected through a package var, not called as unix.Fchownat directly, so
// a test can record whether and how it was called instead of inferring it
// from a filesystem timestamp: ctime's field name is platform-specific
// (Ctim on linux, Ctimespec on darwin), and its coarse, tick-based
// granularity means a self-chown run immediately after mkdir often leaves
// it unchanged even on linux, so a ctime-based detector is both
// non-portable and flaky. The default value is unix.Fchownat itself, so
// production behaviour is a real fd-relative, no-follow chown.
var directSetUIDAtChownAt = unix.Fchownat

// homeEntryChownTestHook, when non-nil, is invoked by chownHomeEntryNoFollow
// (on either platform) with an entry's name immediately before it attempts
// that entry's real chown — not called at all when the entry is skipped
// (the hardlinked-regular-file guard, or an open/stat failure). This exists
// because on GOOS=linux the real directSetUIDAtChownAt call for an entry
// carries an empty name (the per-entry O_PATH fd plus AT_EMPTY_PATH), so a
// test recording that call's raw arguments cannot otherwise tell which
// entry a given call was for. Always nil in production; unexported — this
// package's own tests are the only thing that may set it.
var homeEntryChownTestHook func(name string)

// directSetUIDAt is directSetUID with its file paths as parameters, so a
// test can exercise the "no entry to rewrite" detection against a temp file
// instead of the real /etc/group and /etc/passwd.
//
// requirePrivilegeDrop gates what happens when homeDir itself cannot be
// opened as a plain, non-symlink directory (O_DIRECTORY|O_NOFOLLOW): under
// enforcement this refuses outright, before touching anything; otherwise it
// logs and simply skips the chown pass, matching this function's existing
// best-effort (log-and-continue) treatment of every other chown failure here.
func directSetUIDAt(username, newUID, newGID, groupPath, passwdPath, homeDir string, requirePrivilegeDrop bool) error {
	// Recorded up front but only acted on at the end: every side effect
	// below runs unconditionally, regardless of whether username has a
	// passwd entry to rewrite.
	hasEntry := passwdEntryExists(passwdPath, username)

	// Update /etc/group's GID field, unconditional and best-effort: sed
	// -i's substitute exits 0 whether or not anything matched, and a
	// primary group not literally named after username (e.g. useradd -g
	// users scion) is a legitimate case for this to silently no-op on.
	groupSed := exec.Command("sed", "-i", "-E",
		fmt.Sprintf(`s/^(%s:x:)[0-9]+:/\1%s:/`, username, newGID),
		groupPath)
	if out, err := procreap.CombinedOutputManaged(groupSed); err != nil {
		return fmt.Errorf("sed %s: %w (output: %s)", groupPath, err, string(out))
	}

	// Update /etc/passwd's UID/GID fields, unconditional and best-effort
	// for the same reason as the group sed above.
	passwdSed := exec.Command("sed", "-i", "-E",
		fmt.Sprintf(`s/^(%s:x:)[0-9]+:[0-9]+:/\1%s:%s:/`, username, newUID, newGID),
		passwdPath)
	if out, err := procreap.CombinedOutputManaged(passwdSed); err != nil {
		return fmt.Errorf("sed %s: %w (output: %s)", passwdPath, err, string(out))
	}

	// Chown the home directory and its immediate contents, unconditionally
	// — including when hasEntry is false. Not a recursive walk: the home
	// dir should only hold skeleton files from /etc/skel at this point, so
	// a shallow chown is enough and stays fast on fuse-overlayfs.
	//
	// homeDir itself is opened O_DIRECTORY|O_NOFOLLOW first: a symlink
	// planted there (or anything that isn't a plain directory) is refused
	// rather than followed, which a path-based os.Chown/os.ReadDir pair
	// would otherwise do — chowning whatever the symlink points at (e.g.
	// "/etc") to the workload's own uid before this process has dropped
	// privilege. Every entry inside is then chowned via
	// chownHomeEntryNoFollow, so a symlink among the (expected to be
	// skeleton-only) entries has its own link chowned, never its target —
	// see that function's doc comment (init_setuid_linux.go /
	// init_setuid_other.go) for the per-platform mechanism, the
	// hardlinked-regular-file guard, and why they differ.
	uid := mustAtoi(newUID)
	gid := mustAtoi(newGID)
	homeFd, homeErr := unix.Open(homeDir, unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	switch {
	case homeErr != nil && requirePrivilegeDrop:
		return fmt.Errorf("%s: refusing to chown (not a plain directory, or a symlink): %w", homeDir, homeErr)
	case homeErr != nil:
		log.Warn("%s is a symlink or not a plain directory; skipping ownership chown pass: %v", homeDir, homeErr)
	default:
		homeDirFile := os.NewFile(uintptr(homeFd), homeDir)
		defer func() { _ = homeDirFile.Close() }()

		if err := directSetUIDAtChownAt(int(homeDirFile.Fd()), ".", uid, gid, 0); err != nil {
			log.Debug("Failed to chown home directory %s: %v", homeDir, err)
		}
		names, err := homeDirFile.Readdirnames(-1)
		if err == nil {
			for _, name := range names {
				chownHomeEntryNoFollow(int(homeDirFile.Fd()), homeDir, name, uid, gid, requirePrivilegeDrop)
			}
		}
	}

	// Only now — after every side effect above has run unconditionally —
	// report whether there was anything to rewrite: substrate's
	// requirePrivilegeDrop=true caller fails closed on this, every other
	// caller absorbs it (see errPasswdEntryNotRewritten's own doc comment).
	if !hasEntry {
		return fmt.Errorf("%s: %w", passwdPath, errPasswdEntryNotRewritten)
	}
	return nil
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// passwdEntryExists reports whether path (an /etc/passwd-formatted file)
// has a line for username, matching the exact "username:x:" prefix the
// passwd sed substitution above anchors on — not just "username:", which a
// "scion:*:" or "scion:!:" line (a locked/disabled account, still a valid
// passwd entry) would also match while the sed itself matches nothing.
// Returns false (not an error) if the file can't be read, since that's
// just as much "nothing to rewrite" as the entry being absent. A TOCTOU
// window between this read and the sed -i below is negligible: only root
// in the actor writes these files, and only before the harness starts.
func passwdEntryExists(path, username string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	prefix := username + ":x:"
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// isUIDMapped checks whether uid is a valid container UID by reading
// /proc/self/uid_map. In a non-namespaced process the map covers the full
// 32-bit range so every UID is valid. In a rootless container only a small
// subset of UIDs are mapped; using an unmapped UID in chown or
// syscall.Credential causes EINVAL.
func isUIDMapped(uid int) bool {
	data, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		// Cannot determine mapping; assume mapped (safe for rootful).
		return true
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		insideStart, err1 := strconv.Atoi(fields[0])
		count, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil {
			continue
		}
		if uid >= insideStart && uid < insideStart+count {
			return true
		}
	}
	return false
}

// errGitCloneWorkspacePrivilegeDropRequired is returned when
// gitCloneWorkspace's requirePrivilegeDrop guard trips: RequirePrivilegeDrop
// is set but uid/gid are not both usable to run git as the workload. A
// sentinel (rather than a bare fmt.Errorf) so a caller — or a test proving
// this guard actually fires, not just that some error came back — can
// check for it with errors.Is instead of matching on message text.
var errGitCloneWorkspacePrivilegeDropRequired = errors.New(
	"gitCloneWorkspace: privilege drop required but uid/gid were not both set; refusing to run git as root")

// gitCloneWorkspace clones a git repository into /workspace when SCION_GIT_CLONE_URL
// is set. This supports hub-first git projects where the repository must be cloned
// before the harness starts. When uid > 0, all git commands run as the specified
// user so that the resulting files are owned by the scion user rather than root.
// agentHome is the scion user's home directory, used to write the credential
// helper to the correct .gitconfig (not root's HOME).
// Returns nil if no clone URL is configured (non-git workspace).
//
// requirePrivilegeDrop is the caller's own opts.RequirePrivilegeDrop: when
// set, every git command below must run under configureGitCommand's
// Credential, never under this process's own (root) identity, so this
// refuses outright — before running any git command at all — if uid/gid do
// not both pass the same predicate configureGitCommand's Credential block
// uses, mirroring supervisor.Supervisor.Run's own ErrPrivilegeDropRequired.
// In practice RunInit's own requirePrivilegeDropOrFail already refuses to
// reach this code at all under those conditions, so this is
// belt-and-suspenders against a future caller that calls it directly.
func gitCloneWorkspace(uid, gid int, agentHome string, requirePrivilegeDrop bool) (retErr error) {
	cloneURL := os.Getenv("SCION_GIT_CLONE_URL")
	if cloneURL == "" {
		return nil
	}
	if requirePrivilegeDrop && (uid <= 0 || gid <= 0) {
		return fmt.Errorf("gitCloneWorkspace: uid=%d gid=%d: %w", uid, gid, errGitCloneWorkspacePrivilegeDropRequired)
	}

	workspacePath := os.Getenv("SCION_WORKSPACE_PATH")
	if workspacePath == "" {
		workspacePath = "/workspace"
	}

	// Check if workspace already has content (stop/start scenario).
	// Ignore marker-only directories (e.g. .scion/) that may have been
	// written during provisioning — they don't indicate a real clone.
	if !isWorkspaceEmpty(workspacePath) {
		log.Info("Workspace already populated, skipping git clone")
		return nil
	}

	// Snapshot the workspace's pre-existing top-level entries (e.g.
	// bind-mounted marker directories such as .scion, .scion-volumes,
	// .agents — see isWorkspaceEmpty) before this attempt writes anything.
	// On failure, preExistingWorkspaceEntries lets cleanup remove only what
	// this attempt itself created, leaving anything that was already there
	// untouched.
	preExisting := preExistingWorkspaceEntries(workspacePath)

	// When uid is 0 (broker running as root or no host UID configured), fall
	// back to the scion user so that cloned files are owned by the container
	// user rather than root.
	if uid == 0 {
		if scionUser, err := scionUserLookup("scion"); err == nil {
			uid, _ = strconv.Atoi(scionUser.Uid)
			gid, _ = strconv.Atoi(scionUser.Gid)
			log.Info("Falling back to scion user UID=%d GID=%d for git clone", uid, gid)
		}
	}

	currentEUID := os.Geteuid()
	ensureWorkspaceOwnership(workspacePath, uid, gid, currentEUID, os.Chown)

	token := os.Getenv("GITHUB_TOKEN")
	branch := os.Getenv("SCION_GIT_BRANCH")
	if branch == "" {
		branch = "main"
	}
	depthStr := os.Getenv("SCION_GIT_DEPTH")
	if depthStr == "" {
		depthStr = "1" // default: shallow clone
	}
	depth, err := strconv.Atoi(depthStr)
	if err != nil {
		log.Info("WARNING: SCION_GIT_DEPTH is not a valid integer (%q), defaulting to 1", depthStr)
		depth = 1
		depthStr = "1"
	}
	agentName := os.Getenv("SCION_AGENT_NAME")

	// Helper to configure a git command: run as the scion user and disable
	// interactive credential prompts so git fails immediately instead of
	// hanging when authentication is required but no token is available.
	setupGitCmd := func(cmd *exec.Cmd) {
		configureGitCommand(cmd, uid, gid)
	}

	// Report cloning status to Hub
	normalizedURL := util.NormalizeGitRemote(cloneURL)
	if hubClient := hub.NewClient(); hubClient != nil && hubClient.IsConfigured() {
		hubCtx, hubCancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = hubClient.UpdateStatus(hubCtx, hub.StatusUpdate{
			Phase:   state.PhaseCloning,
			Status:  string(state.PhaseCloning),
			Message: "Cloning repository",
			Metadata: map[string]string{
				"repository": normalizedURL,
				"branch":     branch,
			},
		})
		hubCancel()
	}

	// Build authenticated URL (never log this)
	authURL := buildAuthenticatedURL(cloneURL, token)

	// Determine the agent feature branch name early so we can try cloning it.
	agentBranch := os.Getenv("SCION_AGENT_BRANCH")

	// Initialize the workspace as a git repo. We use git-init + git-fetch
	// instead of git-clone because the workspace directory may already
	// contain bind-mounted directories (e.g. .scion-volumes/) from the
	// container runtime, and git-clone refuses to work in a non-empty dir.
	initCmd := exec.Command("git", "init", workspacePath)
	setupGitCmd(initCmd)
	if out, err := procreap.CombinedOutputManaged(initCmd); err != nil {
		return fmt.Errorf("git init failed: %s", sanitizeGitOutput(string(out), token))
	}

	// Clean up everything this attempt wrote if the clone fails after init,
	// not just .git/: this prevents a credential-bearing remote URL from
	// persisting in .git/config when a subsequent fetch or checkout step
	// errors out (miller79/scion#65), and it restores the workspace to a
	// cloneable state for a retry. The latter matters even when git itself
	// never actually failed — a step like `git checkout` can succeed and
	// populate the working tree, but still be reported as an error because
	// its exec.Cmd.Wait lost the exit status to sciontool init's PID-1
	// zombie reaper racing it for the same PID. Leaving those checked-out
	// files behind (with .git removed) would make a retry's
	// isWorkspaceEmpty() check see a "populated" workspace and skip cloning
	// into it entirely.
	defer func() {
		if retErr != nil {
			cleanFailedCloneAttempt(workspacePath, preExisting)
		}
	}()

	remoteCmd := exec.Command("git", "-C", workspacePath, "remote", "add", "origin", authURL)
	setupGitCmd(remoteCmd)
	if out, err := procreap.CombinedOutputManaged(remoteCmd); err != nil {
		return fmt.Errorf("git remote add failed: %s", sanitizeGitOutput(string(out), token))
	}

	// fetchBranch attempts to fetch a single branch from origin.
	// When depth > 0, a shallow fetch is performed; depth 0 means full clone (no --depth flag).
	// Returns sanitized stderr and whether the fetch succeeded.
	fetchBranch := func(branchToFetch string) (string, bool) {
		fetchArgs := []string{"-C", workspacePath, "fetch"}
		if depth > 0 {
			fetchArgs = append(fetchArgs, "--depth", depthStr)
		}
		fetchArgs = append(fetchArgs, "origin", branchToFetch)
		fetchCmd := exec.Command("git", fetchArgs...)
		setupGitCmd(fetchCmd)
		var stderr bytes.Buffer
		fetchCmd.Stderr = &stderr
		if err := procreap.RunManaged(fetchCmd); err != nil {
			return sanitizeGitOutput(stderr.String(), token), false
		}
		return "", true
	}

	// Fetch strategy: if an agent branch is specified, try fetching that
	// branch first (it may already exist on origin). If that fails, fall
	// back to fetching the default branch (usually main).
	clonedBranch := ""
	if agentBranch != "" && agentBranch != branch {
		log.Info("Attempting to fetch repository %s (branch: %s, depth: %s)", normalizedURL, agentBranch, depthStr)
		errOutput, ok := fetchBranch(agentBranch)
		if ok {
			clonedBranch = agentBranch
			log.Info("Successfully fetched agent branch %s from origin", agentBranch)
		} else {
			if isAuthError(errOutput) {
				return formatCloneError(errOutput, token)
			}
			log.Info("Agent branch %s not found on origin, falling back to %s", agentBranch, branch)
		}
	}

	if clonedBranch == "" {
		log.Info("Fetching repository %s (branch: %s, depth: %s)", normalizedURL, branch, depthStr)
		errOutput, ok := fetchBranch(branch)
		if ok {
			clonedBranch = branch
		} else if isAuthError(errOutput) {
			return formatCloneError(errOutput, token)
		} else {
			// The configured branch doesn't exist. Try to detect the
			// remote's default branch via ls-remote and fetch that instead.
			log.Info("Branch %s not found, detecting default branch from remote", branch)
			detected := detectDefaultBranch(workspacePath, setupGitCmd)
			if detected != "" && detected != branch {
				log.Info("Detected default branch: %s", detected)
				errOutput2, ok2 := fetchBranch(detected)
				if ok2 {
					clonedBranch = detected
				} else {
					return formatCloneError(errOutput2, token)
				}
			} else {
				return formatCloneError(errOutput, token)
			}
		}
	}

	// Check out the fetched branch to populate the working tree.
	checkoutArgs := []string{"-C", workspacePath, "checkout", "-b", clonedBranch, "origin/" + clonedBranch}
	coCmd := exec.Command("git", checkoutArgs...)
	setupGitCmd(coCmd)
	if out, err := procreap.CombinedOutputManaged(coCmd); err != nil {
		return fmt.Errorf("git checkout failed: %s", sanitizeGitOutput(string(out), token))
	}

	// Configure git identity
	gitConfigs := []struct {
		key, value string
	}{
		{"user.name", fmt.Sprintf("Scion Agent (%s)", agentName)},
		{"user.email", "agent@scion.dev"},
	}
	for _, cfg := range gitConfigs {
		cfgCmd := exec.Command("git", "-C", workspacePath, "config", cfg.key, cfg.value)
		setupGitCmd(cfgCmd)
		if err := procreap.RunManaged(cfgCmd); err != nil {
			return fmt.Errorf("failed to set git config %s: %w", cfg.key, err)
		}
	}

	// Sanitize the remote URL to remove the embedded token. The token was
	// needed for the initial fetch, but ongoing auth is handled by the
	// credential helper configured below. Leaving the token in the URL
	// exposes it via `git remote -v`.
	sanitizeCmd := exec.Command("git", "-C", workspacePath, "remote", "set-url", "origin", buildAuthenticatedURL(cloneURL, ""))
	setupGitCmd(sanitizeCmd)
	if out, err := procreap.CombinedOutputManaged(sanitizeCmd); err != nil {
		log.Error("Failed to sanitize remote URL: %s %v", string(out), err)
	}

	// Configure credential helper in the agent user's $HOME/.gitconfig (not
	// the workspace .git/config). This keeps credentials out of the workspace,
	// matching the pattern used by shared-workspace projects. We use the
	// resolved agentHome rather than os.Getenv("HOME") because init runs as
	// root (HOME=/root) but the harness runs as the scion user.
	gitconfigPath := filepath.Join(agentHome, ".gitconfig")

	var credentialHelper string
	if hub.IsGitHubAppEnabled() {
		credentialHelper = "!sciontool credential-helper"
	} else {
		credentialHelper = `!f() { echo "password=${GITHUB_TOKEN}"; echo "username=oauth2"; }; f`
	}
	credCmd := exec.Command("git", "config", "--file", gitconfigPath, "credential.helper", credentialHelper)
	setupGitCmd(credCmd)
	if err := procreap.RunManaged(credCmd); err != nil {
		return fmt.Errorf("failed to configure git credential helper: %w", err)
	}

	// Resolve the agent feature branch name.
	// Priority: SCION_AGENT_BRANCH env var (read earlier) > default "scion/<agentName>"
	branchName := agentBranch
	if branchName == "" {
		branchName = "scion/" + agentName
	}

	// If we already cloned the agent branch directly, we're on it — skip checkout.
	// Otherwise, try to check out the branch locally, fetch from origin, or create it.
	if clonedBranch != branchName {
		checked := false

		// 1. Try local checkout (works if branch matches the cloned branch)
		checkoutCmd := exec.Command("git", "-C", workspacePath, "checkout", branchName)
		setupGitCmd(checkoutCmd)
		if err := procreap.RunManaged(checkoutCmd); err == nil {
			checked = true
		}

		// 2. Try fetching the branch from origin (shallow clone may not have it)
		if !checked {
			fetchCmd := exec.Command("git", "-C", workspacePath, "fetch", "origin", branchName)
			setupGitCmd(fetchCmd)
			if err := procreap.RunManaged(fetchCmd); err == nil {
				// Branch exists on remote — check it out tracking origin
				trackCmd := exec.Command("git", "-C", workspacePath, "checkout", "-b", branchName, "origin/"+branchName)
				setupGitCmd(trackCmd)
				if err := procreap.RunManaged(trackCmd); err == nil {
					checked = true
				}
			}
		}

		// 3. Branch doesn't exist anywhere — create it
		if !checked {
			createCmd := exec.Command("git", "-C", workspacePath, "checkout", "-b", branchName)
			setupGitCmd(createCmd)
			if err := procreap.RunManaged(createCmd); err != nil {
				return fmt.Errorf("failed to create branch %s: %w", branchName, err)
			}
		}
	}

	log.Info("Git clone complete: %s on branch %s", normalizedURL, branchName)
	return nil
}

// isRootOwned is chownTreeRootOwned's default shouldChown filter: only
// entries whose current owning uid is 0 (root) are eligible.
func isRootOwned(entryUID uint32) bool { return entryUID == 0 }

// chownTreeRootOwnedFilter is chownTreeRootOwned's shouldChown call site as
// a package var, so a test can simulate "every entry looks root-owned" (and
// later "no entry does, because a real chown(2) already fixed them") without
// needing the test process to actually own root-owned files or hold
// CAP_CHOWN itself — the real chown(2) calls chownTreeRootOwned issues
// still run for real underneath. Production code always leaves this at its
// default, isRootOwned.
var chownTreeRootOwnedFilter = isRootOwned

// chownTreeRootOwned recursively chowns entries owned by root (UID 0) to
// the specified uid:gid; entries already owned by anyone else are left
// alone. It is called after pre-start hooks to fix up files created by
// provisioners running as root, which would otherwise be undeletable by the
// non-root broker, and — for substrate specifically — by fixupRootfsForScion.
// Returns the number of entries the walk visited in total
// and the number actually rechowned. A missing root (e.g. no /workspace) is
// a silent no-op — nil, 0, 0 — on both branches below.
//
// requirePrivilegeDrop is the caller's own opts.RequirePrivilegeDrop. The
// call from postPreStartOwnershipFixup runs on every runtime whenever the
// calling process is root, not just an enforced one, while sidecar
// services/pre-start-spawned processes may already be alive — but only an
// enforced caller has an actual, less-privileged workload user on the other
// side of that boundary to defend against, and only an enforced caller has
// an ancestor-path symlink threat model where a workload process can plant
// one. A legitimate unenforced setup can symlink an ancestor of $HOME or
// /workspace (e.g. from a bind-mounted host path), and refusing that would
// break it. So:
//   - requirePrivilegeDrop == false: the historical filepath.WalkDir +
//     os.Lchown(path) walk, unconditionally chowning the root filter admits
//     — this is byte-identical to the pre-existing behaviour, ancestor
//     symlinks and all.
//   - requirePrivilegeDrop == true: fsutil.CheckRoot refuses root outright
//     on its own path/name alone when it is a known critical system path or
//     looks like a filesystem root by content; fsutil.CheckMountSource
//     additionally refuses root when it is itself a mount point whose bind
//     source names a critical system directory (see that function's doc
//     comment for exactly what it does and does not detect). Checking
//     CheckRoot first means an already-invalid root is never checked
//     against the mount table at all. Then dirfd.ChownTreeNoFollow, the
//     same openat(O_NOFOLLOW) fd-relative walk supervisor.chownRecursive
//     uses, with its hard-link guard enabled — never a full-path os.Lchown,
//     which re-resolves every intermediate component on every call and can
//     be redirected by a symlink a scion-uid process swaps into one of them
//     mid-walk.
func chownTreeRootOwned(root string, uid, gid int, requirePrivilegeDrop bool) (walked, changed int, err error) {
	if !requirePrivilegeDrop {
		return chownTreeRootOwnedPathBased(root, uid, gid)
	}
	if err := fsutil.CheckRoot(root); err != nil {
		return 0, 0, err
	}
	if err := checkMountSource(root); err != nil {
		return 0, 0, err
	}
	// dirfd.ChownTreeNoFollow's own doc comment: only an error opening or
	// stat'ing root itself comes back as its own returned error; every
	// per-entry problem goes through onErr instead, so the walk always
	// completes and reports its full counts even when individual entries
	// fail. A real per-entry failure must still be reported here, not just
	// logged, so this collects every non-hard-link-guard error onErr sees
	// and joins them into the result below -- deciding from the root-only
	// err returned above first (never a joined error, so errors.Is on it
	// can never be satisfied by an unrelated entry also matching
	// os.ErrNotExist and masking a real failure alongside it) before even
	// looking at the collected per-entry errors.
	var entryErrs []error
	walked, changed, err = dirfd.ChownTreeNoFollow(root, uid, gid, chownTreeRootOwnedFilter, true, func(name string, cerr error) {
		if errors.Is(cerr, dirfd.ErrHardlinkedRegularFile) {
			log.Info("chownTreeRootOwned: WARN: skipping %s: %v", name, cerr)
			return
		}
		log.Error("chownTreeRootOwned: failed to chown %s: %v", name, cerr)
		// dirfd.ChownTreeNoFollow's onErr receives a bare errno-style error
		// with no path or name embedded (unlike the historical
		// filepath.WalkDir-based errors this replaces) -- wrap it with name
		// here, once, so an aggregate of several per-entry failures still
		// says which entries they came from instead of repeating the same
		// unattributed message once per failure.
		entryErrs = append(entryErrs, fmt.Errorf("%s: %w", name, cerr))
	})
	if errors.Is(err, os.ErrNotExist) {
		return 0, 0, nil
	}
	if err != nil {
		return walked, changed, err
	}
	if len(entryErrs) > 0 {
		return walked, changed, errors.Join(entryErrs...)
	}
	return walked, changed, nil
}

// chownTreeRootOwnedPathBased is chownTreeRootOwned's path-based
// implementation, used whenever requirePrivilegeDrop is false, for every
// caller including substrate's own fixupRootfsForScion: it walks with
// filepath.WalkDir and chowns each admitted entry with os.Lchown, reading
// ownership directly off info.Sys().(*syscall.Stat_t). Neither WalkDir nor
// Lchown follows a symlink AT the leaf, but both re-resolve every path
// component above the leaf on every call, so this is not used where
// requirePrivilegeDrop is true.
func chownTreeRootOwnedPathBased(root string, uid, gid int) (walked, changed int, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			// Skip permission errors on walk (e.g., lost+found).
			return nil
		}
		walked++
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !chownTreeRootOwnedFilter(stat.Uid) {
			return nil
		}
		if chErr := os.Lchown(path, uid, gid); chErr != nil {
			log.Error("chownTreeRootOwned: failed to chown %s: %v", path, chErr)
			return nil
		}
		changed++
		return nil
	})
	return walked, changed, err
}

// checkMountSource is fsutil.CheckMountSource, held behind a package
// variable so a test can stub it (to prove chownTreeRootOwned actually
// calls it) without needing a real mount to exercise. The production value
// is fixed; only tests reassign it, and always restore it afterward.
var checkMountSource = fsutil.CheckMountSource

func ensureWorkspaceOwnership(workspacePath string, uid, gid, currentEUID int, chown func(string, int, int) error) {
	// Only root can successfully chown a mounted workspace. In restricted
	// Kubernetes pods the init process may already be running as the scion
	// user, so attempting chown here just adds noise before git init.
	if uid <= 0 {
		return
	}
	if currentEUID != 0 {
		log.Info("Skipping workspace chown for %s; running as non-root UID=%d", workspacePath, currentEUID)
		return
	}
	if err := chown(workspacePath, uid, gid); err != nil {
		log.Error("Failed to chown workspace to UID=%d GID=%d: %v", uid, gid, err)
	}
}

// resolveIsSharedGitWorkspace returns true when the agent is in a shared-plain
// git workspace and should configure git credentials accordingly.
//
// Prefers the new canonical workspace mode env vars (SCION_WORKSPACE_MODE +
// SCION_WORKSPACE_GIT) emitted by brokers >= workspace-mode-env release.
// Falls back to the legacy SCION_SHARED_WORKSPACE var for older broker versions
// that don't yet emit the new vars.
//
// Deprecated: SCION_SHARED_WORKSPACE — use SCION_WORKSPACE_MODE + SCION_WORKSPACE_GIT.
// Removal is tracked in https://github.com/ptone/scion/issues/575.
func resolveIsSharedGitWorkspace() bool {
	if workspaceMode := os.Getenv("SCION_WORKSPACE_MODE"); workspaceMode != "" {
		// New path: broker emits canonical workspace mode vars.
		// A shared-plain workspace is git-backed when SCION_WORKSPACE_GIT=true.
		return workspaceMode == "shared-plain" && util.ParseBoolEnv("SCION_WORKSPACE_GIT", false)
	}
	// Fallback: older broker that only emits SCION_SHARED_WORKSPACE.
	return util.ParseBoolEnv("SCION_SHARED_WORKSPACE", false)
}

// errSharedWorkspaceGitPrivilegeDropRequired is returned when
// RequirePrivilegeDrop is set but uid/gid are not both usable to run git as
// the workload, so an enforced runtime with no real workload identity to
// drop to never runs git config as root. gitCloneWorkspace has no
// equivalent guard of its own: it never receives RequirePrivilegeDrop at
// all, and falls back to the "scion" system user itself whenever uid is 0.
var errSharedWorkspaceGitPrivilegeDropRequired = errors.New(
	"configureSharedWorkspaceGit: privilege drop required but uid/gid were not both set; refusing to run git as root")

// configureSharedWorkspaceGit sets up git credentials for shared-workspace
// (git-workspace hybrid) projects. The workspace is a pre-cloned git repo
// shared by all agents; each agent gets its own credential helper in
// $HOME/.gitconfig so credentials don't pollute the shared workspace.
//
// When uid/gid are both usable, every `git config --file <gitconfigPath>`
// call below runs AS the workload, via configureGitCommand's
// SysProcAttr.Credential: git itself reads and rewrites gitconfigPath under
// the same identity that already owns agentHome and everything in it. A
// symlink at .gitconfig (the layout a dotfile manager such as chezmoi,
// stow, or dotbot commonly produces, or one pointed at a file outside
// agentHome entirely) is then exactly as safe — or unsafe — as if the
// workload had run `git config` itself: git writes through it in place, so
// the symlink survives, and if it targets something the workload identity
// cannot write (e.g. a root-owned file), the write fails with an ordinary
// permission error, leaving that target untouched. A planted symlink
// therefore gives the workload nothing it could not do by running git
// itself.
//
// When RequirePrivilegeDrop is set but uid/gid are not both usable, this
// refuses outright (errSharedWorkspaceGitPrivilegeDropRequired) rather than
// running git config as root against a directory the workload controls.
//
// Otherwise (rootless, where PID 1 already IS the workload's own uid with
// no separate root identity to protect against, or an unenforced runtime
// with no host UID configured at all) there is no workload identity
// distinct from the one already running this code, so git config runs
// directly against gitconfigPath under this process's own identity, no
// Credential override.
//
// Independently of which identity case above applies, gitconfigPath is
// stat'd (never opened) before every run: a FIFO planted there would make
// git's own open block forever with no writer, hanging RunInit, and stat —
// unlike open — never blocks on one. A hung startup is a concrete,
// self-contained failure mode any of those cases can hit, not a
// symlink-specific privilege question the uid separation above already
// answers.
func configureSharedWorkspaceGit(agentHome string, uid, gid int, requirePrivilegeDrop bool) error {
	log.Info("Configuring git credentials for shared workspace")

	gitconfigPath := filepath.Join(agentHome, ".gitconfig")

	if fi, err := os.Stat(gitconfigPath); err == nil && !fi.Mode().IsRegular() {
		log.Warn("Refusing existing %s: not a regular file (mode %v); leaving it untouched and skipping shared-workspace git configuration.", gitconfigPath, fi.Mode())
		return nil
	}

	// This switch deliberately follows the supervisor's own privilege-drop
	// decision (the Credential block in supervisor.Supervisor.Run: drop only
	// when UID > 0 && GID > 0, refuse with ErrPrivilegeDropRequired when
	// RequirePrivilegeDrop is set and it cannot drop), because the
	// harness that later reads this .gitconfig runs under exactly that
	// identity. A uid>0 with gid==0 in unenforced mode is therefore NOT a
	// distinct workload: the supervisor skips the drop, the harness runs as
	// this process (root), and root writing its own credential.helper and
	// identity here is what lets it push. Skipping the write in that case
	// would protect nothing and break git for the harness. See
	// TestConfigureSharedWorkspaceGit_FollowsSupervisorDropDecision.
	var configureCmd func(cmd *exec.Cmd)
	switch {
	case uid > 0 && gid > 0:
		configureCmd = func(cmd *exec.Cmd) { configureGitCommand(cmd, uid, gid) }
	case requirePrivilegeDrop:
		return errSharedWorkspaceGitPrivilegeDropRequired
	default:
		configureCmd = func(cmd *exec.Cmd) {}
	}

	runGitConfig := func(args ...string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		gitPath, rerr := rootexec.Resolve("git")
		if rerr != nil {
			log.Error("Failed to resolve a trusted git binary: %v", rerr)
			return
		}
		cmd := exec.CommandContext(ctx, gitPath, append([]string{"config", "--file", gitconfigPath}, args...)...)
		configureCmd(cmd)
		if out, cerr := procreap.CombinedOutputManaged(cmd); cerr != nil {
			log.Error("Failed to run git config %v: %s %v", args, string(out), cerr)
		}
	}

	// Configure credential helper using sciontool's credential-helper command,
	// which handles both GITHUB_TOKEN env var and GitHub App token refresh.
	var credentialHelper string
	if hub.IsGitHubAppEnabled() {
		// Use sciontool credential-helper for GitHub App token refresh
		credentialHelper = "!sciontool credential-helper"
	} else {
		// Simple credential helper using GITHUB_TOKEN env var
		credentialHelper = `!f() { echo "password=${GITHUB_TOKEN}"; echo "username=oauth2"; }; f`
	}
	// This is idempotent and works even if provisioning already set it.
	runGitConfig("credential.helper", credentialHelper)

	// Configure git identity for the agent
	agentName := os.Getenv("SCION_AGENT_NAME")
	if agentName == "" {
		agentName = "unknown"
	}
	configs := []struct{ key, value string }{
		{"user.name", fmt.Sprintf("Scion Agent (%s)", agentName)},
		{"user.email", "agent@scion.dev"},
	}
	for _, cfg := range configs {
		runGitConfig(cfg.key, cfg.value)
	}
	return nil
}

// configureGitCommandGetuid is a test-only seam: production never reassigns
// it. Tests stub it to 0 to reach configureGitCommand's root-init branch
// without running as root.
var configureGitCommandGetuid = os.Getuid

// configureGitCommand points cmd's environment and (when this process is
// root and uid/gid name someone else) its Credential at the workload
// identity. It has no opinion on RequirePrivilegeDrop: a caller that needs
// to refuse outright when uid/gid<=0 must do so itself before calling this
// (see configureSharedWorkspaceGit's own guard) — this helper only ever
// sets Credential when it can, it never enforces that one was required.
// gitCloneWorkspace, this function's other caller, has no such guard: it
// never receives RequirePrivilegeDrop at all.
func configureGitCommand(cmd *exec.Cmd, uid, gid int) {
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if uid <= 0 {
		return
	}

	currentUID := configureGitCommandGetuid()
	currentGID := os.Getgid()
	if currentUID == uid && currentGID == gid {
		return
	}
	if currentUID != 0 {
		return
	}

	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{
			Uid: uint32(uid),
			Gid: uint32(gid),
		},
	}
}

// isAuthError returns true if the git stderr output indicates an authentication
// or authorization failure (as opposed to a branch-not-found or network error).
func isAuthError(sanitizedStderr string) bool {
	return util.ClassifyGitError(sanitizedStderr).Kind == util.GitErrAuth
}

// formatCloneError builds a descriptive error from sanitized git stderr.
// When no GITHUB_TOKEN is set, the message calls that out specifically.
// Also includes user-facing guidance from the error classification.
func formatCloneError(sanitizedStderr, token string) error {
	gitErr := util.ClassifyGitError(sanitizedStderr)
	if token == "" {
		return fmt.Errorf("git clone failed (no GITHUB_TOKEN secret configured — the repository may require authentication): %s", sanitizedStderr)
	}
	if guidance := gitErr.UserGuidance(); guidance != "" {
		return fmt.Errorf("git clone failed (%s): %s", guidance, sanitizedStderr)
	}
	return fmt.Errorf("git clone failed (unclassified error): %s", sanitizedStderr)
}

// detectDefaultBranch uses `git ls-remote --symref origin HEAD` to discover
// the remote's default branch. Returns the branch name (e.g. "master") or ""
// if detection fails.
func detectDefaultBranch(workspacePath string, setupGitCmd func(*exec.Cmd)) string {
	cmd := exec.Command("git", "-C", workspacePath, "ls-remote", "--symref", "origin", "HEAD")
	setupGitCmd(cmd)
	out, err := procreap.OutputManaged(cmd)
	if err != nil {
		return ""
	}
	// Output format: "ref: refs/heads/master\tHEAD\n..."
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ref: refs/heads/") {
			// Split on tab to separate "ref: refs/heads/branch" from "HEAD"
			ref := strings.SplitN(line, "\t", 2)[0]
			return strings.TrimPrefix(ref, "ref: refs/heads/")
		}
	}
	return ""
}

// sanitizeGitOutput replaces any occurrence of the token in git output with "***".
func sanitizeGitOutput(output, token string) string {
	if token == "" {
		return output
	}
	return strings.ReplaceAll(output, token, "***")
}

// buildAuthenticatedURL constructs an HTTPS URL with embedded OAuth2 credentials.
// If no token is provided, the original URL is returned unchanged.
func buildAuthenticatedURL(cloneURL, token string) string {
	// Ensure the URL has an https:// scheme. The clone URL may arrive
	// without a scheme if it was stored from raw user input (e.g.
	// "github.com/org/repo" instead of "https://github.com/org/repo").
	normalized := cloneURL
	if !strings.Contains(normalized, "://") {
		normalized = "https://" + normalized
	}

	if token == "" {
		return normalized
	}

	parsed, err := url.Parse(normalized)
	if err != nil || parsed.Scheme == "" {
		return normalized
	}

	parsed.User = url.UserPassword("oauth2", token)
	return parsed.String()
}

// isClaude returns true when the child command is for the Claude Code harness.
// It scans all arguments because the harness binary may not be the first
// argument (e.g. "tmux new-session -s scion claude --no-chrome ...").
// It also handles the case where the harness command is joined into a single
// string passed to tmux (e.g. "claude --no-chrome --dangerously-skip-permissions").
func isClaude(childArgs []string) bool {
	for _, arg := range childArgs {
		// Split on whitespace to handle joined command strings
		for _, word := range strings.Fields(arg) {
			base := filepath.Base(word)
			if base == "claude" || strings.HasPrefix(base, "claude-") {
				return true
			}
		}
	}
	return false
}

// blockClaudeDebugSymlink pre-creates debugDir ($HOME/.claude/debug) as
// read-only (0555) so Claude Code cannot later create a symlink inside it —
// see this function's call site for why that matters.
//
// requirePrivilegeDrop is RunInit's own opts.RequirePrivilegeDrop (true only
// for substrate). On every OTHER runtime this keeps the plain
// behaviour — os.MkdirAll followed by os.Chmod(debugDir, ...) — unchanged: a
// legitimate non-substrate setup may bind-mount or symlink .claude itself
// (e.g. from a host directory), and refusing that would break it, with no
// privilege boundary at stake to justify the change.
//
// On substrate this runs as root: os.MkdirAll silently succeeds (via
// os.Stat, which follows symlinks) if debugDir already exists as anything,
// including a symlink, and the os.Chmod that follows it then chmods
// whatever that symlink points at — so a scion-uid process (a sidecar
// service, or a process a pre-start hook spawned) that plants
// ~/.claude/debug as a symlink to an arbitrary root-owned directory before
// this runs gets that directory chmod'd to 0555 (world-readable) by root.
// dirfd.EnsureDirNoFollow refuses a symlinked leaf outright instead of
// creating/resolving through it, and the chmod that follows is fchmod on
// the fd EnsureDirNoFollow already resolved, never a path-based os.Chmod
// that could be redirected by anything changed afterward. A refusal is
// logged (path only) and this simply skips the chmod rather than failing
// init closed — a planted symlink must not be able to stop the workload
// from starting.
func blockClaudeDebugSymlink(debugDir string, requirePrivilegeDrop bool) {
	if !requirePrivilegeDrop {
		if err := os.MkdirAll(debugDir, 0755); err != nil {
			log.Error("Failed to create debug directory %s: %v", debugDir, err)
		} else if err := os.Chmod(debugDir, 0555); err != nil {
			log.Error("Failed to chmod debug directory %s: %v", debugDir, err)
		} else {
			log.Debug("Blocked debug symlink: set %s to read-only", debugDir)
		}
		return
	}

	// EnsureDirNoFollow only creates its own leaf (it requires the parent
	// chain to already exist, unlike os.MkdirAll) — this runs before the
	// harness itself has started, so $HOME/.claude may not exist yet.
	// Ensure it first, no-follow at every component just like the leaf
	// below, then close it: only the leaf (debugDir) needs to stay open for
	// the chmod.
	claudeDir, err := dirfd.EnsureDirNoFollow(filepath.Dir(debugDir), 0755)
	if err != nil {
		log.Error("Refusing debug directory %s: parent is not a plain directory", debugDir)
		return
	}
	_ = claudeDir.Close()

	d, err := dirfd.EnsureDirNoFollow(debugDir, 0755)
	if err != nil {
		// Refuse (e.g. a symlink or non-directory at debugDir) rather than
		// follow/create through it — but this hardening is a convenience
		// mitigation for a Claude Code quirk, not something the workload's
		// startup can be allowed to depend on: log and move on instead of
		// failing init closed.
		log.Error("Refusing debug directory %s: not a plain directory", debugDir)
		return
	}
	defer func() { _ = d.Close() }()
	if blockClaudeDebugAfterEnsureForTest != nil {
		blockClaudeDebugAfterEnsureForTest(debugDir)
	}
	if err := d.Chmod(0555); err != nil {
		log.Error("Failed to chmod debug directory %s: %v", debugDir, err)
		return
	}
	log.Debug("Blocked debug symlink: set %s to read-only", debugDir)
}

// blockClaudeDebugAfterEnsureForTest is a test-only seam: it fires after
// EnsureDirNoFollow(debugDir) has already returned its fd and before the
// chmod that follows. It lets a test simulate the workload swapping
// debugDir's own entry in its parent for a symlink to a victim directory in
// that exact window — the same style of deterministic race injection as
// writeEnvFileAfterWriteForTest — and prove the chmod that follows still
// lands on the fd EnsureDirNoFollow already resolved, never on whatever the
// entry becomes afterward. Always nil in production.
var blockClaudeDebugAfterEnsureForTest func(debugDir string)

// scionEnvVarPrefixes lists environment variable prefixes that are written
// to the scion-env file for shell sessions to source.
var scionEnvVarPrefixes = []string{
	"SCION_",
	"GITHUB_TOKEN",
}

// writeEnvFileAfterWriteForTest is a test-only seam (see its call site in
// writeEnvFile). Production code never sets it.
var writeEnvFileAfterWriteForTest func(scionDir string)

// scionDirOwnerUID is writeEnvFile's "who owns the already-open .scion dir
// fd" call site, factored out as a package var so a test can simulate a
// root-owned directory (uid 0) without needing the test process to actually
// be root — see writeEnvFile's chown-gating doc comment for why only that
// state should trigger a chown. Production code always leaves this at its
// real, fd-based syscall.Fstat implementation.
var scionDirOwnerUID = func(fd int) (uint32, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return 0, err
	}
	return st.Uid, nil
}

// writeEnvFile writes critical SCION_* environment variables to a shell-sourceable
// file at ~/.scion/scion-env. Some harnesses (e.g. Gemini CLI) re-exec with a
// filtered environment, losing env vars that were passed via docker run -e. This
// file is sourced by the agent's .bashrc so that tool-spawned shell processes
// recover the full environment.
func writeEnvFile(agentHome string, uid, gid int) {
	scionDir := filepath.Join(agentHome, ".scion")
	// EnsureDirNoFollow resolves and creates (if missing) .scion via
	// mkdirat, and returns an already-open O_NOFOLLOW fd for it. Chowning
	// through that fd below, rather than a separate path-based os.Chown
	// after the fact, is what makes this safe: a file descriptor stays
	// bound to the inode it was opened against no matter what the
	// workload — which owns $HOME and this call runs as root while it's
	// alive — does to the ".scion" directory entry afterwards (rename it
	// away, replace it with a symlink to any other directory, and so on).
	scionDirFile, err := dirfd.EnsureDirNoFollow(scionDir, 0755)
	if err != nil {
		log.Error("Failed to create .scion dir for env file: %v", err)
		return
	}
	defer func() { _ = scionDirFile.Close() }()

	var lines []string
	lines = append(lines, "# Auto-generated by sciontool init — do not edit")
	for _, e := range os.Environ() {
		for _, prefix := range scionEnvVarPrefixes {
			if strings.HasPrefix(e, prefix) {
				// Split into key=value and quote the value for shell safety
				parts := strings.SplitN(e, "=", 2)
				if len(parts) == 2 {
					lines = append(lines, fmt.Sprintf("export %s=%q", parts[0], parts[1]))
				}
				break
			}
		}
	}

	envPath := filepath.Join(scionDir, "scion-env")
	content := []byte(strings.Join(lines, "\n") + "\n")
	// Ownership of the file itself is applied by WriteFileNoFollowChown
	// (fchown on the open fd, before the rename onto the final path), not
	// by a separate path-based os.Chown call afterwards: this file lives
	// under $HOME/.scion, which the scion user owns and can replace with a
	// symlink between an old-style write+rename and a path-based chown.
	if err := hub.WriteFileNoFollowChown(envPath, content, 0644, uid, gid); err != nil {
		log.Error("Failed to write scion-env file: %v", err)
		return
	}

	// Test-only seam: lets a test simulate the workload swapping out
	// $HOME/.scion for a symlink in the window between the file write
	// completing and the directory chown below, without needing a real
	// inotify-driven race. Always nil in production.
	if writeEnvFileAfterWriteForTest != nil {
		writeEnvFileAfterWriteForTest(scionDir)
	}

	if uid > 0 {
		// Fstat the SAME held fd (never re-resolving the path) to check who
		// currently owns .scion before chowning it. writeEnvFile runs on
		// every refresh, not just the first time: once .scion is already
		// owned by the target uid (the normal steady-state case, after the
		// first run's chown already landed), re-chowning it to the same
		// value on every subsequent refresh is a pure no-op that still
		// costs a chown(2) syscall for nothing. Root-owned (uid 0) is the
		// only state this should actually act on — the first run, before
		// any chown has happened yet; any other owner is unexpected (never
		// legitimately produced by this function or EnsureDirNoFollow's own
		// mkdirat) and is left alone rather than blindly reassigned.
		st, serr := scionDirOwnerUID(int(scionDirFile.Fd()))
		if serr != nil {
			log.Error("Failed to stat %s: %v", scionDir, serr)
		} else if st == 0 {
			// fchown the same fd EnsureDirNoFollow resolved above — never a
			// path-based os.Chown, which would re-resolve ".scion" from
			// scratch and could be redirected to an arbitrary directory by
			// a symlink the workload swapped in after that resolution.
			if err := scionDirFile.Chown(uid, gid); err != nil {
				log.Error("Failed to chown %s: %v", scionDir, err)
			}
		}
	}

	log.Debug("Wrote %d env vars to %s", len(lines)-1, envPath)
}

// isWorkspaceEmpty returns true if the directory doesn't exist or contains
// only provisioning marker entries (e.g. .scion/, .scion-volumes/). A workspace
// with only marker directories is considered empty for git-clone purposes so
// that sciontool proceeds with cloning rather than skipping it.
func isWorkspaceEmpty(path string) bool {
	entries, err := os.ReadDir(path)
	if err != nil {
		return true
	}
	// Filter out known marker entries that don't indicate a real workspace
	// (provisioning markers and shared-dir mount points; see
	// api.IsWorkspaceMarker).
	for _, e := range entries {
		if api.IsWorkspaceMarker(e.Name()) {
			continue
		}
		log.Debug("Workspace not empty: found %q in %s", e.Name(), path)
		return false
	}
	return true
}

// preExistingWorkspaceEntries returns the set of top-level entry names
// already present in path (e.g. bind-mounted marker directories such as
// .scion, .scion-volumes, .agents). Used by cleanFailedCloneAttempt to tell
// apart content that predates a clone attempt from content the attempt
// itself created. Returns an empty set (never nil) if path doesn't exist or
// can't be read, matching isWorkspaceEmpty's "missing dir counts as empty"
// treatment.
func preExistingWorkspaceEntries(path string) map[string]struct{} {
	existing := make(map[string]struct{})
	entries, err := os.ReadDir(path)
	if err != nil {
		return existing
	}
	for _, e := range entries {
		existing[e.Name()] = struct{}{}
	}
	return existing
}

// cleanFailedCloneAttempt restores workspacePath to a cloneable state after
// a failed clone attempt: it removes every top-level entry that is not in
// preExisting, i.e. everything this attempt created (.git, checked-out
// files, partial directories), while leaving anything that was already
// there (preExisting) untouched.
//
// This is needed because a clone step can partially populate the working
// tree and still be reported as a failure — most notably when a step like
// `git checkout` actually succeeds but its exec.Cmd.Wait loses the exit
// status to sciontool init's PID-1 zombie reaper racing it for the same
// PID. Without this, a subsequent retry's isWorkspaceEmpty() check would
// see those leftover files, treat the workspace as already populated, and
// skip cloning into it — leaving a workspace with real file content but no
// usable .git.
func cleanFailedCloneAttempt(workspacePath string, preExisting map[string]struct{}) {
	entries, err := os.ReadDir(workspacePath)
	if err != nil {
		log.Error("Failed to list workspace %s for clone-failure cleanup: %v", workspacePath, err)
		return
	}
	for _, e := range entries {
		if _, existed := preExisting[e.Name()]; existed {
			continue
		}
		entryPath := filepath.Join(workspacePath, e.Name())
		if err := os.RemoveAll(entryPath); err != nil {
			log.Error("Failed to clean up %s after clone failure: %v", entryPath, err)
		}
	}
}

// hubMessageAdapter adapts the Hub client to the autoexpose.MessageClient interface
// so the reconciler can notify the agent about auto-exposed ports.
type hubMessageAdapter struct {
	client *hub.Client
}

func (a *hubMessageAdapter) SendSelfMessage(ctx context.Context, msg string, metadata map[string]string) error {
	if a.client == nil {
		return fmt.Errorf("hub client is nil")
	}
	recipient := "agent:" + a.client.AgentID()
	return a.client.SendSelfMessage(ctx, messages.NewSystemMessage("system", recipient, msg, metadata["system_category"]))
}

// splitSecretKeys parses the SCION_SECRET_KEYS comma-separated value into
// individual key names. Empty segments are silently dropped.
func splitSecretKeys(raw string) []string {
	parts := strings.Split(raw, ",")
	keys := make([]string, 0, len(parts))
	for _, p := range parts {
		k := strings.TrimSpace(p)
		if k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// fetchSecretOverrides calls the hub's POST /api/v1/agent/secrets endpoint
// and returns the successfully fetched values as a key→value map suitable
// for supervisor.Config.SecretOverrides.
//
// Per-key statuses are logged but non-ok keys do NOT abort the agent. P4
// is the phase that decides failure policy for missing keys; P2d records
// the outcome and continues. Never logs a secret value — only key names
// and statuses. (#127, P2d)
func fetchSecretOverrides(client *hub.Client, keys []string) map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.FetchSecrets(ctx, keys)
	if err != nil {
		log.Error("Failed to fetch secrets from hub: %v", err)
		return nil
	}

	overrides := make(map[string]string, len(resp.Secrets))
	for _, s := range resp.Secrets {
		switch s.Status {
		case hub.SecretStatusOK:
			overrides[s.Key] = s.Value
			log.Info("Fetched secret %s: ok", s.Key)
		case hub.SecretStatusUnavailable:
			log.Error("Secret %s: entitled but unavailable — %s", s.Key, s.Error)
		case hub.SecretStatusAccessWithdrawn:
			log.Error("Secret %s: access withdrawn — %s", s.Key, s.Error)
		case hub.SecretStatusNotFound:
			log.Error("Secret %s: not found", s.Key)
		default:
			log.Error("Secret %s: unknown status %q — %s", s.Key, s.Status, s.Error)
		}
	}

	if len(overrides) > 0 {
		log.Info("Fetched %d of %d requested secret(s)", len(overrides), len(keys))
	} else if len(keys) > 0 {
		log.Error("No secrets were successfully fetched (%d requested)", len(keys))
	}

	return overrides
}

// gcloudConfigKeepFile is the one entry cleanGcloudConfigForMetadata never
// removes: it may be bind-mounted as a gcloud-adc secret, independent of the
// rest of gcloud's local config state.
const gcloudConfigKeepFile = "application_default_credentials.json"

// cleanGcloudConfigForMetadata removes gcloud configuration state files from
// the given directory while preserving application_default_credentials.json,
// which may be bind-mounted as a gcloud-adc secret. Clearing the config state
// forces gcloud to re-initialize and discover the emulated metadata server.
//
// requirePrivilegeDrop is RunInit's own opts.RequirePrivilegeDrop (true only
// for substrate). On every OTHER runtime this keeps the plain path-based
// behaviour (os.ReadDir + os.RemoveAll by joined path) —
// unchanged, because a legitimate non-substrate setup may bind-mount a
// symlink at gcloudDir itself (e.g. a host-mounted gcloud config directory)
// and refusing that would break it, with no privilege boundary at stake to
// justify the behaviour change.
//
// On substrate this runs as root, after sidecar services have already
// started (so a scion-uid process may already be alive), against a
// directory the scion user owns: a symlink planted there — deterministically
// before this runs, or swapped mid-walk once it's a real directory being
// emptied — must not let root delete or descend into an attacker-chosen
// target. dirfd.OpenDirNoFollow refuses (rather than follows) a symlinked
// gcloudDir outright, and dirfd.RemoveContentsNoFollow removes its contents
// via unlinkat relative to that held fd (recursing into subdirectories the
// same fd-relative way), never re-resolving a joined path string. A refusal
// here is logged (path only) and this simply skips the cleanup rather than
// failing init closed — a planted symlink must not be able to stop the
// workload from starting.
func cleanGcloudConfigForMetadata(gcloudDir string, requirePrivilegeDrop bool) {
	if !requirePrivilegeDrop {
		entries, err := os.ReadDir(gcloudDir)
		if err != nil {
			// Directory doesn't exist — nothing to clean.
			return
		}
		for _, e := range entries {
			if e.Name() == gcloudConfigKeepFile {
				continue
			}
			p := filepath.Join(gcloudDir, e.Name())
			if err := os.RemoveAll(p); err != nil {
				log.Debug("Could not remove gcloud config entry %s: %v", p, err)
			}
		}
		return
	}

	dir, err := dirfd.OpenDirNoFollow(gcloudDir)
	if err != nil {
		// errors.Is, not os.IsNotExist: OpenDirNoFollow's error is wrapped
		// with fmt.Errorf when a missing component is one of gcloudDir's
		// intermediate directories (e.g. $HOME/.config itself doesn't exist
		// yet) rather than gcloudDir's own leaf, and os.IsNotExist only
		// unwraps the specific *PathError/*LinkError/*SyscallError types,
		// not an arbitrary %w chain — it would otherwise misreport that
		// entirely ordinary case as a refused symlink.
		if !errors.Is(err, os.ErrNotExist) {
			// Refuse (e.g. a symlink or non-directory at gcloudDir) rather
			// than follow it — but this cleanup is a best-effort
			// convenience for gcloud auto-discovery, not something the
			// workload's startup can be allowed to depend on: log and move
			// on instead of failing init closed.
			log.Error("Refusing to clean gcloud config dir %s: not a plain directory", gcloudDir)
		}
		return
	}
	defer func() { _ = dir.Close() }()

	if _, err := dirfd.RemoveContentsNoFollow(dir, func(name string) bool { return name == gcloudConfigKeepFile }, func(name string, rerr error) {
		log.Debug("Could not remove gcloud config entry %s: %v", name, rerr)
	}); err != nil {
		log.Debug("Could not clean gcloud config dir %s: %v", gcloudDir, err)
	}
}

// readServicesYAML reads the sidecar-services config file at path.
// requirePrivilegeDrop is RunInit's own opts.RequirePrivilegeDrop (true only
// for substrate).
//
// On every OTHER runtime this keeps the plain behaviour —
// os.ReadFile(path), which follows a symlink at any component — unchanged:
// a legitimate non-substrate setup may symlink this file (e.g. from a
// mounted config directory), and refusing that would break it, with no
// privilege boundary at stake to justify the change.
//
// On substrate this reads as root, after pre-start hooks have already run
// as the workload user (which owns $HOME/.scion outright): a symlink
// planted at path must not let root read and act on an arbitrary file's
// content. dirfd.OpenParentNoFollow + O_NOFOLLOW refuses (rather than
// follows) a symlinked path, and the opened fd is fstat'd and refused
// unless it is a single-link regular file — a hardlink to an unrelated
// (possibly root-owned) file would otherwise pass a bare "is this a
// regular file" check. A refusal here is logged (path only) and reported
// as an error, which the caller already treats as "no services to start"
// rather than a fatal init error — a planted symlink must not be able to
// stop the workload from starting.
//
// This does NOT defend against the workload simply overwriting the file's
// CONTENT in place — no symlink needed, since it owns the containing
// directory outright, and pre-start hooks (which run as the workload user)
// complete before this is ever read. That is accepted rather than defended
// against for the SERVICE IDENTITY dimension: no service this file can
// describe ever runs above the workload's own uid. Manager.Start takes
// exactly one uid/gid pair for the whole batch of services, sourced from
// RunInit's own RequirePrivilegeDrop-enforced drop target
// (requirePrivilegeDropOrFail fails RunInit closed before this point if
// that target is uid 0 — see its own doc comment), never from any field
// the parsed YAML controls — the service spec type carries no uid/gid/user/
// capability field at all. So a rewritten file can only make root start
// additional processes running AS the workload — no privilege the workload
// does not already have as itself by just running its own code. The
// service NAME dimension (a workload-chosen Name used to build a log file
// path) is a separate, defended-against instance of the same content-trust
// class — see services.ValidateServiceName's doc comment.
//
// The enforced branch's read is bounded (servicesYAMLMaxBytes): without a
// bound, a workload-planted multi-GB regular file (still a single-link
// regular file, so it passes every check above) would make root's own init
// process read the whole thing into memory before the harness ever starts —
// a self-inflicted OOM, not a privilege issue, but cheap to close. The
// non-enforced branch is a plain, unbounded os.ReadFile call.
func readServicesYAML(path string, requirePrivilegeDrop bool) ([]byte, error) {
	if !requirePrivilegeDrop {
		return os.ReadFile(path)
	}

	dirFd, leaf, err := dirfd.OpenParentNoFollow(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Error("Refusing to read %s: %v", path, err)
		}
		return nil, err
	}
	defer func() { _ = syscall.Close(dirFd) }()

	f, err := dirfd.OpenAt(dirFd, leaf, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Error("Refusing to read %s: not a plain file", path)
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		return nil, err
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG || st.Nlink != 1 {
		log.Error("Refusing to read %s: not a single-link regular file", path)
		return nil, fmt.Errorf("refusing to read %s: not a single-link regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, servicesYAMLMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > servicesYAMLMaxBytes {
		log.Error("Refusing to read %s: exceeds %d bytes", path, servicesYAMLMaxBytes)
		return nil, fmt.Errorf("refusing to read %s: exceeds %d bytes", path, servicesYAMLMaxBytes)
	}
	return data, nil
}

// servicesYAMLMaxBytes bounds readServicesYAML's enforced-mode read. A
// legitimate scion-services.yaml describing even a large number of
// sidecars is a few kilobytes; 1 MiB is generous headroom with no
// legitimate case anywhere near it.
const servicesYAMLMaxBytes = 1 << 20

// validateServiceSpecs is the authoritative gate for the service-Name-as-
// path-component content-trust class (see services.ValidateServiceName's
// own doc comment for the exact rule): it runs immediately after
// scion-services.yaml is parsed, before any consumer of Name ever sees it,
// and drops any spec whose Name is invalid — logged, name
// quoted/capped/escaped via services.SafeNameForLog, never a raw
// workload-chosen string — while keeping every other valid spec, regardless
// of position, so one bad entry cannot take down the rest of the batch.
// Applies unconditionally, on every runtime: a legitimate Name is a plain
// identifier, so no valid caller is ever affected.
func validateServiceSpecs(specs []api.ServiceSpec) []api.ServiceSpec {
	valid := make([]api.ServiceSpec, 0, len(specs))
	for _, spec := range specs {
		if err := services.ValidateServiceName(spec.Name); err != nil {
			log.Error("Dropping service with invalid name %s: %v", services.SafeNameForLog(spec.Name), err)
			continue
		}
		valid = append(valid, spec)
	}
	return valid
}

// hasCapSetUID checks whether the current process has CAP_SETUID (bit 7)
// in its effective capability set by reading /proc/self/status.
// Returns true if the capability is present, false if absent or on error
// (safe default: assume restricted, skip privilege operations).
func hasCapSetUID() bool {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	return parseCapSetUID(string(data))
}

// parseCapSetUID parses the content of /proc/self/status and returns true
// if CAP_SETUID (bit 7) is present in the effective capability set.
func parseCapSetUID(statusContent string) bool {
	return parseCapBit(statusContent, 7) // CAP_SETUID = bit 7
}

// parseCapBit parses /proc/self/status content and returns whether the given
// bit is set in the effective capability set (CapEff). Shared by
// parseCapSetUID (above) and hasCapBit (substrate_privilege_drop.go) so they
// can never drift in how they read the file.
func parseCapBit(statusContent string, bit uint) bool {
	for _, line := range strings.Split(statusContent, "\n") {
		if strings.HasPrefix(line, "CapEff:") {
			hexStr := strings.TrimSpace(strings.TrimPrefix(line, "CapEff:"))
			caps, err := strconv.ParseUint(hexStr, 16, 64)
			if err != nil {
				return false
			}
			return caps&(1<<bit) != 0
		}
	}
	return false
}
