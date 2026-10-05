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

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/daemon"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/spf13/cobra"
)

// appendDaemonBoolFlag forwards a boolean flag to the --foreground daemon child.
//
// The child re-runs applyWorkstationDefaults, which re-enables any
// workstation-defaulted flag (--enable-hub, --enable-runtime-broker,
// --enable-web, --dev-auth, --auto-provide) that it does not see as explicitly
// set. So a flag the user explicitly disabled must be forwarded as
// --flag=<value>, not merely omitted when false — otherwise the child treats it
// as unset and the workstation default silently flips it back on (e.g.
// `scion server start --dev-auth=false` would still start with dev-auth
// enabled). When the flag was not set explicitly, keep the historical bare form
// (present only when true).
func appendDaemonBoolFlag(cmd *cobra.Command, args []string, name string, val bool) []string {
	if cmd.Flags().Changed(name) {
		return append(args, fmt.Sprintf("--%s=%t", name, val))
	}
	if val {
		return append(args, "--"+name)
	}
	return args
}

// buildDaemonStartArgs constructs the argv for the `server start --foreground`
// child from the parsed server-start flags/globals. Every flag the user set
// explicitly is forwarded (bools as --flag=<value> via appendDaemonBoolFlag,
// string/int flags as --flag=<value> when Changed) so it survives the re-exec;
// flags left at their defaults are omitted (workstation-defaulted bools keep
// their historical bare-when-true form).
func buildDaemonStartArgs(cmd *cobra.Command) []string {
	daemonArgs := []string{"server", "start", "--foreground"}
	// --hosted selects the server mode; an explicit --hosted=false must survive
	// the re-exec too, else a child with mode:"hosted" in config flips back to
	// hosted and overrides the user's workstation choice. Forward via the helper.
	daemonArgs = appendDaemonBoolFlag(cmd, daemonArgs, "hosted", hostedMode)
	// Workstation-defaulted bools must be forwarded as --flag=<value> when set
	// explicitly, so an explicit disable survives into the --foreground child.
	// Forwarding "bare when true" alone loses --enable-web=false / --dev-auth=false
	// etc.: the child sees the flag as unset and applyWorkstationDefaults re-enables
	// it. See appendDaemonBoolFlag.
	daemonArgs = appendDaemonBoolFlag(cmd, daemonArgs, "enable-hub", enableHub)
	daemonArgs = appendDaemonBoolFlag(cmd, daemonArgs, "enable-runtime-broker", enableRuntimeBroker)
	daemonArgs = appendDaemonBoolFlag(cmd, daemonArgs, "enable-web", enableWeb)
	daemonArgs = appendDaemonBoolFlag(cmd, daemonArgs, "dev-auth", enableDevAuth)
	if enableDebug {
		daemonArgs = append(daemonArgs, "--debug")
	}
	daemonArgs = appendDaemonBoolFlag(cmd, daemonArgs, "auto-provide", serverAutoProvide)
	// Remaining bool flags are not workstation-defaulted, but an explicit value
	// set at `server start` (daemon mode) is still lost to the child's defaults
	// unless forwarded. appendDaemonBoolFlag omits them when unset.
	daemonArgs = appendDaemonBoolFlag(cmd, daemonArgs, "no-auto-migrate", noAutoMigrate)
	daemonArgs = appendDaemonBoolFlag(cmd, daemonArgs, "enable-test-login", enableTestLogin)
	daemonArgs = appendDaemonBoolFlag(cmd, daemonArgs, "simulate-remote-broker", simulateRemoteBroker)
	// Only forward --host when explicitly set. The parent never loads config, so
	// hubHost holds a default here; forwarding it unconditionally would make the
	// child treat --host as changed and clobber a config-file host (and skip the
	// workstation loopback default for the runtime broker). Unset → the child
	// derives its own default (127.0.0.1 in workstation mode).
	if cmd.Flags().Changed("host") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--host=%s", hubHost))
	}
	if cmd.Flags().Changed("port") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--port=%d", hubPort))
	}
	if cmd.Flags().Changed("runtime-broker-port") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--runtime-broker-port=%d", runtimeBrokerPort))
	}
	if cmd.Flags().Changed("web-port") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--web-port=%d", webPort))
	}
	if cmd.Flags().Changed("config") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--config=%s", serverConfigPath))
	}
	if cmd.Flags().Changed("db") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--db=%s", dbURL))
	}
	if cmd.Flags().Changed("storage-bucket") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--storage-bucket=%s", storageBucket))
	}
	if cmd.Flags().Changed("storage-dir") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--storage-dir=%s", storageDir))
	}
	// String/int flags registered only on serverStartCmd: forward when explicitly
	// set so they survive the re-exec into the --foreground child rather than
	// falling back to defaults (e.g. --session-secret would otherwise be
	// regenerated, --base-url/--admin-emails dropped).
	if cmd.Flags().Changed("template-cache-dir") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--template-cache-dir=%s", templateCacheDir))
	}
	if cmd.Flags().Changed("template-cache-max") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--template-cache-max=%d", templateCacheMax))
	}
	if cmd.Flags().Changed("web-assets-dir") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--web-assets-dir=%s", webAssetsDir))
	}
	// NOTE: --session-secret is deliberately NOT forwarded. It is a signing
	// secret, and the daemon argv is both visible in the process list and
	// persisted to server-args.json (via SaveArgs) for restart. Forwarding it
	// would expose the secret there; a stable session secret in daemon mode
	// should be supplied out-of-band (env/config file), not via the child argv.
	if cmd.Flags().Changed("base-url") {
		daemonArgs = append(daemonArgs, fmt.Sprintf("--base-url=%s", webBaseURL))
	}
	if cmd.Flags().Changed("admin-emails") {
		// Forwarded as a single comma-joined value rather than repeated flags:
		// the daemon argv is persisted to server-args.json for restart, and the
		// one-arg form keeps that file byte-compatible with existing installs.
		// The flag still accepts a comma-separated list, so this round-trips.
		daemonArgs = append(daemonArgs, fmt.Sprintf("--admin-emails=%s", strings.Join(adminEmails, ",")))
	}
	if globalMode {
		daemonArgs = append(daemonArgs, "--global")
	}
	return daemonArgs
}

