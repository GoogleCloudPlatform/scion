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

package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/google/uuid"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

type KubernetesRuntime struct {
	Client            *k8s.Client
	DefaultNamespace  string
	GKEMode           bool // Enables GKE-specific features (SecretProviderClass CSI, GCS FUSE)
	GKEAutoDetected   bool // True when GKE was auto-detected (enables Autopilot tolerance only)
	ListAllNamespaces bool // When true, List() queries all namespaces for scion pods

	// execReadyClock supplies the time source and sleep function used by
	// waitForExecReady's backoff loop. It defaults to real time via
	// NewKubernetesRuntime; tests override it to drive the retry/backoff/cap
	// logic deterministically without a real sleep.
	execReadyClock execReadyClock

	// execProbe is the readiness probe waitForExecReady runs on each
	// attempt. Nil (the default from NewKubernetesRuntime) means "exec a
	// cheap no-op command via execInPod"; tests replace it with a func that
	// blocks on its ctx argument to exercise the per-probe timeout without a
	// real exec transport.
	execProbe execProbeFunc

	// podExec, when set, replaces execInPod's exec transport. Tests use it
	// to record the in-pod commands Run issues without a real API server.
	podExec func(ctx context.Context, namespace, podName string, cmd []string) (string, error)

	// homeSync, when set, replaces the home directory copy Run performs
	// (syncToPod). Tests use it to observe the order of the start steps.
	homeSync func(ctx context.Context, namespace, podName, sourcePath, destPath string, excludes []string) error

	// syncTransfer, when set, replaces the copies Sync performs (toPod is
	// true for broker to pod). Tests use it to observe which paths Sync
	// transfers.
	syncTransfer func(ctx context.Context, namespace, podName, src, dest string, toPod bool) error

	// PriorityClassName is the runtime-level default spec.priorityClassName
	// applied to agent pods (settings runtimes.<name>.priority_class_name).
	// An explicit per-template/agent kubernetes.priorityClassName overrides
	// this in buildPod. Empty means no priority class is set (today's
	// behaviour).
	PriorityClassName string
}

// agentContainerName is the name of the primary scion agent container in
// every pod we create. It must be specified on every PodExec call so that
// admission-controller-injected sidecars (Istio, Linkerd, Dynatrace, etc.)
// do not cause the API server to reject the exec request with an
// "a container name must be specified" error.
const agentContainerName = "agent"

var serviceAccountNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

func NewKubernetesRuntime(client *k8s.Client) *KubernetesRuntime {
	return &KubernetesRuntime{
		Client:           client,
		DefaultNamespace: defaultKubernetesNamespace(),
		execReadyClock:   realExecReadyClock(),
	}
}

// DefaultKubernetesNamespace returns the namespace a Kubernetes runtime
// resolves to when a profile sets no explicit namespace: the same
// cheap, local-only chain NewKubernetesRuntime uses (SCION_K8S_NAMESPACE,
// POD_NAMESPACE, the in-cluster service account file, else "default").
// Exported so callers that need this fallback without constructing a runtime
// — e.g. runtimebroker comparing an unresolved profile's effective namespace
// against the broker's own default before deciding whether to fully resolve
// it — don't have to duplicate the chain.
func DefaultKubernetesNamespace() string {
	return defaultKubernetesNamespace()
}

func defaultKubernetesNamespace() string {
	for _, envKey := range []string{"SCION_K8S_NAMESPACE", "POD_NAMESPACE"} {
		if ns := strings.TrimSpace(os.Getenv(envKey)); ns != "" {
			return ns
		}
	}

	if data, err := os.ReadFile(serviceAccountNamespacePath); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}

	return "default"
}

// isGKEScheduling returns true when GKE Autopilot scheduling tolerance
// should be applied — either via explicit GKEMode or auto-detection.
func (r *KubernetesRuntime) isGKEScheduling() bool {
	return r.GKEMode || r.GKEAutoDetected
}

func (r *KubernetesRuntime) Name() string {
	return "kubernetes"
}

func (r *KubernetesRuntime) ExecUser() string {
	return "scion"
}

// resolveNamespace determines the namespace for a pod by looking up the
// scion.namespace annotation on the pod itself. Falls back to DefaultNamespace
// if the pod is not found or has no annotation.
func (r *KubernetesRuntime) resolveNamespace(ctx context.Context, podName string) string {
	// Try to find the pod in the default namespace first
	pod, err := r.Client.Clientset.CoreV1().Pods(r.DefaultNamespace).Get(ctx, podName, metav1.GetOptions{})
	if err == nil {
		if ns, ok := pod.Annotations["scion.namespace"]; ok && ns != "" {
			return ns
		}
		return r.DefaultNamespace
	}

	// If ListAllNamespaces is enabled, search across all namespaces
	if r.ListAllNamespaces {
		pods, err := r.Client.Clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("scion.name=%s", podName),
		})
		if err == nil {
			for _, p := range pods.Items {
				if p.Name == podName {
					return p.Namespace
				}
			}
		}
	}

	return r.DefaultNamespace
}

// parseResourceSafe parses a Kubernetes resource quantity string, returning a
// user-friendly error instead of panicking like resource.MustParse.
func parseResourceSafe(value, fieldName string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return q, fmt.Errorf("invalid %s resource value %q: %w", fieldName, value, err)
	}
	return q, nil
}

// syncMaxRetries is the maximum number of retry attempts for sync operations.
const syncMaxRetries = 3

// syncWithRetry wraps a sync operation with exponential backoff retry for
// transient errors (connection resets, stream interruptions).
func (r *KubernetesRuntime) syncWithRetry(ctx context.Context, op func() error) error {
	var lastErr error
	for attempt := 0; attempt <= syncMaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second // 1s, 2s, 4s
			runtimeLog.Warn("Sync attempt failed, retrying",
				"attempt", attempt, "max_retries", syncMaxRetries,
				"backoff", backoff, "error", lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		lastErr = op()
		if lastErr == nil {
			return nil
		}
		// Only retry on transient errors
		if !isSyncTransientError(lastErr) {
			return lastErr
		}
	}
	return fmt.Errorf("sync failed after %d retries: %w", syncMaxRetries, lastErr)
}

// isSyncTransientError returns true if the error is likely transient and
// the sync operation should be retried.
func isSyncTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	transientPatterns := []string{
		"connection reset",
		"connection refused",
		"broken pipe",
		"stream error",
		"EOF",
		"timeout",
		"i/o timeout",
		"TLS handshake",
		"use of closed network connection",
		// F15: a scale-from-zero node's API-server-to-kubelet exec tunnel
		// (e.g. the GKE konnectivity agent) is not up yet when the first
		// exec lands right after the container starts. Observed as
		// "stream failed: error dialing backend: No agent available".
		// Both substrings are matched independently so either wording
		// variant of the same underlying condition is caught.
		"no agent available",
		"error dialing backend",
	}
	for _, pattern := range transientPatterns {
		if strings.Contains(strings.ToLower(msg), strings.ToLower(pattern)) {
			return true
		}
	}
	return false
}

// execReadyMaxWait bounds the cumulative elapsed time (wall time between
// probe attempts, including time spent inside each probe) that
// waitForExecReady spends retrying the exec-tunnel probe before giving up.
// A single in-flight probe is bounded separately by execReadyProbeTimeout,
// so the actual wall-clock wait is execReadyMaxWait plus at most one more
// execReadyProbeTimeout for the final, cap-triggering probe.
const execReadyMaxWait = 90 * time.Second

// execReadyMaxBackoff caps the exponential backoff between probes.
const execReadyMaxBackoff = 8 * time.Second

// execReadyMaxBackoffShift is the largest exponent execReadyWithRetry's
// backoff computation will shift by (1<<3 == 8, matching execReadyMaxBackoff
// in seconds). Capping the shift itself, not just the resulting duration,
// keeps the computation well away from any shift-count edge cases for a
// larger-than-expected attempt count.
const execReadyMaxBackoffShift = 3

// execReadyProbeTimeout bounds a single readiness probe so one stalled dial
// (e.g. a tunnel that accepts the connection and then hangs) cannot hold
// waitForExecReady well past execReadyMaxWait. A probe that hits this
// per-attempt deadline is treated the same as any other transient probe
// failure and retried under the usual backoff and cap — see
// wrapProbeTimeout. A var, not a const, so tests can shrink it to run the
// per-probe-timeout path in milliseconds instead of execReadyProbeTimeout's
// production value.
var execReadyProbeTimeout = 10 * time.Second

