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
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// gateAction classifies a claim/checkpoint/keepalive answer (design
// t1-async-create-v11.md §3.8.2's gate-answer table, which applies to all
// three report kinds, not only claim/checkpoint).
type gateAction int

const (
	// gateContinue covers applied/duplicate: proceed normally.
	gateContinue gateAction = iota
	// gateCompleted means the Hub already saw this agent reach running
	// during this launch (e.g. a status write landing independently): skip
	// remaining checkpoints, but Run continues, and a later failure must
	// send nothing and never clean up.
	gateCompleted
	// gateAbortNoCleanup covers 409 superseded/other_owner: abort, but the
	// other launch owns the resource names, so no deletion and no terminal.
	gateAbortNoCleanup
	// gateAbortCleanup covers the design's remaining listed reasons (409
	// deleted/stopped/timed_out/lost/failed/not_launched, 403, 404
	// agent_launch_unknown): abort, clean up resources, and delete
	// marker-guarded files for a create launch.
	gateAbortCleanup
	// gateStopNoCleanup covers 400/401: a protocol or auth failure the
	// design's table does not define a reason for. It is not evidence the
	// agent never ran, so it gets neither the "no cleanup" treatment of
	// superseded/other_owner (which implies another launch owns the names)
	// nor the "clean up" treatment of the design's listed reasons: stop
	// reporting and leave everything in place.
	gateStopNoCleanup
)

// classifyGateAnswer maps one claim/checkpoint/keepalive result to a
// gateAction.
func classifyGateAnswer(result *hubclient.AgentLaunchReportResult) gateAction {
	if result.HTTPStatus == 0 {
		if result.Result == hubclient.AgentLaunchReportResultCompleted {
			return gateCompleted
		}
		return gateContinue // applied | duplicate
	}
	if result.HTTPStatus == http.StatusConflict &&
		(result.Reason == hubclient.AgentLaunchReportReasonSuperseded || result.Reason == hubclient.AgentLaunchReportReasonOtherOwner) {
		return gateAbortNoCleanup
	}
	if result.HTTPStatus == http.StatusBadRequest || result.HTTPStatus == http.StatusUnauthorized {
		return gateStopNoCleanup
	}
	// 403, 404 agent_launch_unknown, and every other 409 reason
	// (deleted/stopped/timed_out/lost/failed/not_launched).
	return gateAbortCleanup
}

// shouldCleanupAfterFailureReport decides whether a failed-report's answer
// means the broker must clean up (design §3.8.2 step 7, F2(d)): every answer
// except "completed", 409 superseded/other_owner and 400/401 does.
func shouldCleanupAfterFailureReport(result *hubclient.AgentLaunchReportResult) bool {
	if result.HTTPStatus == 0 {
		return result.Result != hubclient.AgentLaunchReportResultCompleted
	}
	if result.HTTPStatus == http.StatusConflict &&
		(result.Reason == hubclient.AgentLaunchReportReasonSuperseded || result.Reason == hubclient.AgentLaunchReportReasonOtherOwner) {
		return false
	}
	if result.HTTPStatus == http.StatusBadRequest || result.HTTPStatus == http.StatusUnauthorized {
		return false
	}
	return true // 403, 404 agent_launch_unknown, every other 409 reason
}

// shouldCleanupAfterSucceededReport decides whether a succeeded report's
// answer means the broker must remove what it just started (design §3.8.2,
// §3.10 "a late succeeded answered 409 timed_out"). Unlike
// shouldCleanupAfterFailureReport, a 200 answer (applied/duplicate/
// completed) here means the Hub accepted the success, so it never cleans
// up; only a non-2xx answer other than 409 superseded/other_owner (the
// owning launch holds the names) and 400/401 does.
func shouldCleanupAfterSucceededReport(result *hubclient.AgentLaunchReportResult) bool {
	if result.HTTPStatus == 0 {
		return false
	}
	if result.HTTPStatus == http.StatusConflict &&
		(result.Reason == hubclient.AgentLaunchReportReasonSuperseded || result.Reason == hubclient.AgentLaunchReportReasonOtherOwner) {
		return false
	}
	if result.HTTPStatus == http.StatusBadRequest || result.HTTPStatus == http.StatusUnauthorized {
		return false
	}
	return true // 403, 404 agent_launch_unknown, every other 409 reason (notably timed_out)
}

