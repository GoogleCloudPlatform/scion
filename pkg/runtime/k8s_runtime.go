package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/k8s"
	"github.com/ptone/scion-agent/pkg/mutagen"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

type KubernetesRuntime struct {
	Client           *k8s.Client
	DefaultNamespace string
	SyncMode         string
}

func NewKubernetesRuntime(client *k8s.Client) *KubernetesRuntime {
	return &KubernetesRuntime{
		Client:           client,
		DefaultNamespace: "default",
		SyncMode:         "tar", // Default
	}
}

func (r *KubernetesRuntime) Name() string {
	return "kubernetes"
}

func (r *KubernetesRuntime) Run(ctx context.Context, config RunConfig) (string, error) {
	fmt.Printf("Starting agent '%s' on Kubernetes...\n", config.Name)
	namespace := r.DefaultNamespace
	if ns, ok := config.Labels["scion.namespace"]; ok {
		namespace = ns
	} else if ns, ok := config.Labels["namespace"]; ok {
		namespace = ns
	}

	if config.Name == "" {
		config.Name = fmt.Sprintf("scion-%d", time.Now().UnixNano())
	}

	// Persist workspace path in annotations for later sync
	if config.Workspace != "" {
		if config.Annotations == nil {
			config.Annotations = make(map[string]string)
		}
		config.Annotations["scion.workspace"] = config.Workspace
	}

	pod := r.buildPod(namespace, config)

	fmt.Printf("  Provisioning pod '%s' in namespace '%s'...\n", config.Name, namespace)
	createdPod, err := r.Client.Clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to create pod: %w", err)
	}

	// Wait for Ready
	if err := r.waitForPodReady(ctx, namespace, createdPod.Name); err != nil {
		return createdPod.Name, err
	}

	if config.HomeDir != "" {
		destHome := fmt.Sprintf("/home/%s", config.UnixUsername)
		fmt.Printf("  Syncing agent home (%s -> %s)...\n", config.HomeDir, destHome)
		err = r.syncToPod(ctx, namespace, createdPod.Name, config.HomeDir, destHome)
		if err != nil {
			return createdPod.Name, fmt.Errorf("failed to sync home: %w", err)
		}
	}

	if config.Workspace != "" {
		useMutagen := false
		if r.SyncMode == "mutagen" {
			if mutagen.CheckInstalled() {
				fmt.Println("  Initializing live sync session (Mutagen)...")
				if err := mutagen.StartDaemon(); err != nil {
					fmt.Printf("  Warning: failed to start mutagen daemon: %s. Falling back to snapshot sync.\n", err)
				} else {
					// Construct the Mutagen Kubernetes URL.
					// Format: kubernetes://<context>/<namespace>/<pod>/<container>:<path>
					remoteURL := fmt.Sprintf("kubernetes://%s/%s/%s/agent:/workspace",
						r.Client.CurrentContext, namespace, createdPod.Name)

					// Create Sync
					err = mutagen.CreateSync(
						"scion-"+createdPod.Name,
						config.Workspace,
						remoteURL,
						map[string]string{"scion-agent": createdPod.Name},
					)
					if err != nil {
						fmt.Printf("  Warning: failed to create mutagen sync: %s. Falling back to snapshot sync.\n", err)
					} else {
						fmt.Println("  Waiting for initial sync to complete...")
						if err := mutagen.WaitForSync("scion-"+createdPod.Name, 60*time.Second); err != nil {
							fmt.Printf("  Warning: mutagen sync timed out or failed: %s. Proceeding, but sync may be incomplete.\n", err)
						} else {
							fmt.Println("  Mutagen sync active.")
							useMutagen = true
						}
					}
				}
			} else {
				fmt.Println("  Warning: Sync mode is 'mutagen' but mutagen is not installed. Falling back to snapshot sync.")
			}
		}

		if !useMutagen {
			fmt.Printf("  Syncing workspace (%s -> /workspace)...\n", config.Workspace)
			err = r.syncToPod(ctx, namespace, createdPod.Name, config.Workspace, "/workspace")
			if err != nil {
				return createdPod.Name, fmt.Errorf("failed to sync workspace: %w", err)
			}
		}
	}

	fmt.Printf("Agent '%s' started successfully.\n", createdPod.Name)
	return createdPod.Name, nil
}