// runServerStartOrDaemon handles the server start command. By default it launches
// the server as a background daemon. When --foreground is set, it runs directly.
func runServerStartOrDaemon(cmd *cobra.Command, args []string) error {
	if serverStartForeground {
		return runServerStart(cmd, args)
	}

	// Daemon mode
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	// Check if already running
	running, pid, _ := daemon.StatusComponent(serverDaemonComponent, globalDir)
	if running {
		return fmt.Errorf("server is already running (PID: %d)\n\nUse 'scion server stop' to stop it, or check the log at %s",
			pid, daemon.GetLogPathComponent(serverDaemonComponent, globalDir))
	}

	// Check for phantom processes holding server ports even without a PID file
	serverPorts := collectServerPorts(cmd)
	if phantomPorts := daemon.DetectOccupiedPorts(serverPorts); len(phantomPorts) > 0 {
		fmt.Fprintf(os.Stderr, "Error: the following ports are already in use: %v\n", phantomPorts)
		fmt.Fprintf(os.Stderr, "A previous server process may be running without a PID file.\n")
		fmt.Fprintf(os.Stderr, "Run 'scion server stop --force' to kill any process on these ports.\n")
		return fmt.Errorf("port conflict: ports %v are occupied", phantomPorts)
	}

	// Check if hosted mode is set in config (settings.yaml server.mode).
	// LoadServerMode() normalizes the legacy "production" value to "hosted".
	if !cmd.Flags().Changed("hosted") && !cmd.Flags().Changed("production") {
		if config.LoadServerMode() == "hosted" {
			hostedMode = true
		}
	}

	// Apply workstation defaults when not in hosted mode.
	// Workstation mode enables all components, dev-auth, auto-provide,
	// and binds to loopback (127.0.0.1) for single-user security.
	if !hostedMode {
		applyWorkstationDefaults(cmd)
	}

	// Check if at least one component is enabled
	if !enableHub && !enableRuntimeBroker && !enableWeb {
		return fmt.Errorf("no server components enabled; use --enable-hub, --enable-runtime-broker, or --enable-web")
	}

	// Find the scion executable
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find scion executable: %w", err)
	}

	// Build the --foreground child argv, forwarding the flags explicitly set on
	// `server start` so they survive the re-exec (see buildDaemonStartArgs).
	daemonArgs := buildDaemonStartArgs(cmd)

	// Capture onboarding state BEFORE starting the daemon — the child process
	// calls InitGlobal() on startup which creates settings.yaml, so checking
	// afterwards would always see the file as present.
	needsOnboarding := !hostedMode && config.GetSettingsPath(globalDir) == ""

	// Start daemon
	mode := "workstation"
	if hostedMode {
		mode = "hosted"
	}
	fmt.Printf("Starting server as daemon (%s mode)...\n", mode)
	if err := daemon.StartComponent(serverDaemonComponent, executable, daemonArgs, globalDir); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Save the daemon args for restart
	if err := daemon.SaveArgs(serverDaemonComponent, globalDir, daemonArgs); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to save daemon args: %v\n", err)
	}

	// Verify it started
	time.Sleep(500 * time.Millisecond)
	running, pid, _ = daemon.StatusComponent(serverDaemonComponent, globalDir)
	if !running {
		return fmt.Errorf("daemon failed to start. Check log at: %s", daemon.GetLogPathComponent(serverDaemonComponent, globalDir))
	}

	fmt.Printf("Server started (PID: %d)\n", pid)
	fmt.Printf("Log file: %s\n", daemon.GetLogPathComponent(serverDaemonComponent, globalDir))
	fmt.Printf("PID file: %s\n", daemon.GetPIDPathComponent(serverDaemonComponent, globalDir))
	fmt.Println()

	// Print quickstart info for workstation mode
	if !hostedMode {
		printWorkstationQuickstart(needsOnboarding, globalDir, hubHost, webPort, enableWeb, enableDevAuth)
	}

	fmt.Println("Use 'scion server stop' to stop the daemon.")
	fmt.Println("Use 'scion server status' to check status.")

	return nil
}

