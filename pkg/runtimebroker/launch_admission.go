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
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// minAsyncLaunchTimeoutSeconds is the smallest LaunchTimeoutSeconds the
// broker accepts for an async launch: the broker aborts 20s before the
// deadline (design §3.8.2 step 4), so anything at or below that would leave
// ctx' already expired before runLaunch's first claim.
const minAsyncLaunchTimeoutSeconds = 20

// beginAsyncLaunch implements design t1-async-create-v11.md §3.8.2 for a
// createAgent request with AsyncLaunch set (and not ProvisionOnly or
// Reprovision, which createAgent's caller has already excluded). Admission
// up to here is identical to the synchronous path (the registration under
// the agent's name and its supersede wait, buildStartContext,
// attachSkillResolver, withHubAgentDefaults); this adds the one extra
// admission step the async path needs, Manager.Preflight, then hands the
// create's registration (ss) over to the launch, accepts it and continues
// it in runLaunch.
//
// It reports whether it accepted the launch. Every refusal writes its error
// response and returns false; only the path that starts runLaunch returns
// true. A flat instance's caller releases its ownership mirror unless the
// launch was accepted, because only then does runLaunch own the mirror;
// owned is that mirror (nil on a legacy Runtime Broker), which runLaunch
// releases when it ends.
//
// Nothing captured in the launchCtx passed to runLaunch, nor runLaunch
// itself, retains r or any value derived from *http.Request: everything it
// needs (the resolved hub connection name, opts, the request body already
// decoded into req) is plain data copied out here, in this synchronous
// handler goroutine, before go runLaunch starts (design §7 P1b-1 B-6).
func (s *Server) beginAsyncLaunch(w http.ResponseWriter, r *http.Request, ctx context.Context, req CreateAgentRequest, opts api.StartOptions, mgr agent.Manager, runtimeName string, ss *syncStart, owned *ownedStart, attempt *dispatchAttempt, markAttemptFailed func(int, string), span trace.Span, receivedAt time.Time) bool {
	// The GCS download itself runs in runLaunch (design §3.1), but its
	// directory check only reads the filesystem, so an invalid workspace
	// directory is answered here with the same 400 the synchronous path
	// gives, not reported as a failed launch after the 201. runLaunch's
	// download validates again before it creates anything. Any other error
	// from computing the directory is left to runLaunch's download step,
	// which reports it as before.
	if req.WorkspaceStoragePath != "" {
		// A download mode this broker does not know is refused here with the
		// synchronous path's 422, before a launch is accepted.
		signed, modeErr := selectWorkspaceDownloadMode(req)
		if modeErr != nil {
			span.SetStatus(codes.Error, modeErr.Error())
			markAttemptFailed(http.StatusUnprocessableEntity, "workspace download mode not supported")
			writeWorkspaceDownloadModeUnsupported(w)
			return false
		}
		if _, attemptMsg, httpMessage, err := s.resolveGCSWorkspaceDir(req); errors.Is(err, errInvalidWorkspaceDir) {
			span.SetStatus(codes.Error, err.Error())
			markAttemptFailed(http.StatusBadRequest, attemptMsg)
			BadRequest(w, httpMessage)
			return false
		}
		// Likewise a broker with no bucket for the upload refuses here with
		// the synchronous path's 422, before accepting a launch that could
		// only fail (ptone/scion#3422).
		if !signed && s.workspaceStorageBucket(req) == "" {
			span.SetStatus(codes.Error, errWorkspaceStorageUnconfigured.Error())
			markAttemptFailed(http.StatusUnprocessableEntity, "storage bucket not configured")
			writeWorkspaceStorageUnconfigured(w)
			return false
		}
	}

	// With the harness-config policy able to refuse, buildStartContext has
	// already run the same resolution (PolicyPreflight); this Preflight
	// repeats it as the async path's admission step, which also surfaces
	// template/harness-config not-found errors synchronously.
	if err := mgr.Preflight(ctx, opts); err != nil {
		span.SetStatus(codes.Error, err.Error())
		if d, ok := harnessPolicyRefusalFrom(err); ok {
			markAttemptFailed(d.HTTPStatus, d.detail())
			s.writeHarnessPolicyRefusal(w, d, "create agent", req.ID, nil)
			return false
		}
		if errors.Is(err, config.ErrHarnessConfigNotFound) || errors.Is(err, config.ErrTemplateNotFound) {
			markAttemptFailed(http.StatusNotFound, "failed to create agent")
			// Names the resource without err's own text, which can carry
			// broker paths (ptone/scion#3113); the full error is logged.
			s.agentLifecycleLog.Warn("Agent create failed: preflight: template or harness-config not found",
				"agent_id", req.ID, "project_id", req.ProjectID, "run_id", opts.RunID, "error", err)
			writeError(w, http.StatusNotFound, ErrCodeNotFound, notFoundMessage(opCreateAgent, err, opts.TemplateName), nil)
			return false
		}
		markAttemptFailed(http.StatusInternalServerError, "failed to create agent")
		// Fixed text, as the synchronous create gives for a Manager.Start
		// failure (ptone/scion#3113): the raw error can carry runtime
		// detail. The full error reaches only the broker log.
		s.agentLifecycleLog.Error("Agent create failed: preflight", append(append([]any{
			"agent_id", req.ID, "project_id", req.ProjectID, "run_id", opts.RunID,
			"runtime", runtimeName, "profile", opts.Profile},
			failureLogAttrs(err, nil)...), runtimeFailureLogAttrs(err)...)...)
		RuntimeError(w, runtimeOpError(opCreateAgent, err).Error())
		return false
	}

	lc := launchCtx{
		projectID:       req.ProjectID,
		keepalive:       time.Duration(req.LaunchKeepaliveSeconds) * time.Second,
		opts:            opts,
		mgr:             mgr,
		runtimeName:     runtimeName,
		key:             ss.key,
		sharedWorkspace: opts.SharedWorkspace,
		owned:           owned,
		req:             req,
	}
	return s.acceptAsyncLaunch(w, r, ctx, ss, asyncAdmission{
		op:             "create",
		launchID:       req.LaunchID,
		agentID:        req.ID,
		kind:           store.LaunchKindCreate,
		runID:          req.RunID,
		receivedAt:     receivedAt,
		timeoutSeconds: req.LaunchTimeoutSeconds,
		status:         http.StatusCreated,
		runtimeName:    runtimeName,
		lc:             lc,
		onDrainRefused: func() {
			markAttemptFailed(http.StatusServiceUnavailable, api.BrokerErrCodeBrokerDraining)
		},
		writeCancelled: func() {
			s.writeCreateRegistrationError(w, span, req, errCreateStartCancelled, markAttemptFailed)
		},
		onAccepted: func(resp *CreateAgentResponse) {
			// Complete the dispatch attempt as succeeded with the accepted
			// response, so a same-replica RequestID replay returns it
			// without launching twice (design §3.8.2 step 2; B-1).
			if attempt != nil {
				s.dispatchAttemptsMu.Lock()
				s.completeAttempt(attempt, dispatchAttemptSucceeded, http.StatusCreated, resp, nil, "")
				s.dispatchAttemptsMu.Unlock()
			}
		},
	})
}