// launchCtx bundles what runLaunch needs beyond ctx and rec: the original
// create request (for the optional GCS download and ProvisionOnly/Reprovision
// gating, never read for its *http.Request — there is none here), the
// resolved start options, the manager to launch with, the registry key, and
// whether this project uses the shared-workspace layout (for the marker
// path). It is built entirely from values already resolved by the admission
// phase, before the goroutine starts (design §7 P1b-1 B-6: the goroutine
// never touches *http.Request).
type launchCtx struct {
	req             CreateAgentRequest
	opts            api.StartOptions
	mgr             agent.Manager
	key             launchKey
	sharedWorkspace bool
	// owned is the flat instance's ownership mirror for this run, handed
	// over when the launch was accepted (nil on a legacy Runtime Broker).
	owned *ownedStart
	// downloadWorkspaceFromGCS, when set, replaces Server.downloadWorkspaceFromGCS
	// for this launch. Always nil in production (beginAsyncLaunch never sets
	// it); tests use it to control how long the download step takes without
	// a real GCS bucket, e.g. to land a concurrent keepalive answer inside
	// the download's window.
	downloadWorkspaceFromGCS func(ctx context.Context, req CreateAgentRequest, opts api.StartOptions) (api.StartOptions, string, string, error)
	// afterStartSelected, when set, is called with the launch's sender right
	// after runLaunch's select has taken Manager.Start's result. Always nil
	// in production (beginAsyncLaunch never sets it); tests use it to record
	// a keepalive abort at exactly that point, which a real keepalive can
	// only hit by chance.
	afterStartSelected func(*launchSender)
}