func runServerStop(cmd *cobra.Command, args []string) error {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	running, pid, _ := daemon.StatusComponent(serverDaemonComponent, globalDir)

	serverPorts := collectServerPorts(cmd)

	if stopForce {
		return runServerStopForce(globalDir, running, pid, serverPorts)
	}

	if !running {
		// PID file is missing or stale — probe ports to see if a server is
		// still listening. This handles the case where the PID file was
		// deleted while the server was still running.
		ports := serverPorts
		occupied := daemon.DetectOccupiedPorts(ports)
		if len(occupied) == 0 {
			return fmt.Errorf("server daemon is not running")
		}

		fmt.Println("No PID file found, but server port(s) appear to be in use:")
		for _, port := range occupied {
			fmt.Printf("  port %d\n", port)
		}
		fmt.Println()

		if !hubsync.ConfirmAction("Kill the process(es) on these ports?", false, autoConfirm) {
			fmt.Println("Aborted.")
			return nil
		}

		killed := 0
		for _, port := range occupied {
			killedPID, err := daemon.ForceKillPort(port)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to kill process on port %d: %v\n", port, err)
				continue
			}
			if killedPID > 0 {
				fmt.Printf("Killed process %d on port %d\n", killedPID, port)
				killed++
			}
		}

		_ = daemon.RemovePIDComponent(serverDaemonComponent, globalDir)

		if killed == 0 {
			return fmt.Errorf("failed to kill any processes on occupied ports")
		}
		fmt.Println("Server stopped.")
		return nil
	}

	fmt.Printf("Stopping server daemon (PID: %d)...\n", pid)

	if err := daemon.StopComponent(serverDaemonComponent, globalDir); err != nil {
		return fmt.Errorf("failed to stop daemon: %w", err)
	}

	// Verify it stopped
	time.Sleep(500 * time.Millisecond)
	running, _, _ = daemon.StatusComponent(serverDaemonComponent, globalDir)
	if running {
		return fmt.Errorf("daemon may still be running. Check with 'scion server status'")
	}

	fmt.Println("Server daemon stopped.")
	return nil
}

func runServerStopForce(globalDir string, pidRunning bool, pid int, serverPorts []int) error {
	killed := false

	// If PID file exists and process is running, stop it normally first.
	if pidRunning {
		fmt.Printf("Stopping server daemon (PID: %d)...\n", pid)
		if err := daemon.StopComponent(serverDaemonComponent, globalDir); err == nil {
			time.Sleep(500 * time.Millisecond)
			killed = true
		}
	}

	// Probe server ports and kill any process holding them.
	ports := serverPorts
	occupied := daemon.DetectOccupiedPorts(ports)
	if len(occupied) == 0 && !killed {
		fmt.Println("No running server found.")
		return nil
	}

	for _, port := range occupied {
		killedPID, err := daemon.ForceKillPort(port)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to kill process on port %d: %v\n", port, err)
			continue
		}
		if killedPID > 0 {
			fmt.Printf("Killed process %d on port %d\n", killedPID, port)
			killed = true
		}
	}

	// Clean up stale PID file
	_ = daemon.RemovePIDComponent(serverDaemonComponent, globalDir)

	if killed {
		fmt.Println("Server stopped (forced).")
	} else {
		fmt.Println("No running server found.")
	}
	return nil
}