// asyncAdmission is what acceptAsyncLaunch needs from its caller, an async
// create (beginAsyncLaunch) or an async start (beginAsyncStart): the
// launch's identity and budget, the launch context runLaunch continues
// with, and the caller's own way to answer each refusal.
type asyncAdmission struct {
	// op names the operation in the broker_draining refusal ("create" or
	// "start").
	op string
	// launchID, agentID and runID are the Hub's launch, agent and run IDs.
	launchID, agentID, runID string
	// kind is the launch kind recorded locally and journalled. Broker-side
	// it decides only whether the launch may delete agent files (create
	// only); the Hub's launch row holds the authoritative kind.
	kind string
	// receivedAt and timeoutSeconds give the launch's deadline (design
	// §3.8.2 step 4).
	receivedAt     time.Time
	timeoutSeconds int
	// status is the accepted response's HTTP status: 201 for a create, 202
	// for a start (the synchronous start's status).
	status      int
	runtimeName string
	lc          launchCtx
	// onDrainRefused, when set, runs before the broker_draining 503 is
	// written.
	onDrainRefused func()
	// writeCancelled writes the refusal for a registration that a stop,
	// delete or newer start cancelled before the transfer.
	writeCancelled func()
	// onAccepted, when set, runs with the accepted response just before it
	// is written.
	onAccepted func(resp *CreateAgentResponse)
}