// execReadyClock supplies the time source and sleep function used by the
// exec-readiness retry loop. Production code gets this from
// NewKubernetesRuntime; tests replace it with a fake clock so the loop's
// cap and backoff behavior can be exercised without a real sleep.
type execReadyClock struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// realExecReadyClock returns the production clock: wall time plus a sleep
// that honors context cancellation.
func realExecReadyClock() execReadyClock {
	return execReadyClock{
		now: time.Now,
		sleep: func(ctx context.Context, d time.Duration) error {
			// time.NewTimer plus an explicit Stop, rather than time.After,
			// so an early ctx cancellation doesn't leave the timer running
			// (and ineligible for GC) until it fires on its own.
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	}
}

// wrapProbeTimeout rewrites err so that a deadline caused by the probe's own
// execReadyProbeTimeout (not the caller's ctx) reads as a timeout. This
// matters because isSyncTransientError recognizes "timeout" but not Go's
// plain "context deadline exceeded", so without the rewrite a stalled probe
// would be misclassified as non-transient and fail the whole wait instead
// of being retried. When ctx itself is already done, err is returned
// unchanged — that case should propagate (and the next clock.sleep call
// will return ctx.Err() to end the loop) rather than being retried forever.
func wrapProbeTimeout(ctx, probeCtx context.Context, err error) error {
	if err == nil || ctx.Err() != nil || !errors.Is(probeCtx.Err(), context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("exec readiness probe timeout after %s: %w", execReadyProbeTimeout, err)
}

// execProbeFunc is the shape of the readiness probe waitForExecReady runs on
// each attempt. The production default execs a cheap no-op command; tests
// replace KubernetesRuntime.execProbe with a func that blocks on ctx so the
// per-probe timeout (execReadyProbeTimeout, applied by the caller via
// context.WithTimeout before invoking the probe) can be exercised without a
// real exec transport.
type execProbeFunc func(ctx context.Context, namespace, podName string) error

// waitForExecReady probes the pod's exec tunnel with a cheap no-op command
// until it succeeds, a non-transient error occurs, ctx is cancelled, or
// execReadyMaxWait of cumulative elapsed time passes (see
// execReadyProbeTimeout for the per-probe bound). A pod and its main
// container reporting Running (waitForPodReady) does not mean the
// API-server-to-kubelet exec tunnel is ready: on a scale-from-zero node the
// tunnel can still be coming up for tens of seconds afterward, and the
// first real exec (home sync, or the startup-gate touch when HomeDir is
// unset) would otherwise fail outright. Call this once, right after
// waitForPodReady and before any other exec into the pod. agentName is used
// only for log correlation.
func (r *KubernetesRuntime) waitForExecReady(ctx context.Context, namespace, podName, agentName string) error {
	probe := r.execProbe
	if probe == nil {
		probe = func(ctx context.Context, namespace, podName string) error {
			_, err := r.execInPod(ctx, namespace, podName, []string{"true"})
			return err
		}
	}
	return r.execReadyWithRetry(ctx, func() error {
		probeCtx, cancel := context.WithTimeout(ctx, execReadyProbeTimeout)
		defer cancel()
		return wrapProbeTimeout(ctx, probeCtx, probe(probeCtx, namespace, podName))
	}, agentName, namespace, podName)
}

// execReadyWithRetry runs op with bounded exponential backoff (1s, 2s, 4s,
// 8s, capped thereafter at execReadyMaxBackoff), retrying only errors
// isSyncTransientError classifies as transient, until op succeeds, a
// non-transient error is returned (fail fast, no retry), ctx is cancelled,
// or execReadyMaxWait of cumulative wait time is exhausted. Factored out
// from waitForExecReady so tests can drive it with a test-supplied op and a
// fake clock instead of a real exec transport and real sleeps. agentName,
// namespace and podName are log correlation fields only.
func (r *KubernetesRuntime) execReadyWithRetry(ctx context.Context, op func() error, agentName, namespace, podName string) error {
	clock := r.execReadyClock
	if clock.now == nil || clock.sleep == nil {
		// A KubernetesRuntime built as a literal rather than via
		// NewKubernetesRuntime has a zero-value execReadyClock; fall back to
		// the real one instead of a nil-func panic.
		clock = realExecReadyClock()
	}
	start := clock.now()
	logFields := func(extra ...any) []any {
		base := []any{"phase", "wait-exec", "agent", agentName, "namespace", namespace, "pod", podName}
		return append(base, extra...)
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		lastErr = op()
		if lastErr == nil {
			runtimeLog.Info("Pod exec tunnel ready",
				logFields("attempts", attempt+1, "elapsed_ms", clock.now().Sub(start).Milliseconds())...)
			return nil
		}
		if !isSyncTransientError(lastErr) {
			return lastErr
		}

		elapsed := clock.now().Sub(start)
		if elapsed >= execReadyMaxWait {
			return fmt.Errorf("gave up after %s (%d attempts): %w",
				elapsed.Round(time.Second), attempt+1, lastErr)
		}

		// Exponential backoff: 1s, 2s, 4s, 8s, then holds at
		// execReadyMaxBackoff. The shift itself is capped (not just the
		// resulting duration, after the fact) so a surprisingly large
		// attempt count — the elapsed check above is what actually keeps
		// attempt small in practice, so this is a second, independent
		// guard — can't compute a degenerate (zero or, pre-Go's
		// well-defined large-shift semantics, negative-looking) backoff
		// instead of simply holding at the cap.
		shift := attempt
		if shift > execReadyMaxBackoffShift {
			shift = execReadyMaxBackoffShift
		}
		backoff := time.Duration(1<<uint(shift)) * time.Second
		if backoff > execReadyMaxBackoff {
			backoff = execReadyMaxBackoff
		}
		if remaining := execReadyMaxWait - elapsed; backoff > remaining {
			backoff = remaining
		}
		runtimeLog.Warn("Pod exec tunnel not ready, retrying",
			logFields("attempt", attempt+1, "backoff", backoff, "elapsed_ms", elapsed.Milliseconds(), "error", lastErr)...)
		if err := clock.sleep(ctx, backoff); err != nil {
			return err
		}
	}
}

func ensureAnnotations(annotations map[string]string) map[string]string {
	if annotations != nil {
		return annotations
	}
	return make(map[string]string)
}

// chownRecursiveArgs returns the in-pod `chown -R` argv that recursively
// re-owns path to owner:owner, or (nil, false) when owner is empty. An empty
// owner must refuse outright rather than produce a command either caller
// could otherwise build from it: an owner:group spec of ":" (no real target
// user), or, for the home-directory caller, a destination path of
// util.GetHomeDir("") == "/home" -- a critical system directory, not any
// particular user's home.
//
// This returns argv directly, not a shell string, so execInPod runs chown
// itself rather than a shell asked to parse one: owner and path reach the
// process as literal argv elements, with no quoting step in between for
// either value to escape.
func chownRecursiveArgs(owner, path string) (args []string, ok bool) {
	if owner == "" {
		return nil, false
	}
	return []string{"chown", "-R", fmt.Sprintf("%s:%s", owner, owner), path}, true
}

// descriptiveLabels are display-only labels: nothing selects or identifies
// objects by them. Their values come from names (template, harness-config,
// auth method) that are not guaranteed to be valid Kubernetes label values.
var descriptiveLabels = map[string]bool{
	"scion.template":       true,
	"scion.harness_config": true,
	"scion.harness_auth":   true,
}

// filterDescriptiveLabels returns a copy of labels without any descriptive
// label (see descriptiveLabels) whose value is not a valid Kubernetes label
// value; each dropped label is logged as a warning. All other labels,
// including identity labels such as scion.name, the run ID, the start ID,
// agent_id and the project labels, are copied unchanged, so an invalid
// identity value still fails object creation.
func filterDescriptiveLabels(agentName string, labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		if descriptiveLabels[k] {
			if errs := k8svalidation.IsValidLabelValue(v); len(errs) > 0 {
				runtimeLog.Warn("Dropping descriptive label with a value that is not a valid Kubernetes label value",
					"agent", agentName, "label", k, "value", v, "reason", strings.Join(errs, "; "))
				continue
			}
		}
		out[k] = v
	}
	return out
}

func (r *KubernetesRuntime) Run(ctx context.Context, config RunConfig) (podName string, err error) {
	fmt.Printf("Starting agent '%s' on Kubernetes...\n", config.Name)
	namespace := r.runNamespace(config.Labels)

	if config.Name == "" {
		config.Name = fmt.Sprintf("scion-%d", time.Now().UnixNano())
	}

	// Stamp every object this start creates (per-agent Secrets, the
	// SecretProviderClass and the pod, all of which copy scion.* labels from
	// config.Labels) with a per-start ID, so that the cleanup below removes
	// only this start's own objects and never those of a newer agent that has
	// since been created with the same name. The label map is copied so the
	// caller's map is not modified.
	//
	// The copy also drops descriptive labels whose value is not a valid
	// Kubernetes label value (see filterDescriptiveLabels), so that a display
	// value cannot fail creation of these objects.
	startID := uuid.NewString()
	labels := filterDescriptiveLabels(config.Name, config.Labels)
	labels[labelStartID] = startID
	config.Labels = labels

	// Remove what this start created when it does not complete:
	//   - before the pod exists, any failure removes the Secrets and
	//     SecretProviderClass created so far (nothing can use them);
	//   - once the pod exists, a start whose context is done (cancelled by a
	//     delete or stop of the agent, or past its deadline) also removes the
	//     pod, since nothing will ever record it as a running agent. Other
	//     failures after the pod exists keep it, as before, so its status and
	//     logs stay available until the agent is deleted.
	// The cleanup runs on a fresh context because ctx may already be done.
	// cleanupArmed is set just before the first object is created, so early
	// validation failures make no API calls.
	//
	// An async launch (launch hooks set, design t1-async-create-v11.md
	// §3.8.4) skips this cleanup: its objects are removed by the launch's
	// own cleanup from the handles reported below, and only when the hub's
	// answer calls for it (a superseded launch must not delete anything).
	// Keeping one cleanup owner per path avoids this cleanup deleting
	// objects the hub has assigned to a newer launch.
	//
	// The hooks are a checkpoint immediately before each resource-creating
	// or name-based deleting call, and the created object's UID reported
	// after each true create. They are no-ops on the synchronous path.
	hooks := config.launchHooks()
	cleanupArmed := false
	podCreated := false
	defer func() {
		if err == nil || !cleanupArmed || hooks.active() {
			return
		}
		abandoned := ctx.Err() != nil
		if podCreated && !abandoned {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), startCleanupTimeout)
		defer cancel()
		r.cleanupStartResources(cleanupCtx, namespace, config.Name, startID, abandoned)
	}()

	// For non-git environments, Workspace might be empty but we might have it as a volume mount
	if config.Workspace == "" {
		for _, v := range config.Volumes {
			if v.Target == "/workspace" {
				config.Workspace = v.Source
				break
			}
		}
	}

	// Reject a workspace source that is not an allowed workspace path before
	// it is persisted to the pod annotation that both this function's own
	// sync step and the standalone Sync() command later trust. Act on the
	// resolved, symlink-free path it returns. As in buildCommonRunArgs, no
	// per-project root is passed: a workspace outside RepoRoot is a
	// supported shape (an explicit --workspace, in particular) that
	// RunConfig carries no flag to distinguish from a bad value, so only
	// the fixed deny-set applies here; per-project root containment is
	// enforced upstream, at the pkg/agent Start() call site.
	if config.Workspace != "" {
		resolvedWorkspace, err := ValidateWorkspaceSource(config.Workspace, "")
		if err != nil {
			return "", err
		}
		config.Workspace = resolvedWorkspace
	}

	// Persist workspace path in annotations for later sync
	if config.Workspace != "" {
		config.Annotations = ensureAnnotations(config.Annotations)
		config.Annotations["scion.workspace"] = config.Workspace
	}

	if config.GitClone != nil {
		config.Annotations = ensureAnnotations(config.Annotations)
		config.Annotations["scion.git_clone"] = "true"
		config.Annotations["scion.git_clone_url"] = config.GitClone.URL
	}

	// Same reasoning as config.Workspace above: reject a home directory that
	// is not an allowed agent-home path before it is persisted to the pod
	// annotation that Sync() later trusts, and before it is copied into the
	// pod below -- otherwise an unacceptable value would reach the pod on
	// this first Run and only be caught afterward, on the next Sync.
	if config.HomeDir != "" {
		resolvedHomeDir, err := ValidateAgentHomeSource(config.HomeDir, "")
		if err != nil {
			return "", err
		}
		config.HomeDir = resolvedHomeDir
	}

	if config.HomeDir != "" {
		config.Annotations = ensureAnnotations(config.Annotations)
		config.Annotations["scion.homedir"] = config.HomeDir
		config.Annotations["scion.username"] = config.UnixUsername
	}

	// Persist the resolved namespace as an annotation for lifecycle operations
	config.Annotations = ensureAnnotations(config.Annotations)
	config.Annotations["scion.namespace"] = namespace

	// preCreateStart times the pre-create API calls below (secret/SPC
	// cleanup and creation, PVC creation) as one aggregate, for start-time
	// attribution. It does not change what any of these calls do.
	preCreateStart := time.Now()

	// Pre-clean stale resources from a previous agent with the same name.
	// This handles cases where the agent was force-deleted from the hub
	// or the pod was evicted/GC'd by K8s without proper cleanup. These
	// deletes are by name, so an async launch checkpoints once before them:
	// a launch the hub has already ended must not remove a newer launch's
	// same-named objects.
	//
	// A start carrying a run ID removes only what no live run of another ID
	// still holds, and fails with ErrRunConflict rather than delete a live
	// pod of another run (see preCleanForRun).
	if err := hooks.checkpoint(ctx, CheckpointStepPreClean); err != nil {
		return "", err
	}
	// NFS-home agents: one start at a time per agent, across brokers that
	// share the store, from before the previous pod is removed until the new
	// pod exists, so two pods never write to the same home.
	nfsHomeStart, err := nfsHomePod(config)
	if err != nil {
		return "", err
	}
	releaseHomeLock := func() {}
	if nfsHomeStart {
		release, err := acquireHomeStartLock(ctx, config)
		if err != nil {
			return "", err
		}
		releaseHomeLock = release
	}
	defer func() { releaseHomeLock() }()

	if runID := config.Labels[api.LabelRunID]; runID != "" {
		// Run-scoped pre-clean (ptone/scion#2550), with the same NFS-home
		// ordering and termination wait as the name-based path below.
		if err := r.preCleanForRun(ctx, namespace, config.Name, runID, nfsHomeStart, config.HomeStorage); err != nil {
			return "", err
		}
	} else {
		// An NFS-home start removes the previous pod first and its secrets
		// only once the pod is confirmed stopped, so a pod still shutting
		// down keeps the secrets it mounted.
		if !nfsHomeStart {
			r.cleanupAgentSecrets(ctx, namespace, config.Name)
		}
		if err := r.cleanupStalePod(ctx, namespace, config.Name, nfsHomeStart, config.HomeStorage); err != nil {
			return "", err
		}
		if nfsHomeStart {
			r.cleanupAgentSecrets(ctx, namespace, config.Name)
		}
	}
	cleanupArmed = true

	// The hub transport credential is delivered through the per-agent Secret
	// (secretKeyRef) rather than as a plain pod env value.
	config.Env, config.ResolvedSecrets = divertTransportCredential(config.Env, config.ResolvedSecrets)

	// Create K8s Secret or SecretProviderClass before the pod
	if len(config.ResolvedSecrets) > 0 {
		useGKEPath := r.GKEMode
		if useGKEPath {
			hasRef := false
			for _, s := range config.ResolvedSecrets {
				if s.Ref != "" {
					hasRef = true
					break
				}
			}
			useGKEPath = hasRef
		}

		if useGKEPath {
			if _, err := r.createSecretProviderClassWithHooks(ctx, namespace, config.Name, config.ResolvedSecrets, config.Labels, hooks); err != nil {
				return "", fmt.Errorf("failed to create SecretProviderClass: %w", err)
			}
			// GKE hybrid path: the managed SM add-on lacks RBAC to sync
			// secretObjects into K8s Secrets, so we create a K8s Secret
			// directly for environment-type secrets (env vars need
			// secretKeyRef). File-type secrets are served by the CSI mount
			// and must NOT be duplicated into etcd.
			var envSecrets []api.ResolvedSecret
			for _, s := range config.ResolvedSecrets {
				if s.Type != "file" {
					envSecrets = append(envSecrets, s)
				}
			}
			if len(envSecrets) > 0 {
				if _, err := r.createAgentSecretWithHooks(ctx, namespace, config.Name, envSecrets, config.Labels, hooks); err != nil {
					return "", fmt.Errorf("failed to create agent secret for env vars: %w", err)
				}
			}
		} else {
			if _, err := r.createAgentSecretWithHooks(ctx, namespace, config.Name, config.ResolvedSecrets, config.Labels, hooks); err != nil {
				return "", fmt.Errorf("failed to launch container: %w", err)
			}
		}
	}

	// Create K8s Secret for ResolvedAuth files (portable alternative to hostPath).
	if config.ResolvedAuth != nil && len(config.ResolvedAuth.Files) > 0 {
		if err := r.createAuthFileSecretWithHooks(ctx, namespace, config.Name, config.ResolvedAuth.Files, config.Labels, hooks); err != nil {
			return "", fmt.Errorf("failed to create auth file secret: %w", err)
		}
	}

	// Create PVCs for shared directories (project-scoped, reused across agents)
	if len(config.SharedDirs) > 0 {
		if err := r.createSharedDirPVCs(ctx, namespace, config); err != nil {
			return "", fmt.Errorf("failed to create shared dir PVCs: %w", err)
		}
	}

	runtimeLog.Info("Pre-create setup complete", "agent", config.Name, "namespace", namespace,
		"phase", "pre-create", "elapsed_ms", time.Since(preCreateStart).Milliseconds())

	// The pod create's checkpoint comes before the NFS provisioning lock
	// below, not inside it: it can block for as long as the launch deadline
	// while the Hub is unreachable, and other agents of the project would
	// wait on the lock meanwhile. Between here and the pod create there is
	// only local work (the lock and the pod spec), no API object creates.
	if err := hooks.checkpoint(ctx, CheckpointStepPodCreate); err != nil {
		// The launch is over: create nothing further. The secrets this
		// launch created are removed by its cleanup, by UID.
		return "", err
	}

	// --- N2-2b: Per-project advisory lock for NFS init-container provisioning ---
	//
	// When backend=nfs with a bound PV claim, acquire the per-project lock
	// before building the pod spec (F-111: no longer gated on a git clone
	// being configured — see nfsInitContainerInjected). This prevents
	// concurrent first-provision corruption (risk RN1, design §7):
	//   - Lock winner: injects the provisioning init container (existing N2-2 script)
	//   - Lock loser:  injects a wait-for-sentinel init container (polls for
	//                  .scion-provisioned without provisioning)
	//
	// The lock is held until waitForPodReady returns (all init containers
	// complete), mirroring N1-4's "hold during clone" lifetime. On error
	// paths the deferred release ensures no lock leak.
	var nfsProvisionLockRelease func() error
	if nfsInitContainerInjected(config) {
		if config.Locker != nil {
			objID := store.StableProjectHash(config.ProjectID)
			acquired, release, err := config.Locker.TryAdvisoryLockObject(
				ctx, store.LockWorkspaceProvision, objID,
			)
			if err != nil {
				return "", fmt.Errorf("NFS provision advisory lock for project %s: %w", config.ProjectID, err)
			}
			nfsProvisionLockRelease = release
			if !acquired {
				// Another node is currently provisioning this project's workspace.
				// buildPod will inject a wait-for-sentinel init container instead
				// of the cloning one.
				config.nfsProvisionLockLost = true
				runtimeLog.Info("NFS provision lock held by another node — pod will wait for sentinel",
					"agent", config.Name, "project_id", config.ProjectID, "phase", "nfs-lock")
			} else {
				runtimeLog.Info("NFS provision lock acquired — pod will clone workspace",
					"agent", config.Name, "project_id", config.ProjectID, "phase", "nfs-lock")
			}
		} else {
			runtimeLog.Warn("No advisory locker available — NFS provisioning is unguarded (sentinel-only)",
				"agent", config.Name, "project_id", config.ProjectID, "phase", "nfs-lock")
		}
	}
	// Deferred release: held through pod creation + waitForPodReady (init
	// containers complete), then released. Safe to call even when nil.
	defer func() {
		if nfsProvisionLockRelease != nil {
			if err := nfsProvisionLockRelease(); err != nil {
				runtimeLog.Error("Failed to release NFS provision lock", "error", err,
					"agent", config.Name, "project_id", config.ProjectID)
			}
		}
	}()

	pod, err := r.buildPod(namespace, config)
	if err != nil {
		return "", fmt.Errorf("failed to build pod spec: %w", err)
	}

	writeK8sRuntimeDebugFile(config, namespace, pod)

	runtimeLog.Info("Creating pod", "agent", config.Name, "namespace", namespace, "image", config.Image, "phase", "pod-create")
	fmt.Printf("  Provisioning pod '%s' in namespace '%s'...\n", config.Name, namespace)
	podCreateStart := time.Now()
	createdPod, err := r.Client.Clientset.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		// The deferred cleanup removes this start's Secrets (an async
		// launch leaves them to its launch cleanup, by recorded UID). If ctx
		// was cancelled while the request was in flight, the pod may exist
		// even though Create reported an error; the cleanup then removes it
		// too, matched by this start's ID.
		return "", fmt.Errorf("failed to create pod: %w", err)
	}
	podCreated = true
	hooks.created(api.ResourceHandle{Kind: api.ResourceKindPod, Namespace: namespace, Name: createdPod.Name, UID: string(createdPod.UID)})
	releaseHomeLock()
	releaseHomeLock = func() {}
	runtimeLog.Info("Pod created", "agent", config.Name, "namespace", namespace,
		"phase", "pod-create", "elapsed_ms", time.Since(podCreateStart).Milliseconds())

	// Wait for Ready
	runtimeLog.Info("Waiting for pod ready", "agent", config.Name, "namespace", namespace, "phase", "wait-schedule")
	if err := r.waitForPodReady(ctx, namespace, createdPod.Name); err != nil {
		return createdPod.Name, err
	}

	// Wait for Running to really mean exec-able before the first exec below
	// (home sync, or the startup-gate touch when HomeDir is unset). This is
	// the only exec call site that runs unconditionally for every pod this
	// function creates — local or NFS backend, fresh create or a restart's
	// stop-then-create — so placing it here once covers all of them. See
	// waitForExecReady's doc comment for why Running alone is insufficient.
	runtimeLog.Info("Waiting for pod exec tunnel", "agent", config.Name, "namespace", namespace, "phase", "wait-exec")
	if err := r.waitForExecReady(ctx, namespace, createdPod.Name, config.Name); err != nil {
		return createdPod.Name, fmt.Errorf("pod exec tunnel not ready: %w", err)
	}

	// NFS-home pods: home-prepare chose how this start fills the home.
	homeMode := ""
	if nfsHomeStart {
		out, err := r.execInPod(ctx, namespace, createdPod.Name, k8sHomeModeCommand)
		if err != nil {
			return createdPod.Name, fmt.Errorf("failed to read %s: %w", k8sHomeModeFile, err)
		}
		homeMode, err = parseK8sHomeMode(out, config.Labels[labelStartID])
		if err != nil {
			return createdPod.Name, err
		}
		runtimeLog.Info("Agent home prepared", "agent", config.Name, "phase", "home-prepare", "mode", homeMode)
		if config.HomeDir != "" {
			if _, err := r.execInPod(ctx, namespace, createdPod.Name, k8sHomeHooksGuardCommand(util.GetHomeDir(config.UnixUsername))); err != nil {
				return createdPod.Name, fmt.Errorf("agent home check failed: %w", err)
			}
		}
	}

	if config.HomeDir != "" {
		destHome := util.GetHomeDir(config.UnixUsername)
		runtimeLog.Info("Syncing agent home", "agent", config.Name, "source", config.HomeDir, "dest", destHome, "phase", "home-sync")
		fmt.Printf("  Syncing agent home (%s -> %s)...\n", config.HomeDir, destHome)
		homeSyncStart := time.Now()
		// NFS-home pods: the sync never writes over a link target, and the
		// harness bundle's secrets and outputs never reach the export.
		homeLinkTargets := r.k8sHomeLinkTargets(config)
		if nfsHomeStart {
			homeLinkTargets = r.nfsHomeSyncExcludes(config)
		}
		err = r.syncWithRetry(ctx, func() error {
			if r.homeSync != nil {
				return r.homeSync(ctx, namespace, createdPod.Name, config.HomeDir, destHome, homeLinkTargets)
			}
			return r.syncToPod(ctx, namespace, createdPod.Name, config.HomeDir, destHome, homeLinkTargets...)
		})
		if err != nil {
			return createdPod.Name, fmt.Errorf("failed to sync home: %w", err)
		}
		syncMs := time.Since(homeSyncStart).Milliseconds()
		// Fix ownership. The tar extraction runs as the pod user (the pod
		// securityContext sets a non-root RunAsUser), so synced files are
		// normally owned by it already; this chown is best effort and, as
		// that same non-root user, cannot re-own files another user owns.
		chownStart := time.Now()
		//
		// An empty UnixUsername makes destHome itself util.GetHomeDir("") ==
		// "/home" (a critical system directory, not a per-user home), so the
		// in-pod chown below is refused outright rather than recursively
		// re-owning every home directory on the node.
		if nfsHomeStart {
			// The home on the export is written only by the pod user; a
			// recursive chown over it is not needed and could not change
			// files the export maps to another owner.
		} else if chownArgs, ok := chownRecursiveArgs(config.UnixUsername, destHome); !ok {
			runtimeLog.Warn("Skipping home directory chown: UnixUsername is empty", "agent", config.Name, "destHome", destHome)
		} else if _, err := r.execInPod(ctx, namespace, createdPod.Name, chownArgs); err != nil {
			runtimeLog.Debug("Failed to chown home directory (non-fatal)", "error", err)
		}
		runtimeLog.Info("Home sync complete", "agent", config.Name, "phase", "home-sync",
			"sync_ms", syncMs, "chown_ms", time.Since(chownStart).Milliseconds())
	}

	// NFS-home pods: the harness bundle's secrets are staged in memory
	// only, in the directory SCION_HARNESS_SECRETS_DIR names, before the
	// harness runs.
	if nfsHomeStart && config.HomeDir != "" {
		secretsSrc := filepath.Join(config.HomeDir, harnessBundleSecretsRel)
		if st, err := os.Lstat(secretsSrc); err == nil && st.IsDir() {
			if _, err := r.execInPod(ctx, namespace, createdPod.Name, k8sHarnessSecretsDirCommand); err != nil {
				return createdPod.Name, fmt.Errorf("failed to create the in-memory harness secrets directory: %w", err)
			}
			err = r.syncWithRetry(ctx, func() error {
				if r.homeSync != nil {
					return r.homeSync(ctx, namespace, createdPod.Name, secretsSrc, k8sHarnessSecretsDir, nil)
				}
				return r.syncToPod(ctx, namespace, createdPod.Name, secretsSrc, k8sHarnessSecretsDir)
			})
			if err != nil {
				return createdPod.Name, fmt.Errorf("failed to stage the harness secrets: %w", err)
			}
		}
	}

	// A new or interrupted home is marked seeded once the transfer is in
	// it; mark-seeded fails if a newer start has taken over the home.
	if homeMode == k8sHomeModeSeed {
		hs := config.HomeStorage
		if _, err := r.execInPod(ctx, namespace, createdPod.Name,
			k8sHomeMarkSeededCommand(util.GetHomeDir(config.UnixUsername), hs.AgentID, config.Labels[labelStartID])); err != nil {
			return createdPod.Name, fmt.Errorf("failed to mark the agent home seeded: %w", err)
		}
	}

	// Copy staged secret and auth files to their targets in the agent home
	// (see k8s_file_placement.go). This runs whether or not a home was
	// synced, and before the startup gate, so the harness finds them.
	if err := r.placeK8sHomeFiles(ctx, namespace, createdPod.Name, config, k8sHomeFileModeFor(config)); err != nil {
		return createdPod.Name, err
	}

	// Workspace sync: NFS-backed pods have workspace bytes pre-populated by the
	// init container (N2-2), so skip the kubectl-cp workspace sync. This avoids
	// redundantly copying workspace contents that already exist on the shared
	// NFS volume. Local-backend pods RETAIN the existing workspace sync.
	//
	// F-111 (design §9): the skip used to fire for ANY WorkspaceBackendName ==
	// "nfs", unconditionally claiming the workspace was "pre-populated by
	// init container" — but whether that container actually got injected is
	// its own condition (nfsInitContainerInjected). Recomputed here rather
	// than threaded through buildPod's return value, since it's the exact
	// same two fields already on config.
	//
	// Home-dir sync and the startup gate (/tmp/.scion-home-ready) are RETAINED
	// for both backends — they carry agent dotfiles and secrets, not workspace code.
	nfsProvisioned := nfsInitContainerInjected(config)
	if config.Workspace != "" && (config.WorkspaceBackendName != "nfs" || !nfsProvisioned) {
		runtimeLog.Info("Syncing workspace", "agent", config.Name, "source", config.Workspace, "phase", "workspace-sync")
		fmt.Printf("  Syncing workspace (%s -> /workspace)...\n", config.Workspace)
		workspaceSyncStart := time.Now()
		err = r.syncWithRetry(ctx, func() error {
			return r.syncToPod(ctx, namespace, createdPod.Name, config.Workspace, "/workspace")
		})
		if err != nil {
			return createdPod.Name, fmt.Errorf("failed to sync workspace: %w", err)
		}
		syncMs := time.Since(workspaceSyncStart).Milliseconds()
		chownStart := time.Now()
		// Fix workspace ownership for the scion user. An empty UnixUsername
		// would build an owner:group spec of ":" -- refused outright rather
		// than run chown with no actual target user.
		if chownArgs, ok := chownRecursiveArgs(config.UnixUsername, "/workspace"); !ok {
			runtimeLog.Warn("Skipping workspace chown: UnixUsername is empty", "agent", config.Name)
		} else if _, err := r.execInPod(ctx, namespace, createdPod.Name, chownArgs); err != nil {
			runtimeLog.Debug("Failed to chown workspace (non-fatal)", "error", err)
		}
		runtimeLog.Info("Workspace sync complete", "agent", config.Name, "phase", "workspace-sync",
			"sync_ms", syncMs, "chown_ms", time.Since(chownStart).Milliseconds())
	} else if config.WorkspaceBackendName == "nfs" && nfsProvisioned {
		runtimeLog.Info("Skipping workspace sync (NFS backend: workspace pre-populated by init container)",
			"agent", config.Name, "phase", "workspace-sync-skip")
	}

	// Signal the startup gate: all files are synced and ownership is fixed,
	// so it's safe to launch sciontool init → tmux → harness. The gate loop
	// in the pod command polls for this marker file (see buildPod for details).
	runtimeLog.Info("Signaling startup gate", "agent", config.Name, "phase", "startup-gate")
	gateStart := time.Now()
	if _, err := r.execInPod(ctx, namespace, createdPod.Name, []string{"touch", "/tmp/.scion-home-ready"}); err != nil {
		return createdPod.Name, fmt.Errorf("failed to signal startup gate: %w", err)
	}
	runtimeLog.Info("Startup gate signaled", "agent", config.Name, "phase", "startup-gate",
		"elapsed_ms", time.Since(gateStart).Milliseconds())

	runtimeLog.Info("Agent started successfully", "agent", createdPod.Name, "namespace", namespace, "phase", "complete")
	fmt.Printf("Agent '%s' started successfully.\n", createdPod.Name)
	return createdPod.Name, nil
}

// writeK8sRuntimeDebugFile writes a kubectl-style representation of the pod
// spec to the runtime-exec-debug file for diagnostic purposes.
func writeK8sRuntimeDebugFile(config RunConfig, namespace string, pod *corev1.Pod) {
	if !config.Debug || config.HomeDir == "" {
		return
	}
	agentDir := filepath.Dir(config.HomeDir)
	debugPath := filepath.Join(agentDir, "runtime-exec-debug")

	podJSON, err := json.MarshalIndent(pod, "", "  ")
	if err != nil {
		runtimeLog.Debug("Failed to marshal pod spec for debug file", "error", err)
		return
	}

	content := fmt.Sprintf("# kubectl apply -f - <<'EOF'\n%s\n# EOF\n#\n# Equivalent:\n# kubectl apply -n %s -f <this-file's-json-content>\n", string(podJSON), namespace)

	if err := os.WriteFile(debugPath, []byte(content), 0644); err != nil {
		runtimeLog.Debug("Failed to write runtime debug file", "path", debugPath, "error", err)
	}
}