func runServerRestart(cmd *cobra.Command, args []string) error {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	running, pid, _ := daemon.StatusComponent(serverDaemonComponent, globalDir)
	if !running {
		return fmt.Errorf("server daemon is not running\n\nUse 'scion server start' to start it")
	}

	// Stop the daemon
	fmt.Printf("Stopping server daemon (PID: %d)...\n", pid)
	if err := daemon.StopComponent(serverDaemonComponent, globalDir); err != nil {
		return fmt.Errorf("failed to stop daemon: %w", err)
	}

	// Wait for the process to exit
	if err := daemon.WaitForExitComponent(serverDaemonComponent, globalDir, 10*time.Second); err != nil {
		return fmt.Errorf("failed to stop server: %w", err)
	}
	fmt.Println("Server daemon stopped.")

	// Find the current scion executable
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to find scion executable: %w", err)
	}

	// Load saved args from previous start, or fall back to reconstructing from flags.
	daemonArgs, err := daemon.LoadArgs(serverDaemonComponent, globalDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to load saved args: %v\n", err)
	}

	if daemonArgs == nil {
		// No saved args — reconstruct from current flags (legacy behavior).
		// NOTE: these flags are registered on serverStartCmd, not
		// serverRestartCmd, so during `server restart` the globals stay at
		// their defaults and this fallback effectively yields just
		// ["server", "start", "--foreground"]. That is a pre-existing
		// limitation of the restart path; the daemon-start fix above
		// (appendDaemonBoolFlag in runServerStart, whose corrected args are
		// persisted via SaveArgs and reloaded by the normal restart path)
		// is where explicit disables are honored.
		daemonArgs = []string{"server", "start", "--foreground"}
		if enableHub || enableRuntimeBroker || enableWeb {
			if enableHub {
				daemonArgs = append(daemonArgs, "--enable-hub")
			}
			if enableRuntimeBroker {
				daemonArgs = append(daemonArgs, "--enable-runtime-broker")
			}
			if enableWeb {
				daemonArgs = append(daemonArgs, "--enable-web")
			}
		}
		if enableDevAuth {
			daemonArgs = append(daemonArgs, "--dev-auth")
		}
		if enableDebug {
			daemonArgs = append(daemonArgs, "--debug")
		}
	}

	fmt.Println("Starting server with new binary...")
	if err := daemon.StartComponent(serverDaemonComponent, executable, daemonArgs, globalDir); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	// Verify it started
	time.Sleep(500 * time.Millisecond)
	running, pid, _ = daemon.StatusComponent(serverDaemonComponent, globalDir)
	if !running {
		return fmt.Errorf("daemon failed to start. Check log at: %s", daemon.GetLogPathComponent(serverDaemonComponent, globalDir))
	}

	fmt.Printf("Server restarted (PID: %d)\n", pid)
	fmt.Printf("Log file: %s\n", daemon.GetLogPathComponent(serverDaemonComponent, globalDir))
	fmt.Println()

	return nil
}

// Composite /healthz status values (see pkg/hub HealthStatus* constants;
// mirrored here rather than imported to keep this CLI-side probe decoupled
// from the Hub's response types).
//
//   - healthy:   everything is fine.
//   - degraded:  the process is up and serving, but a non-critical check is
//     non-healthy (e.g. colocated_broker). "Process is up" consumers treat
//     this as up and name the non-healthy checks.
//   - unhealthy: a critical check (database) failed; treated as not up.
const (
	probeStatusHealthy   = "healthy"
	probeStatusDegraded  = "degraded"
	probeStatusUnhealthy = "unhealthy"
)

// probeStatusIsUp reports whether a composite /healthz status means the
// process is up and serving: healthy or degraded (ptone/scion#1094).
func probeStatusIsUp(status string) bool {
	return status == probeStatusHealthy || status == probeStatusDegraded
}

// healthProbeComponent is the nested per-component health object in the
// composite (combined-mode) /healthz body.
type healthProbeComponent struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