// acceptAsyncLaunch is the kind-agnostic tail of an async launch's
// admission (design wave 3 §2 D-f): it builds the launch record and its
// deadline, tracks it for the shutdown drain, hands the caller's
// registration under the agent's name (ss) over to it, journals it, writes
// the accepted response (LaunchPending with the launch ID echo) and starts
// runLaunch. It reports whether it accepted the launch; every refusal
// writes its response and returns false.
//
// Nothing captured in a.lc, nor runLaunch itself, retains r or any value
// derived from *http.Request; r is read here only for the hub connection
// the launch reports to.
func (s *Server) acceptAsyncLaunch(w http.ResponseWriter, r *http.Request, ctx context.Context, ss *syncStart, a asyncAdmission) bool {
	key := ss.key
	opts := a.lc.opts

	// design §3.8.2 step 4: the broker's deadline is 20s before the Hub's
	// launch_deadline, measured from receivedAt so admission time counts
	// against the budget. context.WithoutCancel detaches from the request's
	// own cancellation (the response is about to be written and r's
	// lifecycle ends), while WithDeadline still bounds the goroutine.
	//
	// Deliberately derived from ctx (the admission context the caller
	// built), not r.Context() as design §3.8.2 step 4's text literally
	// reads: ctx carries the values attachSkillResolver and
	// withHubAgentDefaults attached after r.Context() was read, which
	// Manager.Start needs just as much on the async path as on the
	// synchronous one.
	deadline := a.receivedAt.Add(time.Duration(a.timeoutSeconds) * time.Second)
	// The cause-carrying parent lets a shutdown cancel ctx' with
	// errBrokerShutdown, which runLaunch tells apart from a local stop or
	// delete (rec.cancel, cause context.Canceled; design wave 3 §1 C-e).
	causeCtx, cancelCause := context.WithCancelCause(context.WithoutCancel(ctx))
	runCtx, cancelDeadline := context.WithDeadline(causeCtx, deadline.Add(-scionrt.AsyncLaunchAdmissionSkew))
	cancel := func() {
		cancelDeadline()
		cancelCause(context.Canceled)
	}

	rec := newLaunchRecord(a.launchID, a.agentID, a.kind, s.resolveHubNameForLaunch(r), deadline, cancel)
	rec.RunID = a.runID
	rec.shutdown = func() { cancelCause(errBrokerShutdown) }
	// A flat create's run is this launch's from the start, so that a
	// failure before Manager.Start, and boot recovery from the first
	// journal write on, finish that run (ptone/scion#5768). A start or
	// restart launch records its run when it reaches Manager.Start, as
	// before.
	if a.kind == store.LaunchKindCreate && a.lc.owned != nil {
		rec.ownedRunMu.Lock()
		rec.ownedRun = ownedRunKey{projectID: a.lc.owned.projectID, agentID: a.lc.owned.agentID, runID: a.lc.owned.runID}
		rec.ownedRunMu.Unlock()
	}
	// A drain that began after the handler's own check refuses here, before
	// the launch is accepted; once tracked, the drain cancels it instead.
	if !s.trackAsyncLaunch(rec) {
		cancel()
		if a.onDrainRefused != nil {
			a.onDrainRefused()
		}
		writeBrokerDraining(w, a.op)
		return false
	}
	// The caller already registered under the name and waited for any
	// earlier start of it before buildStartContext
	// (beginNameRegistration), so the launch takes that registration over
	// instead of beginning a new one: there is nothing left to wait for. If
	// a stop, delete or newer start took the name in the meantime, refuse
	// with the 409 the wait itself gives, before accepting anything.
	if testHookAsyncLaunchBeforeHandOff != nil {
		testHookAsyncLaunchBeforeHandOff(ctx, key, rec, a.lc)
	}
	if !ss.handOff(rec) {
		s.untrackAsyncLaunch(rec)
		cancel()
		a.writeCancelled()
		return false
	}
	// Journal the launch before it is accepted (design wave 3 §1 C-a write
	// 1). A journal failure is logged and the launch proceeds.
	rec.journal = s.launchJournal.admit(rec, journalRecord{
		ProjectID:       key.ProjectID,
		Slug:            key.Slug,
		AgentName:       opts.Name,
		ProjectPath:     opts.ProjectPath,
		HubProjectID:    opts.HubProjectID,
		SharedWorkspace: opts.SharedWorkspace,
		RuntimeName:     a.runtimeName,
		RuntimeIdentity: s.launchRuntimeIdentity(a.lc.mgr),
		Profile:         opts.Profile,
	})

	resp := CreateAgentResponse{
		LaunchPending:    true,
		LaunchID:         a.launchID,
		LaunchInstanceID: s.launchInstanceID,
	}
	if a.onAccepted != nil {
		a.onAccepted(&resp)
	}
	writeJSON(w, a.status, resp)

	go s.runLaunch(runCtx, rec, a.lc)
	return true
}