// createAgentSecret creates a K8s Secret containing all resolved secret values.
// Environment-type secrets are stored as individual keys; variable-type secrets
// are marshaled together as JSON under a "secrets.json" key. File-type secrets
// are stored as individual keys named by secret name.
// Returns the secret name, or empty string if no secrets need to be created.
func (r *KubernetesRuntime) createAgentSecret(ctx context.Context, namespace, agentName string, secrets []api.ResolvedSecret, labels map[string]string) (string, error) {
	return r.createAgentSecretWithHooks(ctx, namespace, agentName, secrets, labels, launchHooks{})
}

// createAgentSecretWithHooks is createAgentSecret with an async launch's
// hooks: hooks.checkpoint runs immediately before the create, and
// hooks.created receives the created Secret's UID (design
// t1-async-create-v11.md §3.8.3, §3.8.4).
func (r *KubernetesRuntime) createAgentSecretWithHooks(ctx context.Context, namespace, agentName string, secrets []api.ResolvedSecret, labels map[string]string, hooks launchHooks) (string, error) {
	if len(secrets) == 0 {
		return "", nil
	}

	secretName := fmt.Sprintf("scion-agent-%s", agentName)
	data := make(map[string][]byte)

	// Collect variable-type secrets for JSON aggregation
	varSecrets := make(map[string]string)

	for _, s := range secrets {
		switch s.Type {
		case "environment":
			data[s.Name] = []byte(s.Value)
		case "file":
			decoded, err := base64.StdEncoding.DecodeString(s.Value)
			if err != nil {
				decoded = []byte(s.Value)
			}
			data[s.Name] = decoded
		case "variable":
			varSecrets[s.Target] = s.Value
		}
	}

	if len(varSecrets) > 0 {
		jsonData, err := json.Marshal(varSecrets)
		if err != nil {
			return "", fmt.Errorf("failed to marshal variable secrets: %w", err)
		}
		data["secrets.json"] = jsonData
	}

	if len(data) == 0 {
		return "", nil
	}

	// Build labels for cleanup
	secretLabels := map[string]string{
		"scion.agent": agentName,
	}
	for k, v := range labels {
		if strings.HasPrefix(k, "scion.") {
			secretLabels[k] = v
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels:    secretLabels,
		},
		Data: data,
	}

	if err := hooks.checkpoint(ctx, CheckpointStepSecrets); err != nil {
		return "", err
	}
	created, err := r.Client.Clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if k8serrors.IsAlreadyExists(err) {
		// Delete the stale secret and retry. With a run ID, never one of
		// another run (see replaceExistingAgentObject).
		if rerr := r.replaceExistingAgentObject(ctx, api.ResourceKindSecret, namespace, secretName, labels[api.LabelRunID]); rerr != nil {
			return "", fmt.Errorf("failed to create agent secret: %w", rerr)
		}
		created, err = r.Client.Clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	}
	if err != nil {
		return "", fmt.Errorf("failed to create agent secret: %w", err)
	}
	hooks.created(api.ResourceHandle{Kind: api.ResourceKindSecret, Namespace: namespace, Name: secretName, UID: string(created.UID)})

	return secretName, nil
}

// transportCredentialSecretKey is the data key under which the hub transport
// credential is stored in the per-agent Secret (scion-agent-<name>). It is
// chosen to be unlikely to collide with the Name of a user- or
// project-defined secret; divertTransportCredential drops any resolved
// secret that uses it.
const transportCredentialSecretKey = "scion-transport-credential"

// divertTransportCredential moves the hub transport credential
// (transportauth.EnvTransportToken) out of the plain KEY=VALUE env list and
// into the resolved secrets as an environment-type entry, so that it lands in
// the per-agent Secret and the pod references it via secretKeyRef.
//
// It returns new slices and never modifies the backing arrays of its inputs.
// When the credential is absent (or empty) from env, both inputs are returned
// unchanged.
//
// The hub-provided value takes precedence: any existing environment-type
// resolved secret that targets the same env var, and any resolved secret
// that uses transportCredentialSecretKey as its Name, is dropped (with a
// warning) so the pod spec carries exactly one entry for the variable and
// the Secret carries exactly one value under the key.
func divertTransportCredential(env []string, secrets []api.ResolvedSecret) ([]string, []api.ResolvedSecret) {
	const prefix = transportauth.EnvTransportToken + "="
	var value string
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, prefix) && len(e) > len(prefix) {
			// Last non-empty occurrence wins; an empty entry is treated as
			// unset (see the absent case below).
			value = e[len(prefix):]
			found = true
		}
	}
	if !found {
		return env, secrets
	}

	outEnv := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		outEnv = append(outEnv, e)
	}

	outSecrets := make([]api.ResolvedSecret, 0, len(secrets)+1)
	for _, s := range secrets {
		if (s.Type == "environment" && s.Target == transportauth.EnvTransportToken) || s.Name == transportCredentialSecretKey {
			runtimeLog.Warn("Dropping resolved secret that conflicts with the hub transport credential",
				"secret", s.Name, "target", s.Target, "source", s.Source)
			continue
		}
		outSecrets = append(outSecrets, s)
	}
	outSecrets = append(outSecrets, api.ResolvedSecret{
		Name:   transportCredentialSecretKey,
		Type:   "environment",
		Target: transportauth.EnvTransportToken,
		Value:  value,
		Source: "hub",
	})
	return outEnv, outSecrets
}

// createSecretProviderClass creates a SecretProviderClass CRD for GKE
// Secrets Store CSI driver integration. It maps GCP Secret Manager
// references to K8s-synced secrets for environment variable injection.
func (r *KubernetesRuntime) createSecretProviderClass(ctx context.Context, namespace, agentName string, secrets []api.ResolvedSecret, labels map[string]string) (string, error) {
	return r.createSecretProviderClassWithHooks(ctx, namespace, agentName, secrets, labels, launchHooks{})
}

// createSecretProviderClassWithHooks is createSecretProviderClass with an
// async launch's hooks: hooks.checkpoint runs immediately before the
// create, and hooks.created receives the created object's UID (design
// t1-async-create-v11.md §3.8.3, §3.8.4).
func (r *KubernetesRuntime) createSecretProviderClassWithHooks(ctx context.Context, namespace, agentName string, secrets []api.ResolvedSecret, labels map[string]string, hooks launchHooks) (string, error) {
	spcName := fmt.Sprintf("scion-agent-%s", agentName)
	// envSecretName is only ever referenced below when !r.GKEMode (the
	// secretObjects block a few lines down is skipped entirely in GKE mode).
	// Run only calls createSecretProviderClass when r.GKEMode is true
	// (useGKEPath starts from r.GKEMode), so today this name is declared but
	// never placed into secretObjects, and the Secrets Store CSI driver never
	// materializes it. cleanupAgentSecrets deliberately does not delete this
	// name; see its doc comment.
	envSecretName := fmt.Sprintf("scion-agent-%s-env", agentName)

	// Build the GCP SM secrets parameter as YAML
	type gcpSecretEntry struct {
		ResourceName string `json:"resourceName"`
		FileName     string `json:"fileName"`
	}
	var gcpSecrets []gcpSecretEntry
	for _, s := range secrets {
		if s.Ref == "" {
			continue
		}
		gcpSecrets = append(gcpSecrets, gcpSecretEntry{
			ResourceName: fmt.Sprintf("%s/versions/latest", s.Ref),
			FileName:     s.Name,
		})
	}

	if len(gcpSecrets) == 0 {
		return "", nil
	}

	secretsParam, err := json.Marshal(gcpSecrets)
	if err != nil {
		return "", fmt.Errorf("failed to marshal secrets parameter: %w", err)
	}

	// Build secretObjects for env-type secrets (synced to a K8s Secret).
	// On GKE managed add-on, skip entirely — the add-on's ClusterRole lacks
	// RBAC to create K8s Secrets, so secretObjects are dead config that
	// produces FailedToCreateSecret events. Env vars are handled by the
	// K8s Secret created via createAgentSecret() in the hybrid path.
	type secretObjectData struct {
		Key        string `json:"key"`
		ObjectName string `json:"objectName"`
	}
	type secretObject struct {
		SecretName string             `json:"secretName"`
		Type       string             `json:"type"`
		Data       []secretObjectData `json:"data"`
	}

	var secretObjects []secretObject
	if !r.GKEMode {
		var envData []secretObjectData
		for _, s := range secrets {
			if s.Ref == "" || s.Type != "environment" {
				continue
			}
			envData = append(envData, secretObjectData{
				Key:        s.Name,
				ObjectName: s.Name,
			})
		}

		if len(envData) > 0 {
			secretObjects = append(secretObjects, secretObject{
				SecretName: envSecretName,
				Type:       "Opaque",
				Data:       envData,
			})
		}
	}

	// Build labels
	spcLabels := map[string]string{
		"scion.agent": agentName,
	}
	for k, v := range labels {
		if strings.HasPrefix(k, "scion.") {
			spcLabels[k] = v
		}
	}

	// GKE's managed Secret Manager add-on registers its provider as "gke",
	// whereas the upstream open-source CSI driver uses "gcp".
	providerName := "gcp"
	if r.GKEMode {
		providerName = "gke"
	}

	spec := map[string]interface{}{
		"provider": providerName,
		"parameters": map[string]interface{}{
			"secrets": string(secretsParam),
		},
	}
	if len(secretObjects) > 0 {
		soJSON, _ := json.Marshal(secretObjects)
		var soSlice []interface{}
		_ = json.Unmarshal(soJSON, &soSlice)
		spec["secretObjects"] = soSlice
	}

	spc := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "secrets-store.csi.x-k8s.io/v1",
			"kind":       "SecretProviderClass",
			"metadata": map[string]interface{}{
				"name":      spcName,
				"namespace": namespace,
				"labels":    toStringInterfaceMap(spcLabels),
			},
			"spec": spec,
		},
	}

	if err := hooks.checkpoint(ctx, CheckpointStepSecrets); err != nil {
		return "", err
	}
	createdSPC, err := r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(namespace).Create(ctx, spc, metav1.CreateOptions{})
	if k8serrors.IsAlreadyExists(err) {
		if rerr := r.replaceExistingAgentObject(ctx, api.ResourceKindSecretProviderClass, namespace, spcName, labels[api.LabelRunID]); rerr != nil {
			return "", fmt.Errorf("failed to create SecretProviderClass: %w", rerr)
		}
		createdSPC, err = r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(namespace).Create(ctx, spc, metav1.CreateOptions{})
	}
	if err != nil {
		return "", fmt.Errorf("failed to create SecretProviderClass: %w", err)
	}
	hooks.created(api.ResourceHandle{Kind: api.ResourceKindSecretProviderClass, Namespace: namespace, Name: spcName, UID: string(createdSPC.GetUID())})

	return spcName, nil
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(v int64) *int64 { return &v }

// dedupeEnvVars removes duplicate env var names from vars, keeping only each
// name's LAST occurrence (dropping earlier ones entirely, Value or
// ValueFrom alike) and leaving that survivor at its original position
// rather than hoisting it to the first occurrence's slot. This mirrors
// Kubernetes' own resolution of duplicate container env names (the last
// entry in the list wins), so the effective value of each name is
// unchanged: every $(VAR) reference in a retained entry resolves to the
// same value as before de-duplication.
//
// Exception: kubelet also expands a literal "$(NAME)" appearing in an
// entry's Value against whatever value NAME held earlier in the list —
// including NAME referencing itself (e.g. NODE_OPTIONS=$(NODE_OPTIONS)
// --foo) and other entries between NAME's occurrences referencing it
// (e.g. X=a, Y=$(X), X=b). Dropping an earlier occurrence in either case
// would change that expansion, since every occurrence before it would
// already be gone and kubelet does not fall back to a later occurrence —
// an in-between "$(NAME)" is left as the literal text once its preceding
// occurrences are removed (kubelet falls back only to service-link
// variables). To keep behavior exactly unchanged, this function keeps
// *all* occurrences of a name if any entry from its first through its
// last occurrence (inclusive) has a Value containing the substring
// "$(NAME)" (a plain substring match, so an escaped $$(NAME), which
// kubelet leaves literal, also counts; that only keeps duplicates and
// never changes expansion); only names without such a reference are
// collapsed to their last occurrence. SCION_AGENT_NAME,
// GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_REGION carry plain values today,
// so they are collapsed to one entry each unless another entry between
// their occurrences references them (e.g. a caller-supplied config.Env
// entry BUCKET=$(GOOGLE_CLOUD_PROJECT)-data), in which case their
// occurrences are kept to preserve that entry's expansion.
func dedupeEnvVars(vars []corev1.EnvVar) []corev1.EnvVar {
	if len(vars) == 0 {
		return vars
	}

	firstIndex := make(map[string]int, len(vars))
	lastIndex := make(map[string]int, len(vars))
	for i, v := range vars {
		if _, ok := firstIndex[v.Name]; !ok {
			firstIndex[v.Name] = i
		}
		lastIndex[v.Name] = i
	}

	keepAllOccurrences := make(map[string]bool)
	for name, first := range firstIndex {
		last := lastIndex[name]
		if first == last {
			continue // not duplicated
		}
		ref := "$(" + name + ")"
		for i := first; i <= last; i++ {
			if strings.Contains(vars[i].Value, ref) {
				keepAllOccurrences[name] = true
				break
			}
		}
	}

	result := make([]corev1.EnvVar, 0, len(vars))
	for i, v := range vars {
		if keepAllOccurrences[v.Name] || lastIndex[v.Name] == i {
			result = append(result, v)
		}
	}
	return result
}

// toStringInterfaceMap converts map[string]string to map[string]interface{} for unstructured objects.
func toStringInterfaceMap(m map[string]string) map[string]interface{} {
	result := make(map[string]interface{}, len(m))
	for k, v := range m {
		result[k] = v
	}
	return result
}

// cleanupAgentSecrets removes the per-agent Secrets (scion-agent-<name>,
// scion-auth-<name>) and, in GKE mode, the SecretProviderClass created for an
// agent. These three names are the only per-agent Secret/SecretProviderClass
// objects this runtime ever creates (createAgentSecret and
// createSecretProviderClass for scion-agent-<name>, createAuthFileSecret for
// scion-auth-<name> — there is no fourth creation site), so deleting exactly
// these three leaves nothing of this runtime's own making behind.
//
// agentName must be the exact identifier used to create those objects —
// config.Name in Run, and the pod-name id in Delete. Delete's id is normally
// the pod name (config.Name): callers resolve the agent to its pod first and
// pass that name, falling back to the raw id only when the pod is not listed
// (see pkg/agent/manager.go Stop). createAgentSecret, createSecretProviderClass,
// and createAuthFileSecret derive their object names deterministically from
// that same agentName (scion-agent-<agentName>, scion-auth-<agentName>),
// which already embeds the project when the caller built it as
// "<project>--<agent>" (see containerName in pkg/agent/run.go). Deleting by
// that exact name is therefore inherently agent- and project-scoped: it
// cannot be confused with another agent's or another project's objects, even
// when several projects share a namespace and an agent's bare name.
//
// It does NOT also derive and delete a "scion-agent-<agentName>-env" name for
// the CSI driver-synced Secret described on envSecretName in
// createSecretProviderClass. Unlike the two names above, that one is built by
// appending a suffix rather than being the object's own creation-time name,
// so it is not guaranteed to be unique: a slug may itself end in "-env" (an
// agent literally named "<agent>-env" is valid), in which case the derived
// name collides with that other agent's own real, deterministic
// scion-agent-<other-agent> Secret. Guessing it is therefore unsafe, and that
// object is not reachable via Run today anyway (see envSecretName's own
// comment), so it is simply left alone.
//
// Deletion here, like the Pod deletion in Delete and cleanupStalePod, is by
// the deterministic name alone, so it is used only when no run ID is known
// (a legacy caller or a start without one). A delete that overlaps a fast
// recreate of the same agent name can then remove the new run's object. A
// Delete or start that carries a run ID uses the run-scoped paths in
// k8s_run_scope.go instead (ptone/scion#2550).
func (r *KubernetesRuntime) cleanupAgentSecrets(ctx context.Context, namespace, agentName string) {
	secretNames := []string{
		fmt.Sprintf("scion-agent-%s", agentName), // env/variable/file secrets (createAgentSecret)
		fmt.Sprintf("scion-auth-%s", agentName),  // ResolvedAuth files (createAuthFileSecret)
	}
	for _, name := range secretNames {
		if err := r.Client.Clientset.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
			runtimeLog.Warn("Failed to delete per-agent Secret during cleanup",
				"kind", "Secret", "name", name, "agent", agentName, "namespace", namespace, "error", err)
		}
	}

	// SecretProviderClass shares the same name as the main agent Secret, but
	// is a distinct GVR (secrets-store.csi.x-k8s.io), so there is no collision.
	if r.GKEMode {
		spcName := fmt.Sprintf("scion-agent-%s", agentName)
		if err := r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(namespace).Delete(ctx, spcName, metav1.DeleteOptions{}); err != nil && !k8serrors.IsNotFound(err) {
			runtimeLog.Warn("Failed to delete per-agent SecretProviderClass during cleanup",
				"kind", "SecretProviderClass", "name", spcName, "agent", agentName, "namespace", namespace, "error", err)
		}
	}
}

// Per-agent object name prefixes. Each is followed by the agent's pod name
// (see createAgentSecret, createAuthFileSecret, createSecretProviderClass).
const (
	agentSecretPrefix     = "scion-agent-"
	agentAuthSecretPrefix = "scion-auth-"
)

// podNameForAgentObject returns the pod name a per-agent Secret or
// SecretProviderClass belongs to, derived from its deterministic name, and
// false when the name is not a per-agent object name.
func podNameForAgentObject(objectName string) (string, bool) {
	for _, prefix := range []string{agentSecretPrefix, agentAuthSecretPrefix} {
		if pod, ok := strings.CutPrefix(objectName, prefix); ok && pod != "" {
			return pod, true
		}
	}
	return "", false
}

// CleanupAgentResources implements AgentResourceCleaner. It removes the
// per-agent Secrets (and, in GKE mode, the SecretProviderClass) of an agent
// whose pod is already gone, for example after the pod was deleted outside
// scion. Delete cannot be used for that case because the agent delete only
// reaches Delete for a pod it can still list.
//
// Objects are selected by the agent's scion.name and scion.project_id
// labels, which Run copies onto every per-agent object, so a same-named
// agent in another project is never touched. An object is removed only when
// all of these hold:
//   - its name is a per-agent object name (scion-agent-* or scion-auth-*);
//   - a Get of the pod named in it returns NotFound. Any other error (a
//     timeout, a permission error) leaves the object and is returned, so
//     an object is never removed on a guess.
//
// NotFound when deleting an object counts as success.
//
// Each delete carries a UID precondition taken from the listed object
// (k8sUIDPrecondition, as deleteRun's pod-gone branch uses): the per-agent
// object names are fixed, so a start can recreate an object under the same
// name between the List and the Delete. Such an object has a new UID; its
// delete fails with Conflict, which leaves it in place and is not an error.
//
// With a runID (ptone/scion#2550), an object labelled with another run is
// never removed, whatever its pod: only runID's objects and legacy objects
// with no run label are candidates (k8sRunMatches), as deleteRun's pod-gone
// branch keeps another run's objects. The per-agent object names are fixed
// per agent, so without this a delete naming an older run would remove the
// Secrets a newer run's start created before its pod. An empty runID
// selects by name and project only, as before.
//
// Objects are looked up in the default namespace, or in every namespace
// when ListAllNamespaces is set, the same scope List uses to find pods. An
// agent started in another namespace (the scion.namespace label) with
// ListAllNamespaces off is therefore not found, and its objects are left
// in place rather than searched for.
func (r *KubernetesRuntime) CleanupAgentResources(ctx context.Context, agentName, projectID, runID string) error {
	if agentName == "" || projectID == "" {
		return nil
	}
	if runID != "" {
		if err := validateRunIDLabel(runID); err != nil {
			return err
		}
	}
	selector, err := labels.ValidatedSelectorFromSet(map[string]string{
		"scion.name":               agentName,
		projectkeys.LabelProjectID: projectID,
	})
	if err != nil {
		return fmt.Errorf("invalid agent selector: %w", err)
	}
	namespace := r.DefaultNamespace
	if r.ListAllNamespaces {
		namespace = ""
	}

	// removable reports whether an object may be removed (see the rules
	// above). A non-nil error means the pod lookup failed and the object
	// must be kept.
	removable := func(ns, objectName string, objLabels map[string]string) (bool, error) {
		if runID != "" && !k8sRunMatches(objLabels[api.LabelRunID], runID) {
			return false, nil
		}
		podName, ok := podNameForAgentObject(objectName)
		if !ok {
			return false, nil
		}
		_, err := r.Client.Clientset.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if err == nil {
			return false, nil
		}
		if k8serrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}

	var errs []error
	secrets, err := r.Client.Clientset.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		errs = append(errs, fmt.Errorf("failed to list agent Secrets: %w", err))
	} else {
		for _, s := range secrets.Items {
			ok, err := removable(s.Namespace, s.Name, s.Labels)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to check pod for Secret %s/%s: %w", s.Namespace, s.Name, err))
				continue
			}
			if !ok {
				continue
			}
			err = r.Client.Clientset.CoreV1().Secrets(s.Namespace).Delete(ctx, s.Name, metav1.DeleteOptions{
				Preconditions: k8sUIDPrecondition(s.UID),
			})
			if k8serrors.IsConflict(err) {
				runtimeLog.Info("Left a per-agent object recreated under the same name since it was listed",
					"kind", "Secret", "name", s.Name, "agent", agentName, "namespace", s.Namespace, "run_id", runID)
				continue
			}
			if err != nil && !k8serrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("failed to delete Secret %s/%s: %w", s.Namespace, s.Name, err))
				continue
			}
			runtimeLog.Info("Removed per-agent object left after its pod was deleted",
				"kind", "Secret", "name", s.Name, "agent", agentName, "namespace", s.Namespace)
		}
	}

	// Run only creates a SecretProviderClass in GKE mode (see
	// cleanupAgentSecrets), and the CRD may not be installed otherwise.
	if r.GKEMode {
		spcs, err := r.Client.ListSecretProviderClasses(ctx, namespace, selector.String())
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to list agent SecretProviderClasses: %w", err))
		} else {
			for _, spc := range spcs.Items {
				ns, name := spc.GetNamespace(), spc.GetName()
				ok, err := removable(ns, name, spc.GetLabels())
				if err != nil {
					errs = append(errs, fmt.Errorf("failed to check pod for SecretProviderClass %s/%s: %w", ns, name, err))
					continue
				}
				if !ok {
					continue
				}
				err = r.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace(ns).Delete(ctx, name, metav1.DeleteOptions{
					Preconditions: k8sUIDPrecondition(spc.GetUID()),
				})
				if k8serrors.IsConflict(err) {
					runtimeLog.Info("Left a per-agent object recreated under the same name since it was listed",
						"kind", "SecretProviderClass", "name", name, "agent", agentName, "namespace", ns, "run_id", runID)
					continue
				}
				if err != nil && !k8serrors.IsNotFound(err) {
					errs = append(errs, fmt.Errorf("failed to delete SecretProviderClass %s/%s: %w", ns, name, err))
					continue
				}
				runtimeLog.Info("Removed per-agent object left after its pod was deleted",
					"kind", "SecretProviderClass", "name", name, "agent", agentName, "namespace", ns)
			}
		}
	}
	return errors.Join(errs...)
}