// runLaunch is the async-create launch goroutine (design §3.8.2 step 5). It
// runs with ctx' = WithDeadline(WithoutCancel(the admission context),
// receivedAt + LaunchTimeoutSeconds - 20s), built by the caller
// (beginAsyncLaunch) before this goroutine starts.
func (s *Server) runLaunch(ctx context.Context, rec *launchRecord, lc launchCtx) {
	// A flat instance's ownership mirror, handed over when the launch was
	// accepted, is released when the launch ends, on every return before
	// Start (for example claim errors and local cancels, gate and keepalive
	// aborts, the superseded wait's timeout, and marker and download
	// failures). Start's own completion consumes the mirror first
	// (completeOwnedStart), which makes this a no-op, and every path that
	// started Start waits for it to return before runLaunch does. Only this
	// launch's own mirror is released, never one a later start installed
	// under the same run ID. Deferred first so it runs after every other
	// deferred step.
	defer s.releaseOwnedMirror(lc.opts.RunID, lc.owned)
	defer rec.cancel() // ctx' must not outlive runLaunch on any path
	defer s.launchRegistry.Finish(lc.key, rec)
	// Runs just before Finish, after every report and cleanup.
	defer s.endLaunchJournal(rec)
	// The marker is kept when deleting the agent's files failed, or the
	// terminal report got no answer (a retried report may confirm a
	// clean-up, ptone/scion#5186): the
	// retry at the next boot checks it before deleting them
	// (ptone/scion#5177), and removes it once it settles.
	defer func() {
		if !rec.filesCleanupFailed.Load() && !rec.terminalUndelivered.Load() {
			removeLaunchMarkerIfMatches(lc.opts.ProjectPath, lc.sharedWorkspace, lc.key.Slug, rec.ID)
		}
	}()

	sender := newLaunchSender(s, rec, rec.AgentID, s.launchInstanceID, time.Duration(lc.req.LaunchKeepaliveSeconds)*time.Second)
	rec.journal.setSender(sender)
	// The keepalive runs throughout, including while the claim itself is
	// still blocked on backoff (design §3.8.5), so it starts before
	// SendClaim, not after.
	sender.StartKeepalive(ctx)
	// Deferred in this order so that, at return, StopKeepalive (running
	// first, LIFO) signals the loop before WaitKeepaliveStopped (running
	// second) blocks on it -- the reverse order would wait on a goroutine
	// nothing has told to stop yet on an early return.
	defer sender.WaitKeepaliveStopped()
	defer sender.StopKeepalive()

	currentStep := api.LaunchStepClaim

	// Step 1: claim, synchronous, before anything touches the runtime. The
	// keepalive is already running (started above), so its answers can end
	// the launch before, during or after this call.
	claimResult, claimErr := sender.SendClaim(ctx)
	if claimErr != nil {
		switch {
		case errors.Is(claimErr, errAbortedByKeepalive):
			s.handleKeepaliveAbort(sender, rec, lc)
			return
		case errors.Is(claimErr, context.Canceled) && shutdownCancelled(ctx):
			// The broker is shutting down (design wave 3 §1 C-e).
			s.failLaunch(ctx, sender, rec, lc, false, currentStep, api.LaunchCodeBrokerRestarted, launchShutdownMessage, "")
			return
		case errors.Is(claimErr, context.Canceled):
			// A local stop/delete (launchRegistry.CancelLocalFor) woke the
			// blocked claim, not a real deadline. The Hub already knows (or
			// will independently learn) this launch is over, so this is not
			// "hub unreachable" and gets no terminal.
			return
		default:
			// ctx' expired without any answer: follow the failure rule
			// (step 7) with failed{hub_unreachable}.
			s.failLaunch(ctx, sender, rec, lc, false, currentStep, api.LaunchCodeHubUnreachable, "claim: hub unreachable", "")
			return
		}
	}

	alreadyCompleted := false
	switch classifyGateAnswer(claimResult) {
	case gateAbortNoCleanup:
		return
	case gateAbortCleanup:
		s.cleanupAbortedLaunch(lc.mgr, rec, lc)
		return
	case gateStopNoCleanup:
		s.agentLifecycleLog.Error("runLaunch: claim got a protocol/auth answer; stopping with no cleanup",
			"agent_id", rec.AgentID, "launch_id", rec.ID)
		return
	case gateCompleted:
		alreadyCompleted = true
		// "completed" skips every remaining checkpoint (design §3.8.2's
		// table), including the runtime's pre-create checkpoints.
		sender.recordKeepaliveCompleted()
	}
	if sender.IsAborted() {
		s.handleKeepaliveAbort(sender, rec, lc)
		return
	}
	// The claim has been applied: any failure from here on is reported
	// under the launching step (design §3.9).
	currentStep = api.LaunchStepLaunching

	// Step 2: write the shared marker (create launches only). There is no
	// superseded launch to wait for here: the create already registered
	// under the name and waited for any earlier start of it before
	// buildStartContext, and the launch took that registration over
	// (createAgent, beginAsyncLaunch; ptone/scion#2632).
	if rec.Kind == store.LaunchKindCreate && lc.opts.ProjectPath != "" {
		if err := writeLaunchMarker(lc.opts.ProjectPath, lc.sharedWorkspace, lc.key.Slug, rec.ID); err != nil {
			// A marker write failure silently disables file cleanup for this
			// launch (cleanupAbortedLaunch's marker check can never match),
			// so fail the launch here rather than starting the runtime with
			// no way to clean up its files on abort.
			//
			// The report carries fixed text (ptone/scion#3113): the os error
			// names broker filesystem paths. The full error is logged here.
			s.agentLifecycleLog.Error("runLaunch: failed to write launch marker; failing the launch rather than risk undeletable files",
				"agent_id", rec.AgentID, "project_id", lc.req.ProjectID, "run_id", lc.opts.RunID,
				"launch_id", rec.ID, "error", err)
			s.failLaunch(ctx, sender, rec, lc, alreadyCompleted, currentStep, api.LaunchCodeRuntimeError, runtimeOpError("write launch marker", err).Error(), "")
			return
		}
	}
	if sender.IsAborted() {
		s.handleKeepaliveAbort(sender, rec, lc)
		return
	}

	// Step 3: optional GCS workspace download, identical to the synchronous
	// path's admission step (§3.1), just run here instead.
	download := s.downloadWorkspaceFromGCS
	if lc.downloadWorkspaceFromGCS != nil {
		download = lc.downloadWorkspaceFromGCS
	}
	opts, _, dlMessage, dlErr := download(ctx, lc.req, lc.opts)
	if dlErr != nil {
		if shutdownCancelled(ctx) {
			s.failLaunch(ctx, sender, rec, lc, alreadyCompleted, currentStep, api.LaunchCodeBrokerRestarted, launchShutdownMessage, "")
			return
		}
		if locallyCancelled(ctx) {
			// Same rule as Start's local-cancel case below: keyed on ctx',
			// not on the error the download returned.
			return
		}
		// Report the synchronous path's client text (dlMessage), never
		// dlErr's own text, which names the workspace path or GCS detail;
		// the download step has logged the cause (ptone/scion#3496). This
		// holds for the invalid-directory and unconfigured-bucket refusals
		// too: admission (beginAsyncLaunch) normally answers those with the
		// sync 400/422, and one only found here reports the same text,
		// under runtime_error.
		s.failLaunch(ctx, sender, rec, lc, alreadyCompleted, currentStep, api.LaunchCodeRuntimeError, dlMessage, "")
		return
	}
	lc.opts = opts
	// Runtime hooks (design §3.8.3, §3.8.4): a Hub-answered checkpoint
	// immediately before each resource-creating call, and each created
	// resource recorded for CleanupLaunch.
	// A flat instance's ownership mirror checkpoint (installed by
	// buildStartContext) runs first.
	lc.opts.Checkpoint = chainCheckpoint(lc.opts.Checkpoint, sender.Checkpoint)
	// Each handle is journalled as it is recorded (design wave 3 §1 C-a
	// write 2), so a restarted broker can remove it.
	lc.opts.OnResourceCreated = func(h agent.ResourceHandle) {
		rec.AddHandle(h)
		rec.journal.update()
	}
	// The non-blocking step hook (design wave 3 §3 E-b): the runtime
	// records each step it reaches, and the progress poster reports the
	// latest one to the Hub at most once per progressInterval. A step
	// change also queues one journal write, without blocking.
	lc.opts.Progress = func(step, message string) {
		sender.Progress(step, message)
		rec.journal.stepChanged()
	}
	sender.StartProgress(ctx)
	// Deferred after the keepalive's stop/wait pair, so it runs before
	// them: StopKeepalive closes the channel the poster also exits on.
	defer sender.WaitProgressStopped()
	defer sender.StopKeepalive()
	if sender.IsAborted() {
		s.handleKeepaliveAbort(sender, rec, lc)
		return
	}

	// Step 4: Manager.Start, run in its own goroutine so a keepalive answer
	// that demands an abort (design §3.8.2's gate-answer table applies to
	// keepalives too) can cancel ctx' and act immediately instead of
	// waiting for Start to return on its own.
	type startResult struct {
		info *api.AgentInfo
		err  error
	}
	startCh := make(chan startResult, 1)
	go func() {
		ownedKey := s.ownedRunKeyFor(lc.opts.RunID)
		rec.ownedRunMu.Lock()
		rec.ownedRun = ownedKey
		rec.ownedRunMu.Unlock()
		info, err := lc.mgr.Start(ctx, lc.opts)
		// The async failure path cleans up the launch's journaled handles.
		err = s.completeOwnedStart(ctx, lc.mgr, lc.opts.RunID, err, false)
		startCh <- startResult{info, err}
	}()

	var sr startResult
	select {
	case sr = <-startCh:
		// Start returned on its own below.
		if lc.afterStartSelected != nil {
			lc.afterStartSelected(sender)
		}
	case <-sender.KeepaliveAborted():
		rec.cancel() // unblocks Start, which honours ctx' cancellation
		<-startCh    // let it unwind before deciding what to do
		s.handleKeepaliveAbort(sender, rec, lc)
		return
	}

	if sender.IsAborted() {
		// Start returned at about the same moment a keepalive answer
		// recorded an abort, and Go's select above chose startCh instead
		// of KeepaliveAborted (its choice between two ready cases is not
		// ordered). Route to the same handling the KeepaliveAborted case
		// above uses, rather than letting sr's result fall through to the
		// success/failure path below with the recorded abort outcome left
		// unacted on.
		s.handleKeepaliveAbort(sender, rec, lc)
		return
	}

	if sr.err != nil {
		if shutdownCancelled(ctx) {
			// The broker is shutting down (design wave 3 §1 C-e): report
			// broker_restarted, then clean up on a confirming answer, as
			// for any failure. Keyed on ctx', like the local-cancel case.
			s.failLaunch(ctx, sender, rec, lc, alreadyCompleted, stepOr(sender.LastStep(), api.LaunchStepLaunching), api.LaunchCodeBrokerRestarted, launchShutdownMessage, "")
			return
		}
		if locallyCancelled(ctx) {
			// A local stop/delete (launchRegistry.CancelLocalFor) woke Start
			// via ctx' cancellation, not a real deadline (the
			// launch_timeout case is context.DeadlineExceeded, handled by
			// classifyStartError below). Same rule as the claim's
			// local-cancel case: the Hub already knows,
			// or will independently learn, this launch is over, so no
			// terminal. Keyed on ctx' rather than on sr.err: the
			// exec-based runtimes (docker, podman, apple) return the killed
			// CLI's *exec.ExitError, not context.Canceled, when ctx' is
			// cancelled under them.
			//
			// The runtime leaves what it created to this launch's cleanup
			// (it skips its own start cleanup when OnResourceCreated is
			// set), so the recorded resources are removed here unless the
			// launch already completed. The local stop/delete handler owns
			// the agent files, so only the resources are removed. Every
			// delete is conditional on the identity recorded at create
			// time, so a newer launch's same-named resources are untouched.
			if !alreadyCompleted && !sender.IsCompleted() {
				rec.resourcesOnlyCleanup.Store(true)
				s.cleanupLaunchResources(lc.mgr, rec)
			}
			return
		}
		// Only now, after the keepalive/progress abort checks above and the
		// local-cancel check, is the error classified: a stop, delete or
		// superseding start that removed the pod must never be reported as
		// pod_disrupted (each cancels ctx' before deleting anything, and
		// the Hub's own answer arrives as an abort).
		code, step, message := classifyStartError(ctx, sr.err, lc.opts.TemplateName, sender.LastStep())
		// message is fixed text for the client (ptone/scion#3113); the
		// full error reaches only the broker's own log.
		// A skill failure anywhere in the error chain has its full text
		// logged once, with a correlation id, whatever the launch code (a
		// resolver cut off by the launch deadline reports launch_timeout).
		// The stored text is the skill failure's fixed text only for
		// skill_resolution_failed.
		launchSkill := skillFailureFrom(sr.err, lc.opts.RunID)
		s.agentLifecycleLog.Error("runLaunch: agent start failed", append(append([]any{
			"agent_id", rec.AgentID, "project_id", lc.req.ProjectID, "run_id", lc.opts.RunID,
			"launch_id", rec.ID, "error_code", code}, failureLogAttrs(sr.err, launchSkill)...),
			runtimeCauseLogAttrs(sr.err, launchSkill)...)...)
		if launchSkill != nil {
			s.logSkillResolutionFailure(launchSkill,
				"agent_id", rec.AgentID, "project_id", lc.req.ProjectID, "run_id", lc.opts.RunID, "launch_id", rec.ID)
			if code == ErrCodeSkillResolution {
				message = launchSkill.publicText()
			}
		}
		if sender.CheckpointUnreachable() {
			// A pre-create checkpoint blocked until ctx' expired with no
			// definitive Hub answer (design §3.8.2: failed{hub_unreachable}).
			code, message = api.LaunchCodeHubUnreachable, "checkpoint: hub unreachable"
		}
		// Computed from the final code: only a runtime_error carries a
		// cause, so the hub_unreachable override above carries none.
		cause := launchFailureCause(code, sr.err)
		s.failLaunch(ctx, sender, rec, lc, alreadyCompleted, step, code, message, cause)
		return
	}

	// Step 6: success. Send succeeded even if an earlier gate already
	// answered completed (design §3.8.2 step 5.6, F3): "This also applies
	// after an earlier completed answer."
	terminalCtx, cancel := s.launchTerminalContext(rec.Deadline)
	defer cancel()
	var info *hubclient.AgentLaunchReportInfo
	if sr.info != nil {
		info = &hubclient.AgentLaunchReportInfo{
			ID:              sr.info.ID,
			Slug:            sr.info.Slug,
			ContainerID:     sr.info.ContainerID,
			Name:            sr.info.Name,
			Template:        sr.info.Template,
			HarnessConfig:   sr.info.HarnessConfig,
			HarnessAuth:     sr.info.HarnessAuth,
			Image:           sr.info.Image,
			Runtime:         sr.info.Runtime,
			RuntimeState:    sr.info.RuntimeState,
			Profile:         sr.info.Profile,
			Phase:           sr.info.Phase,
			Activity:        sr.info.Activity,
			ContainerStatus: sr.info.ContainerStatus,
			RunID:           sr.info.RunID,

			HarnessConfigSource: sr.info.HarnessConfigSource,
			WorkspacePlacement:  sr.info.WorkspacePlacement,
		}
	}
	// Journalled before it is sent (design wave 3 §1 C-a write 3): a
	// restarted broker resends it unchanged and never turns it into a
	// failure.
	rec.journal.terminal(journalTerminal{Succeeded: true, AgentInfo: info})
	result, err := sender.SendTerminal(terminalCtx, true, "", "", "", "", info)
	if err != nil {
		// No definitive answer by the TTL: no cleanup (same rule as a
		// failed report, design §3.8.2 step 7); the journal record is kept so
		// the next boot resends the report (ptone/scion#5186).
		rec.terminalUndelivered.Store(true)
		s.agentLifecycleLog.Warn("runLaunch: succeeded report did not get a definitive answer within the TTL",
			"agent_id", rec.AgentID, "launch_id", rec.ID, "error", err)
		s.forceHeartbeatAll("create", rec.AgentID)
		return
	}
	if shouldCleanupAfterSucceededReport(result) {
		// A late succeeded answered e.g. 409 timed_out means the Hub has
		// already recorded this launch as over; the broker must remove what
		// it just started rather than leave it running.
		s.agentLifecycleLog.Warn("runLaunch: succeeded report was answered after the Hub ended the launch; cleaning up",
			"agent_id", rec.AgentID, "launch_id", rec.ID, "result", result)
		s.cleanupAbortedLaunch(lc.mgr, rec, lc)
		return
	}
	s.forceHeartbeatAll("create", rec.AgentID)
}

