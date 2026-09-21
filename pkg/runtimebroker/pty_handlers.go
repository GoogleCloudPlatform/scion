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

package runtimebroker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/creack/pty"
	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	tmuxSessionWaitTimeout  = 30 * time.Second
	tmuxSessionPollInterval = 500 * time.Millisecond

	// tmuxAttachCmd is the shell cmd that attaches to the agent's
	// tmux session for an interactive PTY stream. The TERM env
	// prefix is required so tmux negotiates the right terminfo for
	// xterm-style sequences from the web terminal client.
	tmuxAttachCmd = "TERM=xterm-256color tmux attach-session -t scion"
)

// PTY endpoint configuration
const (
	ptyMaxDataSize = 32 * 1024 // 32KB max per message

	// cloudRunSandboxBin is the platform-injected sandbox binary path.
	// It is injected at deploy time when --sandbox-launcher is enabled
	// and is never part of the container image.
	cloudRunSandboxBin = "/usr/local/gcp/bin/sandbox"
)

const (
	// processExitGracePeriod is how long to wait for the runtime exec process
	// to exit after the PTY master is closed. The PTY close triggers a terminal
	// hangup (SIGHUP) that should cause the exec process to exit naturally.
	processExitGracePeriod = 3 * time.Second

	// processTermTimeout is how long to wait after SIGTERM before escalating
	// to SIGKILL.
	processTermTimeout = 2 * time.Second
)

// isDockerCompatibleRuntime returns true for runtimes that support docker exec
// -e for env injection and /proc access for PID lookup. Only docker and
// container (Podman-compatible) runtimes qualify. K8s exec uses the
// remotecommand API, CloudRun uses a sandbox binary — these have different exec
// semantics and are excluded.
func isDockerCompatibleRuntime(runtimeCmd string) bool {
	return runtimeCmd == "docker" || runtimeCmd == "container" || runtimeCmd == ""
}

// generateAttachNonce generates a cryptographically random 32-character hex
// string used as a per-attach process identity token. The nonce is injected
// into the container-side process environment via docker exec -e, enabling
// causal identification of the exact tmux client process at cleanup time.
func generateAttachNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// cleanupContainerAttach identifies and signals the container-side tmux client
// process that this attach session owns, using the per-attach nonce for causal
// identification.
//
// This runs BEFORE the host-side PTY close in gracefulShutdownExec. It uses a
// fresh context (not the session's canceled context) with a bounded timeout.
// All failures are non-fatal — the current host-side cleanup always runs.
func cleanupContainerAttach(runtimeCmd, containerID, execUser, nonce string) {
	if nonce == "" || !isDockerCompatibleRuntime(runtimeCmd) {
		return
	}
	if runtimeCmd == "" {
		runtimeCmd = "docker"
	}

	// Fresh context with bounded timeout. The session context is already
	// canceled by the time gracefulShutdownExec runs.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Step 1: Find PID by nonce in /proc/*/environ
	pid, err := findContainerPIDByNonce(ctx, runtimeCmd, containerID, execUser, nonce)
	if err != nil || pid == "" {
		return // No match or error — fall back to host-side cleanup
	}

	// Step 2: Read start time for PID recycling safety
	startTime, err := readContainerPIDStartTime(ctx, runtimeCmd, containerID, execUser, pid)
	if err != nil || startTime == "" {
		return // Cannot verify identity — do nothing
	}

	// Step 3: Atomic verify-and-kill — re-read start time and signal in one exec
	// to minimize TOCTOU window (still not eliminated — see design notes)
	killContainerPID(ctx, runtimeCmd, containerID, execUser, pid, startTime)
}

// findContainerPIDByNonce searches /proc/*/environ inside the container for a
// process whose environment contains the given nonce AND whose cmdline matches
// *tmux*attach*. Returns the PID string if exactly one match is found, empty
// string on zero or multiple matches (no-signal-on-ambiguity).
func findContainerPIDByNonce(ctx context.Context, runtimeCmd, containerID, execUser, nonce string) (string, error) {
	// grep -qz handles NUL-delimited environ entries.
	// Filter: only match processes whose cmdline contains "tmux" AND
	// "attach" to exclude children that inherited the env var.
	script := `found=""
for p in /proc/[0-9]*/environ; do
  pid="${p#/proc/}"
  pid="${pid%%/*}"
  if grep -qz 'SCION_ATTACH_NONCE=` + nonce + `' "$p" 2>/dev/null; then
    cmd=$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null)
    case "$cmd" in
      *tmux*attach*)
        if [ -n "$found" ]; then
          echo ""
          exit 0
        fi
        found="$pid"
        ;;
    esac
  fi
done
echo "$found"`

	cmd := exec.CommandContext(ctx, runtimeCmd, "exec", "--user", execUser, containerID, "sh", "-c", script)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	pid := strings.TrimSpace(string(out))
	if pid == "" {
		return "", nil // No match or multiple matches
	}
	return pid, nil
}