// createAuthFileSecret creates a K8s Secret containing ResolvedAuth file contents
// so that auth files can be projected into pods via volume mounts instead of hostPath.
func (r *KubernetesRuntime) createAuthFileSecret(ctx context.Context, namespace, agentName string, files []api.FileMapping, labels map[string]string) error {
	return r.createAuthFileSecretWithHooks(ctx, namespace, agentName, files, labels, launchHooks{})
}

// createAuthFileSecretWithHooks is createAuthFileSecret with an async
// launch's hooks: hooks.checkpoint runs immediately before the create, and
// hooks.created receives the created Secret's UID (design
// t1-async-create-v11.md §3.8.3, §3.8.4).
func (r *KubernetesRuntime) createAuthFileSecretWithHooks(ctx context.Context, namespace, agentName string, files []api.FileMapping, labels map[string]string, hooks launchHooks) error {
	secretName := fmt.Sprintf("scion-auth-%s", agentName)
	data := make(map[string][]byte)

	for i, f := range files {
		if f.SourcePath == "" {
			continue
		}
		content, err := os.ReadFile(f.SourcePath)
		if err != nil {
			return fmt.Errorf("failed to read auth file %s: %w", f.SourcePath, err)
		}
		keyName := fmt.Sprintf("auth-file-%d", i)
		data[keyName] = content
	}

	secretLabels := map[string]string{
		"scion.agent": agentName,
	}
	for k, v := range labels {
		if strings.HasPrefix(k, "scion.") {
			secretLabels[k] = v
		}
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels:    secretLabels,
		},
		Data: data,
	}

	if err := hooks.checkpoint(ctx, CheckpointStepSecrets); err != nil {
		return err
	}
	created, err := r.Client.Clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if k8serrors.IsAlreadyExists(err) {
		if rerr := r.replaceExistingAgentObject(ctx, api.ResourceKindSecret, namespace, secretName, labels[api.LabelRunID]); rerr != nil {
			return fmt.Errorf("failed to create auth secret: %w", rerr)
		}
		created, err = r.Client.Clientset.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	}
	if err != nil {
		return fmt.Errorf("failed to create auth secret: %w", err)
	}
	hooks.created(api.ResourceHandle{Kind: api.ResourceKindSecret, Namespace: namespace, Name: secretName, UID: string(created.UID)})
	return nil
}

// --- Generalized project RWX claim helpers (N2-5) ---
//
// These helpers manage project-scoped PVCs for both shared directories and
// (future) workspace claims. The naming convention and lifecycle are identical;
// only the label selector differs.
//
// When backend=nfs, shared dirs are served from the workspace NFS PVC via
// subPath (e.g., "projects/<pid>/shared-dirs/<name>") and do NOT need their
// own PVC — the NFS volume already provides RWX access. PVC creation
// short-circuits for NFS.

// projectRWXClaimName returns a deterministic PVC name for a project-scoped
// RWX claim. Usable for shared dirs ("shared") and workspace claims ("workspace").
// PVCs are project-scoped (not agent-scoped), so multiple agents share the same PVC.
func projectRWXClaimName(projectName, claimType, dirName string) string {
	return fmt.Sprintf("scion-%s-%s-%s", claimType, projectName, dirName)
}

// sharedDirPVCName returns the deterministic PVC name for a project shared directory.
// This is a convenience wrapper around projectRWXClaimName for backward compatibility.
func sharedDirPVCName(projectName, dirName string) string {
	return projectRWXClaimName(projectName, "shared", dirName)
}

// runNamespace returns the namespace Run places an agent with labels in:
// the scion.namespace label, else the namespace label, else
// DefaultNamespace.
func (r *KubernetesRuntime) runNamespace(labels map[string]string) string {
	if ns, ok := labels["scion.namespace"]; ok {
		return ns
	}
	if ns, ok := labels["namespace"]; ok {
		return ns
	}
	return r.DefaultNamespace
}

// sharedDirsHaveOwnClaims reports whether the local shared dirs of config
// get a claim of their own (createSharedDirPVCs creates or reuses one per
// dir). They do not when the workspace is on nfs with a bound claim: they
// are then served by subPath from the workspace claim.
func sharedDirsHaveOwnClaims(config RunConfig) bool {
	return config.WorkspaceBackendName != "nfs" || config.NFSPVClaimName == ""
}

// SharedDirUsesClaim implements SharedDirClaimChecker with the same rules
// as createSharedDirPVCs: a dir served from the shared_dir_storage nfs
// export has no claim of its own, and neither has any dir when
// sharedDirsHaveOwnClaims is false.
func (r *KubernetesRuntime) SharedDirUsesClaim(config RunConfig, dirName string) bool {
	if config.SharedDirStorage.Serves(dirName) {
		return false
	}
	return sharedDirsHaveOwnClaims(config)
}

// SharedDirClaimExists implements SharedDirClaimChecker: it looks up, by
// name, the shared-dir PVC that createSharedDirPVCs would create or reuse
// for dirName, in the namespace Run would use for config. Any error other
// than NotFound is returned.
func (r *KubernetesRuntime) SharedDirClaimExists(ctx context.Context, config RunConfig, dirName string) (bool, error) {
	projectName := projectkeys.ProjectNameFromLabels(config.Labels)
	if projectName == "" {
		return false, fmt.Errorf("cannot look up the shared dir PVC: missing scion.project label")
	}
	if r.Client == nil || r.Client.Clientset == nil {
		return false, fmt.Errorf("cannot look up the shared dir PVC: no Kubernetes client")
	}
	namespace := r.runNamespace(config.Labels)
	pvcName := sharedDirPVCName(projectName, dirName)
	_, err := r.Client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err == nil {
		return true, nil
	}
	if k8serrors.IsNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("looking up shared dir PVC %s/%s: %w", namespace, pvcName, err)
}

// defaultSharedDirSize is the default PVC size when not specified in settings.
const defaultSharedDirSize = "10Gi"

// createSharedDirPVCs ensures PVCs exist for all declared shared directories.
// PVCs are project-scoped and persist across agent restarts. If a PVC already
// exists (from a previous agent in the same project), it is reused.
//
// When backend=nfs, shared dirs are served via NFS subPath from the workspace
// PVC and do NOT require separate PVCs — this method is a no-op for NFS.
func (r *KubernetesRuntime) createSharedDirPVCs(ctx context.Context, namespace string, config RunConfig) error {
	if len(config.SharedDirs) == 0 {
		return nil
	}

	// server.shared_dir_storage backend=nfs: shared dirs use subPaths on the
	// dedicated shared NFS PVC, no separate PVCs needed (design
	// deploy-config-explore §3.2.4). Takes precedence over the
	// workspace_storage:nfs branch below. With per-dir backends only the
	// dirs the realization serves are skipped; the rest are handled below.
	sharedDirs := config.SharedDirs
	if config.SharedDirStorage != nil && config.SharedDirStorage.Backend == "nfs" {
		sharedDirs = make([]api.SharedDir, 0, len(config.SharedDirs))
		for _, sd := range config.SharedDirs {
			if !config.SharedDirStorage.Serves(sd.Name) {
				sharedDirs = append(sharedDirs, sd)
			}
		}
		if len(sharedDirs) == 0 {
			runtimeLog.Info("shared_dir_storage nfs: shared dirs served via NFS subPath, skipping PVC creation",
				"shared_dir_count", len(config.SharedDirs))
			return nil
		}
	}

	// NFS backend: shared dirs use subPaths on the workspace NFS PVC,
	// no separate PVCs needed (design §5.3).
	if !sharedDirsHaveOwnClaims(config) {
		runtimeLog.Info("NFS backend: shared dirs served via NFS subPath, skipping PVC creation",
			"shared_dir_count", len(sharedDirs))
		return nil
	}

	projectID := projectkeys.ProjectIDFromLabels(config.Labels)

	projectName := projectkeys.ProjectNameFromLabels(config.Labels)
	if projectName == "" {
		return fmt.Errorf("cannot create shared dir PVCs: missing scion.project label")
	}

	storageClass := ""
	size := defaultSharedDirSize
	if config.Kubernetes != nil {
		if config.Kubernetes.SharedDirStorageClass != "" {
			storageClass = config.Kubernetes.SharedDirStorageClass
		}
		if config.Kubernetes.SharedDirSize != "" {
			size = config.Kubernetes.SharedDirSize
		}
	}

	storageQuantity, err := parseResourceSafe(size, "shared_dir_size")
	if err != nil {
		return err
	}

	for _, sd := range sharedDirs {
		if err := r.ensureProjectRWXClaim(ctx, namespace, projectName, projectID, sd.Name, storageClass, storageQuantity); err != nil {
			return err
		}
	}

	return nil
}

// warnOnStorageClassMismatch logs a warning when a reused claim's storage
// class differs from the requested (non-empty) one. Existing claims are
// never changed, so a new class only applies to newly created claims; the
// warning makes that visible, with the phase when the claim is still Pending
// (for example because its class cannot provision ReadWriteMany volumes).
func warnOnStorageClassMismatch(pvc *corev1.PersistentVolumeClaim, requested string) {
	if pvc == nil || requested == "" {
		return
	}
	existing := ""
	if pvc.Spec.StorageClassName != nil {
		existing = *pvc.Spec.StorageClassName
	}
	if existing == requested {
		return
	}
	args := []any{"pvc", pvc.Name, "existing_storage_class", existing, "requested_storage_class", requested}
	if pvc.Status.Phase == corev1.ClaimPending {
		args = append(args, "phase", string(pvc.Status.Phase))
	}
	runtimeLog.Warn("Reusing existing shared-dir PVC whose storage class differs from the requested class; existing claims are not changed", args...)
}

// ensureProjectRWXClaim is the idempotent get-or-create core for project-scoped
// RWX PVCs. It creates a PVC with a deterministic name if one does not already
// exist. Used by both shared-dir and (future) workspace claim paths.
func (r *KubernetesRuntime) ensureProjectRWXClaim(
	ctx context.Context,
	namespace, projectName, projectID, dirName, storageClass string,
	storageQuantity resource.Quantity,
) error {
	pvcName := sharedDirPVCName(projectName, dirName)

	// Check if PVC already exists (project-scoped, may have been created by another agent)
	existing, err := r.Client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvcName, metav1.GetOptions{})
	if err == nil {
		runtimeLog.Info("Project RWX PVC already exists, reusing", "pvc", pvcName, "dir", dirName)
		warnOnStorageClassMismatch(existing, storageClass)
		return nil
	}

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: namespace,
			Labels: map[string]string{
				"scion.shared-dir": dirName,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageQuantity,
				},
			},
		},
	}

	for k, v := range projectkeys.ProjectNameLabels(projectName) {
		pvc.Labels[k] = v
	}
	if projectID != "" {
		for k, v := range projectkeys.ProjectIDLabels(projectID) {
			pvc.Labels[k] = v
		}
	}

	if storageClass != "" {
		pvc.Spec.StorageClassName = &storageClass
	}

	runtimeLog.Info("Creating project RWX PVC", "pvc", pvcName, "dir", dirName, "storage", storageQuantity.String())
	if _, err := r.Client.Clientset.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("failed to create project RWX PVC %q: %w", pvcName, err)
	}

	return nil
}