// healthProbeResponse covers both shapes /healthz can return, so a single
// probe/parse works whether the Hub answers directly (standalone, port 9810:
// pkg/hub.HealthResponse, checks at the top level) or the web server answers
// on its behalf (combined workstation mode, the default: port 8080,
// pkg/hub.CompositeHealthResponse — the top-level checks are always empty,
// and the Hub's own status/checks are nested under "hub", the broker's under
// "broker". See WebServer.handleHealthz in pkg/hub/web.go). Declared locally
// (rather than importing pkg/hub) to keep this CLI-side probe decoupled from
// the Hub's response type.
type healthProbeResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
	// Hub is set only on the composite (combined-mode) response; nil on a
	// standalone Hub's direct response, which has no "hub" key at all.
	Hub *healthProbeComponent `json:"hub,omitempty"`
	// Broker is set only on the composite response when a co-located broker
	// health provider is registered.
	Broker *healthProbeComponent `json:"broker,omitempty"`
}

// nonHealthyChecks lists what kept a health response from being "healthy",
// as sorted "key: value" strings: non-healthy hub checks from either the
// top level (standalone Hub) or the nested "hub" object (combined mode),
// plus "broker: <status>" when a nested broker reports non-healthy. (Broker
// check values are not "healthy"-valued — e.g. docker: "available" — so
// only its status is consulted.) Returns nil for a healthy or empty
// response.
func nonHealthyChecks(health healthProbeResponse) []string {
	seen := map[string]bool{}
	var out []string
	add := func(checks map[string]string) {
		for k, v := range checks {
			if v == probeStatusHealthy {
				continue
			}
			entry := k + ": " + v
			if !seen[entry] {
				seen[entry] = true
				out = append(out, entry)
			}
		}
	}
	add(health.Checks)
	if health.Hub != nil {
		add(health.Hub.Checks)
	}
	if health.Broker != nil && health.Broker.Status != "" && health.Broker.Status != probeStatusHealthy {
		out = append(out, "broker: "+health.Broker.Status)
	}
	sort.Strings(out)
	return out
}

// healthProblemReason renders nonHealthyChecks as a single human-readable
// reason, e.g. "colocated_broker: unhealthy: registration failed". Returns ""
// when the response is healthy. A non-healthy response that names no check
// falls back to its bare status so the caller still has something to print.
func healthProblemReason(health healthProbeResponse) string {
	if health.Status == probeStatusHealthy || health.Status == "" {
		return ""
	}
	if checks := nonHealthyChecks(health); len(checks) > 0 {
		return strings.Join(checks, "; ")
	}
	return "status: " + health.Status
}

// healthProblemHint returns the recovery hint for a reason produced by
// healthProblemReason. A co-located broker registration failure
// (ptone/scion#2154) is terminal for the process lifetime, so it gets the
// specific "fix the config and restart" advice; anything else just points
// at the server log.
func healthProblemHint(reason string) string {
	if strings.Contains(reason, "colocated_broker: ") {
		return "see server log; restart after fixing the broker config"
	}
	return "see server log"
}

type serverStatusInfo struct {
	DaemonRunning bool   `json:"daemonRunning"`
	DaemonPID     int    `json:"daemonPid,omitempty"`
	LogFile       string `json:"logFile,omitempty"`
	PIDFile       string `json:"pidFile,omitempty"`
	// HubRunning/WebRunning are true when the component answered /healthz
	// with a status that means "up": healthy or degraded. An unhealthy
	// response (critical check failed, e.g. database) leaves them false.
	HubRunning    bool `json:"hubRunning,omitempty"`
	BrokerRunning bool `json:"brokerRunning,omitempty"`
	WebRunning    bool `json:"webRunning,omitempty"`
	// HubStatus/WebStatus carry the composite /healthz status string when
	// the component answered but was not healthy ("degraded" or
	// "unhealthy"); empty when healthy or not detected.
	HubStatus string `json:"hubStatus,omitempty"`
	WebStatus string `json:"webStatus,omitempty"`
	// HubDegradedReason names the non-healthy checks (see
	// healthProblemReason, e.g. "colocated_broker: unhealthy: registration
	// failed") when either probe (the combined web+hub probe on 8080, or the
	// standalone Hub probe on 9810) answered with a non-healthy status —
	// degraded or unhealthy. Empty when the Hub is healthy or unreachable.
	HubDegradedReason string `json:"hubDegradedReason,omitempty"`
	// WebDegradedReason is set only when the 8080 (combined web+hub) probe
	// itself reported a non-healthy status — i.e. the process actually
	// serving the Web Frontend is the degraded one. Kept separate from
	// HubDegradedReason so a standalone Hub-only deployment (port 9810, no
	// web server running at all) does not get an incorrect "Web Frontend:
	// degraded" line just because the Hub is degraded.
	WebDegradedReason string `json:"webDegradedReason,omitempty"`
}