func (r *KubernetesRuntime) buildPod(namespace string, config RunConfig) *corev1.Pod {
	// Command Resolution
	var cmd []string
	if config.Harness != nil {
		cmd = config.Harness.GetCommand(config.Task, config.Resume, config.CommandArgs)
	} else {
		// Fallback if no harness (though RunConfig implies there should be one or defaults)
		cmd = []string{"/bin/sh", "-c", "sleep infinity"}
	}

	// Env Resolution
	envVars := []corev1.EnvVar{}
	for _, e := range config.Env {
		// Parse "KEY=VALUE"
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envVars = append(envVars, corev1.EnvVar{Name: parts[0], Value: parts[1]})
		}
	}

	// Auth Injection (Temporary M1 Solution)
	if config.Auth.GeminiAPIKey != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "GEMINI_API_KEY", Value: config.Auth.GeminiAPIKey})
	}
	if config.Auth.AnthropicAPIKey != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "ANTHROPIC_API_KEY", Value: config.Auth.AnthropicAPIKey})
	}
	if config.Auth.GoogleAPIKey != "" {
		envVars = append(envVars, corev1.EnvVar{Name: "GOOGLE_API_KEY", Value: config.Auth.GoogleAPIKey})
	}
	// Add other keys as needed

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        config.Name,
			Namespace:   namespace,
			Labels:      config.Labels,
			Annotations: config.Annotations,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:            "agent",
					Image:           config.Image,
					Command:         cmd,
					Env:             envVars,
					ImagePullPolicy: corev1.PullIfNotPresent,
					WorkingDir:      "/workspace",
					Stdin:           true,
					TTY:             true,
					VolumeMounts: []corev1.VolumeMount{
						{Name: "workspace", MountPath: "/workspace"},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "workspace",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}
}

func (r *KubernetesRuntime) waitForPodReady(ctx context.Context, namespace, podName string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute) // GKE Autopilot can be slow
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	lastStatus := ""

	fmt.Printf("Waiting for pod '%s' to be ready...\n", podName)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for pod to be ready: %w", ctx.Err())
		case <-ticker.C:
			pod, err := r.Client.Clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				return err
			}

			// Check container statuses for more detail
			var containerStatus *corev1.ContainerStatus
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.Name == "agent" {
					containerStatus = &cs
					break
				}
			}

			statusMsg := string(pod.Status.Phase)
			if containerStatus != nil && containerStatus.State.Waiting != nil {
				statusMsg = fmt.Sprintf("%s (%s)", pod.Status.Phase, containerStatus.State.Waiting.Reason)
			}

			if statusMsg != lastStatus {
				fmt.Printf("  Status: %s\n", statusMsg)
				lastStatus = statusMsg
			}

			// Check for terminal failure reasons in waiting state
			if containerStatus != nil && containerStatus.State.Waiting != nil {
				reason := containerStatus.State.Waiting.Reason
				if reason == "ImagePullBackOff" || reason == "ErrImagePull" || reason == "InvalidImageName" {
					return fmt.Errorf("pod failed to start: %s - %s", reason, containerStatus.State.Waiting.Message)
				}
			}

			if pod.Status.Phase == corev1.PodRunning {
				// Also ensure container is actually running
				if containerStatus != nil && containerStatus.State.Running != nil {
					return nil
				}
			}
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				return fmt.Errorf("pod terminated with status: %s", pod.Status.Phase)
			}
		}
	}
}

func (r *KubernetesRuntime) syncToPod(ctx context.Context, namespace, podName, sourcePath, destPath string) error {
	fmt.Printf("  Preparing tar archive from %s...\n", sourcePath)
	tarCmd := exec.CommandContext(ctx, "tar", "-cz", "-C", sourcePath, ".")
	tarCmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	stdout, err := tarCmd.StdoutPipe()
	if err != nil {
		return err
	}

	if err := tarCmd.Start(); err != nil {
		return err
	}

	// Use sh -c to allow us to ignore certain exit codes if needed, or just to be more flexible.
	// We use -m to avoid utime errors on the mount point.
	remoteCmd := fmt.Sprintf("tar -xz -m --no-same-owner --no-same-permissions -C '%s'", destPath)
	cmd := []string{"sh", "-c", remoteCmd}

	req := r.Client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	option := &corev1.PodExecOptions{
		Command: cmd,
		Stdin:   true,
		Stdout:  true,
		Stderr:  true,
		TTY:     false,
	}

	req.VersionedParams(
		option,
		scheme.ParameterCodec,
	)

	executor, err := remotecommand.NewSPDYExecutor(r.Client.Config, "POST", req.URL())
	if err != nil {
		return err
	}

	fmt.Printf("  Streaming archive to pod '%s' (destination: %s)...\n", podName, destPath)
	var stderr bytes.Buffer
	// We stream to os.Stdout to see if there is any output from tar that helps debugging
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdout,
		Stdout: os.Stdout,
		Stderr: &stderr,
	})

	waitErr := tarCmd.Wait()

	if err != nil {
		// If tar exited with an error, it might be the permission error on .
		// which we want to ignore if the files were actually copied.
		// GNU tar exits with 2 for "fatal errors", which includes the permission error on .
		if strings.Contains(stderr.String(), "Cannot change mode") || strings.Contains(stderr.String(), "Cannot utime") {
			fmt.Printf("  Warning: tar reported permission issues on workspace root, but files may have been synced.\n")
		} else {
			return fmt.Errorf("stream failed: %w (remote stderr: %s)", err, stderr.String())
		}
	}

	if waitErr != nil {
		return fmt.Errorf("local tar failed: %w", waitErr)
	}

	fmt.Printf("  Sync to %s complete.\n", destPath)
	return nil
}