// testHookAsyncLaunchBeforeHandOff, when set (tests only), runs in
// acceptAsyncLaunch immediately before the registration is handed over to
// the launch, with the admission context, the launch record and the launch
// context the launch will run with.
var testHookAsyncLaunchBeforeHandOff func(ctx context.Context, key launchKey, rec *launchRecord, lc launchCtx)

// asyncStartInputs are an async start's identity, budget and resolved
// runtime, as startAgent read and resolved them.
type asyncStartInputs struct {
	// id is the agent's slug (the URL path segment), projectID the
	// request's project ID, and hubAgentID the hub's agent ID (the agentId
	// query parameter).
	id, projectID, hubAgentID string
	// launchID, timeoutSeconds and keepaliveSeconds are the request's
	// launch fields.
	launchID         string
	timeoutSeconds   int
	keepaliveSeconds int
	// receivedAt is when startAgent received the request.
	receivedAt  time.Time
	runtimeName string
	// owned is the flat instance's ownership mirror for this run (nil on a
	// legacy Runtime Broker), handed over to the launch.
	owned *ownedStart
}

// beginAsyncStart is an async start of an existing agent (design wave 3 §2
// D-f; ptone/scion#4669): startAgent has already registered the start under
// the agent's name (ss), run buildStartContext, the runtime checks, the
// harness-config policy gate and the inline config update, and decided
// resume. This hands the registration over to a launch whose runLaunch
// runs Manager.Start, and answers 202 with the launch echo. There is no
// Preflight (create only) and no GCS download (create only).
//
// The launch is recorded as kind start for a restart's start leg too: the
// start request carries no kind, and broker-side the kind decides only that
// a start launch never deletes agent files. The Hub's launch row holds the
// real kind.
func (s *Server) beginAsyncStart(w http.ResponseWriter, r *http.Request, ctx context.Context, ss *syncStart, span trace.Span, opts api.StartOptions, mgr agent.Manager, in asyncStartInputs) bool {
	return s.acceptAsyncLaunch(w, r, ctx, ss, asyncAdmission{
		op:             "start",
		launchID:       in.launchID,
		agentID:        in.hubAgentID,
		kind:           store.LaunchKindStart,
		runID:          opts.RunID,
		receivedAt:     in.receivedAt,
		timeoutSeconds: in.timeoutSeconds,
		status:         http.StatusAccepted,
		runtimeName:    in.runtimeName,
		lc: launchCtx{
			projectID:       in.projectID,
			keepalive:       time.Duration(in.keepaliveSeconds) * time.Second,
			opts:            opts,
			mgr:             mgr,
			runtimeName:     in.runtimeName,
			key:             ss.key,
			sharedWorkspace: opts.SharedWorkspace,
			owned:           in.owned,
		},
		writeCancelled: func() {
			s.writeStartRegistrationError(w, span, in.id, in.projectID, opts.RunID, errCreateStartCancelled)
		},
	})
}
