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
	"maps"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Bounds of the wait for a Cloud Run instance after a create or start
// (ptone/scion#7289), shared with the retries of a start refused while the
// instance settles. The cap is the startup probe's 240s budget
// (buildCloudRunInstance), below the hub's 5-minute launch budget
// (hub DefaultServerConfig LaunchTimeout; a test pins cap < budget), so
// the wait ends with its own last-state error before the launch deadline.
// The wait also ends with ctx.
const (
	cloudRunReadyTimeout    = 240 * time.Second
	cloudRunReadyMaxPolls   = 60
	cloudRunReadyBackoffMin = 1 * time.Second
	cloudRunReadyBackoffMax = 10 * time.Second
	// cloudRunCreateVisibility is how long after a create a NotFound read
	// is taken as the instance not being visible yet.
	cloudRunCreateVisibility = 15 * time.Second
)

// cloudRunReadyBounds are the wait's bounds and clock. Production uses
// defaultCloudRunReadyBounds; tests inject a fake clock.
type cloudRunReadyBounds struct {
	timeout    time.Duration
	maxPolls   int
	backoffMin time.Duration
	backoffMax time.Duration
	visibility time.Duration
	clock      func() time.Time
	sleep      func(ctx context.Context, d time.Duration) error
}

func defaultCloudRunReadyBounds() cloudRunReadyBounds {
	return cloudRunReadyBounds{
		timeout:    cloudRunReadyTimeout,
		maxPolls:   cloudRunReadyMaxPolls,
		backoffMin: cloudRunReadyBackoffMin,
		backoffMax: cloudRunReadyBackoffMax,
		visibility: cloudRunCreateVisibility,
		clock:      time.Now,
		sleep:      sleepCtx,
	}
}

// sleepCtx waits for d, or returns ctx's error when ctx ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *CloudRunRuntime) readyBounds() cloudRunReadyBounds {
	if r.readyBoundsForTest != nil {
		return *r.readyBoundsForTest
	}
	return defaultCloudRunReadyBounds()
}

// errCloudRunInstanceEnded is wrapped by a wait that saw the instance end
// before it was ready: its container stopped or exited with no platform
// retry pending.
var errCloudRunInstanceEnded = errors.New("instance ended before it was ready")

// errCloudRunReadyCap is wrapped by a wait that reached its cap.
var errCloudRunReadyCap = errors.New("instance not ready within the wait cap")

// cloudRunNotRetried reports whether err, from a Cloud Run API call, is a
// definitive refusal that a wait must surface at once and never retry:
// PermissionDenied (for example the missing actAs permission on the
// instance's service account, ptone/scion#7319), Unauthenticated and
// InvalidArgument.
func cloudRunNotRetried(err error) bool {
	switch status.Code(err) {
	case codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument:
		return true
	}
	return false
}

// cloudRunPollRetryable reports whether a GetInstance error during a wait
// is retried on the next poll, within the wait's bounds. Retried:
//   - Unavailable and ResourceExhausted (after the client's own gax retries,
//     defaultCallOpts);
//   - DeadlineExceeded of a single call, Aborted and Internal (a transient
//     server error);
//   - NotFound within cloudRunCreateVisibility after this Run's own create
//     (the instance is not visible yet; see waitCloudRunInstance).
//
// Every other code ends the wait at once with that error.
func cloudRunPollRetryable(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.ResourceExhausted, codes.DeadlineExceeded, codes.Aborted, codes.Internal:
		return true
	}
	return false
}

// cloudRunReadyVerdict is what one sample of an instance says to a wait.
type cloudRunReadyVerdict int

const (
	cloudRunWait cloudRunReadyVerdict = iota
	cloudRunReady
	cloudRunEnded
	cloudRunDeleted
	cloudRunOtherRun
)

// cloudRunReadyTarget is the instance a wait expects.
type cloudRunReadyTarget struct {
	// name is the instance resource name.
	name string
	// runID is the GCP-sanitized run label this Run stamps ("" when the
	// start carries no run ID).
	runID string
	// legacyOK accepts an instance with no run label: the start of an
	// existing legacy instance keeps the label it was created without.
	legacyOK bool
	// minGeneration is the generation of the instance read before a
	// StartInstance. A sample at or below it was read before the start
	// took effect and is not the start's result. 0 after a create.
	minGeneration int64
	// created is set after this Run's own create: a NotFound read within
	// the visibility window is waited for.
	created bool
}

