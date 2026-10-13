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
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// defaultCreateSupersedeWait bounds how long a create waits for an earlier
// start of the same agent name on this broker (one it has just cancelled)
// to finish its own failure cleanup, before refusing with a retryable 503
// (prior_start_in_progress). It is kept well under the hub's create
// deadline (about 30s from the CLI) so the refusal reaches the caller as
// itself rather than as a timeout, and leaves the create budget to
// provision when the wait does end in time. The superseded start's cleanup
// still runs under its own 60s budget (cleanupAbortedLaunch); only the
// newer create gives up.
const defaultCreateSupersedeWait = 15 * time.Second

// createSupersedeRetryAfter is the Retry-After, in seconds, sent with the
// prior_start_in_progress 503.
const createSupersedeRetryAfter = "5"

// errCreateSupersedeTimeout is returned by beginCreateRegistration when an
// earlier start of the same name did not finish within the wait. createAgent
// maps it to 503 prior_start_in_progress.
var errCreateSupersedeTimeout = errors.New("an earlier start of this agent is still finishing")

// errCreateStartCancelled is returned when a create's registration was
// cancelled before the create took the name: by a stop or delete of the
// agent on this broker, by a newer start of the same name, or by the
// request itself ending. createAgent maps it to 409 start_cancelled.
var errCreateStartCancelled = errors.New("the start was cancelled before it began")

// errInvalidLaunchSlug is returned by beginCreateRegistration when the
// agent's slug (or, without one, its name) is not a single path element and
// so cannot name a launch marker file. createAgent maps it to 400.
var errInvalidLaunchSlug = errors.New("agent slug must be a single path element")

// syncStart is the bookkeeping for one create's registration under the
// agent's name, mirroring what runLaunch keeps for an async launch:
//
//   - a launchRegistry record under (projectID, slug), so a delete or stop
//     of the agent on this broker cancels the start
//     (launchRegistry.CancelLocalFor), and a later start for the same name
//     cancels this one and waits for it to finish before doing any
//     name-addressed work;
//   - for a synchronous start, a launch marker for the slug holding this
//     start's own owner value (takeName), so the start's failure cleanup can
//     check, immediately before removing files by name, that no newer start
//     (sync or async, on any replica sharing the storage) has since taken
//     the name.
//
// Every create that is not provision-only takes one before
// buildStartContext. An async create hands its registry record over to the
// launch (handOff) and so never writes a marker here; runLaunch writes its
// own.
type syncStart struct {
	key             launchKey
	rec             *launchRecord
	owner           string
	projectPath     string
	sharedWorkspace bool
	registry        *launchRegistry
	// ctx is done once the registration has been cancelled (stop, delete,
	// newer start). It is a cancellation signal only; see bind.
	ctx context.Context
	// handedOff is set once handOff has replaced rec in the registry with
	// an async launch's record; finish then leaves the registry alone.
	handedOff bool
}