func (r *KubernetesRuntime) buildPod(namespace string, config RunConfig) (*corev1.Pod, error) {
	// Command Resolution — see buildCommonRunArgs for the Docker/Podman
	// equivalent. No-auth mode builds a raw shell command string to avoid
	// the double-sh-c wrapping that previously caused the no-auth command
	// to be injected as terminal input instead of running standalone.
	var cmd []string
	cmdLine, ok := harnessCmdLine(config)
	if !ok {
		cmdLine = "sleep infinity"
	}
	// Wrap the harness so it records its real exit code to a fixed file (see
	// tmuxAgentWindowCmd for rationale). `sciontool init` reads this to
	// report crashes accurately. K8s provides PID 1 a TTY, so attach like
	// Docker/Podman (see buildCommonRunArgs).
	agentWindowCmd := tmuxAgentWindowCmd("sh", cmdLine)
	tmuxCmd := buildTmuxStartCmd(agentWindowCmd, tmuxAttachSession)
	// --- K8s Startup Gate ---
	//
	// Unlike Docker/Podman where volumes are bind-mounted before the container
	// starts, K8s requires the pod to be running before we can exec into it to
	// sync files (home directory, workspace). This creates a chicken-and-egg
	// problem: the container process (sciontool init → tmux → harness) needs
	// dotfiles like .zshrc, .tmux.conf, and .gemini/settings.json to be
	// present at launch, but we can only copy them into a running container.
	//
	// Solution: the pod command starts with a lightweight gate loop that polls
	// for a marker file (/tmp/.scion-home-ready). The broker syncs home +
	// workspace, fixes ownership, then creates the marker. The gate detects
	// it and exec's the real entrypoint (sciontool init → tmux → harness)
	// with all files already in place.
	//
	// The real startup command is passed via the SCION_START_CMD env var to
	// avoid shell quoting issues with the tmux command string.
	//
	// Flow:
	//   1. Pod starts → gate loop (polling /tmp/.scion-home-ready)
	//   2. Broker syncs home dir → syncs workspace → chowns files
	//   3. Broker creates /tmp/.scion-home-ready via execInPod
	//   4. Gate detects marker → exec sciontool init -- sh -c "$SCION_START_CMD"
	//   5. sciontool init sets up user, drops privileges, launches tmux
	//
	gateCmd := `while [ ! -f /tmp/.scion-home-ready ]; do sleep 0.2; done; exec sciontool init -- sh -c "$SCION_START_CMD"`
	cmd = []string{"sh", "-c", gateCmd}

	// Env Resolution — match local runtimes by including harness env + telemetry env.
	envVars := []corev1.EnvVar{
		// The real startup command, consumed by the gate script above.
		{Name: "SCION_START_CMD", Value: tmuxCmd},
	}

	// Harness env (system prompt, agent name, etc.) — parity with buildCommonRunArgs.
	if config.Harness != nil {
		for k, v := range config.Harness.GetEnv(config.Name, config.HomeDir, config.UnixUsername) {
			if v != "" {
				envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
			}
		}
		if config.TelemetryEnabled {
			for k, v := range config.Harness.GetTelemetryEnv() {
				envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
			}
		}
	}

	for _, e := range config.Env {
		// Parse "KEY=VALUE"
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envVars = append(envVars, corev1.EnvVar{Name: parts[0], Value: parts[1]})
		}
	}

	// System env set above always wins: track the names already present so
	// the secret-injection loops below can skip any secret whose target
	// collides with one. Symmetric with the docker/podman/apple_container
	// runtime's equivalent check in buildCommonRunArgs.
	envVarNames := make(map[string]struct{}, len(envVars))
	for _, ev := range envVars {
		envVarNames[ev.Name] = struct{}{}
	}

	// Secret and auth-file mounting. Every file these volumes deliver comes
	// from k8sFileProjections. Targets inside the agent home are not mounted
	// directly: their volume is mounted under k8sFileStagingRoot and
	// placeK8sHomeFiles copies the files into the home after the home sync,
	// so no root-owned mount directory is created inside the home (see
	// k8s_file_placement.go). Targets outside the home keep a subPath mount.
	var extraVolumes []corev1.Volume
	var extraVolumeMounts []corev1.VolumeMount

	nfsHome, err := nfsHomePod(config)
	if err != nil {
		return nil, err
	}
	containerHome := util.GetHomeDir(config.UnixUsername)
	fileProjections := r.k8sFileProjections(config)
	if err := checkK8sHomeFileTargets(r.k8sHomeFilePlacements(config)); err != nil {
		return nil, err
	}
	usedVolumes := map[string]bool{}
	var agentSecretItems []corev1.KeyToPath
	for _, p := range fileProjections {
		usedVolumes[p.Volume] = true
		if p.Volume == k8sAgentSecretsVolume {
			agentSecretItems = append(agentSecretItems, corev1.KeyToPath{Key: p.Key, Path: p.Key})
		}
		if placedInK8sHome(p.Target, config.UnixUsername) {
			continue
		}
		extraVolumeMounts = append(extraVolumeMounts, corev1.VolumeMount{
			Name:      p.Volume,
			MountPath: p.Target,
			SubPath:   p.Key,
			ReadOnly:  true,
		})
	}

	if len(config.ResolvedSecrets) > 0 {
		agentSecretName := fmt.Sprintf("scion-agent-%s", config.Name)

		if r.useGKESecretsPath(config) {
			// GKE hybrid path: CSI volume for file-type secrets, secretKeyRef
			// to K8s Secret (scion-agent-{name}) for env vars. The managed
			// SM add-on cannot sync secretObjects, so env vars reference the
			// Hub-created K8s Secret instead of the CSI-synced -env secret.
			spcName := fmt.Sprintf("scion-agent-%s", config.Name)

			// Add CSI volume (required for file-type secret mounts).
			// GKE's managed add-on registers the driver as
			// "secrets-store-gke.csi.k8s.io"; the upstream open-source
			// driver uses "secrets-store.csi.x-k8s.io".
			csiDriverName := "secrets-store.csi.x-k8s.io"
			if r.GKEMode {
				csiDriverName = "secrets-store-gke.csi.k8s.io"
			}
			extraVolumes = append(extraVolumes, corev1.Volume{
				Name: k8sSecretsStoreVolume,
				VolumeSource: corev1.VolumeSource{
					CSI: &corev1.CSIVolumeSource{
						Driver:   csiDriverName,
						ReadOnly: boolPtr(true),
						VolumeAttributes: map[string]string{
							"secretProviderClass": spcName,
						},
					},
				},
			})
			extraVolumeMounts = append(extraVolumeMounts, corev1.VolumeMount{
				Name:      k8sSecretsStoreVolume,
				MountPath: k8sStagingDir(k8sSecretsStoreVolume),
				ReadOnly:  true,
			})

			for _, s := range config.ResolvedSecrets {
				if s.Type == "environment" {
					if _, collides := envVarNames[s.Target]; collides {
						continue
					}
					envVars = append(envVars, corev1.EnvVar{
						Name: s.Target,
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: agentSecretName},
								Key:                  s.Name,
							},
						},
					})
					envVarNames[s.Target] = struct{}{}
				}
			}
		} else {
			// Fallback path: K8s Secret with secretKeyRef for env, and a
			// Secret volume for file-type secrets and secrets.json. The
			// volume projects only the file keys, not the env values.
			for _, s := range config.ResolvedSecrets {
				if s.Type == "environment" {
					if _, collides := envVarNames[s.Target]; collides {
						continue
					}
					envVars = append(envVars, corev1.EnvVar{
						Name: s.Target,
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{Name: agentSecretName},
								Key:                  s.Name,
							},
						},
					})
					envVarNames[s.Target] = struct{}{}
				}
			}

			if usedVolumes[k8sAgentSecretsVolume] {
				extraVolumes = append(extraVolumes, corev1.Volume{
					Name: k8sAgentSecretsVolume,
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: agentSecretName,
							Items:      agentSecretItems,
						},
					},
				})
				extraVolumeMounts = append(extraVolumeMounts, corev1.VolumeMount{
					Name:      k8sAgentSecretsVolume,
					MountPath: k8sStagingDir(k8sAgentSecretsVolume),
					ReadOnly:  true,
				})
			}
		}
	}

	// ResolvedAuth is always applied when present (composes with ResolvedSecrets).
	// Auth files are injected via a K8s Secret rather than hostPath for portability.
	if config.ResolvedAuth != nil {
		for k, v := range config.ResolvedAuth.EnvVars {
			envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
		}
		if len(config.ResolvedAuth.Files) > 0 {
			extraVolumes = append(extraVolumes, corev1.Volume{
				Name: k8sAuthFilesVolume,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: fmt.Sprintf("scion-auth-%s", config.Name),
					},
				},
			})
			if usedVolumes[k8sAuthFilesVolume] {
				extraVolumeMounts = append(extraVolumeMounts, corev1.VolumeMount{
					Name:      k8sAuthFilesVolume,
					MountPath: k8sStagingDir(k8sAuthFilesVolume),
					ReadOnly:  true,
				})
			}
		}
	}

	// Inject GCP telemetry credential path if the well-known secret is present
	if credPath := findGCPTelemetryCredentialPath(config.ResolvedSecrets, containerHome); credPath != "" {
		// NFS-home pods point at the staged file rather than its home link.
		if staged := r.k8sStagedPathFor(config, telemetryGCPCredentialsSecretName); nfsHome && staged != "" {
			credPath = staged
		}
		envVars = append(envVars, corev1.EnvVar{Name: telemetryGCPCredentialsEnvVar, Value: credPath})
	}

	// NFS-home pods: the memory-backed directory and the env vars that
	// locate the staged files and harness directories (k8s_nfs_home.go).
	if nfsHome {
		memVolume, memMount := nfsHomeVolume()
		extraVolumes = append(extraVolumes, memVolume)
		extraVolumeMounts = append(extraVolumeMounts, memMount)
		nfsEnv, err := r.nfsHomeEnv(config)
		if err != nil {
			return nil, err
		}
		envVars = append(envVars, nfsEnv...)
	}

	// Pass host user UID/GID for container user synchronization
	envVars = append(envVars, corev1.EnvVar{Name: "SCION_HOST_UID", Value: fmt.Sprintf("%d", os.Getuid())})
	envVars = append(envVars, corev1.EnvVar{Name: "SCION_HOST_GID", Value: fmt.Sprintf("%d", os.Getgid())})
	envVars = append(envVars,
		corev1.EnvVar{Name: "HOME", Value: containerHome},
		corev1.EnvVar{Name: "USER", Value: config.UnixUsername},
		corev1.EnvVar{Name: "LOGNAME", Value: config.UnixUsername},
	)

	// Worktree-per-agent on NFS: the agent works in its own worktree at
	// /repo-root/worktrees/<agent name>, not at /workspace. Point sciontool
	// init's workspace path there, so its clone step looks at (and, with an
	// older init container that does not add worktrees, fills) the agent's
	// own directory. Appended last so it wins over any earlier value.
	nfsWorktree := config.NFSWorktreeName != "" && nfsInitContainerInjected(config)
	if nfsWorktree {
		envVars = append(envVars, corev1.EnvVar{Name: "SCION_WORKSPACE_PATH", Value: NFSWorktreeContainerPath(config.NFSWorktreeName)})
	}

	// Env vars are assembled above from several sources (harness env, config.Env,
	// resolved auth, resolved secrets) that can legitimately overlap in name
	// (e.g. SCION_AGENT_NAME, GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_REGION).
	// De-duplicate, collapsing each name to its last occurrence in place
	// (except the $(NAME) case described in dedupeEnvVars), so every
	// $(VAR) reference in a retained entry resolves to the same value as
	// before de-duplication.
	envVars = dedupeEnvVars(envVars)

	// Security context: run agent pods as the image's non-root scion user.
	// FSGroup is branched by workspace backend (N2-4):
	//   - NFS backend: stable GID (default 1000) so files are writable across
	//     pods and nodes without per-start chown (design §9.1).
	//   - Local backend: host GID (today's behavior) so synced files remain
	//     writable by the broker user.
	fsGroupGID := int64(os.Getgid()) // default: host GID (local backend)
	if config.WorkspaceBackendName == "nfs" {
		fsGroupGID = int64(provision.DefaultOwnerID(config.NFSGID))
	}
	runAsNonRoot := true
	allowPrivilegeEscalation := false
	podSecurityContext := &corev1.PodSecurityContext{
		FSGroup:      &fsGroupGID,
		RunAsUser:    int64Ptr(containerUID),
		RunAsGroup:   int64Ptr(containerUID),
		RunAsNonRoot: &runAsNonRoot,
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
		SupplementalGroups: sharedDirSupplementalGroups(config, fsGroupGID),
	}
	// SCION_SUPPLEMENTAL_GIDS is broker-owned: drop any template, user env
	// or secret value, and set it to the nfs leaf gids the pod holds (its
	// supplementalGroups plus a leaf gid skipped there because it equals
	// fsGroup, which the pod holds through fsGroup), so sciontool clears the
	// umask group bits for nfs shared-dir writers (ptone/scion#3155). Pods start
	// as the agent user via runAsUser, so sciontool does no privilege drop
	// here; the variable only drives the umask.
	envVars = withSupplementalGIDsEnv(envVars, sharedDirGroups(config))

	// Determine image pull policy
	pullPolicy := corev1.PullIfNotPresent
	if config.Kubernetes != nil && config.Kubernetes.ImagePullPolicy != "" {
		switch config.Kubernetes.ImagePullPolicy {
		case "Always":
			pullPolicy = corev1.PullAlways
		case "Never":
			pullPolicy = corev1.PullNever
		case "IfNotPresent":
			pullPolicy = corev1.PullIfNotPresent
		default:
			return nil, fmt.Errorf("invalid imagePullPolicy %q: must be Always, IfNotPresent, or Never", config.Kubernetes.ImagePullPolicy)
		}
	}

	// Workspace volume: NFS-backed pods use a PVC+subPath for shared, persistent
	// storage isolated to the project subtree (design §5.1/§9.4).
	// Local-backend pods keep the existing EmptyDir (zero behavior change).
	var workspaceVolume corev1.Volume
	var workspaceVolumeMount corev1.VolumeMount
	if config.WorkspaceBackendName == "nfs" && config.NFSPVClaimName != "" {
		workspaceVolume = corev1.Volume{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: config.NFSPVClaimName,
				},
			},
		}
		workspaceVolumeMount = corev1.VolumeMount{
			Name:      "workspace",
			MountPath: "/workspace",
			SubPath:   config.NFSSubPath,
		}
	} else {
		workspaceVolume = corev1.Volume{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		}
		workspaceVolumeMount = corev1.VolumeMount{
			Name:      "workspace",
			MountPath: "/workspace",
		}
	}
	// The agent container's workspace mounts and working directory. The
	// provisioning init container mounts initWorkspaceMount: the project's
	// shared checkout on NFS, or the agent's directory in clone-per-agent
	// mode.
	agentVolumeMounts := []corev1.VolumeMount{workspaceVolumeMount}
	agentWorkingDir := "/workspace"
	initWorkspaceMount := workspaceVolumeMount
	// provisionAgentDirMount is the provisioning container's mount of the
	// agent directory (clone-per-agent only), which the home mount guard
	// allows.
	var provisionAgentDirMount *corev1.VolumeMount
	nfsAgentDir := config.NFSAgentDirName != "" && nfsInitContainerInjected(config)
	if nfsAgentDir && nfsWorktree {
		return nil, fmt.Errorf("an agent cannot use both a worktree and its own workspace on the NFS workspace")
	}
	if nfsAgentDir {
		// Clone-per-agent: the agent's own workspace at
		// <project>/agents/<agent name>/workspace, mounted at /workspace as
		// on the local runtimes; the agent container clones into it. The
		// init container mounts the agent's directory, so its lock and
		// sentinel stay outside the workspace.
		agentDirSubPath, err := NFSAgentDirSubPath(config.NFSSubPath, config.NFSAgentDirName)
		if err != nil {
			return nil, err
		}
		agentVolumeMounts = []corev1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace", SubPath: filepath.Join(agentDirSubPath, provision.AgentWorkspaceDir)},
		}
		initWorkspaceMount = corev1.VolumeMount{Name: "workspace", MountPath: "/workspace", SubPath: agentDirSubPath}
		m := initWorkspaceMount
		provisionAgentDirMount = &m
	}
	if nfsWorktree {
		// Same layout as the local runtimes: the shared .git at
		// /repo-root/.git and the agent's worktree at
		// /repo-root/worktrees/<agent name>. The worktree's .git file points
		// at ../../.git/worktrees/<agent name> (relative paths), which
		// resolves inside this layout.
		gitSubPath, worktreeSubPath, err := nfsWorktreeSubPaths(config.NFSSubPath, config.NFSWorktreeName)
		if err != nil {
			return nil, err
		}
		agentWorkingDir = NFSWorktreeContainerPath(config.NFSWorktreeName)
		agentVolumeMounts = []corev1.VolumeMount{
			{Name: "workspace", MountPath: "/repo-root/.git", SubPath: gitSubPath},
			{Name: "workspace", MountPath: agentWorkingDir, SubPath: worktreeSubPath},
		}
	}

	// NFS-home pods: the home volume and mount, and the home init
	// containers (k8s_nfs_home.go).
	var homeParts nfsHomePodParts
	if nfsHome {
		workspaceClaim := ""
		if config.WorkspaceBackendName == "nfs" && config.NFSPVClaimName != "" {
			workspaceClaim = config.NFSPVClaimName
		}
		homeParts, err = r.nfsHomePodSpec(config, containerHome, workspaceClaim)
		if err != nil {
			return nil, err
		}
		agentVolumeMounts = append(agentVolumeMounts, homeParts.mount)
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        config.Name,
			Namespace:   namespace,
			Labels:      config.Labels,
			Annotations: withHomeStorageAnnotation(config.Annotations, nfsHome),
		},
		Spec: corev1.PodSpec{
			SecurityContext: podSecurityContext,
			Containers: []corev1.Container{
				{
					Name:            agentContainerName,
					Image:           config.Image,
					Command:         cmd,
					Env:             envVars,
					ImagePullPolicy: pullPolicy,
					WorkingDir:      agentWorkingDir,
					Stdin:           true,
					TTY:             true,
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &allowPrivilegeEscalation,
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
					VolumeMounts: agentVolumeMounts,
				},
			},
			Volumes:       []corev1.Volume{workspaceVolume},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}

	// NFS init container: when backend=nfs and a workspace PVC is bound, add
	// an init container that provisions the workspace before the main
	// container starts. The init container mounts the same workspace
	// PVC+subPath so provisioned files are visible to the main container.
	//
	// F-111 (design §9): this used to also require config.GitCloneForInit !=
	// nil, which meant non-git projects got no init container at all and no
	// mkdir/chown ever ran — the per-project subPath then didn't exist when
	// the main container mounted it, and the kubelet had to create it.
	// The kubelet's mkdir only yields a root:root directory on exports that
	// allow root to create directories (no_root_squash); on exports that map
	// root to an anonymous user it is denied and the pod never starts. When
	// the broker has the export mounted, pkg/agent (ensureNFSWorkspaceLeaf)
	// now creates the workspace subPath (and the subPaths of shared dirs
	// served from the same claim) before the pod exists, so the
	// kubelet only falls back to creating it when the broker has no mount.
	// The gate now keys only on nfs backend + a bound PV claim, matching
	// nfsProvisionCommand's own nil-safety (it already emits a plain
	// `sciontool provision` when gc == nil); GitCloneForInit continues to
	// select clone-vs-plain-provision behavior, not whether provisioning
	// happens at all.
	//
	// Advisory lock integration (N2-2b, design §7, risk RN1): the Go-side
	// Run() method acquires a per-project advisory lock (via TryAdvisoryLockObject)
	// BEFORE reaching this point. The lock result determines the init container
	// behavior:
	//   - Lock winner (nfsProvisionLockLost=false): injects the PROVISIONING
	//     init container that checks the sentinel and provisions (mkdir+chown,
	//     plus clone when GitCloneForInit is set) if absent (N2-2 script).
	//   - Lock loser  (nfsProvisionLockLost=true): injects a WAIT-for-sentinel
	//     init container that polls for .scion-provisioned without cloning.
	//
	// When no advisory locker is available (Locker nil / single-node deploy),
	// nfsProvisionLockLost stays false and the provisioning init container is
	// injected — the sentinel provides idempotent protection but NOT
	// cross-node mutual exclusion.
	if nfsInitContainerInjected(config) {
		// waitOnly: a broker lock loser waits for the project's sentinel
		// instead of provisioning. In worktree-per-agent mode every pod
		// provisions, because each agent adds its own worktree; the
		// provisioning lock on the export serializes the clone and every
		// worktree add.
		// The same holds in clone-per-agent mode, where each pod prepares
		// its own agent directory.
		waitOnly := config.nfsProvisionLockLost && !nfsWorktree && !nfsAgentDir
		// Clone-per-agent: the agent container clones, so the init
		// container gets no clone settings.
		initGitClone := config.GitCloneForInit
		if nfsAgentDir {
			initGitClone = nil
		}
		var initCommand []string
		if waitOnly {
			// Lock loser: wait for the sentinel written by the winning node's
			// provisioning init container. Does NOT provision.
			initCommand = []string{"sciontool", "provision", "--wait-for-sentinel"}
		} else {
			// Lock winner (or no locker available): provision (mkdir+chown,
			// plus clone if GitCloneForInit is set) if sentinel is absent,
			// skip if already provisioned. The command is idempotent.
			// Ownership follows the pod securityContext above: uid is the
			// RunAsUser the agent runs as, gid is the NFS fsGroup. The
			// configured NFS uid is not applied to RunAsUser, so it is not
			// passed here either.
			initCommand = nfsProvisionCommand(initGitClone, containerUID, fsGroupGID)
		}

		// F-111: shared dirs served from the workspace PVC by subPath
		// (nfsSharedDirs below) are siblings of the workspace under the
		// project root, but each is its OWN volume mount at the container
		// level — the workspace mount alone doesn't give this init container
		// filesystem access to them. Mirror the same volumes/targets the main
		// container gets (by index, so the names match what the loop below
		// creates) so `sciontool provision` can mkdir+chown them too. Out of
		// scope here: dirs served by server.shared_dir_storage's own NFS
		// mechanism (SharedDirStorage.Serves below) — a separate subsystem,
		// not implicated in F-111.
		initVolumeMounts := []corev1.VolumeMount{initWorkspaceMount}
		// #2670: the sentinel and the provisioning lock live in the
		// project's provisioning state directory, mounted next to the
		// workspace (init containers only).
		stateMount, stateEnv, err := nfsProvisionStateInitMount(config, nfsAgentDir)
		if err != nil {
			return nil, fmt.Errorf("workspace-provision init container: %w", err)
		}
		if stateMount != nil {
			initVolumeMounts = append(initVolumeMounts, *stateMount)
		}
		// F-111 review (tf-lead nit): SCION_SHARED_DIR_PATHS carries
		// "name=mountPath" pairs, comma-joined — keyed explicitly by each
		// shared dir's own name (nfsSharedDirMount.Name), not derived from
		// the path via filepath.Base on the consuming side. Basenames are
		// unique today, but two shared dirs could produce the same basename
		// through different target shapes (one InWorkspace, one not); naming
		// the key here instead of reconstructing it there removes that risk
		// rather than relying on it never happening.
		var sharedDirPairs []string
		sharedMounts, err := nfsSharedDirInitMounts(config)
		if err != nil {
			return nil, fmt.Errorf("workspace-provision init container: %w", err)
		}
		for _, sm := range sharedMounts {
			initVolumeMounts = append(initVolumeMounts, sm.Mount)
			sharedDirPairs = append(sharedDirPairs, sm.Name+"="+sm.Mount.MountPath)
		}

		initEnv := nfsProvisionEnv(initGitClone)
		if stateEnv != nil {
			initEnv = append(initEnv, *stateEnv)
		}
		// SCION_PROJECT_ID lets the init container's own provisioning logs
		// (cmd/sciontool/commands/provision.go) identify which project they're
		// provisioning, instead of falling back to "unknown" — unconditional
		// and independent of GitCloneForInit, since every NFS-backed pod has a
		// ProjectID regardless of whether the project is git-backed.
		initEnv = append(initEnv, corev1.EnvVar{
			Name:  "SCION_PROJECT_ID",
			Value: config.ProjectID,
		})
		if len(sharedDirPairs) > 0 {
			initEnv = append(initEnv, corev1.EnvVar{
				Name:  "SCION_SHARED_DIR_PATHS",
				Value: strings.Join(sharedDirPairs, ","),
			})
		}
		// Worktree-per-agent: tell sciontool provision to add this agent's
		// worktree after the shared checkout is ready. Env rather than
		// flags, so an older sciontool ignores them and still provisions
		// the shared checkout (the agent container's clone step then fills
		// the agent's directory).
		if nfsWorktree {
			initEnv = append(initEnv,
				corev1.EnvVar{Name: "SCION_WORKSPACE_MODE", Value: string(store.SharingModeWorktreePerAgent)},
				corev1.EnvVar{Name: "SCION_AGENT_SLUG", Value: config.NFSWorktreeName},
				corev1.EnvVar{Name: "SCION_AGENT_BRANCH", Value: config.NFSWorktreeBranch},
			)
		}
		// Clone-per-agent: tell sciontool provision to prepare this agent's
		// directory instead of the shared checkout. An older sciontool
		// ignores these and, with no clone settings, only creates and
		// chowns the agent's directory; the agent container then clones.
		// Empty-per-agent: the same agent directory, with an empty
		// workspace and no branch.
		if nfsAgentDir && config.NFSAgentDirEmpty {
			initEnv = append(initEnv,
				corev1.EnvVar{Name: "SCION_WORKSPACE_MODE", Value: string(store.SharingModeEmptyPerAgent)},
				corev1.EnvVar{Name: "SCION_AGENT_SLUG", Value: config.NFSAgentDirName},
			)
		} else if nfsAgentDir {
			initEnv = append(initEnv,
				corev1.EnvVar{Name: "SCION_WORKSPACE_MODE", Value: string(store.SharingModeClonePerAgent)},
				corev1.EnvVar{Name: "SCION_AGENT_SLUG", Value: config.NFSAgentDirName},
				corev1.EnvVar{Name: "SCION_AGENT_BRANCH", Value: config.NFSAgentBranch},
			)
		}
		// When the broker created the workspace (and claim shared-dir)
		// directories itself, or found them with setgid and group write,
		// agents reach them through their group,
		// so a chown the export does not allow (root mapped to an anonymous
		// user) is logged instead of failing the pod. Only the provisioning
		// (lock-winner) container runs the chown. An env var rather than a
		// flag, so an older sciontool simply ignores it and keeps the strict
		// behavior instead of rejecting an unknown flag.
		if config.NFSWorkspacePreCreated && !waitOnly {
			initEnv = append(initEnv, corev1.EnvVar{
				Name:  provision.ChownBestEffortEnv,
				Value: "1",
			})
		}

		// F-111: chown needs CAP_CHOWN, and fixing a root-owned directory
		// left by a prior kubelet auto-create needs CAP_DAC_OVERRIDE/
		// CAP_FOWNER too — none of which a uid-1000, Drop:ALL container has.
		// The pod's own securityContext sets RunAsUser=1000/RunAsNonRoot=true
		// (design §9.1), so this container must override both at the
		// container level to run as root at all. All three capabilities
		// (CHOWN, FOWNER, DAC_OVERRIDE) are in GKE Autopilot's default
		// allowed set, as is running a container as root — Autopilot's
		// warden rejects capabilities outside that set, not root itself
		// (https://docs.cloud.google.com/kubernetes-engine/docs/concepts/autopilot-security,
		// "Security context and workload identity" — allowed capabilities
		// include chown/dac_override/fowner; "Autopilot allows running as
		// root to enable most workloads"). Only the WINNER container needs
		// this: the wait-for-sentinel (loser) container only os.Stats a
		// file, so it keeps the minimal, fully-dropped, non-root default.
		initSecurityContext := &corev1.SecurityContext{
			AllowPrivilegeEscalation: &allowPrivilegeEscalation,
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
		}
		if !waitOnly {
			initSecurityContext.RunAsUser = int64Ptr(0)
			initSecurityContext.RunAsGroup = int64Ptr(0)
			initSecurityContext.RunAsNonRoot = boolPtr(false)
			initSecurityContext.Capabilities.Add = []corev1.Capability{"CHOWN", "FOWNER", "DAC_OVERRIDE"}
		}

		initContainer := corev1.Container{
			Name:            k8sWorkspaceProvisionContainer,
			Image:           config.Image,
			Command:         initCommand,
			Env:             initEnv,
			VolumeMounts:    initVolumeMounts,
			SecurityContext: initSecurityContext,
		}
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, initContainer)
	}

	if nfsHome {
		// The home init containers run first: the home directory must
		// exist before any container mounts it.
		pod.Spec.InitContainers = append(homeParts.initContainers, pod.Spec.InitContainers...)
		if homeParts.volume != nil {
			pod.Spec.Volumes = append(pod.Spec.Volumes, *homeParts.volume)
		}
		grace := int64(homeTerminationGrace(config.HomeStorage))
		pod.Spec.TerminationGracePeriodSeconds = &grace
		onRootMismatch := corev1.FSGroupChangeOnRootMismatch
		pod.Spec.SecurityContext.FSGroupChangePolicy = &onRootMismatch
	}

	// Append secret volumes and mounts
	if len(extraVolumes) > 0 {
		pod.Spec.Volumes = append(pod.Spec.Volumes, extraVolumes...)
	}
	if len(extraVolumeMounts) > 0 {
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, extraVolumeMounts...)
	}

	// Apply resource requests/limits from the resolved spec and
	// kubernetes.resources. Default requests fill only resources with neither a
	// request nor a limit (see buildK8sResourceRequirements).
	var k8sResources *api.K8sResources
	if config.Kubernetes != nil {
		k8sResources = config.Kubernetes.Resources
	}
	containerResources, err := buildK8sResourceRequirements(config.Resources, k8sResources)
	if err != nil {
		return nil, err
	}
	pod.Spec.Containers[0].Resources = containerResources

	// Process shared directories — mount shared-dir volumes.
	// Build a set of shared dir targets so we can skip them in the regular volume loop.
	//
	// NFS backend (N2-5): shared dirs are served from the SAME workspace NFS PVC
	// via subPath (e.g., "projects/<pid>/shared-dirs/<name>"), avoiding per-dir PVCs.
	// The workspace volume is already defined; we add additional subPath mounts.
	//
	// Local backend: each shared dir gets its own PVC (existing behavior, unchanged).
	k8sContainerWorkspace := config.ContainerWorkspace
	if k8sContainerWorkspace == "" {
		k8sContainerWorkspace = "/workspace"
	}
	sharedDirTargets := make(map[string]bool, len(config.SharedDirs))
	// server.shared_dir_storage backend=nfs takes precedence over the
	// existing workspace_storage:nfs shared-dir branch when both are set
	// (design deploy-config-explore §3.2.4). It applies per shared dir:
	// with per-dir backends the realization serves only the dirs on nfs.
	nfsSharedDirs := config.WorkspaceBackendName == "nfs" && config.NFSPVClaimName != ""
	for i, sd := range config.SharedDirs {
		target := fmt.Sprintf("/scion-volumes/%s", sd.Name)
		if sd.InWorkspace {
			target = fmt.Sprintf("%s/.scion-volumes/%s", k8sContainerWorkspace, sd.Name)
		}
		sharedDirTargets[target] = true

		if config.SharedDirStorage.Serves(sd.Name) {
			// shared_dir_storage nfs: mount the dedicated shared PVC by
			// subPath. Fail closed (design G5) rather than falling back to
			// an unclaimed/EmptyDir volume when the claim name is missing.
			if config.SharedDirStorage.PVClaimName == "" {
				return nil, fmt.Errorf(
					"shared dir %q: server.shared_dir_storage.nfs.shares[0].pv_name is empty; cannot mount shared dir", sd.Name)
			}
			sdSubPath, ok := config.SharedDirStorage.SubPaths[sd.Name]
			if !ok || sdSubPath == "" {
				return nil, fmt.Errorf("shared dir %q: no resolved subPath in server.shared_dir_storage", sd.Name)
			}
			volName := fmt.Sprintf("shared-dir-%d", i)

			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: config.SharedDirStorage.PVClaimName,
						ReadOnly:  sd.ReadOnly,
					},
				},
			})
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: target,
				SubPath:   sdSubPath,
				ReadOnly:  sd.ReadOnly,
			})
		} else if nfsSharedDirs {
			// NFS backend: mount from the workspace PVC with a shared-dir subPath.
			// SubPath root mirrors the nfsBackend.Resolve layout:
			//   <SubPathRoot>/<projectID>/shared-dirs/<name>
			// F-111 review (BLOCKING): nfsSharedDirSubPath validates sd.Name and
			// the resulting path itself now — reject here too, independent of
			// pkg/agent/shared_dir_storage.go's own gate.
			sdSubPath, err := nfsSharedDirSubPath(config.NFSSubPath, sd.Name)
			if err != nil {
				return nil, err
			}
			volName := fmt.Sprintf("shared-dir-%d", i)

			// The volume source is the SAME NFS PVC as the workspace — but K8s
			// requires a separate Volume entry per unique (claimName, subPath)
			// pair in the pod spec, so we add the volume under a distinct name.
			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: config.NFSPVClaimName,
						ReadOnly:  sd.ReadOnly,
					},
				},
			})
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: target,
				SubPath:   sdSubPath,
				ReadOnly:  sd.ReadOnly,
			})
		} else {
			// Local backend: each shared dir gets its own PVC (existing behavior).
			projectName := projectkeys.ProjectNameFromLabels(config.Labels)
			pvcName := sharedDirPVCName(projectName, sd.Name)
			volName := fmt.Sprintf("shared-dir-%d", i)

			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvcName,
						ReadOnly:  sd.ReadOnly,
					},
				},
			})
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: target,
				ReadOnly:  sd.ReadOnly,
			})
		}
	}

	// Process Volumes
	var gcsVolumes []gcsVolumeInfo

	for i, v := range config.Volumes {
		switch v.Type {
		case "gcs":
			volName := fmt.Sprintf("gcs-vol-%d", i)
			attrs := map[string]string{
				"bucketName": v.Bucket,
			}
			if v.Mode != "" {
				attrs["mountOptions"] = v.Mode
			} else {
				attrs["mountOptions"] = "implicit-dirs"
			}

			pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
				Name: volName,
				VolumeSource: corev1.VolumeSource{
					CSI: &corev1.CSIVolumeSource{
						Driver:           "gcsfuse.csi.storage.gke.io",
						VolumeAttributes: attrs,
					},
				},
			})
			pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
				Name:      volName,
				MountPath: v.Target,
				ReadOnly:  v.ReadOnly,
			})

			pod.Annotations = ensureAnnotations(pod.Annotations)
			pod.Annotations["gke-gcsfuse/volumes"] = "true"

			gcsVolumes = append(gcsVolumes, gcsVolumeInfo{
				Source: v.Source,
				Target: v.Target,
				Bucket: v.Bucket,
				Prefix: v.Prefix,
			})
		default:
			// Skip shared dir volumes — they are handled via PVCs above.
			if sharedDirTargets[v.Target] {
				continue
			}
			// Local/bind-mount volumes are not supported on Kubernetes.
			// Log explicitly rather than silently ignoring.
			volType := v.Type
			if volType == "" {
				volType = "local"
			}
			runtimeLog.Warn("Volume type not supported on Kubernetes runtime, skipping",
				"type", volType, "source", v.Source, "target", v.Target)
		}
	}

	if len(gcsVolumes) > 0 {
		if data, err := json.Marshal(gcsVolumes); err == nil {
			encoded := base64.StdEncoding.EncodeToString(data)
			pod.Annotations = ensureAnnotations(pod.Annotations)
			pod.Annotations["scion.gcs_volumes"] = encoded
		}
	}

	if config.Kubernetes != nil {
		if config.Kubernetes.ServiceAccountName != "" {
			pod.Spec.ServiceAccountName = config.Kubernetes.ServiceAccountName
		}
		if config.Kubernetes.RuntimeClassName != "" {
			pod.Spec.RuntimeClassName = &config.Kubernetes.RuntimeClassName
		}
		if len(config.Kubernetes.NodeSelector) > 0 {
			pod.Spec.NodeSelector = config.Kubernetes.NodeSelector
		}
		if len(config.Kubernetes.Tolerations) > 0 {
			for _, t := range config.Kubernetes.Tolerations {
				pod.Spec.Tolerations = append(pod.Spec.Tolerations, corev1.Toleration{
					Key:      t.Key,
					Operator: corev1.TolerationOperator(t.Operator),
					Value:    t.Value,
					Effect:   corev1.TaintEffect(t.Effect),
				})
			}
		}
	}

	// Priority class: an explicit per-template/agent kubernetes.priorityClassName
	// wins over the runtime-level default (settings runtimes.<name>.priority_class_name,
	// carried on r.PriorityClassName). Unset at both levels leaves
	// pod.Spec.PriorityClassName unset — today's behaviour. Scion does not
	// create the PriorityClass object; it must already exist on the cluster.
	effectivePriorityClass := r.PriorityClassName
	prioritySource := "runtimes.<name>.priority_class_name"
	if config.Kubernetes != nil && config.Kubernetes.PriorityClassName != "" {
		effectivePriorityClass = config.Kubernetes.PriorityClassName
		prioritySource = "kubernetes.priorityClassName"
	}
	if effectivePriorityClass != "" {
		if errs := k8svalidation.IsDNS1123Subdomain(effectivePriorityClass); len(errs) > 0 {
			return nil, fmt.Errorf("invalid %s %q: %s", prioritySource, effectivePriorityClass, strings.Join(errs, "; "))
		}
		pod.Spec.PriorityClassName = effectivePriorityClass
	}

	// safe-to-evict: only an explicit false (resolved from the
	// template/agent kubernetes.safeToEvict, else the profile's, else the
	// runtime entry's safe_to_evict) adds the annotation. Nil and true add
	// nothing. The resolved value is authoritative for this key. The map
	// is cloned so the caller's config.Annotations is not modified.
	if k := config.Kubernetes; k != nil && k.SafeToEvict != nil && !*k.SafeToEvict {
		annotations := maps.Clone(pod.Annotations)
		if annotations == nil {
			annotations = make(map[string]string, 1)
		}
		if prev, ok := annotations[annotationSafeToEvict]; ok && prev != "false" {
			runtimeLog.Debug("buildPod: safeToEvict=false replaces the annotation from config", "pod", config.Name, "annotation", annotationSafeToEvict, "previous", prev)
		}
		annotations[annotationSafeToEvict] = "false"
		pod.Annotations = annotations
	}

	if nfsHome {
		rules, err := newHomeMountRules(containerHome, homeParts.mount, config.HomeStorage, provisionAgentDirMount)
		if err != nil {
			return nil, err
		}
		if err := checkHomeMounts(pod, rules); err != nil {
			return nil, err
		}
	}

	return pod, nil
}