// readContainerPIDStartTime reads the start time from /proc/<pid>/stat inside
// the container. The start time (field 22) is used for PID recycling safety:
// if the PID has been recycled, the start time will differ.
//
// IMPORTANT: /proc/<pid>/stat field 2 (comm) can contain spaces and
// parentheses. The correct approach: find the LAST closing paren `)`, then
// count space-delimited fields after it. Starttime is field 22, which is the
// 20th field after the closing paren (fields 3-22 = 20 fields).
func readContainerPIDStartTime(ctx context.Context, runtimeCmd, containerID, execUser, pid string) (string, error) {
	// Safe parsing: find last ')' (end of comm field), then extract field 20
	// after it (which is stat field 22 = starttime).
	script := `stat=$(cat /proc/` + pid + `/stat 2>/dev/null) || exit 1
rest="${stat##*) }"
echo "$rest" | cut -d' ' -f20`

	cmd := exec.CommandContext(ctx, runtimeCmd, "exec", "--user", execUser, containerID, "sh", "-c", script)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// killContainerPID sends SIGTERM to a process inside the container after
// verifying that its start time still matches the expected value. This
// minimizes the TOCTOU window between PID verification and signaling by
// combining both operations in a single docker exec shell script.
//
// The TOCTOU race is NOT eliminated — it is minimized. The PID could
// theoretically be recycled between the shell's read of /proc/<pid>/stat and
// the kill syscall. pidfd_open+pidfd_send_signal is not feasible via docker
// exec shell scripts (see design doc). No signal on ambiguity.
func killContainerPID(ctx context.Context, runtimeCmd, containerID, execUser, pid, expectedStartTime string) {
	// Verify start_time still matches (minimize TOCTOU), then signal.
	// If start_time does not match, the PID was recycled — do nothing.
	script := `stat=$(cat /proc/` + pid + `/stat 2>/dev/null) || exit 0
rest="${stat##*) }"
current=$(echo "$rest" | cut -d' ' -f20)
if [ "$current" = "` + expectedStartTime + `" ]; then
  kill -TERM ` + pid + ` 2>/dev/null
fi`

	cmd := exec.CommandContext(ctx, runtimeCmd, "exec", "--user", execUser, containerID, "sh", "-c", script)
	_ = cmd.Run() // Best-effort, ignore errors
}

// gracefulShutdownExec shuts down a runtime exec process (docker exec, sandbox
// exec) by closing the PTY master first — triggering a terminal hangup that the
// container runtime can propagate to the container-side process — then
// escalating through SIGTERM to SIGKILL only if necessary.
//
// Background (TW-UAT-002): exec.CommandContext sends SIGKILL on context cancel,
// which may kill the host-side docker exec instantly without giving the runtime
// a chance to propagate the signal to the container-side process. This is a
// hypothesized cause of residual tmux attach-session processes observed in
// containers. By closing the PTY first and using SIGTERM, we give Docker the
// chance to propagate the hangup signal to the in-container process. Whether
// this fully prevents the residual process in all cases requires UAT
// verification.
//
// The caller must not call cmd.Wait() separately; this function reaps the
// process.
//
// Close() also closes ptyMaster before this defer runs. Both calls target the
// same *os.File object, and Go's os.File.Close() uses an internal poll.FD that
// tracks closed state — the second Close() returns os.ErrClosed without issuing
// a second syscall.Close on the raw fd, so there is no fd-reuse race.
func gracefulShutdownExec(cmd *exec.Cmd, ptyMaster *os.File, slug string,
	runtimeCmd, containerID, execUser, attachNonce string) {

	// NEW: Container-side cleanup before host-side PTY close.
	// Uses fresh bounded context. All failures fall through to host cleanup.
	cleanupContainerAttach(runtimeCmd, containerID, execUser, attachNonce)

	// Step 1: Close PTY master — triggers SIGHUP on the slave side. For
	// Docker exec, this breaks the stdio pipes, which Docker handles by
	// sending SIGHUP to the container-side process.
	if ptyMaster != nil {
		_ = ptyMaster.Close()
	}

	if cmd == nil || cmd.Process == nil {
		return
	}

	// Already reaped (e.g., process exited before cleanup started).
	if cmd.ProcessState != nil {
		return
	}

	// Step 2: Wait for process exit from PTY hangup.
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	select {
	case <-exited:
		slog.Debug("PTY exec exited after hangup", "slug", slug)
		return
	case <-time.After(processExitGracePeriod):
	}

	// Step 3: SIGTERM — more likely than SIGKILL to propagate to the
	// container-side process through the runtime's exec infrastructure.
	slog.Debug("PTY exec did not exit after hangup, sending SIGTERM", "slug", slug)
	_ = cmd.Process.Signal(syscall.SIGTERM)

	select {
	case <-exited:
		slog.Debug("PTY exec exited after SIGTERM", "slug", slug)
		return
	case <-time.After(processTermTimeout):
	}

	// Step 4: SIGKILL — last resort. Container-side process likely survives.
	slog.Warn("PTY exec did not exit after SIGTERM, sending SIGKILL", "slug", slug)
	_ = cmd.Process.Kill()
	<-exited
}

// resizeSandboxTerminal relays a terminal resize event. For cloudrun-sandbox
// runtimes, it sends the resize through the sandbox boundary via tmux
// resize-window because SIGWINCH does not cross the boundary — PTY fd
// properties propagate but signal delivery does not (design doc section
// 4.4a-rev). NOTE: NOT refresh-client -C — that needs a control-mode client
// and is the wrong tool (confirmed by spike-uds-b).
//
// It also applies pty.Setsize to the launcher-side PTY master when present —
// both are needed.
func resizeSandboxTerminal(ctx context.Context, runtimeCmd, containerID, logID string, cols, rows int, ptyMaster *os.File) {
	if runtimeCmd == "cloudrun-sandbox" {
		resizeCmd := exec.CommandContext(ctx, cloudRunSandboxBin, "exec", containerID, "--",
			"/usr/bin/tmux", "resize-window", "-t", "scion",
			"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows))
		if err := resizeCmd.Run(); err != nil {
			slog.Debug("Sandbox tmux resize failed", "id", logID, "error", err)
		}
	}
	if ptyMaster != nil {
		if err := pty.Setsize(ptyMaster, &pty.Winsize{
			Cols: uint16(cols),
			Rows: uint16(rows),
		}); err != nil {
			slog.Debug("PTY resize failed", "id", logID, "error", err)
		} else {
			slog.Debug("PTY resized", "id", logID, "cols", cols, "rows", rows)
		}
	}
}

// sanitizeExecUser returns user if it matches runtime.ValidExecUserName,
// otherwise returns "scion" and logs a warning. Callers default to
// "scion" already when the value is empty, so passing an empty
// string is harmless. The shared regex enforces the same
// defense-in-depth check against shell injection from agent metadata
// that KubernetesRuntime.Attach applies, so values flowing into
// runtime.ExecAsUserCmd are validated by exactly one rule.
func sanitizeExecUser(user string) string {
	if user == "" {
		return "scion"
	}
	if !runtime.ValidExecUserName.MatchString(user) {
		slog.Warn("Invalid exec user, falling back to 'scion'", "user", user)
		return "scion"
	}
	return user
}

// activeWindowOSC builds an OSC 7337 escape sequence encoding the active tmux
// window name. The web terminal client parses this to sync its toolbar selector.
func activeWindowOSC(windowName string) []byte {
	return []byte(fmt.Sprintf("\033]7337;tmuxwindow=%s\007", windowName))
}

// queryTmuxActiveWindow queries the currently active tmux window name via
// container exec (Docker / Apple Virtualization runtimes).
func queryTmuxActiveWindow(ctx context.Context, runtimeCmd, containerID, execUser string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var cmd *exec.Cmd
	if runtimeCmd == "cloudrun-sandbox" {
		// sandbox exec has no --user flag; absolute paths required (PATH is empty).
		cmd = exec.CommandContext(ctx, cloudRunSandboxBin, "exec", containerID, "--",
			"/usr/bin/tmux", "display-message", "-t", "scion", "-p", "#{window_name}")
	} else {
		cmd = exec.CommandContext(ctx, runtimeCmd, "exec", "--user", execUser, containerID,
			"tmux", "display-message", "-t", "scion", "-p", "#{window_name}")
	}
	out, err := cmd.Output()
	if err != nil {
		slog.Debug("Failed to query tmux active window", "containerID", containerID, "error", err)
		return ""
	}
	return strings.TrimSpace(string(out))
}

// queryTmuxActiveWindowK8s queries the active tmux window name in a K8s pod.
func queryTmuxActiveWindowK8s(ctx context.Context, config *rest.Config, clientset kubernetes.Interface, namespace, podName, execUser string) string {
	if namespace == "" {
		namespace = "default"
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	req.VersionedParams(&corev1.PodExecOptions{
		Container: "agent",
		Command:   runtime.ExecAsUserCmd(execUser, "tmux display-message -t scion -p '#{window_name}'"),
		Stdin:     false,
		Stdout:    true,
		Stderr:    false,
		TTY:       false,
	}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		slog.Debug("Failed to create executor for tmux window query", "pod", podName, "error", err)
		return ""
	}

	var buf bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &buf,
		Stderr: io.Discard,
	}); err != nil {
		slog.Debug("Failed to query tmux active window in K8s", "pod", podName, "error", err)
		return ""
	}
	return strings.TrimSpace(buf.String())
}

// waitForTmuxSession polls the container until the tmux session "scion" is
// available. After starting a container, sciontool init needs time to set up
// the user, run pre-start hooks, and launch the tmux session. Without this
// wait, an immediate attach would fail with "no sessions".
func waitForTmuxSession(ctx context.Context, runtimeCmd, containerID, namespace, execUser string, k8sConfig *rest.Config, k8sClientset kubernetes.Interface) error {
	ctx, cancel := context.WithTimeout(ctx, tmuxSessionWaitTimeout)
	defer cancel()

	ticker := time.NewTicker(tmuxSessionPollInterval)
	defer ticker.Stop()

	execUser = sanitizeExecUser(execUser)

	isK8s := runtimeCmd == "kubernetes" || runtimeCmd == "k8s"
	isCloudRunSandbox := runtimeCmd == "cloudrun-sandbox"

	if isCloudRunSandbox {
		for {
			select {
			case <-ctx.Done():
				return fmt.Errorf("timed out waiting for tmux session in sandbox '%s'", containerID)
			case <-ticker.C:
				cmd := exec.CommandContext(ctx, cloudRunSandboxBin, "exec", containerID, "--",
					"/usr/bin/tmux", "has-session", "-t", "scion")
				if cmd.Run() == nil {
					return nil
				}
				slog.Debug("Waiting for tmux session", "sandbox", containerID, "runtime", runtimeCmd)
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for tmux session in container '%s' to become ready", containerID)
		case <-ticker.C:
			var checkErr error
			if isK8s && k8sConfig != nil && k8sClientset != nil {
				// The tmux session runs as the scion user (via sciontool init privilege drop),
				// so we must check as that user — root can't see scion's tmux socket.
				checkErr = k8sExecCheck(ctx, k8sConfig, k8sClientset, namespace, containerID, runtime.ExecAsUserCmd(execUser, "tmux has-session -t scion"))
			} else {
				cmd := exec.CommandContext(ctx, runtimeCmd, "exec", "--user", execUser, containerID, "tmux", "has-session", "-t", "scion")
				checkErr = cmd.Run()
			}
			if checkErr == nil {
				return nil
			}
			slog.Debug("Waiting for tmux session", "containerID", containerID, "runtime", runtimeCmd)
		}
	}
}

// k8sExecCheck runs a non-interactive command in a pod container via the K8s
// Go client API. Returns nil if the command exits 0.
func k8sExecCheck(ctx context.Context, config *rest.Config, clientset kubernetes.Interface, namespace, podName string, command []string) error {
	if namespace == "" {
		namespace = "default"
	}
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	req.VersionedParams(&corev1.PodExecOptions{
		Container: "agent",
		Command:   command,
		Stdin:     false,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return err
	}

	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
}

var ptyUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true // Auth is handled separately
	},
}

// handleAgentAttach handles direct WebSocket PTY connections.
// This is used when clients connect directly to the runtime broker.
// Route: GET /api/v1/agents/{id}/attach
func (s *Server) handleAgentAttach(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	agentID := extractAgentIDFromAttachPath(r.URL.Path)
	if agentID == "" {
		BadRequest(w, "Invalid agent ID")
		return
	}

	// Verify WebSocket upgrade
	if !isPTYWebSocketUpgrade(r) {
		BadRequest(w, "WebSocket upgrade required")
		return
	}

	// Look up agent using LookupAgent for runtime-aware info
	projectID := r.URL.Query().Get("projectId")
	if projectID == "" {
		projectID = r.URL.Query().Get("groveId")
	}
	result, err := s.LookupAgent(ctx, agentID, projectID)
	if err != nil {
		NotFound(w, "Agent")
		return
	}

	containerID := result.ContainerID

	// Upgrade to WebSocket
	conn, err := ptyUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("WebSocket upgrade failed for agent", "agent_id", agentID, "error", err)
		return
	}
	defer func() { _ = conn.Close() }()

	// Get terminal size from query params
	cols := 80
	rows := 24
	if c := r.URL.Query().Get("cols"); c != "" {
		_, _ = fmt.Sscanf(c, "%d", &cols)
	}
	if rowStr := r.URL.Query().Get("rows"); rowStr != "" {
		_, _ = fmt.Sscanf(rowStr, "%d", &rows)
	}

	runtimeCmd := result.RuntimeName
	if runtimeCmd == "" {
		runtimeCmd = s.RuntimeCommand()
	}

	slog.Info("Attach session started", "agent_id", agentID, "containerID", containerID, "runtime", runtimeCmd)

	// Start PTY session
	session := newLocalPTYSession(ctx, agentID, containerID, runtimeCmd, result.ExecUser, result.Namespace, conn, cols, rows, result.K8sConfig, result.K8sClientset)
	if err := session.Run(); err != nil && err != io.EOF {
		slog.Error("Attach session error", "agent_id", agentID, "error", err)
	}

	slog.Info("Attach session ended", "agent_id", agentID)
}