// probeServerStatus probes the web, hub, and broker health endpoints at the
// given base URLs (no trailing slash, e.g. "http://127.0.0.1:8080") and
// returns the resulting component-status fields of serverStatusInfo. Split
// out from runServerStatus, and parameterized on the base URLs rather than
// hardcoding the default ports, so tests can point it at httptest.Server
// instances instead of real listeners on 127.0.0.1.
//
// Parses JSON responses to verify composite health rather than relying
// solely on HTTP 200 (the web server returns 200 even when degraded).
func probeServerStatus(client *http.Client, webBaseURL, hubBaseURL, brokerBaseURL string) serverStatusInfo {
	var status serverStatusInfo

	// webPortHasHub tracks whether the combined web port returned the scion
	// combined composite body (any status) — i.e. health.Hub != nil, which
	// CompositeHealthResponse always populates when a Hub provider is
	// registered (see WebServer.handleHealthz in pkg/hub/web.go) — as
	// opposed to status.HubRunning, which is only set on an "up" status
	// (healthy or degraded). Combined mode (--enable-web) never starts the
	// standalone Hub listener (port 9810), so once the web port has answered
	// with that composite body, a follow-up probe to the standalone hub port
	// is redundant, whatever status it reported. This must not key off "any
	// parseable 200 JSON body": an unrelated service on 8080 (a common dev
	// port) can answer 200 with an unrelated JSON object, and health.Hub ==
	// nil then, so the standalone hub still gets probed.
	var webPortHasHub bool

	// Check web/hub on the combined web port. In combined mode this is the
	// only listener (see healthProbeResponse for the nested-hub JSON shape
	// it returns).
	if resp, err := client.Get(webBaseURL + "/healthz"); err == nil {
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && readErr == nil {
			var health healthProbeResponse
			if json.Unmarshal(body, &health) == nil {
				webPortHasHub = health.Hub != nil
				switch {
				case health.Status == probeStatusHealthy:
					status.WebRunning = true
					status.HubRunning = true
				case health.Status == probeStatusDegraded || webPortHasHub:
					// Degraded is up; unhealthy is not, but a scion composite
					// body (webPortHasHub) is still worth naming instead of a
					// bare "not detected". The webPortHasHub guard keeps an
					// unrelated service on 8080 from being reported.
					up := probeStatusIsUp(health.Status)
					status.WebRunning = up
					status.HubRunning = up
					status.WebStatus = health.Status
					status.HubStatus = health.Status
					reason := healthProblemReason(health)
					status.HubDegradedReason = reason
					status.WebDegradedReason = reason
				}
			}
		}
	}

	// Check standalone hub port if not already found on the web port. This
	// probe intentionally leaves WebDegradedReason/WebStatus unset: a
	// standalone Hub (no web server) must not print "Web Frontend: degraded"
	// just because the Hub itself is degraded.
	if !status.HubRunning && !webPortHasHub {
		if resp, err := client.Get(hubBaseURL + "/healthz"); err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && readErr == nil {
				var health healthProbeResponse
				if json.Unmarshal(body, &health) == nil && health.Status != "" {
					status.HubRunning = probeStatusIsUp(health.Status)
					if health.Status != probeStatusHealthy {
						status.HubStatus = health.Status
						if status.HubDegradedReason == "" {
							status.HubDegradedReason = healthProblemReason(health)
						}
					}
				}
			}
		}
	}

	// Check broker on the default broker port.
	if resp, err := client.Get(brokerBaseURL + "/healthz"); err == nil {
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && readErr == nil {
			var health struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(body, &health) == nil {
				status.BrokerRunning = true
			}
		}
	}

	return status
}

