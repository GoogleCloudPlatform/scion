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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// gateAction classifies a claim/checkpoint/keepalive answer (design
// t1-async-create-v11.md §3.8.2's gate-answer table, which applies to all
// three report kinds, not only claim/checkpoint; review r1 F-5).
type gateAction int

const (
	// gateContinue covers applied/duplicate: proceed normally.
	gateContinue gateAction = iota
	// gateCompleted means the Hub already saw this agent reach running
	// during this launch (e.g. a race with a heartbeat/status write): skip
	// remaining checkpoints, but Run continues, and a later failure must
	// send nothing and never clean up.
	gateCompleted
	// gateAbortNoCleanup covers 409 superseded/other_owner: abort, but the
	// other launch owns the resource names, so no deletion and no terminal.
	gateAbortNoCleanup
	// gateAbortCleanup covers every other non-2xx answer: abort, clean up
	// resources, and delete marker-guarded files for a create launch.
	gateAbortCleanup
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
	// 403, 404 agent_launch_unknown, and every other 409 reason
	// (deleted/stopped/timed_out/lost/failed/not_launched).
	return gateAbortCleanup
}

// shouldCleanupAfterFailureReport decides whether a failed-report's answer
// means the broker must clean up (design §3.8.2 step 7, F2(d)): every answer
// except "completed" and 409 superseded/other_owner does.
func shouldCleanupAfterFailureReport(result *hubclient.AgentLaunchReportResult) bool {
	if result.HTTPStatus == 0 {
		return result.Result != hubclient.AgentLaunchReportResultCompleted
	}
	if result.HTTPStatus == http.StatusConflict &&
		(result.Reason == hubclient.AgentLaunchReportReasonSuperseded || result.Reason == hubclient.AgentLaunchReportReasonOtherOwner) {
		return false
	}
	return true // 403, 404 agent_launch_unknown, every other 409 reason
}