func (r *KubernetesRuntime) syncFromPod(ctx context.Context, namespace, podName, remotePath, localPath string) error {
	if err := os.MkdirAll(localPath, 0755); err != nil {
		return fmt.Errorf("failed to create local workspace directory: %w", err)
	}
	fmt.Printf("  Preparing remote tar archive from %s...\n", remotePath)

	remoteCmd := fmt.Sprintf("tar -cz -C '%s' .", remotePath)
	cmd := []string{"sh", "-c", remoteCmd}

	req := r.Client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	option := &corev1.PodExecOptions{
		Command: cmd,
		Stdin:   false,
		Stdout:  true,
		Stderr:  true,
		TTY:     false,
	}

	req.VersionedParams(
		option,
		scheme.ParameterCodec,
	)

	executor, err := remotecommand.NewSPDYExecutor(r.Client.Config, "POST", req.URL())
	if err != nil {
		return err
	}

	// Prepare local tar
	tarCmd := exec.CommandContext(ctx, "tar", "-xz", "-m", "-C", localPath)
	tarCmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	stdin, err := tarCmd.StdinPipe()
	if err != nil {
		return err
	}

	if err := tarCmd.Start(); err != nil {
		return err
	}

	fmt.Printf("  Streaming archive from pod '%s' (destination: %s)...\n", podName, localPath)
	var stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: stdin,
		Stderr: &stderr,
	})

	// Close stdin to tell local tar that stream is finished
	stdin.Close()
	waitErr := tarCmd.Wait()

	if err != nil {
		return fmt.Errorf("stream failed: %w (remote stderr: %s)", err, stderr.String())
	}

	if waitErr != nil {
		return fmt.Errorf("local tar failed: %w", waitErr)
	}

	fmt.Printf("  Sync from %s complete.\n", remotePath)
	return nil
}

func (r *KubernetesRuntime) Stop(ctx context.Context, id string) error {
	return r.Delete(ctx, id)
}

func (r *KubernetesRuntime) Delete(ctx context.Context, id string) error {
	// Terminate Mutagen Sync if exists
	if mutagen.CheckInstalled() {
		// We use the label selector we applied during creation
		_ = mutagen.TerminateSync(fmt.Sprintf("scion-agent=%s", id))
	}

	namespace := r.DefaultNamespace
	// 'id' is the pod name
	err := r.Client.Clientset.CoreV1().Pods(namespace).Delete(ctx, id, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete pod: %w", err)
	}
	return nil
}

func (r *KubernetesRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	namespace := r.DefaultNamespace

	var selector string
	if len(labelFilter) > 0 {
		var selectors []string
		for k, v := range labelFilter {
			selectors = append(selectors, fmt.Sprintf("%s=%s", k, v))
		}
		selector = strings.Join(selectors, ",")
	} else {
		// Default to filtering for scion agents if no specific filter is provided
		selector = "scion.name"
	}

	pods, err := r.Client.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, err
	}

	var agents []api.AgentInfo
	for _, p := range pods.Items {
		// We already filtered by selector, but we still double check if scion.name is present
		// just in case the selector logic changes or is broader.
		if _, ok := p.Labels["scion.name"]; !ok {
			continue
		}

		status := string(p.Status.Phase)
		agentStatus := ""
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			agentStatus = "ended"
		}

		// Try to get more detail from container status
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == "agent" {
				if cs.State.Waiting != nil {
					status = fmt.Sprintf("%s (%s)", p.Status.Phase, cs.State.Waiting.Reason)
				} else if cs.State.Terminated != nil {
					status = fmt.Sprintf("%s (%s)", p.Status.Phase, cs.State.Terminated.Reason)
					if agentStatus == "" {
						agentStatus = "ended"
					}
				}
				break
			}
		}

		grovePath := p.Annotations["scion.grove_path"]
		if grovePath == "" {
			grovePath = p.Labels["scion.grove_path"]
		}

		agents = append(agents, api.AgentInfo{
			ID:              p.Name,
			Name:            p.Labels["scion.name"],
			Template:        p.Labels["scion.template"],
			Grove:           p.Labels["scion.grove"],
			GrovePath:       grovePath,
			Labels:          p.Labels,
			Annotations:     p.Annotations,
			ContainerStatus: status,
			Status:          agentStatus,
			Image:           p.Spec.Containers[0].Image,
			Runtime:         r.Name(),
		})
	}
	return agents, nil
}