// beginCreateRegistration registers a create for req under the agent's
// name, before any name-addressed work (buildStartContext, the GCS
// workspace download, provisioning) runs. The registration is cancelled
// when the agent is deleted or stopped on this broker, or when a newer
// start for the same name begins; bind carries that cancellation onto the
// context the start later runs under. The caller must call finish exactly
// once when the create ends.
//
// If an earlier start of the same name is still running here, it is
// cancelled and waited for (bounded by ctx and the supersede wait) before
// this returns, so that start's failure cleanup has fully run before this
// create writes anything addressed by the name. The wait ends with
// errCreateSupersedeTimeout or errCreateStartCancelled, and the
// registration is released.
func (s *Server) beginCreateRegistration(ctx context.Context, req CreateAgentRequest) (*syncStart, error) {
	slug := req.Slug
	if slug == "" {
		slug = req.Name
	}
	// The slug is joined onto the launch markers directory later. createAgent
	// already checks Name and Slug at the request boundary; this check keeps
	// the helper safe on its own, before any registry or filesystem use.
	if !isSingleCleanPathElement(slug) {
		return nil, errInvalidLaunchSlug
	}
	// regCtx is only a cancellation signal (ss.ctx), never the start's own
	// context: it is derived from ctx before createAgent attaches the
	// harness-config policy, skill resolver, hub agent defaults and
	// worktree signal, so the start must not run under it (see bind).
	regCtx, cancel := context.WithCancel(ctx)
	ss := &syncStart{
		key:      launchKey{ProjectID: req.ProjectID, Slug: slug},
		owner:    "sync-" + uuid.NewString(),
		registry: s.launchRegistry,
		ctx:      regCtx,
	}
	ss.rec = newLaunchRecord(ss.owner, req.ID, store.LaunchKindCreate, "", time.Time{}, cancel)
	ss.rec.RunID = req.RunID

	var supersededDone <-chan struct{}
	if ss.registry != nil {
		supersededDone = ss.registry.Begin(ss.key, ss.rec)
	}
	if supersededDone != nil {
		wait := s.createSupersedeWait
		if wait <= 0 {
			wait = defaultCreateSupersedeWait
		}
		timer := time.NewTimer(wait)
		select {
		case <-supersededDone:
			timer.Stop()
		case <-regCtx.Done():
			timer.Stop()
			ss.finish()
			return nil, errCreateStartCancelled
		case <-timer.C:
			ss.finish()
			return nil, errCreateSupersedeTimeout
		}
	}
	return ss, nil
}

// bind returns a context derived from ctx -- the dispatch context with
// everything createAgent attached after the registration (harness-config
// policy hook, skill resolver, hub agent defaults, provisioned-worktree
// signal) -- that is also cancelled when this registration is cancelled (a
// stop, delete or newer start of the name), and a release func the caller
// must call once the start has returned.
func (ss *syncStart) bind(ctx context.Context) (context.Context, func()) {
	startCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ss.ctx, cancel)
	return startCtx, func() {
		stop()
		cancel()
	}
}

// cancelled reports whether this registration has been cancelled: a stop or
// delete of the agent, or a newer start of the name, reached it.
func (ss *syncStart) cancelled() bool {
	return ss.ctx.Err() != nil
}

// takeName writes this start's launch marker for the slug, under the
// project path and workspace sharing buildStartContext resolved. A
// synchronous start calls it once, before provisioning anything; with no
// project path there is nothing to write, and ownsName is always false.
func (ss *syncStart) takeName(opts api.StartOptions) error {
	ss.projectPath = opts.ProjectPath
	ss.sharedWorkspace = opts.SharedWorkspace
	if ss.projectPath == "" {
		return nil
	}
	if err := writeLaunchMarker(ss.projectPath, ss.sharedWorkspace, ss.key.Slug, ss.owner); err != nil {
		// Without the marker the failure cleanup can never confirm
		// ownership and would leave the files behind, so fail here, as
		// runLaunch does, rather than start with no way to clean up. Clear
		// the path so finish does not try to remove a marker it never
		// wrote.
		ss.projectPath = ""
		return fmt.Errorf("failed to write launch marker: %w", err)
	}
	return nil
}

// handOff replaces this registration's record in the registry with rec, an
// async launch's record, without cancelling either: the launch continues
// the create under the name this registration has already waited for. It
// returns false, leaving the registry unchanged, if the registration was
// already cancelled or replaced (a stop, delete or newer start took the
// name while buildStartContext ran); the caller then refuses the create.
func (ss *syncStart) handOff(rec *launchRecord) bool {
	if ss.registry == nil {
		return false
	}
	if !ss.registry.Handoff(ss.key, ss.rec, rec, ss.cancelled) {
		return false
	}
	ss.handedOff = true
	return true
}

// ownsName reports, via a fresh read of the slug's launch marker, whether
// this start still owns the agent name. It must be checked immediately
// before removing any files by name. Always false without a project path,
// since no marker was written and nothing name-addressed may be removed.
func (ss *syncStart) ownsName() bool {
	if ss.projectPath == "" {
		return false
	}
	return launchMarkerMatches(ss.projectPath, ss.sharedWorkspace, ss.key.Slug, ss.owner)
}