// extractAgentIDFromAttachPath extracts agent ID from /api/v1/agents/{id}/attach
func extractAgentIDFromAttachPath(path string) string {
	const prefix = "/api/v1/agents/"
	const suffix = "/attach"

	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return ""
	}

	path = strings.TrimPrefix(path, prefix)
	path = strings.TrimSuffix(path, suffix)
	return path
}

// isPTYWebSocketUpgrade checks if the request is a WebSocket upgrade.
func isPTYWebSocketUpgrade(r *http.Request) bool {
	return strings.ToLower(r.Header.Get("Upgrade")) == "websocket" &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// LocalPTYSession manages a local PTY session attached to a container.
type LocalPTYSession struct {
	ctx         context.Context
	cancel      context.CancelFunc
	agentID     string
	containerID string
	runtimeCmd  string // Container runtime command (docker, container, kubernetes, etc.)
	execUser    string // Container user for exec (e.g., "scion" or "root" for rootless Podman)
	namespace   string // Kubernetes namespace (empty for non-k8s runtimes)
	conn        *websocket.Conn
	cols        int
	rows        int
	cmd         *exec.Cmd
	ptyMaster   *os.File
	ptySlave    *os.File
	writeMu     sync.Mutex
	attachNonce string // Per-attach nonce for container-side PID identification (empty = disabled)

	// K8s Go client for direct API exec
	k8sConfig    *rest.Config
	k8sClientset kubernetes.Interface
}

// newLocalPTYSession creates a new local PTY session.
func newLocalPTYSession(ctx context.Context, agentID, containerID, runtimeCmd, execUser, namespace string, conn *websocket.Conn, cols, rows int, k8sConfig *rest.Config, k8sClientset kubernetes.Interface) *LocalPTYSession {
	if runtimeCmd == "" {
		// The caller (handleAgentAttach) resolves runtimeCmd from the agent's
		// RuntimeName or the server's detected RuntimeCommand, so this branch
		// should not be reached. Log a warning so the fallback is visible
		// rather than silently using a binary that may not exist on podman-only
		// or non-standard-path hosts.
		slog.Warn("PTY session created without explicit runtime command, falling back to docker",
			"agent_id", agentID)
		runtimeCmd = "docker"
	}
	execUser = sanitizeExecUser(execUser)
	ctx, cancel := context.WithCancel(ctx)
	return &LocalPTYSession{
		ctx:          ctx,
		cancel:       cancel,
		agentID:      agentID,
		containerID:  containerID,
		runtimeCmd:   runtimeCmd,
		execUser:     execUser,
		namespace:    namespace,
		conn:         conn,
		cols:         cols,
		rows:         rows,
		k8sConfig:    k8sConfig,
		k8sClientset: k8sClientset,
	}
}

// Run starts the PTY session.
func (s *LocalPTYSession) Run() error {
	isK8s := (s.runtimeCmd == "kubernetes" || s.runtimeCmd == "k8s") && s.k8sConfig != nil && s.k8sClientset != nil
	isCloudRunSandbox := s.runtimeCmd == "cloudrun-sandbox"

	if isCloudRunSandbox {
		if err := s.startCloudRunSandboxExec(); err != nil {
			return fmt.Errorf("failed to start sandbox exec: %w", err)
		}
		// Send initial active window.
		if wn := queryTmuxActiveWindow(s.ctx, s.runtimeCmd, s.containerID, s.execUser); wn != "" {
			msg := wsprotocol.NewPTYDataMessage(activeWindowOSC(wn))
			_ = s.writeToWebSocket(msg)
		}
		// Fall through to the same read/write/resize loop as Docker.
	} else if isK8s {
		return s.runK8sExec()
	} else {
		// Start docker/container exec with PTY
		if err := s.startDockerExec(); err != nil {
			return fmt.Errorf("failed to start exec: %w", err)
		}

		// Send the active tmux window name so the web toolbar reflects the
		// correct initial state (the default assumption is "agent").
		if wn := queryTmuxActiveWindow(s.ctx, s.runtimeCmd, s.containerID, s.execUser); wn != "" {
			msg := wsprotocol.NewPTYDataMessage(activeWindowOSC(wn))
			_ = s.writeToWebSocket(msg)
		}
	}

	defer func() {
		// readFromWebSocket has already exited (joined below).
		// Safe to close PTY — no in-flight resize.
		// Graceful shutdown: close PTY (terminal hangup) → wait → SIGTERM →
		// SIGKILL. This gives the container runtime a chance to propagate
		// the hangup to the container-side tmux attach process (TW-UAT-002
		// mitigation). If SIGKILL is required and the runtime does not
		// propagate the kill signal, a residual container-side tmux client
		// may remain until the container restarts — this is a known gap
		// pending UAT verification.
		gracefulShutdownExec(s.cmd, s.ptyMaster, s.agentID,
			s.runtimeCmd, s.containerID, s.execUser, s.attachNonce)
	}()

	errCh := make(chan error, 2)
	wsDone := make(chan struct{})

	// Read from PTY, write to WebSocket
	go func() {
		errCh <- s.readFromPTY()
	}()

	// Read from WebSocket, write to PTY.
	// wsDone is closed when this goroutine exits, providing a happens-before
	// guarantee that no in-flight resize (Setsize) is running when we close
	// the PTY. This mirrors StreamPTYHandler's resizeDone join pattern.
	go func() {
		defer close(wsDone)
		errCh <- s.readFromWebSocket()
	}()

	// Wait for first I/O completion or context cancellation.
	// Without exec.CommandContext, context cancellation alone does not kill
	// the process or close the PTY, so we must handle ctx.Done() explicitly.
	var err error
	select {
	case err = <-errCh:
	case <-s.ctx.Done():
		err = s.ctx.Err()
	}
	s.cancel()

	// Close WebSocket to unblock readFromWebSocket if still blocked on
	// conn.ReadMessage(). This also prevents any future resize messages.
	if s.conn != nil {
		_ = s.conn.Close()
	}
	// Join readFromWebSocket — ensures no in-flight resize (Setsize).
	// readFromWebSocket exits promptly: ReadMessage returns error from
	// conn.Close(), or ctx.Done() check at loop top.
	<-wsDone

	// NOW readFromWebSocket has fully exited — no concurrent Setsize.
	// PTY close happens in deferred gracefulShutdownExec.
	// readFromPTY unblocks when gracefulShutdownExec closes PTY.
	// Both goroutines send to errCh (capacity 2) — no leak.
	return err
}

// runK8sExec attaches to a K8s pod using the Go client's remotecommand API,
// bridging the SPDY exec stream directly to the WebSocket connection.
func (s *LocalPTYSession) runK8sExec() error {
	namespace := s.namespace
	if namespace == "" {
		namespace = "default"
	}

	if err := waitForTmuxSession(s.ctx, s.runtimeCmd, s.containerID, namespace, s.execUser, s.k8sConfig, s.k8sClientset); err != nil {
		return err
	}

	// Send the active tmux window name so the web toolbar reflects the
	// correct initial state (the default assumption is "agent").
	if wn := queryTmuxActiveWindowK8s(s.ctx, s.k8sConfig, s.k8sClientset, namespace, s.containerID, s.execUser); wn != "" {
		msg := wsprotocol.NewPTYDataMessage(activeWindowOSC(wn))
		_ = s.writeToWebSocket(msg)
	}

	req := s.k8sClientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(s.containerID).
		Namespace(namespace).
		SubResource("exec")

	// Run as the configured exec user (default "scion"): the tmux
	// session is owned by that user (sciontool init drops privileges),
	// so root can't see the session.
	req.VersionedParams(&corev1.PodExecOptions{
		Container: "agent",
		Command:   runtime.ExecAsUserCmd(s.execUser, tmuxAttachCmd),
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       true,
	}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(s.k8sConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create SPDY executor: %w", err)
	}

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	// Build resize queue from WebSocket resize messages
	resizeCh := make(chan [2]int, 4)
	sizeQueue := &k8sSizeQueue{
		resizeCh: resizeCh,
		closeCh:  make(chan struct{}),
		ctx:      s.ctx,
		initial:  &remotecommand.TerminalSize{Width: uint16(s.cols), Height: uint16(s.rows)},
	}

	errCh := make(chan error, 3)

	// Run SPDY executor
	go func() {
		execErr := executor.StreamWithContext(s.ctx, remotecommand.StreamOptions{
			Stdin:             stdinReader,
			Stdout:            stdoutWriter,
			Stderr:            stdoutWriter,
			Tty:               true,
			TerminalSizeQueue: sizeQueue,
		})
		_ = stdoutWriter.Close()
		_ = stdinReader.Close()
		errCh <- execErr
	}()

	// Read from SPDY stdout, send to WebSocket
	go func() {
		buf := make([]byte, ptyMaxDataSize)
		for {
			n, readErr := stdoutReader.Read(buf)
			if n > 0 {
				msg := wsprotocol.NewPTYDataMessage(buf[:n])
				if sendErr := s.writeToWebSocket(msg); sendErr != nil {
					errCh <- sendErr
					return
				}
			}
			if readErr != nil {
				errCh <- readErr
				return
			}
		}
	}()

	// Read from WebSocket, write to SPDY stdin (and handle resize)
	go func() {
		for {
			select {
			case <-s.ctx.Done():
				errCh <- s.ctx.Err()
				return
			default:
			}

			_, data, readErr := s.conn.ReadMessage()
			if readErr != nil {
				errCh <- readErr
				return
			}

			env, parseErr := wsprotocol.ParseEnvelope(data)
			if parseErr != nil {
				continue
			}

			switch env.Type {
			case wsprotocol.TypeData:
				var msg wsprotocol.PTYDataMessage
				if err := json.Unmarshal(data, &msg); err != nil {
					continue
				}
				if _, writeErr := stdinWriter.Write(msg.Data); writeErr != nil {
					errCh <- writeErr
					return
				}
			case wsprotocol.TypeResize:
				var msg wsprotocol.PTYResizeMessage
				if err := json.Unmarshal(data, &msg); err != nil {
					continue
				}
				select {
				case resizeCh <- [2]int{msg.Cols, msg.Rows}:
				default:
				}
			}
		}
	}()

	err = <-errCh
	s.cancel()
	_ = stdinWriter.Close()
	_ = stdoutReader.Close()
	return err
}

// startCloudRunSandboxExec starts a sandbox exec session with tmux attach using a real PTY.
// Unlike Docker, sandbox exec has no --user flag and requires --env for TERM.
// TERM=xterm-256color is load-bearing: without it the inner tmux sees TERM=dumb
// and exits with "terminal does not support clear" (design doc section 4.4a-rev).
func (s *LocalPTYSession) startCloudRunSandboxExec() error {
	if err := waitForTmuxSession(s.ctx, s.runtimeCmd, s.containerID, s.namespace, s.execUser, nil, nil); err != nil {
		return err
	}

	args := []string{
		"exec", s.containerID,
		"--env", "TERM=xterm-256color",
		"--", "/usr/bin/tmux", "attach-session", "-t", "scion",
	}

	// Use exec.Command (NOT exec.CommandContext) so that context cancellation
	// does not immediately SIGKILL the process. See gracefulShutdownExec.
	s.cmd = exec.Command(cloudRunSandboxBin, args...)

	ptmx, err := pty.StartWithSize(s.cmd, &pty.Winsize{
		Cols: uint16(s.cols),
		Rows: uint16(s.rows),
	})
	if err != nil {
		return fmt.Errorf("failed to start sandbox exec with PTY: %w", err)
	}

	s.ptyMaster = ptmx
	s.ptySlave = ptmx
	return nil
}

// startDockerExec starts a docker exec session with tmux attach using a real PTY.
func (s *LocalPTYSession) startDockerExec() error {
	if err := waitForTmuxSession(s.ctx, s.runtimeCmd, s.containerID, s.namespace, s.execUser, nil, nil); err != nil {
		return err
	}

	// Generate per-attach nonce for container-side PID identification.
	// Only for Docker-compatible runtimes that support -e env injection.
	if isDockerCompatibleRuntime(s.runtimeCmd) {
		nonce, err := generateAttachNonce()
		if err != nil {
			// Nonce generation failure is not fatal — cleanup falls back to current behavior
			slog.Debug("attach nonce generation failed, container cleanup disabled", "error", err)
		} else {
			s.attachNonce = nonce
		}
	}

	args := []string{"exec", "-it"}
	if s.attachNonce != "" {
		args = append(args, "-e", "SCION_ATTACH_NONCE="+s.attachNonce)
	}
	args = append(args, "-e", "TERM=xterm-256color",
		"--user", s.execUser,
		s.containerID,
		"tmux", "attach-session", "-t", "scion",
	)

	// Use exec.Command (NOT exec.CommandContext) so that context cancellation
	// does not immediately SIGKILL the process. See gracefulShutdownExec.
	s.cmd = exec.Command(s.runtimeCmd, args...)

	ptmx, err := pty.StartWithSize(s.cmd, &pty.Winsize{
		Cols: uint16(s.cols),
		Rows: uint16(s.rows),
	})
	if err != nil {
		return fmt.Errorf("failed to start %s exec with PTY: %w", s.runtimeCmd, err)
	}

	s.ptyMaster = ptmx
	s.ptySlave = ptmx
	return nil
}

// readFromPTY reads data from the PTY and sends to WebSocket.
func (s *LocalPTYSession) readFromPTY() error {
	buf := make([]byte, ptyMaxDataSize)

	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		default:
		}

		n, err := s.ptySlave.Read(buf)
		if err != nil {
			return err
		}

		if n > 0 {
			msg := wsprotocol.NewPTYDataMessage(buf[:n])
			if err := s.writeToWebSocket(msg); err != nil {
				return err
			}
		}
	}
}