// classifyCloudRunInstance reads one sample of inst against target, in
// order:
//   - another run's label: cloudRunOtherRun (never this Run's instance);
//   - delete_time set: cloudRunDeleted;
//   - reconciling, observed_generation behind generation, or a generation
//     at or below target.minGeneration: cloudRunWait;
//   - terminal condition succeeded: cloudRunReady;
//   - terminal condition failed with a Retry condition (the platform is
//     retrying the container): cloudRunWait;
//   - terminal condition failed otherwise: cloudRunEnded;
//   - otherwise (pending, reconciling, unspecified or no condition):
//     cloudRunWait.
func classifyCloudRunInstance(inst *runpb.Instance, target cloudRunReadyTarget) cloudRunReadyVerdict {
	run := inst.GetLabels()[sanitizeGCPLabelKey(api.LabelRunID)]
	if target.runID != "" && run != target.runID && (!target.legacyOK || run != "") {
		return cloudRunOtherRun
	}
	if inst.GetDeleteTime() != nil {
		return cloudRunDeleted
	}
	if inst.GetReconciling() || inst.GetObservedGeneration() < inst.GetGeneration() {
		return cloudRunWait
	}
	if target.minGeneration > 0 && inst.GetGeneration() <= target.minGeneration {
		return cloudRunWait
	}
	switch inst.GetTerminalCondition().GetState() {
	case runpb.Condition_CONDITION_SUCCEEDED:
		return cloudRunReady
	case runpb.Condition_CONDITION_FAILED:
		if cloudRunHasRetryCondition(inst) {
			return cloudRunWait
		}
		return cloudRunEnded
	}
	return cloudRunWait
}

func cloudRunHasRetryCondition(inst *runpb.Instance) bool {
	return slices.ContainsFunc(inst.GetConditions(), func(c *runpb.Condition) bool { return c.GetType() == "Retry" })
}

// describeCloudRunInstance names inst's state for logs and errors: the
// terminal condition state and reason enums, the Retry flag,
// reconciling, the generations and whether it is deleted. Never a
// message, label value, env or Args.
func describeCloudRunInstance(inst *runpb.Instance) string {
	if inst == nil {
		return "no instance read"
	}
	tc := inst.GetTerminalCondition()
	parts := []string{"condition=" + tc.GetState().String()}
	if r := tc.GetReason(); r != runpb.Condition_COMMON_REASON_UNDEFINED {
		parts = append(parts, "reason="+r.String())
	}
	if r := tc.GetRevisionReason(); r != runpb.Condition_REVISION_REASON_UNDEFINED {
		parts = append(parts, "revision_reason="+r.String())
	}
	if r := tc.GetExecutionReason(); r != runpb.Condition_EXECUTION_REASON_UNDEFINED {
		parts = append(parts, "execution_reason="+r.String())
	}
	parts = append(parts,
		fmt.Sprintf("retry=%t", cloudRunHasRetryCondition(inst)),
		fmt.Sprintf("reconciling=%t", inst.GetReconciling()),
		fmt.Sprintf("generation=%d/%d", inst.GetObservedGeneration(), inst.GetGeneration()),
	)
	if inst.GetDeleteTime() != nil {
		parts = append(parts, "deleted=true")
	}
	return strings.Join(parts, " ")
}