// locallyCancelled reports whether ctx' was cancelled rather than having
// reached its deadline, other than by the broker's shutdown drain
// (shutdownCancelled, checked first by every caller). Once the
// keepalive-abort paths have returned, the only remaining source of that
// cancellation is a local stop/delete (launchRegistry.CancelLocalFor):
// runLaunch itself cancels ctx' only on the KeepaliveAborted branch, which
// returns, and in its deferred cleanup.
func locallyCancelled(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled) && !errors.Is(context.Cause(ctx), errBrokerShutdown)
}

// handleKeepaliveAbort acts on the outcome a keepalive answer recorded
// (design §3.8.2's gate-answer table applies to keepalives too):
// gateAbortCleanup cleans up; gateAbortNoCleanup sends no terminal and
// cleans up nothing, because the owning launch holds the resource names.
// Callers have already made sure KeepaliveAborted has fired before calling
// this.
func (s *Server) handleKeepaliveAbort(sender *launchSender, rec *launchRecord, lc launchCtx) {
	outcome := sender.LastAbortOutcome()
	if outcome != nil && outcome.action == gateAbortCleanup {
		s.cleanupAbortedLaunch(lc.mgr, rec, lc)
	}
}

// failLaunch implements design §3.8.2 step 7 (F2(d), "report first, then
// clean up only if the Hub confirms the agent never ran"). alreadyCompleted
// skips the report entirely, per "if an earlier gate or keepalive already
// answered completed, send nothing and do not clean up". step records which
// generic step (claim/launching) the launch was in (design §3.9). cause is
// the fixed runtime cause code for a runtime_error (launchFailureCause), or
// "".
func (s *Server) failLaunch(ctx context.Context, sender *launchSender, rec *launchRecord, lc launchCtx, alreadyCompleted bool, step, errorCode, message, cause string) {
	if alreadyCompleted || sender.IsCompleted() {
		return
	}

	// The terminal's own context: retries continue past ctx' (which may
	// already be expired, e.g. the launch-timeout case) until deadline +
	// 10 min (design §3.8.5), never ctx' itself.
	terminalCtx, cancel := s.launchTerminalContext(rec.Deadline)
	defer cancel()

	rec.journal.terminal(journalTerminal{Step: step, ErrorCode: errorCode, Message: message, Cause: cause})
	result, err := sender.SendTerminal(terminalCtx, false, step, errorCode, message, cause, nil)
	if err != nil {
		// No definitive answer by the TTL: the Hub has reaped the record by
		// then. No cleanup here; the journal record is kept so the next
		// boot resends the report (ptone/scion#5186).
		rec.terminalUndelivered.Store(true)
		s.agentLifecycleLog.Warn("runLaunch: failed report did not get a definitive answer within the TTL",
			"agent_id", rec.AgentID, "launch_id", rec.ID, "error_code", errorCode, "error", err)
		return
	}
	if shouldCleanupAfterFailureReport(result) {
		s.cleanupAbortedLaunch(lc.mgr, rec, lc)
	}
}