func (r *KubernetesRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	namespace := r.DefaultNamespace
	podName := id // id is now pod name

	req := r.Client.Clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{})
	podLogs, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer podLogs.Close()

	data, err := io.ReadAll(podLogs)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func (r *KubernetesRuntime) Attach(ctx context.Context, id string) error {
	namespace := r.DefaultNamespace
	podName := id

	// Find pod first to check status
	agents, err := r.List(ctx, map[string]string{"scion.name": id})
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	var agent *api.AgentInfo
	for _, a := range agents {
		if a.ID == id || a.Name == id {
			agent = &a
			break
		}
	}

	if agent == nil {
		return fmt.Errorf("agent '%s' pod not found. It may have been deleted.", id)
	}

	// For Kubernetes, we want to ensure it is in Running phase
	if !strings.EqualFold(agent.ContainerStatus, string(corev1.PodRunning)) {
		return fmt.Errorf("agent '%s' is not running (status: %s). Use 'scion start %s' to resume it.", id, agent.ContainerStatus, id)
	}

	fmt.Printf("Attaching to pod '%s' (use Ctrl-P, Ctrl-Q to detach)...\n", podName)

	req := r.Client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("attach")

	option := &corev1.PodAttachOptions{
		Container: "agent",
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       true,
	}

	req.VersionedParams(
		option,
		scheme.ParameterCodec,
	)

	executor, err := remotecommand.NewSPDYExecutor(r.Client.Config, "POST", req.URL())
	if err != nil {
		return err
	}

	// Put the terminal into raw mode to support TUI interactions
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		oldState, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("failed to set raw mode: %w", err)
		}
		defer term.Restore(fd, oldState)
	}

	// Create a context that can be canceled by our detach sequence
	attachCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Setup terminal resizing support
	sizeQueue := &terminalSizeQueue{
		resizeChan: make(chan remotecommand.TerminalSize, 1),
	}

	// Initial size
	if w, h, err := term.GetSize(fd); err == nil {
		sizeQueue.resizeChan <- remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}
	}

	// Monitor for resize signals (SIGWINCH)
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGWINCH)
	go func() {
		for {
			select {
			case <-sigChan:
				if w, h, err := term.GetSize(fd); err == nil {
					sizeQueue.resizeChan <- remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}
				}
			case <-attachCtx.Done():
				return
			}
		}
	}()
	defer signal.Stop(sigChan)

	// Wrap stdin with a reader that looks for the detach sequence
	stdin := &detachReader{
		reader: os.Stdin,
		cancel: cancel,
	}

	// Trigger a "resize dance" to force TUI redraw. Some TUIs only redraw
	// when they receive a SIGWINCH where the dimensions actually change.
	go func() {
		// Wait for the SPDY stream to be fully established
		time.Sleep(500 * time.Millisecond)
		if w, h, err := term.GetSize(fd); err == nil {
			// 1. Send slightly modified size
			sizeQueue.resizeChan <- remotecommand.TerminalSize{Width: uint16(w - 1), Height: uint16(h)}
			time.Sleep(100 * time.Millisecond)
			// 2. Restore original size
			sizeQueue.resizeChan <- remotecommand.TerminalSize{Width: uint16(w), Height: uint16(h)}
		}
	}()

	err = executor.StreamWithContext(attachCtx, remotecommand.StreamOptions{
		Stdin:             stdin,
		Stdout:            os.Stdout,
		Stderr:            os.Stderr,
		Tty:               true,
		TerminalSizeQueue: sizeQueue,
	})

	if err != nil {
		// Suppress "context canceled" error when it's the result of a deliberate detach
		if errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "context canceled") {
			return nil
		}
		// Also ignore EOF which can happen on clean detach
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