// shouldCleanupAfterSucceededReport decides whether a succeeded report's
// answer means the broker must remove what it just started (design §3.8.2,
// §3.10 "a late succeeded answered 409 timed_out"; review r1 F-6). Unlike
// shouldCleanupAfterFailureReport, a 200 answer (applied/duplicate/
// completed) here means the Hub accepted the success, so it never cleans
// up; only a non-2xx answer other than 409 superseded/other_owner (the
// owning launch holds the names) does.
func shouldCleanupAfterSucceededReport(result *hubclient.AgentLaunchReportResult) bool {
	if result.HTTPStatus == 0 {
		return false
	}
	if result.HTTPStatus == http.StatusConflict &&
		(result.Reason == hubclient.AgentLaunchReportReasonSuperseded || result.Reason == hubclient.AgentLaunchReportReasonOtherOwner) {
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
	supersededDone  <-chan struct{}
}

// runLaunch is the async-create launch goroutine (design §3.8.2 step 5). It
// runs with ctx' = WithDeadline(WithoutCancel(the admission context),
// receivedAt + LaunchTimeoutSeconds - 20s), built by the caller
// (beginAsyncLaunch) before this goroutine starts.
func (s *Server) runLaunch(ctx context.Context, rec *launchRecord, lc launchCtx) {
	defer rec.cancel() // review r1 F-10: ctx' must not outlive runLaunch on any path
	defer s.launchRegistry.Finish(lc.key, rec)
	defer removeLaunchMarkerIfMatches(lc.opts.ProjectPath, lc.sharedWorkspace, lc.key.Slug, rec.ID)

	sender := newLaunchSender(s, rec, rec.AgentID, s.launchInstanceID, time.Duration(lc.req.LaunchKeepaliveSeconds)*time.Second)
	// The keepalive runs throughout, including while the claim itself is
	// still blocked on backoff (design §3.8.5, B-3 r9-4; review r1 F-18), so
	// it starts before SendClaim, not after.
	sender.StartKeepalive(ctx)
	// Deferred in this order so that, at return, StopKeepalive (running
	// first, LIFO) signals the loop before WaitKeepaliveStopped (running
	// second) blocks on it -- the reverse order would wait on a goroutine
	// nothing has told to stop yet on an early return (review r1 F-10).
	defer sender.WaitKeepaliveStopped()
	defer sender.StopKeepalive()

	currentStep := "claim"

	// Step 1: claim, synchronous, before anything touches the runtime.
	claimResult, claimErr := sender.SendClaim(ctx)
	if claimErr != nil {
		// Hub unreachable for the whole blocking window: ctx' has expired.
		// Follow the failure rule (step 7) with failed{hub_unreachable}.
		s.failLaunch(ctx, sender, rec, lc, false, currentStep, "hub_unreachable", "claim: hub unreachable")
		return
	}

	alreadyCompleted := false
	switch classifyGateAnswer(claimResult) {
	case gateAbortNoCleanup:
		return
	case gateAbortCleanup:
		s.cleanupAbortedLaunch(lc.mgr, rec, lc)
		return
	case gateCompleted:
		alreadyCompleted = true
	}

	// Step 2, F5: wait for a superseded record's cleanup before writing the
	// shared marker, then write it (create launches only).
	WaitSuperseded(ctx, lc.supersededDone)
	currentStep = "launching"
	if rec.Kind == store.LaunchKindCreate && lc.opts.ProjectPath != "" {
		if err := writeLaunchMarker(lc.opts.ProjectPath, lc.sharedWorkspace, lc.key.Slug, rec.ID); err != nil {
			// review r1 F-20: a marker write failure silently disables file
			// cleanup for this launch (cleanupAbortedLaunch's marker check
			// can never match), so fail the launch here rather than starting
			// the runtime with no way to clean up its files on abort.
			s.agentLifecycleLog.Error("runLaunch: failed to write launch marker; failing the launch rather than risk undeletable files",
				"agent_id", rec.AgentID, "launch_id", rec.ID, "error", err)
			s.failLaunch(ctx, sender, rec, lc, alreadyCompleted, currentStep, "runtime_error", "failed to write launch marker: "+err.Error())
			return
		}
	}

	// Step 3: optional GCS workspace download, identical to the synchronous
	// path's admission step (§3.1), just run here instead.
	opts, _, _, dlErr := s.downloadWorkspaceFromGCS(ctx, lc.req, lc.opts)
	if dlErr != nil {
		s.failLaunch(ctx, sender, rec, lc, alreadyCompleted, currentStep, "runtime_error", dlErr.Error())
		return
	}
	lc.opts = opts

	// Step 4: Manager.Start, run in its own goroutine so a keepalive answer
	// that demands an abort (design §3.8.2's gate-answer table applies to
	// keepalives too) can cancel ctx' and act immediately instead of
	// waiting for Start to return on its own (review r1 F-5).
	type startResult struct {
		info *api.AgentInfo
		err  error
	}
	startCh := make(chan startResult, 1)
	go func() {
		info, err := lc.mgr.Start(ctx, lc.opts)
		startCh <- startResult{info, err}
	}()

	var sr startResult
	select {
	case sr = <-startCh:
		// Start returned on its own below.
	case <-sender.KeepaliveAborted():
		outcome := sender.LastAbortOutcome()
		rec.cancel() // unblocks Start, which honours ctx' cancellation
		sr = <-startCh
		if outcome.action == gateAbortCleanup {
			s.cleanupAbortedLaunch(lc.mgr, rec, lc)
		}
		// gateAbortNoCleanup (superseded/other_owner): no cleanup, no
		// terminal -- the Hub already told us this launch is over and
		// another one owns the names.
		return
	}

	if sr.err != nil {
		code, message := classifyStartError(ctx, sr.err)
		s.failLaunch(ctx, sender, rec, lc, alreadyCompleted, currentStep, code, message)
		return
	}

	// Step 6: success. Send succeeded even if an earlier gate already
	// answered completed (design §3.8.2 step 5.6, F3): "This also applies
	// after an earlier completed answer."
	terminalCtx, cancel := terminalContext(rec.Deadline)
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
		}
	}
	result, err := sender.SendTerminal(terminalCtx, true, "", "", "", info)
	if err != nil {
		// No definitive answer by the TTL: no cleanup (same rule as a
		// failed report, design §3.8.2 step 7).
		s.agentLifecycleLog.Warn("runLaunch: succeeded report did not get a definitive answer within the TTL",
			"agent_id", rec.AgentID, "launch_id", rec.ID, "error", err)
		s.forceHeartbeatAll("create", rec.AgentID)
		return
	}
	if shouldCleanupAfterSucceededReport(result) {
		// review r1 F-6: a late succeeded answered e.g. 409 timed_out means
		// the Hub has already recorded this launch as over; the broker must
		// remove what it just started rather than leave it running.
		s.agentLifecycleLog.Warn("runLaunch: succeeded report was answered after the Hub ended the launch; cleaning up",
			"agent_id", rec.AgentID, "launch_id", rec.ID, "result", result)
		s.cleanupAbortedLaunch(lc.mgr, rec, lc)
		return
	}
	s.forceHeartbeatAll("create", rec.AgentID)
}