// cleanupAbortedLaunch deletes the launch's runtime resources and, for a
// create launch whose marker still holds this launch's ID, its agent files
// (design §3.8.4). It returns the failed steps (cleanupFailure, each also
// recorded on rec), or nil; a skip is not a failure.
func (s *Server) cleanupAbortedLaunch(mgr agent.Manager, rec *launchRecord, lc launchCtx) error {
	return errors.Join(
		s.cleanupLaunchResources(mgr, rec),
		s.cleanupLaunchFiles(rec, lc.opts, lc.sharedWorkspace, lc.key.Slug),
	)
}

// cleanupLaunchFiles deletes a create launch's agent files when its marker
// still holds this launch's ID and no other run owns the files (design
// §3.8.4). opts needs only Name, ProjectPath, HubProjectID,
// SharedWorkspace and RunID, so boot recovery can rebuild it from the
// launch journal.
func (s *Server) cleanupLaunchFiles(rec *launchRecord, opts api.StartOptions, sharedWorkspace bool, slug string) error {
	if rec.Kind != store.LaunchKindCreate || opts.ProjectPath == "" {
		return nil
	}
	// The ownership checks and the removal run under the process-wide
	// workspace lock on the agent's files.
	lockWait := 60 * time.Second
	if s.agentFilesLockWait > 0 {
		lockWait = s.agentFilesLockWait
	}
	lockCtx, cancelLock := context.WithTimeout(context.Background(), lockWait)
	defer cancelLock()
	unlock, err := s.lockAgentFiles(lockCtx, opts.ProjectPath, opts.Name)
	if err != nil {
		s.agentLifecycleLog.Warn("runLaunch: skipped agent file cleanup: the agent's workspace lock is unavailable",
			"agent_id", rec.AgentID, "launch_id", rec.ID, "error", err)
		rec.filesCleanupFailed.Store(true)
		return failCleanup(rec, cleanupFailFilesLock)
	}
	defer unlock()
	if !launchMarkerMatches(opts.ProjectPath, sharedWorkspace, slug, rec.ID) {
		// A newer launch's marker write means this one's files are no
		// longer this launch's to delete (design §3.8.4).
		return nil
	}
	target := agent.RunOwnerTargetFor(opts)
	target.SharedWorkspace = target.SharedWorkspace || sharedWorkspace
	if owner, ownerErr := agentFilesRunOwner(target, opts.RunID); ownerErr != nil {
		// The owner of the files is unknown (ptone/scion#4771): keep them.
		s.agentLifecycleLog.Warn("runLaunch: skipped agent file cleanup: the owner of the agent's files is unknown",
			"agent_id", rec.AgentID, "launch_id", rec.ID, "run_id", opts.RunID, "error", ownerErr)
		return nil
	} else if owner != "" {
		// The files record a different run as their owner
		// (ptone/scion#2675): they are not this launch's to delete.
		s.agentLifecycleLog.Info("runLaunch: skipped agent file cleanup: the agent's files belong to another run",
			"agent_id", rec.AgentID, "launch_id", rec.ID, "run_id", opts.RunID, "files_run_id", owner)
		return nil
	}
	if _, err := agent.DeleteAgentFiles(opts.Name, opts.ProjectPath, true); err != nil && !errors.Is(err, agent.ErrAgentProjectUnresolved) {
		s.agentLifecycleLog.Warn("runLaunch: failed to clean up agent files",
			"agent_id", rec.AgentID, "launch_id", rec.ID, "error", err)
		rec.filesCleanupFailed.Store(true)
		return failCleanup(rec, cleanupFailFilesDelete)
	}
	return nil
}

