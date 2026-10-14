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
	"sync"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The Cloud Run Instances runtime's async launch cleanup (ptone/scion#4761,
// design async-start/cloudrun §5.2). Run reports an instance's handle
// before CreateInstance; DeleteResource deletes the instance a failed or
// aborted async launch may have created.
//
// UNVERIFIED (the live API check, ptone/scion#4760, has not run), so each
// is handled conservatively:
//   - NotFound is gRPC NOT_FOUND at GetInstance, DeleteInstance and the
//     delete operation's wait;
//   - a cut-off or accepted create may become visible late (handled by the
//     in-doubt re-poll);
//   - InvalidArgument, PermissionDenied or Unauthenticated from the server
//     means no instance was created;
//   - a delete refused while the instance reconciles comes back as
//     FailedPrecondition or Aborted, and Instance.Reconciling clears.

// The cleanup's timing. Variables so tests can shorten them; production
// never changes them.
var (
	// cloudRunCleanupBudget bounds one DeleteResource, including the
	// in-doubt re-poll and the reconciling wait. It is applied to the
	// calls themselves too, and fits inside the broker's 60 s cleanup
	// context (AsyncLaunchCleanupBudget).
	cloudRunCleanupBudget = 45 * time.Second
	// cloudRunInDoubtRepoll is how long a create still in doubt is
	// re-polled, counted from the first NotFound DeleteResource sees.
	cloudRunInDoubtRepoll = 30 * time.Second
	// cloudRunRepollInterval is the wait between in-doubt re-polls.
	cloudRunRepollInterval = 2 * time.Second
	// cloudRunReconcilePoll is the wait between reads of an instance
	// that is reconciling.
	cloudRunReconcilePoll = 2 * time.Second
	// cloudRunRefusalBackoff is the wait after each refused delete of an
	// instance that is not reconciling; the last value repeats.
	cloudRunRefusalBackoff = []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
)

// errCloudRunUnlabelled is instanceOp's strict refusal of an instance with
// no run label: a legacy instance, never an async launch's.
var errCloudRunUnlabelled = errors.New("instance has no run label; not this launch's")

// cloudRunInDoubtKey identifies one async create: the instance resource
// name and the launch's run ID.
type cloudRunInDoubtKey struct {
	name  string
	runID string
}

// cloudRunCreatesInDoubt is the process-wide set of async creates not yet
// observed, each with the time its mark expires
// (AsyncCleanupHorizon of the launch ctx's deadline, after which no
// cleanup looks for it). A mark is added before CreateInstance and
// removed once the create is observed (its operation succeeded, the
// cleanup read the instance or had a delete accepted) or the server
// definitively rejected it. While a create is marked, DeleteResource
// does not take a NotFound as gone. The set is process memory: a broker
// restart loses it.
var cloudRunCreatesInDoubt = struct {
	mu    sync.Mutex
	marks map[cloudRunInDoubtKey]time.Time
}{marks: map[cloudRunInDoubtKey]time.Time{}}

// cloudRunInDoubtExpired reports whether a mark expiring at until has
// expired at now.
func cloudRunInDoubtExpired(until, now time.Time) bool {
	return !now.Before(until)
}

// markCloudRunCreateInDoubt marks the create of instance name by runID in
// doubt until the cleanup horizon of ctx's deadline (of now, without a
// deadline), and sweeps expired marks. A create without a run ID is not
// marked: its handle has no UID and DeleteResource refuses it.
func markCloudRunCreateInDoubt(ctx context.Context, name, runID string) {
	if runID == "" {
		return
	}
	now := time.Now()
	dl, ok := ctx.Deadline()
	if !ok {
		dl = now
	}
	s := &cloudRunCreatesInDoubt
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, until := range s.marks {
		if cloudRunInDoubtExpired(until, now) {
			delete(s.marks, k)
		}
	}
	s.marks[cloudRunInDoubtKey{name: name, runID: runID}] = AsyncCleanupHorizon(dl)
}

// clearCloudRunCreateInDoubt removes the mark of the create of name by
// runID, if any.
func clearCloudRunCreateInDoubt(name, runID string) {
	s := &cloudRunCreatesInDoubt
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.marks, cloudRunInDoubtKey{name: name, runID: runID})
}