// annotationSafeToEvict is the cluster-autoscaler pod annotation. "false"
// keeps the autoscaler from removing the node while the pod runs and, on GKE
// Autopilot, requests extended run duration for the pod.
const annotationSafeToEvict = "cluster-autoscaler.kubernetes.io/safe-to-evict"

// classifyTerminalWaitingReason turns a terminal (non-retryable)
// ContainerStateWaiting reason into an error, or returns nil if reason is
// not one of the terminal reasons. initContainerName should be the empty
// string for the main agent container, and the container's name for init
// containers — the resulting error names the init container so it can be
// told apart from the main container in logs. Shared by the init-container
// and main-container checks in waitForPodReady so their wording can't drift.
func classifyTerminalWaitingReason(podName, initContainerName, reason, message string) error {
	container := fmt.Sprintf("pod %q", podName)
	if initContainerName != "" {
		container = fmt.Sprintf("init container %q in pod %q", initContainerName, podName)
	}
	switch reason {
	case "ImagePullBackOff", "ErrImagePull":
		return fmt.Errorf("image pull failed for %s: %s — verify the image name and registry access (image pull policy: check kubernetes.imagePullPolicy)", container, message)
	case "InvalidImageName":
		return fmt.Errorf("invalid image name for %s: %s", container, message)
	case "CreateContainerConfigError":
		return fmt.Errorf("container configuration error for %s: %s — check secret references and volume mounts", container, message)
	case "CrashLoopBackOff":
		if initContainerName != "" {
			return fmt.Errorf("init container %q is crash-looping in pod %q: %s — check container logs with 'scion logs'", initContainerName, podName, message)
		}
		return fmt.Errorf("container is crash-looping in pod %q: %s — check container logs with 'scion logs'", podName, message)
	default:
		return nil
	}
}

// podTimingUnavailable marks a podLifecycleTimings field as not computable,
// e.g. because the pod lacks the corresponding condition or container state
// yet. Callers omit the log attribute rather than reporting a misleading 0.
const podTimingUnavailable int64 = -1

// podLifecycleTimings holds elapsed durations, in milliseconds, from a pod's
// CreationTimestamp to each lifecycle milestone. Computed from pod status
// fields already fetched by waitForPodReady's existing Get call — no extra
// API calls. Any field left at podTimingUnavailable means the corresponding
// condition or container state was missing (e.g. not yet reported) when the
// pod snapshot was taken.
//
// Precision note: CreationTimestamp and the condition/container timestamps
// below are all metav1.Time, serialized at whole-second (RFC3339) precision
// by the API server, scheduler, kubelet and container runtime respectively.
// So on a real cluster these values are always a multiple of 1000 with up to
// about ±1s of rounding error, despite the _ms suffix — they are not
// sub-second measurements.
type podLifecycleTimings struct {
	scheduledMs        int64
	initializedMs      int64
	containersReadyMs  int64
	containerStartedMs int64
}

// nonNegativeMs converts d to milliseconds, clamping negative results to 0.
// LastTransitionTime/StartedAt and CreationTimestamp are written by
// different components (apiserver, scheduler, kubelet, container runtime)
// on different clocks, all at whole-second precision, so clock skew plus
// truncation can occasionally make a "later" event look earlier than pod
// creation. Clamping avoids both logging a misleading negative duration and
// an unlucky -1ms colliding with the podTimingUnavailable sentinel.
func nonNegativeMs(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

// computePodLifecycleTimings derives podLifecycleTimings for containerName
// from pod's conditions and container statuses, relative to pod's
// CreationTimestamp. It is pure and side-effect free so it can be unit
// tested without a real (or fake) Kubernetes API server.
func computePodLifecycleTimings(pod *corev1.Pod, containerName string) podLifecycleTimings {
	t := podLifecycleTimings{
		scheduledMs:        podTimingUnavailable,
		initializedMs:      podTimingUnavailable,
		containersReadyMs:  podTimingUnavailable,
		containerStartedMs: podTimingUnavailable,
	}
	if pod == nil {
		return t
	}
	created := pod.CreationTimestamp.Time
	if created.IsZero() {
		return t
	}

	for _, cond := range pod.Status.Conditions {
		if cond.Status != corev1.ConditionTrue || cond.LastTransitionTime.IsZero() {
			continue
		}
		switch cond.Type {
		case corev1.PodScheduled:
			t.scheduledMs = nonNegativeMs(cond.LastTransitionTime.Sub(created))
		case corev1.PodInitialized:
			t.initializedMs = nonNegativeMs(cond.LastTransitionTime.Sub(created))
		case corev1.ContainersReady:
			t.containersReadyMs = nonNegativeMs(cond.LastTransitionTime.Sub(created))
		}
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != containerName {
			continue
		}
		if cs.State.Running != nil && !cs.State.Running.StartedAt.IsZero() {
			t.containerStartedMs = nonNegativeMs(cs.State.Running.StartedAt.Sub(created))
		}
	}

	return t
}

// logPodLifecycleTimings emits one Info log line with wait_ready_ms (the
// total time spent in waitForPodReady) plus any lifecycle milestones that
// computePodLifecycleTimings could determine, omitting unavailable ones.
func logPodLifecycleTimings(namespace, podName string, waitMs int64, t podLifecycleTimings) {
	attrs := []any{"pod", podName, "namespace", namespace, "phase", "wait-schedule", "wait_ready_ms", waitMs}
	if t.scheduledMs != podTimingUnavailable {
		attrs = append(attrs, "scheduled_ms", t.scheduledMs)
	}
	if t.initializedMs != podTimingUnavailable {
		attrs = append(attrs, "initialized_ms", t.initializedMs)
	}
	if t.containersReadyMs != podTimingUnavailable {
		attrs = append(attrs, "containers_ready_ms", t.containersReadyMs)
	}
	if t.containerStartedMs != podTimingUnavailable {
		attrs = append(attrs, "container_started_ms", t.containerStartedMs)
	}
	runtimeLog.Info("Pod ready", attrs...)
}

func (r *KubernetesRuntime) waitForPodReady(ctx context.Context, namespace, podName string) error {
	waitStart := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute) // GKE Autopilot can be slow
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	lastStatus := ""
	autopilotWaitLogged := false

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

			// F-111 review (tf-lead): init container failures (most notably
			// workspace-provision, whose whole job is now a fatal chown —
			// RequireChownSuccess, F-111) were invisible here: this function
			// only ever inspected the MAIN container's status, so a failed
			// init container just sat as "PodInitializing" until the full
			// 10-minute timeout above fired with a generic, unhelpful error.
			// Name the failed init container and its actual exit reason
			// immediately instead.
			for _, ics := range pod.Status.InitContainerStatuses {
				if ics.State.Terminated != nil && ics.State.Terminated.ExitCode != 0 {
					runtimeLog.Error("Init container failed", "pod", podName, "container", ics.Name,
						"exitCode", ics.State.Terminated.ExitCode, "reason", ics.State.Terminated.Reason,
						"message", ics.State.Terminated.Message, "phase", "init-container")
					return fmt.Errorf("init container %q failed in pod %q: exit code %d (%s): %s",
						ics.Name, podName, ics.State.Terminated.ExitCode, ics.State.Terminated.Reason, ics.State.Terminated.Message)
				}
				if ics.State.Waiting != nil {
					if err := classifyTerminalWaitingReason(podName, ics.Name, ics.State.Waiting.Reason, ics.State.Waiting.Message); err != nil {
						runtimeLog.Error("Init container failed", "pod", podName, "container", ics.Name,
							"reason", ics.State.Waiting.Reason, "message", ics.State.Waiting.Message, "phase", "init-container")
						return err
					}
				}
			}

			// Check container statuses for more detail
			var containerStatus *corev1.ContainerStatus
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.Name == agentContainerName {
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
				message := containerStatus.State.Waiting.Message
				switch reason {
				case "ImagePullBackOff", "ErrImagePull":
					runtimeLog.Error("Image pull failed", "pod", podName, "reason", reason, "message", message, "phase", "image-pull")
					return classifyTerminalWaitingReason(podName, "", reason, message)
				case "InvalidImageName":
					runtimeLog.Error("Invalid image name", "pod", podName, "message", message, "phase", "image-pull")
					return classifyTerminalWaitingReason(podName, "", reason, message)
				case "CreateContainerConfigError":
					runtimeLog.Error("Container config error", "pod", podName, "message", message, "phase", "container-config")
					return classifyTerminalWaitingReason(podName, "", reason, message)
				case "CrashLoopBackOff":
					runtimeLog.Error("Container crash loop", "pod", podName, "message", message, "phase", "crash-loop")
					return classifyTerminalWaitingReason(podName, "", reason, message)
				case "Unschedulable":
					if r.isGKEScheduling() {
						runtimeLog.Info("Pod unschedulable (GKE Autopilot will auto-provision nodes)", "pod", podName, "message", message, "phase", "scheduling")
					} else {
						runtimeLog.Error("Pod unschedulable", "pod", podName, "message", message, "phase", "scheduling")
						return fmt.Errorf("pod %q cannot be scheduled: %s — check node selectors, tolerations, and resource availability", podName, message)
					}
				}
			}

			// Check pod-level conditions for scheduling failures
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == "Unschedulable" {
					if r.isGKEScheduling() {
						// On GKE Autopilot, Unschedulable is transient — the cluster
						// will auto-provision nodes. Continue waiting instead of failing.
						if !autopilotWaitLogged {
							runtimeLog.Info("Pod unschedulable (GKE Autopilot will auto-provision nodes)", "pod", podName, "message", cond.Message, "phase", "scheduling")
							fmt.Printf("  Waiting for GKE Autopilot to provision node capacity...\n")
							autopilotWaitLogged = true
						}
					} else {
						runtimeLog.Error("Pod unschedulable", "pod", podName, "message", cond.Message, "phase", "scheduling")
						return fmt.Errorf("pod %q cannot be scheduled: %s — check node selectors, tolerations, and resource availability", podName, cond.Message)
					}
				}
			}

			if pod.Status.Phase == corev1.PodRunning {
				// Also ensure container is actually running
				if containerStatus != nil && containerStatus.State.Running != nil {
					logPodLifecycleTimings(namespace, podName, time.Since(waitStart).Milliseconds(),
						computePodLifecycleTimings(pod, agentContainerName))
					return nil
				}
			}
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				if containerStatus != nil && containerStatus.State.Terminated != nil {
					return fmt.Errorf("pod failed to start: %s - %s", containerStatus.State.Terminated.Reason, containerStatus.State.Terminated.Message)
				}
				return fmt.Errorf("pod terminated with status: %s", pod.Status.Phase)
			}
		}
	}
}