// waitCloudRunInstance waits until the instance target.name is ready
// (classifyCloudRunInstance) and returns its last sample. first, the
// instance a create or start operation returned, is the first sample
// (nil for none); further samples come from GetInstance by name, which is
// strongly consistent (List is never used here: it can lag).
//
// The wait only reads: it never re-issues a create or start. It ends:
//   - ready: the sample, nil;
//   - with an error, at once: another run's instance (ErrRunConflict), a
//     deleted instance, an instance that ended (errCloudRunInstanceEnded),
//     a refusal cloudRunNotRetried reports, or any GetInstance error
//     cloudRunPollRetryable does not retry;
//   - at the cap (bounds.timeout or bounds.maxPolls polls), wrapping
//     errCloudRunReadyCap, or when ctx ends, with ctx's error. Both name
//     the last state read or the last API error, never a bare timeout.
func waitCloudRunInstance(ctx context.Context, c cloudrun.InstancesAPI, target cloudRunReadyTarget, first *runpb.Instance, bounds cloudRunReadyBounds, hooks launchHooks) (*runpb.Instance, error) {
	short := cloudRunShortInstanceID(target.name)
	start := bounds.clock()
	hooks.progress(api.LaunchStepContainerStart)
	var last *runpb.Instance
	var lastErr error
	lastState := func() string {
		if lastErr != nil && last != nil {
			return "last error: " + status.Code(lastErr).String() + "; last state: " + describeCloudRunInstance(last)
		}
		if lastErr != nil {
			return "last error: " + status.Code(lastErr).String()
		}
		return "last state: " + describeCloudRunInstance(last)
	}
	sample := first
	backoff := bounds.backoffMin
	for polls := 0; ; {
		if sample != nil {
			last, lastErr = sample, nil
			switch classifyCloudRunInstance(sample, target) {
			case cloudRunReady:
				runtimeLog().Info("Cloud Run instance is ready",
					"instance", short, "run_id", target.runID, "polls", polls, "elapsed_ms", bounds.clock().Sub(start).Milliseconds())
				return sample, nil
			case cloudRunOtherRun:
				return nil, fmt.Errorf("instance %s belongs to another run, not %q: %w", short, target.runID, ErrRunConflict)
			case cloudRunDeleted:
				return nil, fmt.Errorf("cloudrun: instance %s was deleted before it was ready (%s)", short, describeCloudRunInstance(sample))
			case cloudRunEnded:
				return nil, fmt.Errorf("cloudrun: instance %s: %w (%s)", short, errCloudRunInstanceEnded, describeCloudRunInstance(sample))
			}
			runtimeLog().Debug("Cloud Run instance not ready yet",
				"instance", short, "run_id", target.runID, "attempt", polls, "state", describeCloudRunInstance(sample))
		}
		elapsed := bounds.clock().Sub(start)
		if polls >= bounds.maxPolls || elapsed >= bounds.timeout {
			return nil, fmt.Errorf("cloudrun: instance %s after %d polls in %s: %w (%s)", short, polls, elapsed.Round(time.Second), errCloudRunReadyCap, lastState())
		}
		if err := bounds.sleep(ctx, backoff); err != nil {
			return nil, fmt.Errorf("cloudrun: waiting for instance %s: %w (%s)", short, err, lastState())
		}
		backoff = min(time.Duration(float64(backoff)*1.5), bounds.backoffMax)
		polls++
		inst, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: target.name}, defaultCallOpts...)
		sample = nil
		switch {
		case err != nil && ctx.Err() != nil:
			// The caller's context ended during the read: name the state
			// recorded before it.
			return nil, fmt.Errorf("cloudrun: waiting for instance %s: %w (%s)", short, ctx.Err(), lastState())
		case err == nil && inst == nil:
			lastErr = status.Error(codes.Unknown, "GetInstance returned no instance")
		case err == nil:
			sample = inst
		case cloudRunNotRetried(err):
			return nil, fmt.Errorf("cloudrun: %s reading instance %s (not retried): %w", status.Code(err), short, err)
		case status.Code(err) == codes.NotFound && target.created && bounds.clock().Sub(start) < bounds.visibility:
			lastErr = err
		case status.Code(err) == codes.NotFound:
			return nil, fmt.Errorf("cloudrun: instance %s is gone (%s): %w", short, lastState(), err)
		case cloudRunPollRetryable(err):
			lastErr = err
		default:
			return nil, fmt.Errorf("cloudrun: reading instance %s: %w", short, err)
		}
	}
}

// waitCloudRunInstanceGone waits until GetInstance reports the instance
// name NotFound, within bounds, after a delete. Errors follow
// waitCloudRunInstance: refusals are not retried, cloudRunPollRetryable
// errors are, and the cap names the last state read.
func waitCloudRunInstanceGone(ctx context.Context, c cloudrun.InstancesAPI, name string, bounds cloudRunReadyBounds) error {
	short := cloudRunShortInstanceID(name)
	start := bounds.clock()
	var last *runpb.Instance
	backoff := bounds.backoffMin
	for polls := 0; ; polls++ {
		inst, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: name}, defaultCallOpts...)
		switch {
		case err != nil && ctx.Err() != nil:
			return fmt.Errorf("cloudrun: waiting for instance %s to be deleted: %w (last state: %s)", short, ctx.Err(), describeCloudRunInstance(last))
		case status.Code(err) == codes.NotFound:
			return nil
		case err == nil:
			last = inst
		case cloudRunNotRetried(err):
			return fmt.Errorf("cloudrun: %s reading instance %s (not retried): %w", status.Code(err), short, err)
		case !cloudRunPollRetryable(err):
			return fmt.Errorf("cloudrun: reading instance %s: %w", short, err)
		}
		elapsed := bounds.clock().Sub(start)
		if polls+1 >= bounds.maxPolls || elapsed >= bounds.timeout {
			return fmt.Errorf("cloudrun: instance %s still present after %d polls in %s: %w (last state: %s)", short, polls+1, elapsed.Round(time.Second), errCloudRunReadyCap, describeCloudRunInstance(last))
		}
		if err := bounds.sleep(ctx, backoff); err != nil {
			return fmt.Errorf("cloudrun: waiting for instance %s to be deleted: %w (last state: %s)", short, err, describeCloudRunInstance(last))
		}
		backoff = min(time.Duration(float64(backoff)*1.5), bounds.backoffMax)
	}
}

