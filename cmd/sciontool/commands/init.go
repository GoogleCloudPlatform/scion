/*
Copyright 2025 The Scion Authors.
*/

package commands

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/metadata"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/services"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/supervisor"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

var (
	gracePeriod time.Duration
)

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
		exitCode := runInit(args)
		os.Exit(exitCode)
	},
}

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

func runInit(args []string) int {
	// Start the reaper goroutine for zombie process cleanup.
	// This is critical when running as PID 1 in a container.
	supervisor.StartReaper()

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

	// Set up scion user UID/GID to match host user
	targetUID, targetGID, rootless := setupHostUser()
	log.Info("setupHostUser result: targetUID=%d, targetGID=%d, rootless=%v (now euid=%d, egid=%d)", targetUID, targetGID, rootless, os.Geteuid(), os.Getegid())

	// Chown the log file so the scion user can write to it even if it was created by root
	if targetUID != 0 {
		if err := log.Chown(targetUID, targetGID); err != nil {
			log.Error("Failed to chown log file: %v", err)
		}
	}

	// Start telemetry pipeline if configured
	var telemetryPipeline *telemetry.Pipeline
	if pipeline := telemetry.New(); pipeline != nil {
		telemetryCtx, telemetryCancel := context.WithCancel(context.Background())
		if err := pipeline.Start(telemetryCtx); err != nil {
			log.Error("Failed to start telemetry: %v", err)
			telemetryCancel()
			// Continue anyway - telemetry failure shouldn't block agent
		} else {
			telemetryPipeline = pipeline
			log.Info("Telemetry pipeline started")
		}
		defer func() {
			if telemetryPipeline != nil {
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := telemetryPipeline.Stop(shutdownCtx); err != nil {
					log.Error("Failed to stop telemetry: %v", err)
				}
				shutdownCancel()
			}
			telemetryCancel()
		}()
	}

	// Resolve the scion user's home directory early. Init runs as root
	// (HOME=/root), but agent-info.json and other agent state files live
	// in the scion user's home directory. This must happen before the
	// StatusHandler is created so it writes to the correct path.
	// In rootless mode, targetUID is 0 but we still need the scion user's
	// home directory since the child process environment will use it.
	agentHome := os.Getenv("HOME")
	if targetUID != 0 {
		if scionUser, err := user.LookupId(strconv.Itoa(targetUID)); err == nil {
			agentHome = scionUser.HomeDir
		} else {
			log.Debug("Could not look up user for UID %d: %v", targetUID, err)
		}
	} else if rootless {
		if scionUser, err := user.Lookup("scion"); err == nil {
			agentHome = scionUser.HomeDir
		} else {
			log.Debug("Could not look up scion user in rootless mode: %v", err)
		}
	}

	// Initialize lifecycle hooks manager
	lifecycleManager := hooks.NewLifecycleManager()
	lifecycleManager.AgentHome = agentHome
	// Register the per-agent hooks directory so container-script harnesses
	// (whose pre-start wrapper is staged at $HOME/.scion/hooks/pre-start.d/)
	// participate in the standard hook discovery alongside system hooks.
	lifecycleManager.AddHooksDir(filepath.Join(agentHome, ".scion", "hooks"))

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

		var tp trace.TracerProvider
		var lp otellog.LoggerProvider
		var mp metric.MeterProvider
		if lifecycleProviders != nil {
			tp = lifecycleProviders.TracerProvider
			lp = lifecycleProviders.LoggerProvider
			if lifecycleProviders.MeterProvider != nil {
				mp = lifecycleProviders.MeterProvider
			}
		}
		telemetryHandler = handlers.NewTelemetryHandler(tp, lp, redactor, mp)
		log.Info("Telemetry handler initialized for hook-to-span conversion")

		// Register telemetry handler for lifecycle events
		for _, eventName := range []string{hooks.EventPreStart, hooks.EventPostStart, hooks.EventPreStop, hooks.EventSessionEnd} {
			lifecycleManager.RegisterHandler(eventName, telemetryHandler.Handle)
		}
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
		// Treat parse errors on a present manifest as fatal — the harness
		// staged something we cannot interpret.
		return 1
	}

	// Run pre-start hooks (after setup, before child process)
	log.Info("Running pre-start hooks...")
	if err := lifecycleManager.RunPreStart(); err != nil {
		log.Error("Pre-start hooks failed: %v", err)
		if harnessReq.Required {
			log.Error("Container-script harness pre-start provisioning is required; aborting startup")
			statusHandler.UpdatePhase(state.PhaseError, "", "")
			statusHandler.SetMessage(fmt.Sprintf("pre-start provisioning failed: %v", err))
			return 1
		}
		// Continue anyway - hooks failing shouldn't prevent startup
	}

	// Load the env overlay produced by the pre-start provisioner. Resolve
	// any from_file references to in-memory values so secrets are not
	// written back to logs or persistent JSON. Fail startup when the
	// overlay is malformed or references missing files for a required
	// container-script harness — the child would otherwise launch without
	// its credentials.
	var harnessEnvOverlay map[string]string
	if harnessReq.EnvOverlayPath != "" {
		overlayPath := hooks.ResolveContainerPath(harnessReq.EnvOverlayPath, agentHome)
		allowedRoots := []string{harnessReq.BundleDir, agentHome}
		overlay, err := hooks.LoadEnvOverlay(overlayPath, allowedRoots)
		if err != nil {
			log.Error("Failed to load harness env overlay %s: %v", overlayPath, err)
			if harnessReq.Required {
				statusHandler.UpdatePhase(state.PhaseError, "", "")
				statusHandler.SetMessage(fmt.Sprintf("invalid harness env overlay: %v", err))
				return 1
			}
		} else if len(overlay) > 0 {
			harnessEnvOverlay = overlay
			log.Info("Loaded %d env overlay entries from %s", len(overlay), overlayPath)
		}
	}

	// Clone git workspace if configured (hub-first git groves)
	if err := gitCloneWorkspace(targetUID, targetGID, agentHome); err != nil {
		log.Error("Git clone failed: %v", err)

		// Update local agent-info.json to error state so local status readers
		// and the broker heartbeat see the failure and error message.
		errMsg := fmt.Sprintf("git clone failed: %v", err)
		statusHandler.UpdatePhase(state.PhaseError, "", "")
		statusHandler.SetMessage(errMsg)

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

	// Configure git credentials for shared-workspace groves (git-workspace hybrid).
	// The workspace is pre-cloned on the host; agents need credentials to push/pull.
	if os.Getenv("SCION_SHARED_WORKSPACE") == "true" {
		configureSharedWorkspaceGit(agentHome)
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
		if err := os.MkdirAll(debugDir, 0755); err != nil {
			log.Error("Failed to create debug directory %s: %v", debugDir, err)
		} else if err := os.Chmod(debugDir, 0555); err != nil {
			log.Error("Failed to chmod debug directory %s: %v", debugDir, err)
		} else {
			log.Debug("Blocked debug symlink: set %s to read-only", debugDir)
		}
	}

	servicesPath := filepath.Join(agentHome, ".scion", "scion-services.yaml")
	log.Debug("Looking for services config at: %s", servicesPath)
	if data, err := os.ReadFile(servicesPath); err == nil {
		var specs []api.ServiceSpec
		if err := yaml.Unmarshal(data, &specs); err != nil {
			log.Error("Failed to parse scion-services.yaml: %v", err)
		} else if len(specs) > 0 {
			log.Info("Starting %d sidecar service(s)...", len(specs))
			svcManager = services.New(gracePeriod)
			svcCtx := context.Background()
			if err := svcManager.Start(svcCtx, specs, targetUID, targetGID, "scion"); err != nil {
				log.Error("Failed to start services: %v", err)
				// Continue — service failure shouldn't block harness
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
		cleanGcloudConfigForMetadata(filepath.Join(agentHome, ".config", "gcloud"))
		// Wire up dynamic token retrieval so the metadata server always
		// uses the latest agent token after refresh, not the startup value.
		metaCfg.TokenFunc = func() string {
			return hub.ReadTokenFile()
		}
		metadataServer = metadata.New(*metaCfg)
		metaCtx := context.Background()
		if err := metadataServer.Start(metaCtx); err != nil {
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
			os.Remove(f.Name())
			f.Close()
			log.Debug("Pre-flight: agent home %s is writable (uid=%d)", agentHome, os.Geteuid())
		}
		if _, err := exec.LookPath("tmux"); err != nil {
			log.Error("Pre-flight: tmux not found on PATH: %v", err)
		}
	}

	// Create supervisor with configuration
	config := supervisor.Config{
		GracePeriod: gracePeriod,
		UID:         targetUID,
		GID:         targetGID,
		Username:    "scion",
		Rootless:    rootless,
		EnvOverlay:  harnessEnvOverlay,
	}
	sup := supervisor.New(config)

	// Create a cancellable context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up signal handling with pre-stop hook for graceful shutdown
	sigHandler := supervisor.NewSignalHandler(sup, cancel).
		WithPreStopHook(func() error {
			log.Info("Running pre-stop hooks...")
			return lifecycleManager.RunPreStop()
		})
	sigHandler.Start()
	defer sigHandler.Stop()

	// Run the child process under supervision
	// We use a goroutine to allow post-start hooks to run after process starts
	exitChan := make(chan struct {
		code int
		err  error
	}, 1)

	go func() {
		code, err := sup.Run(ctx, childArgs)
		exitChan <- struct {
			code int
			err  error
		}{code, err}
	}()

	// Heartbeat and token refresh control variables - declared here so they're accessible during shutdown
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
		hubClient := hub.NewClient()
		log.Debug("Hub client check: client=%v, configured=%v", hubClient != nil, hubClient != nil && hubClient.IsConfigured())
		log.Debug("Hub env: SCION_HUB_ENDPOINT=%q, SCION_HUB_URL=%q, token_file=%v, SCION_AGENT_ID=%q",
			os.Getenv("SCION_HUB_ENDPOINT"), os.Getenv("SCION_HUB_URL"), hub.ReadTokenFile() != "", os.Getenv("SCION_AGENT_ID"))
		if hubClient != nil && hubClient.IsConfigured() {
			hubCtx, hubCancel := context.WithTimeout(context.Background(), 10*time.Second)
			startedAtStr := time.Now().UTC().Format(time.RFC3339)
			zeroCount := 0
			s := state.AgentState{Phase: state.PhaseRunning, Activity: state.ActivityIdle}
			if err := hubClient.UpdateStatus(hubCtx, hub.StatusUpdate{
				Phase:             state.PhaseRunning,
				Activity:          state.ActivityIdle,
				Status:            s.DisplayStatus(),
				Message:           "Agent started",
				StartedAt:         startedAtStr,
				CurrentTurns:      &zeroCount,
				CurrentModelCalls: &zeroCount,
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

			// Read the agent token from the canonical token file (written by
			// the host-side agent manager before the container started).
			// Init runs as root — chown the file so the scion user can read it.
			token := hub.ReadTokenFile()
			if token != "" && targetUID > 0 {
				if err := os.Chown(hub.TokenFilePath(), targetUID, targetGID); err != nil {
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
					// Token is already within the refresh window or expired
					if time.Now().Before(tokenExpiry) {
						// Still valid, refresh immediately
						refreshAt = time.Now()
						log.Info("Token within refresh window, refreshing immediately (expires: %s)", tokenExpiry.Format(time.RFC3339))
					} else {
						// Token has already expired
						log.Error("AUTH_EXPIRED: Agent token has expired at %s - hub communication will fail", tokenExpiry.Format(time.RFC3339))
						log.Error("AUTH_EXPIRED: Agent limits (max-duration, max-turns, max-model-calls) are enforced locally and remain active")
						refreshAt = time.Time{} // signal not to start refresh
					}
				} else {
					log.Info("Token refresh scheduled at %s (token expires: %s)",
						refreshAt.Format(time.RFC3339), tokenExpiry.Format(time.RFC3339))
				}

				if !refreshAt.IsZero() {
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
			}
		} else {
			log.Debug("Hub client not configured - skipping status report")
		}

		// Warn if user-provided GITHUB_TOKEN overlaps with GitHub App
		if os.Getenv(hub.EnvUserGitHubToken) == "true" {
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
				if err := hub.WriteGitHubTokenFile(tokenPath, initialToken); err != nil {
					log.Error("Failed to write initial GitHub token file: %v", err)
				} else {
					log.Info("Wrote initial GitHub token to %s", tokenPath)
					if targetUID > 0 {
						if err := os.Chown(tokenPath, targetUID, targetGID); err != nil {
							log.Error("Failed to chown GitHub token file to UID=%d: %v", targetUID, err)
						}
					}
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
					// detect stale tokens even before the first refresh cycle.
					if err := hub.WriteGitHubTokenExpiry(tokenPath, ghTokenExpiry); err != nil {
						log.Error("Failed to write initial GitHub token expiry file: %v", err)
					} else if targetUID > 0 {
						expiryPath := hub.GitHubTokenExpiryPath(tokenPath)
						if err := os.Chown(expiryPath, targetUID, targetGID); err != nil {
							log.Error("Failed to chown GitHub token expiry file to UID=%d: %v", targetUID, err)
						}
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
		if err := handlers.InitLimitsFile(limitsPath, maxTurns, maxModelCalls); err != nil {
			log.Error("Failed to initialize agent-limits.json: %v", err)
		} else {
			log.Info("Limits initialized: max_turns=%d, max_model_calls=%d", maxTurns, maxModelCalls)
		}
		// Chown the limits file so the scion user (hook processes) can read/write it.
		// Init runs as root but hooks run as the dropped-privilege scion user.
		if targetUID != 0 {
			if err := os.Chown(limitsPath, targetUID, targetGID); err != nil {
				log.Error("Failed to chown agent-limits.json: %v", err)
			}
		}
		// Remove stale trigger file from a previous run
		os.Remove(handlers.LimitsTriggerFile)
	}

	// Watch for limits-exceeded trigger file (works across UID boundaries).
	// This supplements SIGUSR1 which may fail when hooks run as non-root.
	triggerChan := make(chan struct{}, 1)
	triggerCtx, triggerCancel := context.WithCancel(context.Background())
	defer triggerCancel()
	if maxTurns > 0 || maxModelCalls > 0 {
		go watchLimitsTriggerFile(triggerCtx, triggerChan)
	}

	// Wait for child to exit, duration limit, SIGUSR1, or trigger file
	var result struct {
		code int
		err  error
	}
	limitsExceeded := false

	select {
	case r := <-exitChan:
		result = r
	case <-durationTimer:
		limitsExceeded = true
		handleLimitsExceeded(sup, "duration", fmt.Sprintf("max_duration of %s exceeded", maxDurStr))
		result = <-exitChan
	case <-usr1Chan:
		// SIGUSR1 received from hook handler - limits already set in agent-info.json
		limitsExceeded = true
		log.TaggedInfo("LIMITS_EXCEEDED", "Received SIGUSR1: limit exceeded, initiating shutdown")
		// Initiate graceful shutdown of the child process
		if err := sup.Signal(syscall.SIGTERM); err != nil {
			log.Error("Failed to send SIGTERM to child: %v", err)
		}
		result = <-exitChan
	case <-triggerChan:
		// Trigger file detected from hook handler - limits already set in agent-info.json
		limitsExceeded = true
		log.TaggedInfo("LIMITS_EXCEEDED", "Trigger file detected: limit exceeded, initiating shutdown")
		if err := sup.Signal(syscall.SIGTERM); err != nil {
			log.Error("Failed to send SIGTERM to child: %v", err)
		}
		result = <-exitChan
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
	finalCode := result.code
	if limitsExceeded {
		finalCode = handlers.ExitCodeLimitsExceeded
	} else if result.err != nil && result.code == 0 {
		finalCode = 1
	}
	isCrash := !limitsExceeded && finalCode != 0

	// Build crash message: distinguish real child exit codes from
	// synthetic ones produced by supervisor errors.
	var crashMsg string
	if isCrash {
		if result.err != nil && result.code == 0 {
			crashMsg = fmt.Sprintf("Agent crashed (supervisor error: %v)", result.err)
		} else {
			crashMsg = fmt.Sprintf("Agent crashed with exit code %d", finalCode)
		}
	}

	// Update local agent-info.json BEFORE the Hub report so the broker
	// heartbeat can relay crash/limits state even if the Hub call is slow
	// or fails entirely.
	if isCrash {
		statusHandler.UpdatePhase(state.PhaseStopped, state.ActivityCrashed, "")
		statusHandler.SetMessage(crashMsg)
	} else if limitsExceeded {
		statusHandler.UpdatePhase(state.PhaseStopped, state.ActivityLimitsExceeded, "")
		statusHandler.SetMessage("limits exceeded")
	}

	// Report final status to Hub, distinguishing clean stop from crash.
	if hubClient := hub.NewClient(); hubClient != nil && hubClient.IsConfigured() {
		hubCtx, hubCancel := context.WithTimeout(context.Background(), 5*time.Second)
		var hubErr error
		if isCrash {
			s := state.AgentState{Phase: state.PhaseStopped, Activity: state.ActivityCrashed}
			hubErr = hubClient.UpdateStatus(hubCtx, hub.StatusUpdate{
				Phase:    state.PhaseStopped,
				Activity: state.ActivityCrashed,
				Status:   s.DisplayStatus(),
				Message:  crashMsg,
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
			log.Info("Reported final status to Hub (exitCode=%d, crash=%v)", finalCode, isCrash)
		}
		hubCancel()
	}

	if limitsExceeded {
		log.Info("Exiting with code %d (limits exceeded)", handlers.ExitCodeLimitsExceeded)
		return handlers.ExitCodeLimitsExceeded
	}

	if result.err != nil {
		log.Error("Supervisor error: %v", result.err)
		return 1
	}

	log.Info("Child exited with code %d", result.code)
	return result.code
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

// extractChildCommand extracts the command arguments.
// Cobra handles -- separator, so args contains everything after --.
func extractChildCommand(args []string) []string {
	return args
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

func setupHostUser() (int, int, bool) {
	// Only run privilege operations if we're root. When running under
	// --userns=keep-id (rootless Podman), PID 1 starts as the scion user
	// (UID 1000) rather than root. In that case, no usermod/groupmod/chown
	// is needed — the UID already matches the scion user and bind-mounted
	// files have correct host ownership via the keep-id mapping. We return
	// rootless=true so the supervisor sets HOME/USER/LOGNAME without
	// attempting a credential drop.
	if os.Getuid() != 0 {
		if scionUser, err := user.Lookup("scion"); err == nil {
			scionUID, _ := strconv.Atoi(scionUser.Uid)
			if os.Getuid() == scionUID {
				log.Info("Already running as scion user (UID %d) in rootless mode, skipping privilege operations", scionUID)
				return 0, 0, true
			}
		}
		log.Debug("Not running as root, skipping user setup")
		return 0, 0, false
	}

	hostUID := os.Getenv("SCION_HOST_UID")
	hostGID := os.Getenv("SCION_HOST_GID")

	if hostUID == "" || hostGID == "" {
		log.Debug("SCION_HOST_UID/GID not set, skipping user setup")
		return 0, 0, false // Continue as root
	}

	uid, err := strconv.Atoi(hostUID)
	if err != nil {
		log.Error("Invalid SCION_HOST_UID: %v", err)
		return 0, 0, false
	}
	gid, err := strconv.Atoi(hostGID)
	if err != nil {
		log.Error("Invalid SCION_HOST_GID: %v", err)
		return 0, 0, false
	}

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
			if scionUser, err := user.Lookup("scion"); err == nil {
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
	if !isUIDMapped(uid) {
		log.Info("UID %d is not mapped in the container user namespace (rootless container); skipping user remapping", uid)
		return 0, 0, true
	}

	// Skip if UID/GID already match (1001 is the default)
	currentInfo, _ := user.Lookup("scion")
	if currentInfo != nil {
		currentUID, _ := strconv.Atoi(currentInfo.Uid)
		currentGID, _ := strconv.Atoi(currentInfo.Gid)
		log.Debug("Current scion user: UID=%d, GID=%d (Target: UID=%d, GID=%d)", currentUID, currentGID, uid, gid)
		if currentUID == uid && currentGID == gid {
			log.Debug("scion user already has correct UID/GID")
			return uid, gid, false
		}
	} else {
		log.Error("scion user not found in system")
	}

	log.Info("Adjusting scion user to UID=%d, GID=%d", uid, gid)

	if useDirectPasswdEdit() {
		log.Info("Using direct /etc/passwd edit (avoiding slow usermod on this runtime)")
		if err := directSetUID("scion", hostUID, hostGID); err != nil {
			log.Error("Direct passwd/group edit failed: %v", err)
			return 0, 0, false
		}
	} else {
		// Modify group first (if different from current)
		if err := exec.Command("groupmod", "-o", "-g", hostGID, "scion").Run(); err != nil {
			log.Error("Failed to modify scion group to %s: %v", hostGID, err)
		}

		// Modify user UID and primary group
		if err := exec.Command("usermod", "-o", "-u", hostUID, "-g", hostGID, "scion").Run(); err != nil {
			// usermod can fail with exit code 12 on runtimes where the home
			// directory is a mount point (e.g. Apple Virtualization / VirtioFS)
			// because it tries a recursive chown that the filesystem rejects.
			// Fall back to direct /etc/passwd editing which skips recursive chown.
			log.Info("usermod failed (exit: %v), falling back to direct passwd edit", err)
			if err := directSetUID("scion", hostUID, hostGID); err != nil {
				log.Error("Direct passwd/group fallback also failed: %v", err)
				return 0, 0, false
			}
		}
	}

	// Verify the change
	if updatedInfo, err := user.Lookup("scion"); err == nil {
		log.Info("Successfully adjusted scion user: UID=%s, GID=%s", updatedInfo.Uid, updatedInfo.Gid)
	} else {
		log.Error("Failed to verify scion user after adjustment: %v", err)
	}

	return uid, gid, false
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

// directSetUID modifies /etc/passwd and /etc/group directly to change a user's
// UID and GID without the recursive chown that usermod performs. This also
// chowns the user's home directory and its immediate contents so ownership is
// correct. The home directory should only contain skeleton files from useradd,
// so this is fast even on fuse-overlayfs.
func directSetUID(username, newUID, newGID string) error {
	// Update /etc/group: replace the GID (3rd field) for the matching group
	groupSed := exec.Command("sed", "-i", "-E",
		fmt.Sprintf(`s/^(%s:x:)[0-9]+:/\1%s:/`, username, newGID),
		"/etc/group")
	if out, err := groupSed.CombinedOutput(); err != nil {
		return fmt.Errorf("sed /etc/group: %w (output: %s)", err, string(out))
	}

	// Update /etc/passwd: replace both UID (3rd field) and GID (4th field)
	// Format: username:x:UID:GID:...
	passwdSed := exec.Command("sed", "-i", "-E",
		fmt.Sprintf(`s/^(%s:x:)[0-9]+:[0-9]+:/\1%s:%s:/`, username, newUID, newGID),
		"/etc/passwd")
	if out, err := passwdSed.CombinedOutput(); err != nil {
		return fmt.Errorf("sed /etc/passwd: %w (output: %s)", err, string(out))
	}

	// Chown the home directory and its immediate contents (skeleton files).
	// We avoid a deep recursive walk since that's the expensive part of
	// usermod on fuse-overlayfs. The home dir should only have dotfiles
	// from /etc/skel at this point.
	uid := mustAtoi(newUID)
	gid := mustAtoi(newGID)
	homeDir := fmt.Sprintf("/home/%s", username)
	if err := os.Chown(homeDir, uid, gid); err != nil {
		log.Debug("Failed to chown home directory %s: %v", homeDir, err)
	}
	entries, err := os.ReadDir(homeDir)
	if err == nil {
		for _, e := range entries {
			p := filepath.Join(homeDir, e.Name())
			if err := os.Chown(p, uid, gid); err != nil {
				log.Debug("Failed to chown %s: %v", p, err)
			}
		}
	}

	return nil
}

func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
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

// gitCloneWorkspace clones a git repository into /workspace when SCION_GIT_CLONE_URL
// is set. This supports hub-first git groves where the repository must be cloned
// before the harness starts. When uid > 0, all git commands run as the specified
// user so that the resulting files are owned by the scion user rather than root.
// agentHome is the scion user's home directory, used to write the credential
// helper to the correct .gitconfig (not root's HOME).
// Returns nil if no clone URL is configured (non-git workspace).
func gitCloneWorkspace(uid, gid int, agentHome string) error {
	cloneURL := os.Getenv("SCION_GIT_CLONE_URL")
	if cloneURL == "" {
		return nil
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

	// When uid is 0 (broker running as root or no host UID configured), fall
	// back to the scion user so that cloned files are owned by the container
	// user rather than root.
	if uid == 0 {
		if scionUser, err := user.Lookup("scion"); err == nil {
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
		hubClient.UpdateStatus(hubCtx, hub.StatusUpdate{
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
	if out, err := initCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git init failed: %s", sanitizeGitOutput(string(out), token))
	}

	remoteCmd := exec.Command("git", "-C", workspacePath, "remote", "add", "origin", authURL)
	setupGitCmd(remoteCmd)
	if out, err := remoteCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git remote add failed: %s", sanitizeGitOutput(string(out), token))
	}

	// fetchBranch attempts a shallow fetch of a single branch from origin.
	// Returns sanitized stderr and whether the fetch succeeded.
	fetchBranch := func(branchToFetch string) (string, bool) {
		fetchCmd := exec.Command("git", "-C", workspacePath, "fetch", "--depth", depthStr, "origin", branchToFetch)
		setupGitCmd(fetchCmd)
		var stderr bytes.Buffer
		fetchCmd.Stderr = &stderr
		if err := fetchCmd.Run(); err != nil {
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
	if out, err := coCmd.CombinedOutput(); err != nil {
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
		if err := cfgCmd.Run(); err != nil {
			return fmt.Errorf("failed to set git config %s: %w", cfg.key, err)
		}
	}

	// Sanitize the remote URL to remove the embedded token. The token was
	// needed for the initial fetch, but ongoing auth is handled by the
	// credential helper configured below. Leaving the token in the URL
	// exposes it via `git remote -v`.
	sanitizeCmd := exec.Command("git", "-C", workspacePath, "remote", "set-url", "origin", buildAuthenticatedURL(cloneURL, ""))
	setupGitCmd(sanitizeCmd)
	if out, err := sanitizeCmd.CombinedOutput(); err != nil {
		log.Error("Failed to sanitize remote URL: %s %v", string(out), err)
	}

	// Configure credential helper in the agent user's $HOME/.gitconfig (not
	// the workspace .git/config). This keeps credentials out of the workspace,
	// matching the pattern used by shared-workspace groves. We use the
	// resolved agentHome rather than os.Getenv("HOME") because init runs as
	// root (HOME=/root) but the harness runs as the scion user.
	gitconfigPath := filepath.Join(agentHome, ".gitconfig")

	var credentialHelper string
	if os.Getenv("SCION_GITHUB_APP_ENABLED") == "true" {
		credentialHelper = "!sciontool credential-helper"
	} else {
		credentialHelper = `!f() { echo "password=${GITHUB_TOKEN}"; echo "username=oauth2"; }; f`
	}
	credCmd := exec.Command("git", "config", "--file", gitconfigPath, "credential.helper", credentialHelper)
	setupGitCmd(credCmd)
	if err := credCmd.Run(); err != nil {
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
		if err := checkoutCmd.Run(); err == nil {
			checked = true
		}

		// 2. Try fetching the branch from origin (shallow clone may not have it)
		if !checked {
			fetchCmd := exec.Command("git", "-C", workspacePath, "fetch", "origin", branchName)
			setupGitCmd(fetchCmd)
			if err := fetchCmd.Run(); err == nil {
				// Branch exists on remote — check it out tracking origin
				trackCmd := exec.Command("git", "-C", workspacePath, "checkout", "-b", branchName, "origin/"+branchName)
				setupGitCmd(trackCmd)
				if err := trackCmd.Run(); err == nil {
					checked = true
				}
			}
		}

		// 3. Branch doesn't exist anywhere — create it
		if !checked {
			createCmd := exec.Command("git", "-C", workspacePath, "checkout", "-b", branchName)
			setupGitCmd(createCmd)
			if err := createCmd.Run(); err != nil {
				return fmt.Errorf("failed to create branch %s: %w", branchName, err)
			}
		}
	}

	log.Info("Git clone complete: %s on branch %s", normalizedURL, branchName)
	return nil
}

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

// configureSharedWorkspaceGit sets up git credentials for shared-workspace
// (git-workspace hybrid) groves. The workspace is a pre-cloned git repo shared
// by all agents; each agent gets its own credential helper in $HOME/.gitconfig
// so credentials don't pollute the shared workspace.
func configureSharedWorkspaceGit(agentHome string) {
	log.Info("Configuring git credentials for shared workspace")

	// Configure credential helper using sciontool's credential-helper command,
	// which handles both GITHUB_TOKEN env var and GitHub App token refresh.
	gitconfigPath := filepath.Join(agentHome, ".gitconfig")

	var credentialHelper string
	if os.Getenv("SCION_GITHUB_APP_ENABLED") == "true" {
		// Use sciontool credential-helper for GitHub App token refresh
		credentialHelper = "!sciontool credential-helper"
	} else {
		// Simple credential helper using GITHUB_TOKEN env var
		credentialHelper = `!f() { echo "password=${GITHUB_TOKEN}"; echo "username=oauth2"; }; f`
	}

	// Use git config to set the credential helper in the user's gitconfig.
	// This is idempotent and works even if provisioning already set it.
	cmd := exec.Command("git", "config", "--file", gitconfigPath, "credential.helper", credentialHelper)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Error("Failed to configure credential helper: %s %v", string(out), err)
	}

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
		cmd := exec.Command("git", "config", "--file", gitconfigPath, cfg.key, cfg.value)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Error("Failed to set git config %s: %s %v", cfg.key, string(out), err)
		}
	}
}

func configureGitCommand(cmd *exec.Cmd, uid, gid int) {
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if uid <= 0 {
		return
	}

	currentUID := os.Getuid()
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
	return fmt.Errorf("git clone failed (GITHUB_TOKEN may be invalid or lack Contents read access): %s", sanitizedStderr)
}

// detectDefaultBranch uses `git ls-remote --symref origin HEAD` to discover
// the remote's default branch. Returns the branch name (e.g. "master") or ""
// if detection fails.
func detectDefaultBranch(workspacePath string, setupGitCmd func(*exec.Cmd)) string {
	cmd := exec.Command("git", "-C", workspacePath, "ls-remote", "--symref", "origin", "HEAD")
	setupGitCmd(cmd)
	out, err := cmd.Output()
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

// scionEnvVarPrefixes lists environment variable prefixes that are written
// to the scion-env file for shell sessions to source.
var scionEnvVarPrefixes = []string{
	"SCION_",
	"GITHUB_TOKEN",
}

// writeEnvFile writes critical SCION_* environment variables to a shell-sourceable
// file at ~/.scion/scion-env. Some harnesses (e.g. Gemini CLI) re-exec with a
// filtered environment, losing env vars that were passed via docker run -e. This
// file is sourced by the agent's .bashrc so that tool-spawned shell processes
// recover the full environment.
func writeEnvFile(agentHome string, uid, gid int) {
	scionDir := filepath.Join(agentHome, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		log.Error("Failed to create .scion dir for env file: %v", err)
		return
	}

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
	if err := os.WriteFile(envPath, []byte(strings.Join(lines, "\n")+"\n"), 0644); err != nil {
		log.Error("Failed to write scion-env file: %v", err)
		return
	}

	if uid > 0 {
		// Chown the directory and file so the scion user can read them
		_ = os.Chown(scionDir, uid, gid)
		_ = os.Chown(envPath, uid, gid)
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
	for _, e := range entries {
		switch e.Name() {
		case ".scion", ".scion-volumes":
			// Provisioning marker / shared-dir mount directory — ignore
			continue
		default:
			log.Debug("Workspace not empty: found %q in %s", e.Name(), path)
			return false
		}
	}
	return true
}

// cleanGcloudConfigForMetadata removes gcloud configuration state files from
// the given directory while preserving application_default_credentials.json,
// which may be bind-mounted as a gcloud-adc secret. Clearing the config state
// forces gcloud to re-initialize and discover the emulated metadata server.
func cleanGcloudConfigForMetadata(gcloudDir string) {
	entries, err := os.ReadDir(gcloudDir)
	if err != nil {
		// Directory doesn't exist — nothing to clean.
		return
	}
	for _, e := range entries {
		if e.Name() == "application_default_credentials.json" {
			continue
		}
		p := filepath.Join(gcloudDir, e.Name())
		if err := os.RemoveAll(p); err != nil {
			log.Debug("Could not remove gcloud config entry %s: %v", p, err)
		}
	}
}