// cleanupLaunchResources deletes the runtime resources the launch recorded
// (design §3.8.4), on a fresh context: ctx' may already be done.
func (s *Server) cleanupLaunchResources(mgr agent.Manager, rec *launchRecord) error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), scionrt.AsyncLaunchCleanupBudget)
	defer cancel()
	handles := rec.HandlesSnapshot()
	if err := mgr.CleanupLaunch(cleanupCtx, handles); err != nil {
		s.agentLifecycleLog.Warn("runLaunch: failed to clean up launch resources",
			"agent_id", rec.AgentID, "launch_id", rec.ID, "error", err)
		return failCleanup(rec, cleanupFailRuntimeResources)
	}
	// A flat instance finishes only this launch's run in its records.
	rec.ownedRunMu.Lock()
	key := rec.ownedRun
	rec.ownedRunMu.Unlock()
	s.finishCleanedRun(key, handles)
	return nil
}

// launchFailureCause returns the cause code a failed launch report carries
// for a Manager.Start error classified as code (ptone/scion#4666): the
// fixed details.cause code a synchronous create's runtime_error answer
// would carry (runtimeFailureCause, writeCreateRuntimeError), and only for a
// runtime_error, which is the code a cause qualifies. Never the error's
// text.
func launchFailureCause(code string, err error) string {
	if code != ErrCodeRuntimeError {
		return ""
	}
	_, cause, _ := runtimeFailureCause(err)
	return cause
}