// cloudRunSpecDiff names the fields in which existing, an instance found
// at start, differs from want, the instance this start computed
// (ptone/scion#7297). The Instances API has no update, so an instance
// that differs in any of them is recreated, never reused. Compared:
//   - the container Args and Image;
//   - the instance service account;
//   - the secret-backed env: every env var in secretEnv (the staged
//     secrets and env-type secrets), by name and value, and every env var
//     whose value comes from a source rather than a literal;
//   - the set of env var names, so an env var this start does not set
//     (for example a removed secret) is never kept;
//   - the volumes.
//
// Only field names are returned, never values.
func cloudRunSpecDiff(existing, want *runpb.Instance, secretEnv map[string]bool) []string {
	var diff []string
	ec, wc := firstContainer(existing), firstContainer(want)
	if !slices.Equal(ec.GetArgs(), wc.GetArgs()) {
		diff = append(diff, "args")
	}
	if ec.GetImage() != wc.GetImage() {
		diff = append(diff, "image")
	}
	if existing.GetServiceAccount() != want.GetServiceAccount() {
		diff = append(diff, "service_account")
	}
	if !maps.Equal(secretEnvOf(ec, secretEnv), secretEnvOf(wc, secretEnv)) {
		diff = append(diff, "secret_env")
	}
	if !slices.Equal(envNames(ec), envNames(wc)) {
		diff = append(diff, "env_names")
	}
	if !slices.EqualFunc(existing.GetVolumes(), want.GetVolumes(), func(a, b *runpb.Volume) bool { return proto.Equal(a, b) }) {
		diff = append(diff, "volumes")
	}
	return diff
}

func firstContainer(inst *runpb.Instance) *runpb.Container {
	if cs := inst.GetContainers(); len(cs) > 0 {
		return cs[0]
	}
	return nil
}

// envNames returns the sorted names of c's env vars.
func envNames(c *runpb.Container) []string {
	names := make([]string, 0, len(c.GetEnv()))
	for _, ev := range c.GetEnv() {
		names = append(names, ev.GetName())
	}
	slices.Sort(names)
	return names
}

// secretEnvOf returns c's secret-backed env vars (see cloudRunSpecDiff),
// keyed by name, each as a deterministic encoding of the EnvVar.
func secretEnvOf(c *runpb.Container, secretEnv map[string]bool) map[string]string {
	out := map[string]string{}
	for _, ev := range c.GetEnv() {
		if !secretEnv[ev.GetName()] && ev.GetValueSource() == nil {
			continue
		}
		b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(ev)
		out[ev.GetName()] = string(b)
	}
	return out
}

