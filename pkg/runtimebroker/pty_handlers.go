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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
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
)

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

	cmd := exec.CommandContext(ctx, runtimeCmd, "exec", "--user", execUser, containerID,
		"tmux", "display-message", "-t", "scion", "-p", "#{window_name}")
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
	result, err := s.LookupAgent(ctx, agentID)
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
	defer conn.Close()

	// Get terminal size from query params
	cols := 80
	rows := 24
	if c := r.URL.Query().Get("cols"); c != "" {
		fmt.Sscanf(c, "%d", &cols)
	}
	if rowStr := r.URL.Query().Get("rows"); rowStr != "" {
		fmt.Sscanf(rowStr, "%d", &rows)
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

	// K8s Go client for direct API exec
	k8sConfig    *rest.Config
	k8sClientset kubernetes.Interface
}

// newLocalPTYSession creates a new local PTY session.
func newLocalPTYSession(ctx context.Context, agentID, containerID, runtimeCmd, execUser, namespace string, conn *websocket.Conn, cols, rows int, k8sConfig *rest.Config, k8sClientset kubernetes.Interface) *LocalPTYSession {
	if runtimeCmd == "" {
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

	if isK8s {
		return s.runK8sExec()
	}

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

	defer func() {
		if s.ptyMaster != nil {
			s.ptyMaster.Close()
		}
		if s.cmd != nil && s.cmd.Process != nil {
			s.cmd.Process.Kill()
			s.cmd.Wait()
		}
	}()

	errCh := make(chan error, 2)

	// Read from PTY, write to WebSocket
	go func() {
		errCh <- s.readFromPTY()
	}()

	// Read from WebSocket, write to PTY
	go func() {
		errCh <- s.readFromWebSocket()
	}()

	// Wait for either direction to fail
	err := <-errCh
	s.cancel()
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
		stdoutWriter.Close()
		stdinReader.Close()
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
	stdinWriter.Close()
	stdoutReader.Close()
	return err
}

// startDockerExec starts a docker exec session with tmux attach using a real PTY.
func (s *LocalPTYSession) startDockerExec() error {
	if err := waitForTmuxSession(s.ctx, s.runtimeCmd, s.containerID, s.namespace, s.execUser, nil, nil); err != nil {
		return err
	}

	args := []string{
		"exec", "-it",
		"-e", "TERM=xterm-256color",
		"--user", s.execUser,
		s.containerID,
		"tmux", "attach-session", "-t", "scion",
	}

	s.cmd = exec.CommandContext(s.ctx, s.runtimeCmd, args...)

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
			if s.ptyMaster != nil {
				if err := pty.Setsize(s.ptyMaster, &pty.Winsize{
					Cols: uint16(msg.Cols),
					Rows: uint16(msg.Rows),
				}); err != nil {
					slog.Debug("PTY resize failed", "agent_id", s.agentID, "error", err)
				} else {
					slog.Debug("PTY resized", "agent_id", s.agentID, "cols", msg.Cols, "rows", msg.Rows)
				}
			}
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

	if isK8s {
		return h.runK8sExec()
	}

	// Start docker/container exec with tmux attach
	if err := h.startDockerExec(); err != nil {
		return err
	}

	// Send the active tmux window name so the web toolbar reflects the
	// correct initial state (the default assumption is "agent").
	if wn := queryTmuxActiveWindow(h.ctx, runtimeCmd, h.containerID, h.execUser); wn != "" {
		_ = h.client.SendStreamData(h.handler.streamID, activeWindowOSC(wn))
	}

	defer func() {
		// With real PTY, ptyMaster and ptySlave are the same fd, so only close once
		if h.ptyMaster != nil {
			h.ptyMaster.Close()
		}
		if h.cmd != nil && h.cmd.Process != nil {
			// Kill only if still running
			if h.cmd.ProcessState == nil {
				h.cmd.Process.Kill()
			}
			if err := h.cmd.Wait(); err != nil {
				slog.Debug("PTY command exited with error", "slug", h.slug, "error", err)
			}
		}
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
	go h.handleResize()

	err := <-errCh
	h.cancel()
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
		stdoutWriter.Close()
		stdinReader.Close()
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
				stdinWriter.Close()
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
	stdinWriter.Close()
	stdoutReader.Close()
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
			if h.ptyMaster != nil {
				if err := pty.Setsize(h.ptyMaster, &pty.Winsize{
					Cols: uint16(cols),
					Rows: uint16(rows),
				}); err != nil {
					slog.Debug("PTY resize failed", "slug", h.slug, "error", err)
				} else {
					slog.Debug("PTY resized", "slug", h.slug, "cols", cols, "rows", rows)
				}
			}
		}
	}
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

	args := []string{
		"exec", "-it",
		"--user", h.execUser,
		h.containerID,
		"tmux", "attach-session", "-t", "scion",
	}

	h.cmd = exec.CommandContext(h.ctx, runtimeCmd, args...)

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

// Close stops the PTY handler.
func (h *StreamPTYHandler) Close() {
	h.cancel()
	// With real PTY, ptyMaster and ptySlave are the same fd, so only close once
	if h.ptyMaster != nil {
		h.ptyMaster.Close()
	}
	if h.cmd != nil && h.cmd.Process != nil {
		h.cmd.Process.Kill()
	}
}

// handlePTYStreamWithAgent is called by the control channel to handle PTY streams.
func (c *ControlChannelClient) handlePTYStreamWithAgent(handler *StreamHandler, cols, rows int, containerID, runtimeCmd, execUser, namespace string, k8sConfig *rest.Config, k8sClientset kubernetes.Interface) {
	ptyHandler := NewStreamPTYHandler(c, handler, containerID, runtimeCmd, execUser, namespace, cols, rows, k8sConfig, k8sClientset)
	if err := ptyHandler.Run(); err != nil && err != io.EOF {
		slog.Error("PTY stream error", "slug", handler.slug, "error", err)
	}
}