// readFromWebSocket reads messages from WebSocket and writes to PTY.
func (s *LocalPTYSession) readFromWebSocket() error {
	for {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		default:
		}

		_, data, err := s.conn.ReadMessage()
		if err != nil {
			return err
		}

		env, err := wsprotocol.ParseEnvelope(data)
		if err != nil {
			continue
		}

		switch env.Type {
		case wsprotocol.TypeData:
			var msg wsprotocol.PTYDataMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			if _, err := s.ptyMaster.Write(msg.Data); err != nil {
				return err
			}

		case wsprotocol.TypeResize:
			var msg wsprotocol.PTYResizeMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			resizeSandboxTerminal(s.ctx, s.runtimeCmd, s.containerID, s.agentID, msg.Cols, msg.Rows, s.ptyMaster)
		}
	}
}

// writeToWebSocket writes a message to the WebSocket connection.
func (s *LocalPTYSession) writeToWebSocket(v interface{}) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.conn.WriteJSON(v)
}

// StreamPTYHandler handles PTY streams coming through the control channel.
type StreamPTYHandler struct {
	client      *ControlChannelClient
	handler     *StreamHandler
	slug        string
	containerID string
	runtimeCmd  string // Container runtime command (docker, container, kubernetes, etc.)
	execUser    string // Container user for exec (e.g., "scion" or "root" for rootless Podman)
	namespace   string // Kubernetes namespace (empty for non-k8s runtimes)
	cols        int
	rows        int
	ptyMaster   *os.File
	ptySlave    *os.File
	cmd         *exec.Cmd
	ctx         context.Context
	cancel      context.CancelFunc
	attachNonce string // Per-attach nonce for container-side PID identification (empty = disabled)

	// K8s Go client for direct API exec (avoids needing kubectl binary)
	k8sConfig    *rest.Config
	k8sClientset kubernetes.Interface
}