func runServerStatus(cmd *cobra.Command, args []string) error {
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		return fmt.Errorf("failed to get global directory: %w", err)
	}

	status := serverStatusInfo{}

	// Check daemon status
	running, pid, _ := daemon.StatusComponent(serverDaemonComponent, globalDir)
	status.DaemonRunning = running
	status.DaemonPID = pid
	if running {
		status.LogFile = daemon.GetLogPathComponent(serverDaemonComponent, globalDir)
		status.PIDFile = daemon.GetPIDPathComponent(serverDaemonComponent, globalDir)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	probed := probeServerStatus(client, "http://127.0.0.1:8080", "http://127.0.0.1:9810", "http://127.0.0.1:9800")
	status.HubRunning = probed.HubRunning
	status.BrokerRunning = probed.BrokerRunning
	status.WebRunning = probed.WebRunning
	status.HubStatus = probed.HubStatus
	status.WebStatus = probed.WebStatus
	status.HubDegradedReason = probed.HubDegradedReason
	status.WebDegradedReason = probed.WebDegradedReason

	if serverStatusJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(status)
	}

	// Human-readable output
	fmt.Println("Scion Server Status")
	if status.DaemonRunning {
		fmt.Printf("  Daemon:        running (PID: %d)\n", status.DaemonPID)
		fmt.Printf("  Log file:      %s\n", status.LogFile)
		fmt.Printf("  PID file:      %s\n", status.PIDFile)
	} else {
		fmt.Println("  Daemon:        not running")
	}
	fmt.Println()
	fmt.Println("Components:")
	for _, line := range formatServerStatusComponents(status) {
		fmt.Println(line)
	}

	return nil
}

// formatServerStatusComponents renders the "Components:" lines of
// `scion server status` from an already-populated serverStatusInfo. Split
// out from runServerStatus so the formatting — in particular, naming the
// non-healthy checks (e.g. colocated_broker, ptone/scion#2154) instead of a
// bare "not detected" — can be unit tested without spinning up HTTP servers.
func formatServerStatusComponents(status serverStatusInfo) []string {
	var lines []string

	lines = append(lines, "  Hub API:         "+formatComponentState(status.HubRunning, status.HubStatus, status.HubDegradedReason))

	if status.BrokerRunning {
		lines = append(lines, "  Runtime Broker:  running")
	} else {
		lines = append(lines, "  Runtime Broker:  not detected")
	}

	// WebStatus/WebDegradedReason (not the Hub fields): a standalone Hub-only
	// deployment with no web server at all must not print "Web Frontend:
	// degraded" just because the Hub is degraded.
	lines = append(lines, "  Web Frontend:    "+formatComponentState(status.WebRunning, status.WebStatus, status.WebDegradedReason))

	return lines
}

// formatComponentState renders one component's state for
// formatServerStatusComponents:
//   - running (healthy):            "running"
//   - running, degraded:            "running, degraded (<checks>) — <hint>"
//   - answered but not up (unhealthy): "unhealthy (<checks>) — <hint>"
//   - no answer:                    "not detected"
func formatComponentState(running bool, healthStatus, reason string) string {
	switch {
	case running && reason == "":
		return "running"
	case running:
		return fmt.Sprintf("running, degraded (%s) — %s", reason, healthProblemHint(reason))
	case healthStatus != "" || reason != "":
		label := healthStatus
		if label == "" || label == probeStatusDegraded {
			// Not up but no usable status string; never claim "degraded"
			// (which means up) for a component that is not running.
			label = probeStatusUnhealthy
		}
		if reason == "" {
			return label
		}
		return fmt.Sprintf("%s (%s) — %s", label, reason, healthProblemHint(reason))
	default:
		return "not detected"
	}
}

// serverReadyPollInterval is how often waitForServerReady polls /healthz.
// A variable so tests can shorten it.
var serverReadyPollInterval = 250 * time.Millisecond

