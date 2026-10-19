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
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	utilexec "k8s.io/client-go/util/exec"
)

// execDialRetryMaxWait bounds the cumulative time execInPodDialRetry spends
// retrying one exec. It is shorter than execReadyMaxWait: the exec tunnel
// has already answered once (waitForExecReady), so this only has to ride
// out a short drop while a freshly scaled node finishes joining.
const execDialRetryMaxWait = 60 * time.Second

// execDialRetryAttemptMargin is the time execInPodDialRetry leaves for the
// next exec itself before ctx's deadline. A retry that would start with
// less than this left is not made, so the step fails with the dial error
// rather than with a context deadline error from an attempt that had no
// time to finish.
const execDialRetryAttemptMargin = 5 * time.Second

// isExecDialTransientError reports whether err is the transient exec dial
// failure seen on a node that is still joining the cluster: the API server
// could not reach the kubelet ("error dialing backend", often with "No agent
// available" from the GKE konnectivity proxy). The command never started, so
// running it again cannot apply anything twice.
//
// The match is deliberately narrow. An error carrying an exit status means
// the command started and ran, so it is never retried, even if the
// command's stderr (which execInPod puts in the error text) contains the
// dial text. Authentication, authorization and not-found answers are never
// transient, even if their message carries the dial text, and neither are
// context errors. Other stream errors (EOF, timeouts, resets) are not
// retried here because they can happen after the command has already run.
func isExecDialTransientError(err error) bool {
	if err == nil {
		return false
	}
	var exitErr utilexec.ExitError
	if errors.As(err, &exitErr) {
		return false
	}
	if k8serrors.IsUnauthorized(err) || k8serrors.IsForbidden(err) || k8serrors.IsNotFound(err) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "error dialing backend") || strings.Contains(msg, "no agent available")
}

// execInPodDialRetry runs cmd in the pod with execInPod and retries it only
// while it fails with a transient dial error (isExecDialTransientError).
// Backoff is 1s, 2s, 4s, 8s, then held at execReadyMaxBackoff, for at most
// execDialRetryMaxWait in total (the last backoff is shortened to fit). It
// does not retry when the backoff plus execDialRetryAttemptMargin would
// reach ctx's deadline: the last dial error is returned at once, so the
// caller fails inside the launch deadline with the real cause. A cancelled
// ctx ends the wait immediately; the returned error wraps ctx.Err() and
// names the last dial error. Only cmd[0] and phase are logged, never the
// command's arguments.
func (r *KubernetesRuntime) execInPodDialRetry(ctx context.Context, namespace, podName string, cmd []string, agentName, phase string) (string, error) {
	clock := r.execReadyClock
	if clock.now == nil || clock.sleep == nil {
		clock = realExecReadyClock()
	}
	cmdName := ""
	if len(cmd) > 0 {
		cmdName = cmd[0]
	}
	logFields := func(extra ...any) []any {
		base := []any{"phase", phase, "agent", agentName, "namespace", namespace, "pod", podName, "cmd", cmdName}
		return append(base, extra...)
	}
	start := clock.now()
	for attempt := 1; ; attempt++ {
		out, err := r.execInPod(ctx, namespace, podName, cmd)
		if err == nil || !isExecDialTransientError(err) {
			return out, err
		}

		now := clock.now()
		elapsed := now.Sub(start)
		shift := attempt - 1
		if shift > execReadyMaxBackoffShift {
			shift = execReadyMaxBackoffShift
		}
		backoff := time.Duration(1<<uint(shift)) * time.Second
		if backoff > execReadyMaxBackoff {
			backoff = execReadyMaxBackoff
		}

		if remaining := execDialRetryMaxWait - elapsed; backoff > remaining {
			backoff = remaining
		}

		giveUp := ""
		if elapsed >= execDialRetryMaxWait {
			giveUp = "retry limit reached"
		} else if deadline, ok := ctx.Deadline(); ok && !now.Add(backoff+execDialRetryAttemptMargin).Before(deadline) {
			giveUp = "launch deadline too close"
		}
		if giveUp != "" {
			runtimeLog().Warn("Pod exec dial still failing, giving up",
				logFields("attempts", attempt, "elapsed_ms", elapsed.Milliseconds(), "reason", giveUp, "error", err)...)
			return out, fmt.Errorf("gave up after %s (%d attempts, %s): %w",
				elapsed.Round(time.Second), attempt, giveUp, err)
		}
		runtimeLog().Debug("Pod exec dial failed, retrying",
			logFields("attempt", attempt, "backoff", backoff, "elapsed_ms", elapsed.Milliseconds(), "error", err)...)
		if serr := clock.sleep(ctx, backoff); serr != nil {
			return "", fmt.Errorf("%w (last exec error: %v)", serr, err)
		}
	}
}