// failLaunch implements design §3.8.2 step 7 (F2(d), "report first, then
// clean up only if the Hub confirms the agent never ran"). alreadyCompleted
// skips the report entirely, per "if an earlier gate or keepalive already
// answered completed, send nothing and do not clean up". step records which
// generic step (claim/launching) the launch was in (design §3.9; review r1
// F-11).
func (s *Server) failLaunch(ctx context.Context, sender *launchSender, rec *launchRecord, lc launchCtx, alreadyCompleted bool, step, errorCode, message string) {
	if alreadyCompleted || sender.IsCompleted() {
		return
	}

	// The terminal's own context: retries continue past ctx' (which may
	// already be expired, e.g. the launch-timeout case) until deadline +
	// 10 min (design §3.8.5), never ctx' itself.
	terminalCtx, cancel := terminalContext(rec.Deadline)
	defer cancel()

	result, err := sender.SendTerminal(terminalCtx, false, step, errorCode, message, nil)
	if err != nil {
		// No definitive answer by the TTL: the Hub has reaped the record by
		// then, and `scion delete` removes the resources. No cleanup here.
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
// (design §3.8.4).
func (s *Server) cleanupAbortedLaunch(mgr agent.Manager, rec *launchRecord, lc launchCtx) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if err := mgr.CleanupLaunch(cleanupCtx, rec.Handles); err != nil {
		s.agentLifecycleLog.Warn("runLaunch: failed to clean up launch resources",
			"agent_id", rec.AgentID, "launch_id", rec.ID, "error", err)
	}

	if rec.Kind != store.LaunchKindCreate || lc.opts.ProjectPath == "" {
		return
	}
	if !launchMarkerMatches(lc.opts.ProjectPath, lc.sharedWorkspace, lc.key.Slug, rec.ID) {
		// A newer launch's marker write means this one's files are no
		// longer this launch's to delete (design §3.8.4).
		return
	}
	if _, err := agent.DeleteAgentFiles(lc.opts.Name, lc.opts.ProjectPath, true); err != nil {
		s.agentLifecycleLog.Warn("runLaunch: failed to clean up agent files",
			"agent_id", rec.AgentID, "launch_id", rec.ID, "error", err)
	}
}

// terminalContext returns a fresh context bounded at deadline + 10 min
// (design §3.8.5's terminal TTL), never ctx' — a launch that timed out must
// still be able to retry its failed{launch_timeout} report after ctx' itself
// has expired.
func terminalContext(deadline time.Time) (context.Context, context.CancelFunc) {
	ttl := time.Until(deadline.Add(10 * time.Minute))
	if ttl < 0 {
		ttl = 0
	}
	return context.WithTimeout(context.Background(), ttl)
}

// classifyStartError maps a Manager.Start failure to a launch error code
// (design §3.9; full progress/error-code detail is P3 scope). ctx having
// already expired (DeadlineExceeded) takes precedence: that is ctx', so it
// means the launch ran out of its budget, which is launch_timeout regardless
// of the error Start happened to return when it unwound.
func classifyStartError(ctx context.Context, err error) (code, message string) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "launch_timeout", "launch timed out before the agent started"
	}
	switch {
	case errors.Is(err, agent.ErrContainerNameInUse):
		return "name_in_use", err.Error()
	case errors.Is(err, config.ErrTemplateNotFound), errors.Is(err, config.ErrHarnessConfigNotFound):
		return "template_not_found", err.Error()
	default:
		return "runtime_error", err.Error()
	}
}