// countingReader wraps an io.Reader and counts the bytes read through it.
// Used to measure the size of the tar stream sent to a pod during sync
// without needing to buffer it.
type countingReader struct {
	r     io.Reader
	count int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.count += int64(n)
	return n, err
}

// syncArchiveCreateArgs returns the broker-side tar arguments syncToPod uses
// to archive sourcePath. Each exclude is a path relative to sourcePath that
// is left out of the archive; it is matched literally.
func syncArchiveCreateArgs(sourcePath string, excludes ...string) []string {
	args := []string{"-cz"}
	for _, e := range excludes {
		args = append(args, "--exclude="+tarLiteralPattern("./"+path.Clean(e)))
	}
	return append(args, "-C", sourcePath, ".")
}

// tarLiteralPattern quotes the tar wildcard characters in p with a backslash so that an
// exclude pattern matches p only.
func tarLiteralPattern(p string) string {
	var b strings.Builder
	for _, c := range p {
		switch c {
		case '\\', '*', '?', '[', ']':
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// syncArchiveExtractCommand returns the shell command syncToPod runs in the
// pod to extract the archive into destPath. It runs as the pod user (the
// pod securityContext sets a non-root RunAsUser), so every directory it
// writes into must be writable by that user. -m avoids utime errors on the
// mount point.
func syncArchiveExtractCommand(destPath string) string {
	return fmt.Sprintf("tar -xz -m --no-same-owner --no-same-permissions -C '%s'", destPath)
}

func (r *KubernetesRuntime) syncToPod(ctx context.Context, namespace, podName, sourcePath, destPath string, excludes ...string) error {
	// Guard against fake/test clientsets where Config is nil (no real API
	// server), same as execInPod. Also guard Client itself: a KubernetesRuntime
	// built as a literal rather than via NewKubernetesRuntime has a nil
	// Client, and r.Client.Config would panic before ever reaching the
	// Config check. Without either guard, a test or caller hitting this path
	// with no exec transport gets a nil-pointer panic from the REST client
	// deep inside the SPDY executor setup below instead of a clear error.
	if r.Client == nil || r.Client.Config == nil {
		return fmt.Errorf("K8s REST config not available (test environment)")
	}
	syncStart := time.Now()
	fmt.Printf("  Preparing tar archive from %s...\n", sourcePath)
	tarCmd := exec.CommandContext(ctx, "tar", syncArchiveCreateArgs(sourcePath, excludes...)...)
	tarCmd.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	stdout, err := tarCmd.StdoutPipe()
	if err != nil {
		return err
	}
	counted := &countingReader{r: stdout}

	if err := tarCmd.Start(); err != nil {
		return err
	}

	cmd := []string{"sh", "-c", syncArchiveExtractCommand(destPath)}

	req := r.Client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	option := &corev1.PodExecOptions{
		Container: agentContainerName,
		Command:   cmd,
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
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
		Stdin:  counted,
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
	runtimeLog.Info("Sync to pod complete", "pod", podName, "dest", destPath,
		"elapsed_ms", time.Since(syncStart).Milliseconds(), "bytes", counted.count)
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
		Container: agentContainerName,
		Command:   cmd,
		Stdin:     false,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
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
	_ = stdin.Close()
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

// Stop is Delete on Kubernetes: ref.RunID is enforced by Delete (a pod of
// another run is left untouched and ErrRunMismatch is returned).
func (r *KubernetesRuntime) Stop(ctx context.Context, ref RunRef) error {
	return r.Delete(ctx, ref)
}

// Delete removes the pod ref.ID and its secrets. ref.ID is the pod name, or
// namespace/pod.
//
// When ref.RunID is set, only that run's pod and objects are removed, each
// with a UID precondition (see deleteRun and k8s_run_scope.go): a pod of
// another run is left untouched and ErrRunMismatch is returned. Without a
// run ID the pod and per-agent objects are removed by name, as before run
// IDs existed.
func (r *KubernetesRuntime) Delete(ctx context.Context, ref RunRef) error {
	id := ref.ID
	var namespace string

	// Support namespace/pod format
	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		namespace = parts[0]
		id = parts[1]
	} else {
		namespace = r.resolveNamespace(ctx, id)
	}

	if ref.RunID != "" {
		return r.deleteRun(ctx, namespace, id, ref.RunID)
	}

	// Clean up agent secrets and SecretProviderClasses before deleting the
	// pod. id is the pod name regardless of whether the pod itself still
	// exists, so this is scoped to the right agent and project even when
	// the pod was already gone (see cleanupAgentSecrets).
	r.cleanupAgentSecrets(ctx, namespace, id)

	// 'id' is the pod name. Delete is used for removal (scion rm, stop),
	// not graceful shutdown, so pods are deleted immediately, except
	// NFS-home pods, which are deleted with their grace period so they stop
	// writing to the home before another pod of the agent starts (see
	// podDeleteOptions). Either way the call returns once the delete is
	// accepted. If the pod cannot be read, the graceful delete is used.
	pods := r.Client.Clientset.CoreV1().Pods(namespace)
	opts := metav1.DeleteOptions{}
	pod, getErr := pods.Get(ctx, id, metav1.GetOptions{})
	switch {
	case k8serrors.IsNotFound(getErr):
		return nil
	case getErr == nil:
		opts = podDeleteOptions(pod)
	}
	err := pods.Delete(ctx, id, opts)
	if err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete pod: %w", err)
	}
	return nil
}

// labelStartID is the label Run puts on every object it creates, holding an
// ID unique to that one start. It identifies the objects a particular start
// owns, which the deterministic per-agent names cannot: a newer agent created
// with the same name reuses those names.
const labelStartID = "scion.start_id"

// startCleanupTimeout bounds the cleanup Run performs for a start that did
// not complete.
const startCleanupTimeout = 30 * time.Second

// cleanupStartResources deletes the per-agent objects created by the start
// identified by startID: the scion-agent-<name> and scion-auth-<name>
// Secrets, the SecretProviderClass (GKE mode) and, when includePod is set,
// the pod. Objects are found by listing with a labelStartID selector and are
// deleted with a UID precondition, so an object belonging to another start of
// the same agent name (in particular a newer agent created after this one was
// deleted) is never removed. Listing rather than reading by name keeps the
// cleanup within the create/list/delete permissions the runtime already
// needs. Shared-dir PVCs are project-scoped and are never deleted here.
func (r *KubernetesRuntime) cleanupStartResources(ctx context.Context, namespace, agentName, startID string, includePod bool) {
	selector := metav1.ListOptions{LabelSelector: labelStartID + "=" + startID}
	warn := func(kind, name string, err error) {
		if err != nil && !k8serrors.IsNotFound(err) && !k8serrors.IsConflict(err) {
			runtimeLog.Warn("Failed to delete object of an incomplete start",
				"kind", kind, "name", name, "agent", agentName, "namespace", namespace, "error", err)
		}
	}
	removed := r.deleteAgentSecretsBySelector(ctx, namespace, agentName, selector.LabelSelector, warn)
	// deleted records the outcome of one delete call.
	deleted := func(kind, name string, err error) {
		if err == nil {
			removed++
			return
		}
		warn(kind, name, err)
	}
	uidPrecondition := k8sUIDPrecondition

	if includePod {
		pods := r.Client.Clientset.CoreV1().Pods(namespace)
		if list, err := pods.List(ctx, selector); err != nil {
			warn("Pod", agentName, err)
		} else {
			for i := range list.Items {
				p := &list.Items[i]
				if p.Name != agentName {
					continue
				}
				opts := podDeleteOptions(p)
				opts.Preconditions = uidPrecondition(p.UID)
				deleted("Pod", agentName, pods.Delete(ctx, agentName, opts))
			}
		}
	}
	if removed > 0 {
		runtimeLog.Info("Removed objects of an incomplete start",
			"agent", agentName, "namespace", namespace, "count", removed)
	} else {
		runtimeLog.Debug("No objects of an incomplete start to remove",
			"agent", agentName, "namespace", namespace)
	}
}

// cleanupStalePod deletes an existing pod with the given name if it exists.
// This prevents "already exists" errors when recreating an agent. Other
// pods are deleted immediately and failures are only logged, as before. An
// NFS-home pod is deleted with its grace period and the start waits until
// its containers are confirmed stopped (waitForPodTermination), so the new
// pod never writes to the home while the old one still does; a pod that
// cannot be confirmed stopped fails the start with a retryable error.
//
// nfsHome reports whether this start uses an NFS home (its home storage
// backend). A start with an NFS home, or with home storage set, never
// force-deletes a previous pod it could not read. hs supplies the
// configured termination wait (nil uses the default); the grace period is
// the previous pod's own.
func (r *KubernetesRuntime) cleanupStalePod(ctx context.Context, namespace, podName string, nfsHome bool, hs *HomeStorageRealization) error {
	pods := r.Client.Clientset.CoreV1().Pods(namespace)
	pod, err := pods.Get(ctx, podName, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return nil
	}
	if err != nil && (nfsHome || hs != nil) {
		// An NFS-home start never force-deletes a pod it could not read:
		// it may be a previous pod of this agent still writing to the home.
		return fmt.Errorf("%w: cannot read the previous pod %s: %v", errPreviousPodUnconfirmed, podName, err)
	}
	if err == nil && isNFSHomePod(pod) {
		if derr := pods.Delete(ctx, podName, podDeleteOptions(pod)); derr != nil && !k8serrors.IsNotFound(derr) {
			return fmt.Errorf("%w: failed to delete the previous pod %s: %v", errPreviousPodUnconfirmed, podName, derr)
		}
		bound := nfsHomeTerminationBound(pod, hs)
		runtimeLog.Info("Waiting for the previous pod to stop", "pod", podName, "namespace", namespace, "bound", bound.String(), "phase", "home-wait")
		return r.waitForPodTermination(ctx, namespace, podName, pod.UID, bound)
	}
	gracePeriod := int64(0)
	err = pods.Delete(ctx, podName, metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
	})
	if err != nil && !k8serrors.IsNotFound(err) {
		runtimeLog.Debug("Failed to clean up stale pod", "pod", podName, "namespace", namespace, "error", err)
	}
	return nil
}

// k8sDisruptionExitReason inspects a pod for signs that it was removed by a
// Kubernetes-initiated disruption rather than a normal stop or a container
// crash: the pod-level status reason "Evicted" (kubelet node-pressure
// eviction), or a DisruptionTarget condition. Returns "" when neither signal
// is present. Callers should only use the result once the pod has actually
// reached a terminal phase, or is already committed to termination (a
// non-nil DeletionTimestamp alongside a live DisruptionTarget condition).
func k8sDisruptionExitReason(pod *corev1.Pod) string {
	if pod.Status.Reason == "Evicted" {
		return string(state.ExitReasonEvicted)
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type != corev1.DisruptionTarget || cond.Status != corev1.ConditionTrue {
			continue
		}
		if cond.Reason == corev1.PodReasonPreemptionByScheduler {
			return string(state.ExitReasonPreempted)
		}
		// TerminationByKubelet, EvictionByEvictionAPI, or any other
		// DisruptionTarget reason (for example a taint-manager or pod-GC
		// removal) is reported as evicted.
		return string(state.ExitReasonEvicted)
	}
	return ""
}

func (r *KubernetesRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	namespace := r.DefaultNamespace
	// When ListAllNamespaces is enabled, query across all namespaces
	if r.ListAllNamespaces {
		namespace = ""
	}

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
		var exitCode *int
		var exitReason string
		switch p.Status.Phase {
		case corev1.PodSucceeded:
			agentStatus = string(state.PhaseStopped)
		case corev1.PodFailed:
			agentStatus = string(state.PhaseError)
		case corev1.PodPending, corev1.PodRunning, corev1.PodUnknown:
			// Non-terminal pod phases are represented via ContainerStatus and
			// local agent-info state until the agent exits.
		}

		// Try to get more detail from container status
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == agentContainerName {
				if cs.State.Waiting != nil {
					status = fmt.Sprintf("%s (%s)", p.Status.Phase, cs.State.Waiting.Reason)
				} else if cs.State.Terminated != nil {
					status = fmt.Sprintf("%s (%s)", p.Status.Phase, cs.State.Terminated.Reason)
					ec := int(cs.State.Terminated.ExitCode)
					exitCode = &ec
					if ec != 0 {
						exitReason = string(state.ExitReasonCrashed)
					}
					if agentStatus == "" {
						if cs.State.Terminated.ExitCode == 0 {
							agentStatus = string(state.PhaseStopped)
						} else {
							agentStatus = string(state.PhaseError)
						}
					}
				}
				break
			}
		}

		// A pod that reached a terminal phase may have gotten there through a
		// Kubernetes-initiated disruption rather than a normal stop or a
		// container crash. Check only once the pod is terminal (agentStatus
		// is stopped or error): a DisruptionTarget condition can appear on a
		// pod that is still running out its grace period, and that pod must
		// not be reported as preempted/evicted before it actually is —
		// unless it is already committed to termination (deletionTimestamp),
		// which the branch below covers.
		if agentStatus == string(state.PhaseStopped) || agentStatus == string(state.PhaseError) {
			if reason := k8sDisruptionExitReason(&p); reason != "" {
				exitReason = reason
			}
		} else if p.DeletionTimestamp != nil {
			// Scheduler preemption and Eviction API deletions remove the pod
			// object outright once termination completes — often before any
			// heartbeat observes a terminal phase at all, since List() polls
			// rather than watches. A pod with a deletionTimestamp and a live
			// DisruptionTarget condition is already committed to that
			// termination, so report the reason now, ahead of it actually
			// stopping. agentStatus (the reported Phase) is deliberately
			// left alone — this pod has not stopped yet.
			if reason := k8sDisruptionExitReason(&p); reason != "" {
				exitReason = reason
			}
		}

		projectPath := projectkeys.ProjectPathFromLabels(p.Annotations)
		if projectPath == "" {
			projectPath = projectkeys.ProjectPathFromLabels(p.Labels)
		}

		var agentImage string
		for _, c := range p.Spec.Containers {
			if c.Name == agentContainerName {
				agentImage = c.Image
				break
			}
		}

		agents = append(agents, api.AgentInfo{
			ContainerID:     p.Name, // Pod name serves as the container identifier
			RunID:           p.Labels[api.LabelRunID],
			Name:            p.Labels["scion.name"],
			Template:        p.Labels["scion.template"],
			Project:         projectkeys.ProjectNameFromLabels(p.Labels),
			ProjectID:       projectkeys.ProjectIDFromLabels(p.Labels),
			ProjectPath:     projectPath,
			Labels:          p.Labels,
			Annotations:     p.Annotations,
			ContainerStatus: status,
			Phase:           agentStatus,
			ExitCode:        exitCode,
			ExitReason:      exitReason,
			Image:           agentImage,
			Runtime:         r.Name(),
			Kubernetes: &api.AgentK8sMetadata{
				Namespace: p.Namespace,
				PodName:   p.Name,
				UID:       string(p.UID),
			},
		})
	}
	return agents, nil
}

func (r *KubernetesRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	var namespace string
	podName := id

	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		namespace = parts[0]
		podName = parts[1]
	} else {
		namespace = r.resolveNamespace(ctx, podName)
	}

	req := r.Client.Clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{Container: agentContainerName})
	podLogs, err := req.Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = podLogs.Close() }()

	data, err := io.ReadAll(podLogs)
	if err != nil {
		return "", err
	}

	return string(data), nil
}

func (r *KubernetesRuntime) Attach(ctx context.Context, id string) error {
	var namespace string
	podName := id

	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		namespace = parts[0]
	} else {
		namespace = r.resolveNamespace(ctx, podName)
	}

	// Find pod first to check status
	agents, err := r.List(ctx, map[string]string{"scion.name": id})
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	var agent *api.AgentInfo
	for _, a := range agents {
		if a.ContainerID == id || a.Name == id {
			agent = &a
			break
		}
	}

	if agent == nil {
		return fmt.Errorf("agent '%s' pod not found, it may have been deleted", id)
	}

	// Use the actual pod name (ContainerID) which may include a project prefix
	// (e.g., "sciontest--hello" instead of just "hello")
	podName = agent.ContainerID

	// For Kubernetes, we want to ensure it is in Running phase
	if agent.Phase != string(state.PhaseRunning) {
		return fmt.Errorf("agent '%s' is not running (status: %s), use 'scion start %s' to resume it", id, agent.ContainerStatus, id)
	}

	fmt.Printf("Attaching to pod '%s' (use Ctrl-b d to detach)...\n", podName)

	req := r.Client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	// Determine the container username so we attach as the correct user
	// (K8s exec has no --user flag; we use su to switch from root).
	username := "scion"
	if u, ok := agent.Annotations["scion.username"]; ok && u != "" {
		username = u
	}

	// Validate username to prevent shell injection via pod annotations.
	if !ValidExecUserName.MatchString(username) {
		return fmt.Errorf("invalid username in pod annotation: %q", username)
	}

	// Wrap the exec command with the whoami-skip-su helper so it works
	// on both root-entrypoint images (where su is needed) and non-root
	// entrypoint images (where su would prompt for a password). See
	// ExecAsUserCmd godoc for the underlying PAM rationale.
	execCmd := ExecAsUserCmd(username, "tmux attach -t scion")

	option := &corev1.PodExecOptions{
		Container: agentContainerName,
		Command:   execCmd,
		Stdin:     true,
		Stdout:    true,
		Stderr:    true,
		TTY:       true,
	}
	req.VersionedParams(option, scheme.ParameterCodec)
	realStdin := os.Stdin

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
		defer func() { _ = term.Restore(fd, oldState) }()
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
		Stdin:             realStdin,
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

func (r *KubernetesRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	// Validate image format before accepting it
	if image == "" {
		return false, fmt.Errorf("image name is empty")
	}
	if strings.ContainsAny(image, " \t\n") {
		return false, fmt.Errorf("image name %q contains whitespace", image)
	}
	// K8s pulls images if not present, so we can assume it "exists" or will be pulled.
	// Pull failures are caught during waitForPodReady with detailed error messages.
	return true, nil
}

func (r *KubernetesRuntime) ImageID(ctx context.Context, image string) (string, error) {
	// K8s doesn't have local images — images are pulled by the kubelet.
	return "", nil
}