// cloudRunCreateInDoubt reports whether the create of name by runID is
// marked and the mark has not expired.
func cloudRunCreateInDoubt(name, runID string) bool {
	s := &cloudRunCreatesInDoubt
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.marks[cloudRunInDoubtKey{name: name, runID: runID}]
	return ok && !cloudRunInDoubtExpired(until, time.Now())
}

// cloudRunServerRejected reports whether a CreateInstance error is the
// server's definitive rejection (InvalidArgument, PermissionDenied or
// Unauthenticated in a gRPC status) rather than a ctx or transport error
// that leaves the create in doubt.
func cloudRunServerRejected(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	switch status.Code(err) {
	case codes.InvalidArgument, codes.PermissionDenied, codes.Unauthenticated:
		return true
	}
	return false
}

// cloudRunSleep waits d, or returns ctx's error if ctx ends first.
func cloudRunSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// parent returns the runtime's resource parent,
// projects/<project>/locations/<location>.
func (r *CloudRunRuntime) parent() string {
	return fmt.Sprintf("projects/%s/locations/%s", r.config.ProjectID, r.config.Location)
}

// DeleteResource deletes the Cloud Run instance an async launch reported
// (ptone/scion#4761): h.Kind is api.ResourceKindCloudRunInstance,
// h.Namespace the runtime's parent, h.Name the instance ID and h.UID the
// launch's run ID. It satisfies agent.UIDPreconditionDeleter; the broker's
// CleanupLaunch calls it on a fresh context after the launch has ended.
//
// The delete is run-checked and strict (instanceOp): an instance labelled
// with another run, or with no run label, is not this launch's and is
// left untouched (nil). Otherwise:
//   - NotFound (at the read, the delete or the delete's wait) is success,
//     unless the create is still in doubt (cloudRunCreatesInDoubt): then
//     the read is repeated every cloudRunRepollInterval for
//     cloudRunInDoubtRepoll from the first NotFound, and only after that
//     window is the instance taken as never created;
//   - a FailedPrecondition or Aborted refusal waits, while the instance
//     reports Reconciling, for it to clear, then retries; a refusal of an
//     instance that is not reconciling retries after cloudRunRefusalBackoff;
//   - anything else is an error.
//
// All of it runs within cloudRunCleanupBudget, which bounds the calls too,
// on one Instances client opened for the whole cleanup. When the budget
// runs out the error wraps both the ctx error and the last API error.
// A handle in another namespace, of another kind or with no UID is
// refused before any call.
func (r *CloudRunRuntime) DeleteResource(ctx context.Context, h api.ResourceHandle) error {
	if h.Kind != api.ResourceKindCloudRunInstance {
		return fmt.Errorf("cloudrun: unsupported resource kind %q", h.Kind)
	}
	if h.UID == "" {
		return errNoUID(h)
	}
	if err := r.resolveConfig(ctx); err != nil {
		return fmt.Errorf("failed to resolve Cloud Run config: %w", err)
	}
	if parent := r.parent(); h.Namespace != parent {
		return fmt.Errorf("cloudrun: instance %s is in %q, not this runtime's %q; not deleting it", h.Name, h.Namespace, parent)
	}

	budget, cancel := context.WithTimeout(ctx, cloudRunCleanupBudget)
	defer cancel()
	name := r.instanceResourceName(h.Name)
	ref := RunRef{ID: h.Name, RunID: h.UID}
	c, err := r.client(budget)
	if err != nil {
		return fmt.Errorf("cloudrun: launch cleanup of instance %s: failed to create client: %w", h.Name, err)
	}
	defer func() { _ = c.Close() }()
	// overBudget is the error once the budget has run out: it wraps the
	// ctx error (cause) and the last API error (last).
	overBudget := func(cause, last error) error {
		return fmt.Errorf("cloudrun: launch cleanup of instance %s did not finish within %s: %w (last error: %w)", h.Name, cloudRunCleanupBudget, cause, last)
	}
	var seen bool // a read in this attempt found this run's instance
	deleteCall := func(dc cloudrun.InstancesAPI, n, etag string) (cloudrun.InstanceOperation, error) {
		// instanceOpWith issues the call only after its read found the
		// instance labelled with this run.
		seen = true
		return dc.DeleteInstance(budget, &runpb.DeleteInstanceRequest{Name: n, Etag: etag}, defaultCallOpts...)
	}

	var repollUntil time.Time
	refusals := 0
	for {
		seen = false
		err := r.instanceOpWith(budget, c, ref, "delete", true, deleteCall)
		switch {
		case err == nil:
			clearCloudRunCreateInDoubt(name, h.UID)
			runtimeLog().Info("Launch cleanup deleted a Cloud Run instance", "instance", h.Name, "run_id", h.UID)
			return nil
		case errors.Is(err, ErrRunMismatch):
			clearCloudRunCreateInDoubt(name, h.UID)
			runtimeLog().Info("Launch cleanup skipped a Cloud Run instance recreated by another launch",
				"instance", h.Name, "run_id", h.UID)
			return nil
		case errors.Is(err, errCloudRunUnlabelled):
			clearCloudRunCreateInDoubt(name, h.UID)
			runtimeLog().Info("Launch cleanup skipped an unlabelled Cloud Run instance; not this launch's",
				"instance", h.Name, "run_id", h.UID)
			return nil
		}
		if budget.Err() != nil {
			return overBudget(budget.Err(), err)
		}
		switch status.Code(err) {
		case codes.NotFound:
			if seen || !cloudRunCreateInDoubt(name, h.UID) {
				clearCloudRunCreateInDoubt(name, h.UID)
				return nil
			}
			now := time.Now()
			if repollUntil.IsZero() {
				repollUntil = now.Add(cloudRunInDoubtRepoll)
			}
			if !now.Before(repollUntil) {
				clearCloudRunCreateInDoubt(name, h.UID)
				runtimeLog().Warn("Launch cleanup: a Cloud Run create in doubt never appeared; taking it as not created",
					"instance", h.Name, "run_id", h.UID, "window", cloudRunInDoubtRepoll)
				return nil
			}
			if serr := cloudRunSleep(budget, cloudRunRepollInterval); serr != nil {
				return overBudget(serr, err)
			}
		case codes.FailedPrecondition, codes.Aborted:
			// The read found the instance: the create is observed.
			clearCloudRunCreateInDoubt(name, h.UID)
			reconciled, werr := waitCloudRunReconciled(budget, c, name)
			if werr != nil {
				return overBudget(werr, err)
			}
			if reconciled {
				continue
			}
			d := cloudRunRefusalBackoff[min(refusals, len(cloudRunRefusalBackoff)-1)]
			refusals++
			runtimeLog().Info("Launch cleanup: Cloud Run refused the delete; retrying",
				"instance", h.Name, "run_id", h.UID, "after", d, "error", err)
			if serr := cloudRunSleep(budget, d); serr != nil {
				return overBudget(serr, err)
			}
		default:
			return fmt.Errorf("cloudrun: launch cleanup of instance %s: %w", h.Name, err)
		}
	}
}

// waitCloudRunReconciled reads instance name through c and, while it
// reports Reconciling, reads it again every cloudRunReconcilePoll. It reports
// whether it waited for a reconcile to clear (or saw the instance gone),
// so the caller retries at once; false means the instance was not
// reconciling (or could not be read) and the caller backs off. The error
// is ctx's, when ctx ends first.
func waitCloudRunReconciled(ctx context.Context, c cloudrun.InstancesAPI, name string) (bool, error) {
	waited := false
	for {
		inst, err := c.GetInstance(ctx, &runpb.GetInstanceRequest{Name: name}, defaultCallOpts...)
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if err != nil {
			// Gone: the retry's read reports it. Otherwise back off.
			return status.Code(err) == codes.NotFound, nil
		}
		if inst == nil || !inst.GetReconciling() {
			return waited, nil
		}
		waited = true
		if err := cloudRunSleep(ctx, cloudRunReconcilePoll); err != nil {
			return false, err
		}
	}
}