// startCloudRunInstanceSettled issues StartInstance for existing, this
// run's matching instance, and returns its operation, the last read of
// the instance and the bounds left for the ready wait. A refusal while the
// instance settles (a stop or other change still in progress) is retried,
// after a re-read for the current etag, within bounds' cap and backoff:
//   - FailedPrecondition or Aborted (the etag changed): retried while the
//     re-read reports reconciling. A settled re-read whose etag differs
//     from the one sent is started once more with its current etag; a
//     settled instance that refuses its own current etag fails at once,
//     naming the instance and its state;
//   - never retried: cloudRunNotRetried refusals (PermissionDenied, incl.
//     the missing actAs permission, Unauthenticated, InvalidArgument), any
//     other code, a re-read of another run's instance (ErrRunConflict) or
//     of an instance being deleted.
//
// Errors and logs name the instance, the refusal's code and the state read
// (describeCloudRunInstance), never the platform's message text; the
// returned error wraps a status of the same code.
func startCloudRunInstanceSettled(ctx context.Context, c cloudrun.InstancesAPI, name string, existing *runpb.Instance, want string, bounds cloudRunReadyBounds) (cloudrun.InstanceOperation, *runpb.Instance, cloudRunReadyBounds, error) {
	short := cloudRunShortInstanceID(name)
	start := bounds.clock()
	read := existing
	backoff := bounds.backoffMin
	settledRetried := false
	for attempt := 1; ; attempt++ {
		op, err := c.StartInstance(ctx, &runpb.StartInstanceRequest{Name: name, Etag: read.GetEtag()}, defaultCallOpts...)
		if err == nil {
			left := bounds
			left.timeout -= bounds.clock().Sub(start)
			left.maxPolls -= attempt - 1
			return op, read, left, nil
		}
		if ctx.Err() != nil {
			return nil, nil, bounds, fmt.Errorf("cloudrun: starting instance %s: %w (last state: %s)", short, ctx.Err(), describeCloudRunInstance(read))
		}
		code := status.Code(err)
		refused := status.Error(code, code.String())
		if cloudRunNotRetried(err) {
			return nil, nil, bounds, fmt.Errorf("failed to start existing instance %s (%s, not retried; %s): %w", short, code, describeCloudRunInstance(read), refused)
		}
		if code != codes.FailedPrecondition && code != codes.Aborted {
			return nil, nil, bounds, fmt.Errorf("failed to start existing instance: %w", err)
		}
		cur, gerr := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: name}, defaultCallOpts...)
		switch {
		case gerr != nil && ctx.Err() != nil:
			return nil, nil, bounds, fmt.Errorf("cloudrun: starting instance %s: %w (last state: %s)", short, ctx.Err(), describeCloudRunInstance(read))
		case gerr != nil:
			return nil, nil, bounds, fmt.Errorf("failed to start existing instance %s (%s); re-reading it: %w: %w", short, code, gerr, err)
		case cur == nil:
			return nil, nil, bounds, fmt.Errorf("failed to start existing instance %s (%s); re-read returned no instance: %w", short, code, err)
		}
		if run := cur.GetLabels()[sanitizeGCPLabelKey(api.LabelRunID)]; want != "" && run != "" && run != want {
			return nil, nil, bounds, fmt.Errorf("failed to start existing instance %s (%s); it belongs to run %q, not %q: %w: %w", short, code, run, want, ErrRunConflict, err)
		}
		if cur.GetDeleteTime() != nil {
			return nil, nil, bounds, fmt.Errorf("failed to start existing instance %s (%s); it is being deleted (%s): %w", short, code, describeCloudRunInstance(cur), err)
		}
		if !cur.GetReconciling() {
			// Settled. If it settled after the refused start (its etag
			// differs from the one sent), that instance was never asked
			// to start: start it once with its current etag. A settled
			// instance that refuses its own current etag is not retried.
			if settledRetried || cur.GetEtag() == read.GetEtag() {
				again := ""
				if settledRetried {
					again = " again"
				}
				return nil, nil, bounds, fmt.Errorf("failed to start existing instance %s (%s, not retried%s: the instance is not reconciling; %s): %w", short, code, again, describeCloudRunInstance(cur), refused)
			}
			settledRetried = true
		}
		read = cur
		elapsed := bounds.clock().Sub(start)
		if attempt >= bounds.maxPolls || elapsed >= bounds.timeout {
			return nil, nil, bounds, fmt.Errorf("cloudrun: instance %s refused StartInstance (%s) in each of %d attempts in %s: %w (last state: %s): %w",
				short, code, attempt, elapsed.Round(time.Second), errCloudRunReadyCap, describeCloudRunInstance(cur), refused)
		}
		runtimeLog().Info("Cloud Run refused StartInstance while the instance settles; retrying",
			"instance", short, "run_id", want, "attempt", attempt, "code", code.String(), "state", describeCloudRunInstance(cur))
		if err := bounds.sleep(ctx, backoff); err != nil {
			return nil, nil, bounds, fmt.Errorf("cloudrun: waiting to start instance %s: %w (last state: %s)", short, err, describeCloudRunInstance(cur))
		}
		backoff = min(time.Duration(float64(backoff)*1.5), bounds.backoffMax)
	}
}