// terminalSizeQueue implements remotecommand.TerminalSizeQueue
type terminalSizeQueue struct {
	resizeChan chan remotecommand.TerminalSize
}

func (t *terminalSizeQueue) Next() *remotecommand.TerminalSize {
	size, ok := <-t.resizeChan
	if !ok {
		return nil
	}
	return &size
}

// detachReader wraps an io.Reader to look for the Ctrl-P, Ctrl-Q sequence
type detachReader struct {
	reader io.Reader
	cancel context.CancelFunc
	state  int // 0: normal, 1: saw Ctrl-P
}

func (r *detachReader) Read(p []byte) (int, error) {
	for {
		n, err := r.reader.Read(p)
		if err != nil {
			return n, err
		}
		if n == 0 {
			continue
		}

		outIdx := 0
		for i := 0; i < n; i++ {
			b := p[i]
			if r.state == 1 {
				if b == 0x11 { // Ctrl-Q
					fmt.Print("\r\nDetached from agent.\r\n")
					r.cancel()
					return outIdx, io.EOF
				}
				// Not part of the sequence, we should ideally re-insert the Ctrl-P (0x10)
				// but for simplicity in this byte-by-byte fix, we just reset.
				// Most TUIs will handle a missing Ctrl-P better than a hang.
				r.state = 0
			}

			if b == 0x10 { // Ctrl-P
				r.state = 1
				continue
			}

			p[outIdx] = b
			outIdx++
		}

		if outIdx > 0 {
			return outIdx, nil
		}
		// If we consumed everything (like just a Ctrl-P), loop to read more
	}
}

func (r *KubernetesRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	// K8s pulls images if not present, so we can assume it "exists" or will be pulled.
	// Implementing a strict check would require querying the node or registry which is complex here.
	return true, nil
}

func (r *KubernetesRuntime) PullImage(ctx context.Context, image string) error {
	// Not strictly needed as Pod creation handles pulling.
	return nil
}

func (r *KubernetesRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {
	// Find pod first
	agents, err := r.List(ctx, map[string]string{"scion.name": id})
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	var agent *api.AgentInfo
	for _, a := range agents {
		if a.ID == id || a.Name == id {
			agent = &a
			break
		}
	}

	if agent == nil {
		return fmt.Errorf("agent '%s' pod not found", id)
	}

	workspacePath := agent.Annotations["scion.workspace"]
	if workspacePath == "" {
		return fmt.Errorf("agent '%s' does not have a workspace path recorded", id)
	}

	// Resolve namespace
	namespace := r.DefaultNamespace
	if ns, ok := agent.Labels["scion.namespace"]; ok {
		namespace = ns
	} else if ns, ok := agent.Labels["namespace"]; ok {
		namespace = ns
	}

	if r.SyncMode == "mutagen" {
		if !mutagen.CheckInstalled() {
			return fmt.Errorf("mutagen not installed but sync mode is mutagen")
		}
		// Check if sync exists
		syncName := "scion-" + agent.ID
		if err := mutagen.WaitForSync(syncName, 1*time.Second); err == nil {
			fmt.Println("Mutagen sync is already active.")
			return nil
		}

		// Try to recreate if missing
		fmt.Println("Mutagen sync not found. Creating...")
		if err := mutagen.StartDaemon(); err != nil {
			return fmt.Errorf("failed to start mutagen daemon: %w", err)
		}

		// Clean up any existing session for this agent to avoid name collisions
		_ = mutagen.TerminateSync("scion-agent=" + agent.ID)

		remoteURL := fmt.Sprintf("kubernetes://%s/%s/%s/agent:/workspace",
			r.Client.CurrentContext, namespace, agent.ID)

		err = mutagen.CreateSync(
			syncName,
			workspacePath,
			remoteURL,
			map[string]string{"scion-agent": agent.ID},
		)
		if err != nil {
			return fmt.Errorf("failed to create mutagen sync: %w", err)
		}
		fmt.Println("Mutagen sync created.")
		return nil
	}

	// Default to tar sync (Snapshot)
	if direction == SyncUnspecified {
		return fmt.Errorf("direction (to or from) must be specified for tar sync. Example: scion sync to %s", agent.ID)
	}

	if direction == SyncFrom {
		fmt.Printf("Syncing workspace (agent -> %s)...\n", workspacePath)
		return r.syncFromPod(ctx, namespace, agent.ID, "/workspace", workspacePath)
	}

	fmt.Printf("Syncing workspace (%s -> agent)...\n", workspacePath)
	return r.syncToPod(ctx, namespace, agent.ID, workspacePath, "/workspace")
}