// terminalContext returns a fresh context bounded at deadline + 10 min
// (design §3.8.5's terminal TTL), never ctx' — a launch that timed out must
// still be able to retry its failed{launch_timeout} report after ctx' itself
// has expired.
func terminalContext(deadline time.Time) (context.Context, context.CancelFunc) {
	return terminalContextFrom(context.Background(), deadline)
}

// terminalContextFrom is terminalContext derived from parent.
func terminalContextFrom(parent context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	ttl := time.Until(deadline.Add(scionrt.AsyncLaunchReportTTL))
	if ttl < 0 {
		ttl = 0
	}
	return context.WithTimeout(parent, ttl)
}

// launchTerminalContext is the context of a running launch's terminal send:
// terminalContext, also ended when a shutdown drain's wait ends, so the
// send stops and its terminal stays in the journal for the next boot
// (design wave 3 §1 C-e).
func (s *Server) launchTerminalContext(deadline time.Time) (context.Context, context.CancelFunc) {
	return terminalContextFrom(s.terminalSendContext(), deadline)
}

// shutdownCancelled reports whether ctx' was cancelled by the broker's
// shutdown drain (DrainLaunches) rather than by a local stop or delete.
func shutdownCancelled(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled) && errors.Is(context.Cause(ctx), errBrokerShutdown)
}

// stepOr returns step, or def when step is empty.
func stepOr(step, def string) string {
	if step == "" {
		return def
	}
	return step
}

// endLaunchJournal ends a launch's journal record when runLaunch returns:
// the launch leaves the drain set, and its record is removed (design wave
// 3 §1 C-a write 4). Once a shutdown drain's wait has ended, the record is
// kept instead: its terminal may not have been delivered, or its cleanup
// may have been cut short, and the next boot's recovery finishes both.
//
// A launch whose clean-up failed keeps its record too, as cleanup_pending,
// so the next boot's recovery retries the clean-up (ptone/scion#5177).
func (s *Server) endLaunchJournal(rec *launchRecord) {
	s.untrackAsyncLaunch(rec)
	if s.drainExpired() {
		return
	}
	if rec.terminalUndelivered.Load() {
		// The terminal report got no hub answer: its terminal_pending
		// record stays for the next boot to resend (ptone/scion#5186).
		s.agentLifecycleLog.Warn("runLaunch: the terminal report got no hub answer; its journal record is kept for a retry at the next broker start",
			"agent_id", rec.AgentID, "launch_id", rec.ID, "run_id", rec.RunID)
		return
	}
	if class := rec.cleanupFailure.Load(); class != nil {
		s.agentLifecycleLog.Warn("runLaunch: launch clean-up failed; its journal record is kept for a retry at the next broker start",
			"agent_id", rec.AgentID, "launch_id", rec.ID, "run_id", rec.RunID, "error_class", string(*class))
		scope := ""
		if rec.resourcesOnlyCleanup.Load() {
			scope = cleanupScopeResources
		}
		if rec.journal.cleanupPending(time.Now(), scope) {
			return
		}
		// The record could not be kept: remove it, as before #5177, so
		// it never sits in launching or terminal_pending after the hub
		// has answered.
	}
	rec.journal.remove()
}

// Async launch failure codes reported to the hub (alongside
// ErrCodeHarnessConfigUnusable and ErrCodeSkillResolution).
const (
	// LaunchErrCodeHarnessConfigPolicy: the harness-config policy refused
	// the launch (the synchronous paths answer 403 forbidden).
	LaunchErrCodeHarnessConfigPolicy = api.LaunchCodeHarnessConfigPolicy
	// LaunchErrCodeAgentStateUnavailable: the agent's broker-side state
	// directory is unavailable (config.ErrAgentStateDirUnavailable).
	LaunchErrCodeAgentStateUnavailable = api.LaunchCodeAgentStateUnavailable
	// LaunchErrCodeAgentStateConflict: the agent's broker-side state exists
	// but cannot be used as recorded, e.g. an unusable image provenance
	// record (config.ErrAgentStateConflict).
	LaunchErrCodeAgentStateConflict = api.LaunchCodeAgentStateConflict
)

// launchFailureText is the fixed report text for each launch code a
// runtime.LaunchFailure can carry (design wave 3 §3 E-d). classifyStartError
// appends only the runtime's reason enum, never the error's own text, which
// names the image reference, pod and namespace (ptone/scion#3113).
var launchFailureText = map[string]string{
	api.LaunchCodeImagePullFailed:     "the agent image could not be pulled",
	api.LaunchCodeInvalidImage:        "the agent image reference is not valid",
	api.LaunchCodeContainerConfig:     "the agent container could not be configured",
	api.LaunchCodeCrashLoop:           "the agent container keeps crashing on start",
	api.LaunchCodeInitContainerFailed: "an init container of the agent pod failed",
	api.LaunchCodeUnschedulable:       "the agent pod cannot be scheduled",
	api.LaunchCodePodDisrupted:        "the agent pod was preempted, evicted or deleted while starting",
	api.LaunchCodeLaunchTimeout:       "launch timed out waiting for the agent pod to be ready",
}