// testHookCreateBeforeBuildStartContext, when set (tests only), runs in
// createAgent immediately before buildStartContext.
var testHookCreateBeforeBuildStartContext func(req CreateAgentRequest)

// testHookSyncStartFinish, when set (tests only), runs at the start of
// syncStart.finish.
var testHookSyncStartFinish func(key launchKey)

// finish removes this start's marker (only if it still holds this start's
// owner value), then releases the registry record, which lets a newer start
// waiting in beginCreateRegistration proceed. The marker goes first so its
// check-and-remove cannot remove a marker the newer start writes. After a
// handOff the registry record belongs to the async launch, so only this
// registration's own done channel is closed.
func (ss *syncStart) finish() {
	if testHookSyncStartFinish != nil {
		testHookSyncStartFinish(ss.key)
	}
	if ss.projectPath != "" {
		removeLaunchMarkerIfMatches(ss.projectPath, ss.sharedWorkspace, ss.key.Slug, ss.owner)
	}
	if ss.registry != nil && !ss.handedOff {
		ss.registry.Finish(ss.key, ss.rec)
	} else {
		close(ss.rec.done)
	}
	ss.rec.cancel()
}

// cancelLocalLaunch cancels any in-flight start (sync or async) registered
// on this broker under key, unless it belongs to an agent other than
// agentID (the hub's agent ID from the stop or delete; "" matches any, see
// launchRegistry.CancelLocalFor). A server built without a launch registry
// (for example, constructed directly in a test rather than via New) has
// nothing to cancel.
func (s *Server) cancelLocalLaunch(key launchKey, agentID string) {
	s.cancelLocalLaunchForRun(key, agentID, "")
}

// cancelLocalLaunchForRun is cancelLocalLaunch for a stop or delete naming
// run runID; see launchRegistry.CancelLocalFor. It reports whether it woke
// a launch.
func (s *Server) cancelLocalLaunchForRun(key launchKey, agentID, runID string) bool {
	if s.launchRegistry == nil {
		return false
	}
	return s.launchRegistry.CancelLocalFor(key, agentID, runID)
}

// writeCreateRegistrationError answers a create whose registration under
// the agent's name failed (beginCreateRegistration, or an async create's
// handOff): 400 for an invalid slug, a retryable 503 prior_start_in_progress
// (with Retry-After) when an earlier start of the name did not finish within
// the wait, and 409 start_cancelled when the registration was cancelled
// first. The dispatch attempt is marked failed with the same status, so a
// RequestID replay runs the create again rather than replaying the refusal.
func (s *Server) writeCreateRegistrationError(w http.ResponseWriter, span trace.Span, req CreateAgentRequest, err error, markAttemptFailed func(int, string)) {
	s.agentLifecycleLog.Warn("Agent create refused before start",
		"agent_id", req.ID, "project_id", req.ProjectID, "run_id", req.RunID,
		"name", req.Name, "slug", req.Slug, "error", err)
	span.SetStatus(codes.Error, err.Error())
	switch {
	case errors.Is(err, errInvalidLaunchSlug):
		markAttemptFailed(http.StatusBadRequest, "invalid slug")
		ValidationError(w, "invalid slug", nil)
	case errors.Is(err, errCreateSupersedeTimeout):
		markAttemptFailed(http.StatusServiceUnavailable, api.BrokerErrCodePriorStartInProgress)
		w.Header().Set("Retry-After", createSupersedeRetryAfter)
		writeError(w, http.StatusServiceUnavailable, api.BrokerErrCodePriorStartInProgress,
			"Failed to create agent: "+err.Error()+"; retry shortly", nil)
	case errors.Is(err, errCreateStartCancelled):
		markAttemptFailed(http.StatusConflict, api.BrokerErrCodeStartCancelled)
		writeError(w, http.StatusConflict, api.BrokerErrCodeStartCancelled,
			"Failed to create agent: "+err.Error()+" (stopped, deleted, or superseded by a newer start of the same name)", nil)
	default:
		markAttemptFailed(http.StatusInternalServerError, "failed to create agent")
		RuntimeError(w, "Failed to create agent: "+err.Error())
	}
}