// NewStreamPTYHandler creates a handler for a PTY stream from the control channel.
func NewStreamPTYHandler(client *ControlChannelClient, handler *StreamHandler, containerID, runtimeCmd, execUser, namespace string, cols, rows int, k8sConfig *rest.Config, k8sClientset kubernetes.Interface) *StreamPTYHandler {
	ctx, cancel := context.WithCancel(context.Background())
	execUser = sanitizeExecUser(execUser)
	return &StreamPTYHandler{
		client:       client,
		handler:      handler,
		slug:         handler.slug,
		containerID:  containerID,
		runtimeCmd:   runtimeCmd,
		execUser:     execUser,
		namespace:    namespace,
		cols:         cols,
		rows:         rows,
		ctx:          ctx,
		cancel:       cancel,
		k8sConfig:    k8sConfig,
		k8sClientset: k8sClientset,
	}
}

// Run starts the PTY stream handler.
func (h *StreamPTYHandler) Run() error {
	runtimeCmd := h.runtimeCmd
	if runtimeCmd == "" {
		runtimeCmd = "docker"
	}
	isK8s := (runtimeCmd == "kubernetes" || runtimeCmd == "k8s") && h.k8sConfig != nil && h.k8sClientset != nil
	isCloudRunSandbox := runtimeCmd == "cloudrun-sandbox"

	if isCloudRunSandbox {
		if err := h.startCloudRunSandboxExec(); err != nil {
			return err
		}
		// Send initial active window.
		if wn := queryTmuxActiveWindow(h.ctx, runtimeCmd, h.containerID, h.execUser); wn != "" {
			_ = h.client.SendStreamData(h.handler.streamID, activeWindowOSC(wn))
		}
		// Fall through to the same read/write/resize loop as Docker.
	} else if isK8s {
		return h.runK8sExec()
	} else {
		// Start docker/container exec with tmux attach
		if err := h.startDockerExec(); err != nil {
			return err
		}

		// Send the active tmux window name so the web toolbar reflects the
		// correct initial state (the default assumption is "agent").
		if wn := queryTmuxActiveWindow(h.ctx, runtimeCmd, h.containerID, h.execUser); wn != "" {
			_ = h.client.SendStreamData(h.handler.streamID, activeWindowOSC(wn))
		}
	}

	defer func() {
		// Graceful shutdown: close PTY (terminal hangup) → wait → SIGTERM →
		// SIGKILL. This gives the container runtime a chance to propagate
		// the hangup to the container-side tmux attach process (TW-UAT-002
		// mitigation). If SIGKILL is required and the runtime does not
		// propagate the kill signal, a residual container-side tmux client
		// may remain until the container restarts — this is a known gap
		// pending UAT verification.
		gracefulShutdownExec(h.cmd, h.ptyMaster, h.slug,
			h.runtimeCmd, h.containerID, h.execUser, h.attachNonce)
	}()

	errCh := make(chan error, 2)

	// Read from PTY, send to control channel
	go func() {
		errCh <- h.readFromPTY()
	}()

	// Read from control channel, write to PTY
	go func() {
		errCh <- h.readFromStream()
	}()

	// Handle resize events
	resizeDone := make(chan struct{})
	go func() {
		defer close(resizeDone)
		h.handleResize()
	}()

	// Wait for an I/O goroutine to fail OR for context cancellation.
	// readFromStream is select-driven with ctx.Done() and unblocks
	// immediately on cancel. readFromPTY blocks on ptySlave.Read —
	// we close the PTY below (after resize join) to unblock it.
	var err error
	select {
	case err = <-errCh:
	case <-h.ctx.Done():
		err = h.ctx.Err()
	}
	h.cancel()
	// Setsize accesses the raw descriptor, so join the resize worker before
	// closing the PTY. handleResize returns promptly on cancel because its
	// select includes h.ctx.Done() and h.handler.closeCh.
	<-resizeDone
	// Close PTY to unblock readFromPTY (ptySlave.Read). This is safe to do
	// here because the resize worker has exited — no concurrent Setsize/Fd
	// calls. The deferred gracefulShutdownExec double-closes safely (same
	// *os.File, Go's poll.FD tracks closed state).
	if h.ptyMaster != nil {
		_ = h.ptyMaster.Close()
	}
	return err
}