// maxLaunchReasonLen bounds the runtime reason a failure message carries.
const maxLaunchReasonLen = 64

// launchReasonEnum returns reason when it looks like a runtime reason enum
// (ASCII letters and digits, e.g. "ImagePullBackOff"), else "": anything else
// may be free text and is never reported.
func launchReasonEnum(reason string) string {
	if reason == "" || len(reason) > maxLaunchReasonLen {
		return ""
	}
	for _, c := range reason {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return ""
		}
	}
	return reason
}

// classifyStartError maps a Manager.Start failure to a launch error code,
// the step the launch failed in, and the report text (design §3.9; wave 3
// §3 E-d). lastStep is the latest step the launch reached
// (launchSender.LastStep); the step is a typed failure's own step, else
// lastStep, else launching. ctx having already expired (DeadlineExceeded)
// takes precedence: that is ctx', so it means the launch ran out of its
// budget, which is launch_timeout regardless of the error Start happened to
// return when it unwound.
func classifyStartError(ctx context.Context, err error, templateSlug, lastStep string) (code, step, message string) {
	step = lastStep
	if step == "" {
		step = api.LaunchStepLaunching
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		if step == api.LaunchStepAwaitingCapacity {
			// A GKE launch still waiting for node capacity (OQ-E2).
			return api.LaunchCodeLaunchTimeout, step, "timed out waiting for node capacity"
		}
		return api.LaunchCodeLaunchTimeout, step, "launch timed out before the agent started"
	}
	code, message = classifyStartErrorCode(err, templateSlug)
	if code != "" {
		return code, step, message
	}
	var lf *scionrt.LaunchFailure
	if errors.As(err, &lf) {
		if text, ok := launchFailureText[lf.Code]; ok {
			if lf.Step != "" {
				step = lf.Step
			}
			if reason := launchReasonEnum(lf.Reason); reason != "" {
				text += " (" + reason + ")"
			}
			return lf.Code, step, text
		}
	}
	// The same fixed text the synchronous create returns for a
	// Manager.Start failure (runtimeOpError): the raw error routinely
	// embeds a container ID, pod name, namespace, image reference or file
	// path. The full error is logged by runLaunch.
	return api.LaunchCodeRuntimeError, step, runtimeOpError(opCreateAgent, err).Error()
}

// classifyStartErrorCode is classifyStartError's cases that do not depend on
// the launch step or a runtime-typed failure. It returns "" when none
// matches.
func classifyStartErrorCode(err error, templateSlug string) (code, message string) {
	if d, ok := harnessPolicyRefusalFrom(err); ok {
		return LaunchErrCodeHarnessConfigPolicy, d.Message
	}
	if ue, ok := unusableProvisionerFrom(err); ok {
		return ErrCodeHarnessConfigUnusable, ue.PublicMessage()
	}
	var skillErr *agent.SkillResolutionError
	switch {
	case errors.Is(err, agent.ErrContainerNameInUse):
		// Fixed text, as on the synchronous create, start and restart
		// paths: a wrapped error can carry runtime detail (container name,
		// backend message) the client has no entitlement to see.
		return api.LaunchCodeNameInUse, agent.ErrContainerNameInUse.Error()
	case isRunNameConflict(err):
		// Another live run holds the agent name, and the runtime deleted
		// nothing of it (ptone/scion#2550). Fixed text: the wrapped error
		// names the namespace, object and the other run's ID.
		return api.LaunchCodeNameInUse, scionrt.ErrRunConflict.Error()
	case errors.As(err, &skillErr):
		// A required skill could not be resolved: the same code a
		// synchronous create or start returns, with a fixed reason phrase
		// naming the skill. runLaunch adds the correlation id and logs the
		// full error (see skillResolutionPublicText).
		return ErrCodeSkillResolution, skillResolutionPublicText(skillErr, "")
	case errors.Is(err, config.ErrTemplateNotFound), errors.Is(err, config.ErrHarnessConfigNotFound):
		// The same text the synchronous create gives: it names the
		// template or harness-config (the ptone/scion#1316 404 contract)
		// but not err's own text, which can name broker filesystem paths
		// or a content hash (ptone/scion#3113; see notFoundResourceText).
		// The full error is logged by runLaunch.
		return api.LaunchCodeTemplateNotFound, notFoundMessage(opCreateAgent, err, templateSlug)
	case errors.Is(err, config.ErrAgentStateDirUnavailable):
		return LaunchErrCodeAgentStateUnavailable, err.Error()
	case errors.Is(err, config.ErrAgentStateConflict):
		return LaunchErrCodeAgentStateConflict, err.Error()
	default:
		return "", ""
	}
}