func (r *KubernetesRuntime) RemoveImage(ctx context.Context, image string) error {
	// K8s doesn't have local images — image lifecycle is managed by the kubelet.
	return nil
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
		if a.ContainerID == id || a.Name == id {
			agent = &a
			break
		}
	}

	if agent == nil {
		return fmt.Errorf("agent '%s' pod not found", id)
	}

	// Check for GCS volumes
	if encoded := agent.Annotations["scion.gcs_volumes"]; encoded != "" {
		return syncGCSVolumes(ctx, encoded, direction)
	}

	workspacePath := agent.Annotations["scion.workspace"]
	if workspacePath == "" {
		return fmt.Errorf("agent '%s' does not have a workspace path recorded", id)
	}

	// Only a path that passes the fixed floors may be used here, as either a
	// sync source (broker disk -> agent, SyncTo direction) or a sync
	// destination (agent -> broker disk, SyncFrom direction). This is a
	// value read straight from a persisted pod annotation, not freshly
	// computed, so it needs this check independent of whatever validated it
	// (or didn't) when it was first written. No per-project root is
	// available at this call site — only agent annotations/labels are in
	// scope, with no project directory or settings to derive one from — so
	// only the fixed deny-set applies, not per-project root containment.
	resolvedWorkspacePath, err := ValidateWorkspaceSource(workspacePath, "")
	if err != nil {
		return err
	}
	workspacePath = resolvedWorkspacePath

	homeDir := agent.Annotations["scion.homedir"]
	username := agent.Annotations["scion.username"]

	// Same reasoning as workspacePath above: homeDir is a host path read
	// straight from a persisted annotation, used below as either a sync
	// destination (SyncFrom) or a sync source (SyncTo), and needs the same
	// check independent of whatever validated it when first written. This is
	// an agent's home directory, not a workspace, so it goes through
	// ValidateAgentHomeSource rather than ValidateWorkspaceSource: the two
	// admit disjoint shapes under ~/.scion (see ValidateAgentHomeSource's
	// doc comment). Agent homes under ~/.scion take one of three layouts
	// (global, hub-managed, or externalized git project); a home outside
	// ~/.scion entirely (a plain in-repo project's own agent home, for
	// example) is subject to the fixed floors only, the same as any other
	// source this function's sibling validates.
	if homeDir != "" {
		resolvedHomeDir, err := ValidateAgentHomeSource(homeDir, "")
		if err != nil {
			return err
		}
		homeDir = resolvedHomeDir
	}

	// Resolve namespace
	namespace := r.DefaultNamespace
	if ns, ok := agent.Labels["scion.namespace"]; ok {
		namespace = ns
	} else if ns, ok := agent.Labels["namespace"]; ok {
		namespace = ns
	}

	// Tar sync (Snapshot)
	if direction == SyncUnspecified {
		return fmt.Errorf("direction (to or from) must be specified for tar sync. Example: scion sync to %s", agent.ContainerID)
	}

	// The home of an NFS-home agent is kept on the export across starts;
	// it is not copied to or from the broker's copy.
	if homeDir != "" && homeKeptOnExport(agent.Annotations) {
		fmt.Printf("Agent home is kept on persistent storage; not syncing it.\n")
		homeDir = ""
	}

	if direction == SyncFrom {
		fmt.Printf("Syncing workspace (agent -> %s)...\n", workspacePath)
		if err := r.syncWithRetry(ctx, func() error {
			return r.syncTransferFn(ctx, namespace, agent.ContainerID, "/workspace", workspacePath, false)
		}); err != nil {
			return err
		}
		if homeDir != "" && username != "" {
			destHome := util.GetHomeDir(username)
			fmt.Printf("Syncing agent home (agent -> %s)...\n", homeDir)
			if err := r.syncWithRetry(ctx, func() error {
				return r.syncTransferFn(ctx, namespace, agent.ContainerID, destHome, homeDir, false)
			}); err != nil {
				return err
			}
		}
		return nil
	}

	fmt.Printf("Syncing workspace (%s -> agent)...\n", workspacePath)
	if err := r.syncWithRetry(ctx, func() error {
		return r.syncTransferFn(ctx, namespace, agent.ContainerID, workspacePath, "/workspace", true)
	}); err != nil {
		return err
	}
	if homeDir != "" && username != "" {
		destHome := util.GetHomeDir(username)
		fmt.Printf("Syncing agent home (%s -> agent)...\n", homeDir)
		if err := r.syncWithRetry(ctx, func() error {
			return r.syncTransferFn(ctx, namespace, agent.ContainerID, homeDir, destHome, true)
		}); err != nil {
			return err
		}
	}
	return nil
}

// syncTransferFn copies src to dest for Sync, from the broker to the pod
// when toPod is true and back otherwise.
func (r *KubernetesRuntime) syncTransferFn(ctx context.Context, namespace, podName, src, dest string, toPod bool) error {
	if r.syncTransfer != nil {
		return r.syncTransfer(ctx, namespace, podName, src, dest, toPod)
	}
	if toPod {
		return r.syncToPod(ctx, namespace, podName, src, dest)
	}
	return r.syncFromPod(ctx, namespace, podName, src, dest)
}

func (r *KubernetesRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	return r.execWithOptionalStdin(ctx, id, cmd, nil)
}

// ExecWithStdin runs cmd in the pod with stdin piped from the given reader,
// instead of embedding data in cmd's argv. Used to deliver secrets without
// exposing them via a process's command line. See #1355.
func (r *KubernetesRuntime) ExecWithStdin(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
	return r.execWithOptionalStdin(ctx, id, cmd, stdin)
}

// wrapExecStreamError builds the error execWithOptionalStdin returns for a
// failed exec stream. It normally embeds stderr for diagnostics, but when
// ctx is marked via WithSensitiveExec it omits stderr entirely: the target
// process's stderr, or (for a stdin-delivered call such as SendKeys's tmux
// invocation) its stdin, can carry caller-supplied content, and embedding it
// here would defeat suppression done anywhere else in the stack. Factored
// out from execWithOptionalStdin so it can be unit-tested without a real
// Kubernetes API server (see TestWrapExecStreamError_SensitiveOmitsStderr).
//
// execWithOptionalStdin's own call site is not separately covered: driving
// a real failing exec stream through remotecommand.NewSPDYExecutor needs a
// server speaking the Kubernetes exec subprotocol, not just a fake
// clientset, and that scaffolding was judged not worth adding for one call
// site that does nothing but forward to this already-tested helper.
func wrapExecStreamError(ctx context.Context, err error, stderr string) error {
	if IsSensitiveExec(ctx) {
		return fmt.Errorf("exec failed: %w", err)
	}
	return fmt.Errorf("exec failed: %w (stderr: %s)", err, stderr)
}

// execWithOptionalStdin is the shared implementation behind Exec and
// ExecWithStdin. stdin may be nil, in which case the exec has no stdin
// stream attached (the historical Exec behaviour).
func (r *KubernetesRuntime) execWithOptionalStdin(ctx context.Context, id string, cmd []string, stdin io.Reader) (string, error) {
	var namespace string
	podName := id

	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		namespace = parts[0]
		podName = parts[1]
	} else {
		namespace = r.resolveNamespace(ctx, podName)
	}

	req := r.Client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	// Wrap command to run as the scion user (K8s exec has no --user flag).
	// Shell-quote each argument to handle spaces and special characters,
	// then wrap with the whoami-skip-su helper so it works on images that
	// run as root (su needed) and images that run as scion (su would
	// prompt for a password — see ExecAsUserCmd godoc).
	quoted := make([]string, len(cmd))
	for i, arg := range cmd {
		quoted[i] = fmt.Sprintf("'%s'", strings.ReplaceAll(arg, "'", "'\"'\"'"))
	}
	suCmd := ExecAsUserCmd(r.ExecUser(), strings.Join(quoted, " "))

	option := &corev1.PodExecOptions{
		Container: agentContainerName,
		Command:   suCmd,
		Stdin:     stdin != nil,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}

	req.VersionedParams(
		option,
		scheme.ParameterCodec,
	)

	executor, err := remotecommand.NewSPDYExecutor(r.Client.Config, "POST", req.URL())
	if err != nil {
		return "", err
	}

	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
	})

	if err != nil {
		return stdout.String(), wrapExecStreamError(ctx, err, stderr.String())
	}

	return stdout.String(), nil
}

// execInPod runs a command in the pod's "agent" container as root (the default
// K8s exec user). This is used for administrative tasks like chown after syncing files.
func (r *KubernetesRuntime) execInPod(ctx context.Context, namespace, podName string, cmd []string) (string, error) {
	execStart := time.Now()
	// cmdName identifies the exec for logging without ever including its
	// arguments: call sites pass fixed command verbs (sh, touch) but never
	// secret or credential values, and this keeps it that way even if a
	// future call site's arguments did carry something sensitive.
	cmdName := ""
	if len(cmd) > 0 {
		cmdName = cmd[0]
	}

	if r.podExec != nil {
		return r.podExec(ctx, namespace, podName, cmd)
	}
	// Guard against fake/test clientsets where Config is nil (no real API server).
	if r.Client.Config == nil {
		return "", fmt.Errorf("K8s REST config not available (test environment)")
	}
	req := r.Client.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec")

	option := &corev1.PodExecOptions{
		Container: agentContainerName,
		Command:   cmd,
		Stdin:     false,
		Stdout:    true,
		Stderr:    true,
		TTY:       false,
	}
	req.VersionedParams(option, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(r.Client.Config, "POST", req.URL())
	if err != nil {
		return "", err
	}

	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		runtimeLog.Debug("exec in pod failed", "pod", podName, "cmd", cmdName,
			"elapsed_ms", time.Since(execStart).Milliseconds(), "error", err)
		return stdout.String(), fmt.Errorf("exec failed: %w (stderr: %s)", err, stderr.String())
	}
	runtimeLog.Debug("exec in pod complete", "pod", podName, "cmd", cmdName,
		"elapsed_ms", time.Since(execStart).Milliseconds())
	return stdout.String(), nil
}

// GetWorkspacePath returns the local workspace path for a Kubernetes pod.
// For K8s, this returns the workspace path stored in annotations when the pod was created.
func (r *KubernetesRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	var namespace string

	// Parse namespace from id if present (format: namespace/podname)
	if strings.Contains(id, "/") {
		parts := strings.SplitN(id, "/", 2)
		namespace = parts[0]
		id = parts[1]
	} else {
		namespace = r.resolveNamespace(ctx, id)
	}

	pod, err := r.Client.Clientset.CoreV1().Pods(namespace).Get(ctx, id, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("failed to get pod: %w", err)
	}

	// Check annotations for workspace path
	if workspace, ok := pod.Annotations["scion.workspace"]; ok && workspace != "" {
		return workspace, nil
	}

	return "", fmt.Errorf("no workspace path found for pod %s", id)
}

// nfsSharedDirSubPath computes the NFS subPath for a shared directory given the
// workspace subPath. The workspace subPath is like "projects/<pid>/workspace";
// shared dirs are siblings: "projects/<pid>/shared-dirs/<name>".
//
// This mirrors the nfsBackend.Resolve layout (design §5.3).
//
// F-111 review (tf-lead/tf-review-nfsfix, BLOCKING): sharedDirName is data —
// it can come from a cloned repo's in-repo settings.yaml — and
// filepath.Join silently collapses ".." segments before Kubernetes' own
// subPath escape check ever sees the resulting string. A name like
// "../../<other-project>/workspace" would resolve to another project's real
// workspace directory; since F-111 gave the winner init container CHOWN/
// FOWNER/DAC_OVERRIDE, that's not just a data leak, it's a cross-project
// ownership hijack (chown -R -h on someone else's tree). This is defense in
// depth alongside pkg/agent/shared_dir_storage.go's own validation gate
// (resolveSharedDirs, fails closed when workspace_storage.backend is nfs):
// this function refuses to build a bad subPath at all, independently, in
// case anything else ever reaches buildPod with unvalidated SharedDirs.
//
// Two independent checks, not one: api.ValidateSharedDirs rejects anything
// that isn't a valid slug (lowercase alphanumeric + internal hyphens only —
// which cannot produce a path separator or ".." by construction), and the
// joined-path-prefix check below is a second, structurally different gate
// that doesn't depend on the slug regex ever staying correct.
func nfsSharedDirSubPath(workspaceSubPath, sharedDirName string) (string, error) {
	if err := api.ValidateSharedDirs([]api.SharedDir{{Name: sharedDirName}}); err != nil {
		return "", fmt.Errorf("shared dir name %q: %w", sharedDirName, err)
	}
	// workspaceSubPath is "projects/<pid>/workspace"
	// We need "projects/<pid>/shared-dirs/<name>"
	parent := filepath.Dir(workspaceSubPath) // "projects/<pid>"
	wantDir := filepath.Join(parent, "shared-dirs")
	joined := filepath.Join(wantDir, sharedDirName)
	if !strings.HasPrefix(joined, wantDir+string(filepath.Separator)) {
		return "", fmt.Errorf("shared dir name %q: resolved subPath %q escapes %s", sharedDirName, joined, wantDir)
	}
	return joined, nil
}

// nfsSharedDirMount pairs a shared dir's own name (config.SharedDirs[i].Name,
// e.g. "scratchpad") with its init-container VolumeMount — kept together so
// callers never have to re-derive the name from the mount's k8s volume name
// or its MountPath (F-111 review, tf-lead: keying SCION_SHARED_DIR_PATHS off
// filepath.Base(mountPath) works today but silently collides if two shared
// dirs ever produced the same basename via different target shapes, e.g. one
// InWorkspace and one not; carrying the real name explicitly removes that
// risk rather than reconstructing it from a path).
type nfsSharedDirMount struct {
	Name  string
	Mount corev1.VolumeMount
}

// nfsSharedDirInitMounts returns the workspace-provision init container's
// additional VolumeMounts for shared dirs served from the workspace NFS PVC
// by subPath (F-111, design §9) — mirrors buildPod's own nfsSharedDirs branch
// below exactly (same volume names, by index, same subPath/target
// computation), so the volumes these mounts reference are guaranteed to
// exist in pod.Spec.Volumes once that branch runs later in the same buildPod
// call. Returns nil for any other shared-dir mechanism
// (server.shared_dir_storage's own NFS backend, or the local per-dir-PVC
// backend) — those are separate subsystems, not implicated in F-111.
func nfsSharedDirInitMounts(config RunConfig) ([]nfsSharedDirMount, error) {
	nfsSharedDirs := config.WorkspaceBackendName == "nfs" && config.NFSPVClaimName != ""
	if !nfsSharedDirs || len(config.SharedDirs) == 0 {
		return nil, nil
	}

	k8sContainerWorkspace := config.ContainerWorkspace
	if k8sContainerWorkspace == "" {
		k8sContainerWorkspace = "/workspace"
	}

	var mounts []nfsSharedDirMount
	for i, sd := range config.SharedDirs {
		// A dir served by server.shared_dir_storage nfs is not on the
		// workspace claim. The index stays the pod volume index.
		if config.SharedDirStorage.Serves(sd.Name) {
			continue
		}
		target := fmt.Sprintf("/scion-volumes/%s", sd.Name)
		if sd.InWorkspace {
			target = fmt.Sprintf("%s/.scion-volumes/%s", k8sContainerWorkspace, sd.Name)
		}
		subPath, err := nfsSharedDirSubPath(config.NFSSubPath, sd.Name)
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, nfsSharedDirMount{
			Name: sd.Name,
			Mount: corev1.VolumeMount{
				Name:      fmt.Sprintf("shared-dir-%d", i),
				MountPath: target,
				SubPath:   subPath,
			},
		})
	}
	return mounts, nil
}

// NFSWorktreeContainerPath is where the agent container mounts its own
// worktree in worktree-per-agent mode on the NFS backend, and its working
// directory: /repo-root/worktrees/<agent name>, the same path the local
// runtimes use.
func NFSWorktreeContainerPath(name string) string {
	return "/repo-root/worktrees/" + name
}

// nfsWorktreeSubPaths returns the subPaths of the shared checkout's .git and
// of the agent's worktree (<workspace>/worktrees/<agent name>) for
// worktree-per-agent mode. name must be the agent's slug (lower-case
// letters, digits and dashes), so it is always a single path segment.
func nfsWorktreeSubPaths(workspaceSubPath, name string) (gitSubPath, worktreeSubPath string, err error) {
	if slug, err := api.ValidateAgentName(name); err != nil || slug != name {
		return "", "", fmt.Errorf("worktree-per-agent: agent name %q is not an agent slug", name)
	}
	if workspaceSubPath == "" {
		return "", "", fmt.Errorf("worktree-per-agent: the NFS workspace subPath is empty")
	}
	// A Kubernetes subPath always uses forward slashes, whatever the OS.
	return path.Join(workspaceSubPath, ".git"), path.Join(workspaceSubPath, "worktrees", name), nil
}

// NFSAgentDirSubPath returns the export-relative path of a clone-per-agent
// agent's directory, <project>/agents/<agent name>, a sibling of the
// project's workspace path workspaceSubPath (<project>/workspace). name must
// be the agent's slug (lower-case letters, digits and dashes), so it is
// always a single path segment.
func NFSAgentDirSubPath(workspaceSubPath, name string) (string, error) {
	if slug, err := api.ValidateAgentName(name); err != nil || slug != name {
		return "", fmt.Errorf("clone-per-agent: agent name %q is not an agent slug", name)
	}
	if workspaceSubPath == "" || !filepath.IsLocal(workspaceSubPath) || filepath.Base(workspaceSubPath) != "workspace" || filepath.Dir(workspaceSubPath) == "." {
		return "", fmt.Errorf("clone-per-agent: unexpected NFS workspace subPath %q", workspaceSubPath)
	}
	return filepath.Join(filepath.Dir(workspaceSubPath), "agents", name), nil
}

// nfsInitContainerInjected reports whether buildPod would add the
// workspace-provision init container for this config — the same gate used
// there (F-111: nfs backend + a bound PV claim, independent of git config).
// Used by Run() to decide whether it's safe to skip the tar-based workspace
// sync (only true when something actually pre-populates the workspace).
func nfsInitContainerInjected(config RunConfig) bool {
	return config.WorkspaceBackendName == "nfs" && config.NFSPVClaimName != ""
}

// containerUID is the uid agent pods run as (pod RunAsUser/RunAsGroup):
// the image's non-root scion user. The NFS provisioning init container
// chowns the workspace to this uid so the agent owns what it clones.
const containerUID int64 = 1000

// nfsProvisionCommand builds the Command slice for the lock-winner init
// container. It invokes `sciontool provision` with numeric flags for depth
// and the workspace ownership uid/gid. URL and branch are passed via env vars
// (nfsProvisionEnv) to prevent shell injection.
//
// buildPod passes the identity the agent container runs as: uid is the pod
// RunAsUser (containerUID) and gid is the pod fsGroup (the resolved
// workspace_storage.nfs gid). A zero value omits the flag, so sciontool's
// own default of 1000 applies.
func nfsProvisionCommand(gc *api.GitCloneConfig, uid, gid int64) []string {
	cmd := []string{"sciontool", "provision"}
	if gc != nil && gc.URL != "" && gc.Depth != nil {
		cmd = append(cmd, "--depth", fmt.Sprintf("%d", *gc.Depth))
	}
	if uid != 0 {
		cmd = append(cmd, "--uid", strconv.FormatInt(uid, 10))
	}
	if gid != 0 {
		cmd = append(cmd, "--gid", strconv.FormatInt(gid, 10))
	}
	return cmd
}

// nfsProvisionEnv returns the environment variables for the NFS init
// container. URL and branch are passed as env vars to prevent shell injection.
func nfsProvisionEnv(gc *api.GitCloneConfig) []corev1.EnvVar {
	if gc == nil {
		return nil
	}
	envs := []corev1.EnvVar{
		{Name: "SCION_CLONE_URL", Value: gc.URL},
	}
	if gc.Branch != "" {
		envs = append(envs, corev1.EnvVar{Name: "SCION_CLONE_BRANCH", Value: gc.Branch})
	}
	return envs
}

// sharedDirSupplementalGroups returns the nfs shared-dir leaf groups
// (RunConfig.SharedDirStorage.SupplementalGroups, already guarded by
// pkg/agent) to add as pod supplementalGroups, so the agent can write files
// other agent kinds create in the leaf's group (ptone/scion#3155). A gid
// equal to fsGroup is skipped, since Kubernetes already adds fsGroup. Nil
// when the agent mounts no nfs shared dir, leaving the pod spec unchanged.
func sharedDirSupplementalGroups(config RunConfig, fsGroup int64) []int64 {
	if config.SharedDirStorage == nil || config.SharedDirStorage.Backend != "nfs" || len(config.SharedDirs) == 0 {
		return nil
	}
	var out []int64
	for _, gid := range config.SharedDirStorage.SupplementalGroups {
		if gid <= 0 || gid == fsGroup {
			continue
		}
		out = append(out, gid)
	}
	return out
}

// withSupplementalGIDsEnv removes every SupplementalGIDsEnvVar entry from
// env and, when groups is non-empty, appends the broker's own value listing
// exactly those gids (ptone/scion#3155).
func withSupplementalGIDsEnv(env []corev1.EnvVar, groups []int64) []corev1.EnvVar {
	// A new backing array (room for the broker's own entry), so the
	// caller's slice is never modified.
	out := make([]corev1.EnvVar, 0, len(env)+1)
	for _, ev := range env {
		if ev.Name != SupplementalGIDsEnvVar {
			out = append(out, ev)
		}
	}
	if len(groups) == 0 {
		return out
	}
	ids := make([]string, 0, len(groups))
	for _, gid := range groups {
		ids = append(ids, strconv.FormatInt(gid, 10))
	}
	return append(out, corev1.EnvVar{Name: SupplementalGIDsEnvVar, Value: strings.Join(ids, ",")})
}