// runK8sExec attaches to a K8s pod using the Go client's remotecommand API,
// bridging the SPDY exec stream directly to the control channel stream.
// No local PTY or kubectl binary is needed.
func (h *StreamPTYHandler) runK8sExec() error {
	namespace := h.namespace
	if namespace == "" {
		namespace = "default"
	}

	// Wait for tmux session readiness using Go client
	if err := waitForTmuxSession(h.ctx, h.runtimeCmd, h.containerID, namespace, h.execUser, h.k8sConfig, h.k8sClientset); err != nil {
		return err
	}

	// Send the active tmux window name so the web toolbar reflects the
	// correct initial state (the default assumption is "agent").
	if wn := queryTmuxActiveWindowK8s(h.ctx, h.k8sConfig, h.k8sClientset, namespace, h.containerID, h.execUser); wn != "" {
		_ = h.client.SendStreamData(h.handler.streamID, activeWindowOSC(wn))
	}

	req := h.k8sClientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(h.containerID).
		Namespace(namespace).
		SubResource("exec")

	// Run as the configured exec user (default "scion"): the tmux
	// session is owned by that user (sciontool init drops privileges),
	// so root can't see the session.
	req.VersionedParams(&corev1.PodExecOptions{
		Container: "agent",
		Command:   runtime.ExecAsUserCmd(h.execUser, tmuxAttachCmd),
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       true,
	}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(h.k8sConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create SPDY executor: %w", err)
	}

	// Create a pipe for stdin: control channel data → pipe writer → SPDY stdin
	stdinReader, stdinWriter := io.Pipe()

	// Create a pipe for stdout: SPDY stdout → pipe writer → control channel
	stdoutReader, stdoutWriter := io.Pipe()

	// Build resize queue
	sizeQueue := &k8sSizeQueue{
		resizeCh: h.handler.resizeCh,
		closeCh:  h.handler.closeCh,
		ctx:      h.ctx,
		initial:  &remotecommand.TerminalSize{Width: uint16(h.cols), Height: uint16(h.rows)},
	}

	errCh := make(chan error, 3)

	// Run SPDY executor in background
	go func() {
		err := executor.StreamWithContext(h.ctx, remotecommand.StreamOptions{
			Stdin:             stdinReader,
			Stdout:            stdoutWriter,
			Stderr:            stdoutWriter, // merge stderr into stdout
			Tty:               true,
			TerminalSizeQueue: sizeQueue,
		})
		_ = stdoutWriter.Close()
		_ = stdinReader.Close()
		errCh <- err
	}()

	// Read from SPDY stdout, send to control channel
	go func() {
		buf := make([]byte, ptyMaxDataSize)
		for {
			n, readErr := stdoutReader.Read(buf)
			if n > 0 {
				if sendErr := h.client.SendStreamData(h.handler.streamID, buf[:n]); sendErr != nil {
					errCh <- sendErr
					return
				}
			}
			if readErr != nil {
				errCh <- readErr
				return
			}
		}
	}()

	// Read from control channel, write to SPDY stdin
	go func() {
		for {
			select {
			case <-h.ctx.Done():
				errCh <- h.ctx.Err()
				return
			case <-h.handler.closeCh:
				_ = stdinWriter.Close()
				errCh <- io.EOF
				return
			case data := <-h.handler.dataCh:
				if _, writeErr := stdinWriter.Write(data); writeErr != nil {
					errCh <- writeErr
					return
				}
			}
		}
	}()

	err = <-errCh
	h.cancel()
	_ = stdinWriter.Close()
	_ = stdoutReader.Close()
	return err
}

// k8sSizeQueue implements remotecommand.TerminalSizeQueue for K8s exec resize.
type k8sSizeQueue struct {
	resizeCh <-chan [2]int
	closeCh  <-chan struct{}
	ctx      context.Context
	initial  *remotecommand.TerminalSize
}

func (q *k8sSizeQueue) Next() *remotecommand.TerminalSize {
	// Return initial size on first call
	if q.initial != nil {
		size := q.initial
		q.initial = nil
		return size
	}
	select {
	case <-q.ctx.Done():
		return nil
	case <-q.closeCh:
		return nil
	case size := <-q.resizeCh:
		return &remotecommand.TerminalSize{Width: uint16(size[0]), Height: uint16(size[1])}
	}
}

// handleResize listens for resize events and applies them to the PTY.
func (h *StreamPTYHandler) handleResize() {
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-h.handler.closeCh:
			return
		case size := <-h.handler.resizeCh:
			cols, rows := size[0], size[1]
			resizeSandboxTerminal(h.ctx, h.runtimeCmd, h.containerID, h.slug, cols, rows, h.ptyMaster)
		}
	}
}