// waitForServerReady polls the server's /healthz endpoint until it returns 200
// with a "healthy" composite status, or the timeout expires.
// The web server's /healthz always returns HTTP 200 but reports a composite
// status that reflects hub and broker readiness. On first start the hub
// database may still be migrating when the HTTP listener begins accepting
// connections, and the co-located broker registers just after the listener
// starts (colocated_broker is transiently non-healthy), so we parse the JSON
// body and keep polling for "healthy" until the deadline.
//
// If the deadline passes and the last observed status was "degraded", the
// process is up and serving with a non-critical problem, so this still
// reports ready (ptone/scion#1094); the caller checks lastHealth.Status and
// warns, naming the checks via healthProblemReason. "unhealthy" (a critical
// check such as the database failed) or no answer at all is not ready.
//
// It also returns the last health response it observed (zero value if the
// endpoint was never reachable), so a caller can name the specific checks
// that kept the composite status from going healthy — e.g. colocated_broker
// (ptone/scion#2154) — instead of printing a bare "not ready".
func waitForServerReady(host string, port int, timeout time.Duration) (ready bool, lastHealth healthProbeResponse) {
	client := &http.Client{Timeout: 2 * time.Second}
	url := fmt.Sprintf("http://%s:%d/healthz", host, port)
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if resp, err := client.Get(url); err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && readErr == nil {
				var health healthProbeResponse
				if json.Unmarshal(body, &health) == nil {
					lastHealth = health
					if health.Status == probeStatusHealthy {
						return true, lastHealth
					}
				}
			}
		}
		time.Sleep(serverReadyPollInterval)
	}
	return lastHealth.Status == probeStatusDegraded, lastHealth
}

// printWorkstationQuickstart prints the first-run quickstart information
// including the developer token and web UI URL after a workstation-mode daemon starts.
// When the machine hasn't been onboarded yet, it prints and opens the /onboarding URL.
func printWorkstationQuickstart(needsOnboarding bool, globalDir string, host string, wPort int, webEnabled, devAuth bool) {
	if webEnabled {
		displayHost := host
		if displayHost == "0.0.0.0" || displayHost == "" {
			displayHost = "127.0.0.1"
		}

		// Auto-configure hub.endpoint so that `scion hub` commands work
		// immediately after workstation-mode start.
		hubEndpoint := fmt.Sprintf("http://%s:%d", displayHost, wPort)
		if vs, err := config.LoadSingleFileVersioned(globalDir); err == nil {
			if vs.GetHubEndpoint() == "" {
				if err := config.UpdateVersionedSetting(globalDir, "hub.endpoint", hubEndpoint); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to auto-configure hub endpoint: %v\n", err)
				} else {
					fmt.Printf("Configured hub endpoint: %s (run 'scion hub status' to verify)\n", hubEndpoint)
				}
			}
		}

		// Point to /onboarding when the machine hadn't been set up before daemon start.
		// This state is captured before the daemon launches (which auto-creates settings.yaml).
		path := ""
		if needsOnboarding {
			path = "/onboarding"
		}

		url := fmt.Sprintf("http://%s:%d%s", displayHost, wPort, path)
		fmt.Printf("Web UI:  %s\n", url)

		// Auto-open the browser in interactive terminals once the server is ready.
		if os.Getenv("SCION_NO_BROWSER") == "" && util.IsTerminal() && !util.IsHeadlessEnvironment() {
			ready, lastHealth := waitForServerReady(displayHost, wPort, 20*time.Second)
			reason := healthProblemReason(lastHealth)
			switch {
			case ready && reason != "":
				// Degraded: the process is up and serving, so open the
				// browser, but name what is wrong (e.g. a co-located broker
				// that failed to register, ptone/scion#2154, which does not
				// retry: fix the config and restart).
				fmt.Printf("  Warning: server is up but degraded: %s — %s\n", reason, healthProblemHint(reason))
				_ = util.OpenBrowser(url)
			case ready:
				_ = util.OpenBrowser(url)
			case reason != "":
				// Answering /healthz but unhealthy (a critical check failed).
				fmt.Printf("  (server is up but %s: %s — open the URL manually; %s)\n", lastHealth.Status, reason, healthProblemHint(reason))
			default:
				fmt.Println("  (server not yet ready — open the URL manually once it starts)")
			}
		}
	}

	if devAuth {
		// Read the dev token from the token file (written by the daemon child process)
		tokenFile := filepath.Join(globalDir, "dev-token")
		if data, err := os.ReadFile(tokenFile); err == nil {
			token := strings.TrimSpace(string(data))
			if token != "" {
				fmt.Println()
				fmt.Println("Developer token (for CLI authentication):")
				fmt.Printf("  export SCION_DEV_TOKEN=%s\n", token)
			}
		}
	}
	fmt.Println()
}

// collectServerPorts returns the list of TCP ports the server would bind based
// on the flags the user passed (or their defaults).
func collectServerPorts(cmd *cobra.Command) []int {
	seen := map[int]bool{}
	var ports []int
	add := func(p int) {
		if !seen[p] {
			seen[p] = true
			ports = append(ports, p)
		}
	}
	add(webPort)
	add(hubPort)
	add(runtimeBrokerPort)
	return ports
}