// startCloudRunSandboxExec starts a sandbox exec session with tmux attach using a real PTY.
// Unlike Docker, sandbox exec has no --user flag and requires --env for TERM.
// TERM=xterm-256color is load-bearing: without it the inner tmux sees TERM=dumb
// and exits with "terminal does not support clear" (design doc section 4.4a-rev).
func (h *StreamPTYHandler) startCloudRunSandboxExec() error {
	if err := waitForTmuxSession(h.ctx, h.runtimeCmd, h.containerID, h.namespace, h.execUser, nil, nil); err != nil {
		return err
	}

	args := []string{
		"exec", h.containerID,
		"--env", "TERM=xterm-256color",
		"--", "/usr/bin/tmux", "attach-session", "-t", "scion",
	}

	// Use exec.Command (NOT exec.CommandContext) so that context cancellation
	// does not immediately SIGKILL the process. See gracefulShutdownExec.
	h.cmd = exec.Command(cloudRunSandboxBin, args...)

	ptmx, err := pty.StartWithSize(h.cmd, &pty.Winsize{
		Cols: uint16(h.cols),
		Rows: uint16(h.rows),
	})
	if err != nil {
		return fmt.Errorf("failed to start sandbox exec with PTY: %w", err)
	}

	h.ptyMaster = ptmx
	h.ptySlave = ptmx
	return nil
}

// startDockerExec starts container exec with tmux attach using the configured runtime.
// Uses a real PTY for proper terminal handling with Docker and Apple runtimes.
// K8s runtimes are handled by runK8sExec() instead.
func (h *StreamPTYHandler) startDockerExec() error {
	runtimeCmd := h.runtimeCmd
	if runtimeCmd == "" {
		runtimeCmd = "docker"
	}

	// Wait for the tmux session to be ready before attaching
	if err := waitForTmuxSession(h.ctx, runtimeCmd, h.containerID, h.namespace, h.execUser, nil, nil); err != nil {
		return err
	}

	// Generate per-attach nonce for container-side PID identification.
	// Only for Docker-compatible runtimes that support -e env injection.
	if isDockerCompatibleRuntime(runtimeCmd) {
		nonce, err := generateAttachNonce()
		if err != nil {
			// Nonce generation failure is not fatal — cleanup falls back to current behavior
			slog.Debug("attach nonce generation failed, container cleanup disabled", "error", err)
		} else {
			h.attachNonce = nonce
		}
	}

	args := []string{"exec", "-it"}
	if h.attachNonce != "" {
		args = append(args, "-e", "SCION_ATTACH_NONCE="+h.attachNonce)
	}
	args = append(args,
		"--user", h.execUser,
		h.containerID,
		"tmux", "attach-session", "-t", "scion",
	)

	// Use exec.Command (NOT exec.CommandContext) so that context cancellation
	// does not immediately SIGKILL the process. See gracefulShutdownExec.
	h.cmd = exec.Command(runtimeCmd, args...)

	// Start with a real PTY - this provides proper terminal handling
	ptmx, err := pty.StartWithSize(h.cmd, &pty.Winsize{
		Cols: uint16(h.cols),
		Rows: uint16(h.rows),
	})
	if err != nil {
		return fmt.Errorf("failed to start %s exec with PTY: %w", runtimeCmd, err)
	}

	h.ptyMaster = ptmx
	h.ptySlave = ptmx
	return nil
}

// readFromPTY reads from the PTY and sends to the control channel stream.
func (h *StreamPTYHandler) readFromPTY() error {
	buf := make([]byte, ptyMaxDataSize)

	for {
		select {
		case <-h.ctx.Done():
			return h.ctx.Err()
		case <-h.handler.closeCh:
			return io.EOF
		default:
		}

		n, err := h.ptySlave.Read(buf)
		if err != nil {
			return err
		}

		if n > 0 {
			if err := h.client.SendStreamData(h.handler.streamID, buf[:n]); err != nil {
				return err
			}
		}
	}
}

// readFromStream reads from the control channel stream and writes to PTY.
func (h *StreamPTYHandler) readFromStream() error {
	for {
		select {
		case <-h.ctx.Done():
			return h.ctx.Err()
		case <-h.handler.closeCh:
			return io.EOF
		case data := <-h.handler.dataCh:
			if _, err := h.ptyMaster.Write(data); err != nil {
				return err
			}
		}
	}
}

// Close stops the PTY handler. It initiates shutdown by canceling the context
// and sending SIGTERM to the process. Run() handles all PTY closes after
// joining the resize worker to avoid a data race between os.File.Close() and
// handleResize's pty.Setsize (which calls os.File.Fd()). The full graceful
// shutdown sequence (PTY close → wait → SIGTERM → SIGKILL) runs in Run()'s
// deferred gracefulShutdownExec after the resize worker exits.
//
// Close() does NOT close h.ptyMaster directly — that would race with
// handleResize if the resize worker hasn't exited yet. Context cancellation
// causes Run()'s select to unblock on ctx.Done(), which then joins
// resizeDone before closing the PTY.
func (h *StreamPTYHandler) Close() {
	h.cancel()
	if h.cmd != nil && h.cmd.Process != nil {
		// SIGTERM instead of SIGKILL — gives the container runtime a chance
		// to propagate the signal to the container-side process (TW-UAT-002).
		_ = h.cmd.Process.Signal(syscall.SIGTERM)
	}
}

// handlePTYStreamWithAgent is called by the control channel to handle PTY streams.
func (c *ControlChannelClient) handlePTYStreamWithAgent(handler *StreamHandler, cols, rows int, containerID, runtimeCmd, execUser, namespace string, k8sConfig *rest.Config, k8sClientset kubernetes.Interface) {
	ptyHandler := NewStreamPTYHandler(c, handler, containerID, runtimeCmd, execUser, namespace, cols, rows, k8sConfig, k8sClientset)
	if err := ptyHandler.Run(); err != nil && err != io.EOF {
		slog.Error("PTY stream error", "slug", handler.slug, "error", err)
	}
}
