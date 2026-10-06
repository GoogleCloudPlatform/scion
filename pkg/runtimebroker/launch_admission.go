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
// up to here is identical to the synchronous path (buildStartContext,
// attachSkillResolver, withHubAgentDefaults); this adds the one extra
// admission step the async path needs, Manager.Preflight, then accepts the
// launch and continues it in runLaunch.
//
// Nothing captured in the launchCtx passed to runLaunch, nor runLaunch
// itself, retains r or any value derived from *http.Request: everything it
// needs (the resolved hub connection name, opts, the request body already
// decoded into req) is plain data copied out here, in this synchronous
// handler goroutine, before go runLaunch starts (design §7 P1b-1 B-6).
func (s *Server) beginAsyncLaunch(w http.ResponseWriter, r *http.Request, ctx context.Context, req CreateAgentRequest, opts api.StartOptions, mgr agent.Manager, attempt *dispatchAttempt, markAttemptFailed func(int, string), span trace.Span, receivedAt time.Time) {
	// The GCS download itself runs in runLaunch (design §3.1), but its
	// directory check only reads the filesystem, so an invalid workspace
	// directory is answered here with the same 400 the synchronous path
	// gives, not reported as a failed launch after the 201. runLaunch's
	// download validates again before it creates anything. Any other error
	// from computing the directory is left to runLaunch's download step,
	// which reports it as before.
	if req.WorkspaceStoragePath != "" {
		if _, attemptMsg, httpMessage, err := s.resolveGCSWorkspaceDir(req); errors.Is(err, errInvalidWorkspaceDir) {
			span.SetStatus(codes.Error, err.Error())
			markAttemptFailed(http.StatusBadRequest, attemptMsg)
			BadRequest(w, httpMessage)
			return
		}
	}

	if err := mgr.Preflight(ctx, opts); err != nil {
		span.SetStatus(codes.Error, err.Error())
		if errors.Is(err, config.ErrHarnessConfigNotFound) || errors.Is(err, config.ErrTemplateNotFound) {
			markAttemptFailed(http.StatusNotFound, "failed to create agent")
			// Names the resource without err's own text, which can carry
			// broker paths (ptone/scion#3113); the full error is logged.
			s.agentLifecycleLog.Warn("Agent create failed: preflight: template or harness-config not found",
				"agent_id", req.ID, "project_id", req.ProjectID, "run_id", opts.RunID, "error", err)
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "Failed to create agent: "+notFoundResourceText(err, opts.TemplateName), nil)
			return
		}
		markAttemptFailed(http.StatusInternalServerError, "failed to create agent")
		// Fixed text, as the synchronous create gives for a Manager.Start
		// failure (ptone/scion#3113): the raw error can carry runtime
		// detail. The full error reaches only the broker log.
		s.agentLifecycleLog.Error("Agent create failed: preflight",
			"agent_id", req.ID, "project_id", req.ProjectID, "run_id", opts.RunID, "error", err)
		RuntimeError(w, runtimeOpError(opCreateAgent, err).Error())
		return
	}

	slug := req.Slug
	if slug == "" {
		slug = req.Name
	}
	key := launchKey{ProjectID: req.ProjectID, Slug: slug}

	// design §3.8.2 step 4: the broker's deadline is 20s before the Hub's
	// launch_deadline, measured from receivedAt so admission time counts
	// against the budget. context.WithoutCancel detaches from the request's
	// own cancellation (the response is about to be written and r's
	// lifecycle ends), while WithDeadline still bounds the goroutine.
	//
	// Deliberately derived from ctx (the admission context createAgent
	// built), not r.Context() as design §3.8.2 step 4's text literally
	// reads: ctx carries the values attachSkillResolver and
	// withHubAgentDefaults attached after r.Context() was read, which
	// Manager.Start needs just as much on the async path as on the
	// synchronous one.
	deadline := receivedAt.Add(time.Duration(req.LaunchTimeoutSeconds) * time.Second)
	runCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), deadline.Add(-20*time.Second))

	rec := newLaunchRecord(req.LaunchID, req.ID, store.LaunchKindCreate, s.resolveHubNameForLaunch(r), deadline, cancel)
	rec.RunID = req.RunID
	supersededDone := s.launchRegistry.Begin(key, rec)

	resp := CreateAgentResponse{
		LaunchPending:    true,
		LaunchID:         req.LaunchID,
		LaunchInstanceID: s.launchInstanceID,
	}
	// Complete the dispatch attempt as succeeded with the accepted response,
	// so a same-replica RequestID replay returns it without launching twice
	// (design §3.8.2 step 2; B-1).
	if attempt != nil {
		s.dispatchAttemptsMu.Lock()
		s.completeAttempt(attempt, dispatchAttemptSucceeded, http.StatusCreated, &resp, nil, "")
		s.dispatchAttemptsMu.Unlock()
	}
	writeJSON(w, http.StatusCreated, resp)

	lc := launchCtx{
		req:             req,
		opts:            opts,
		mgr:             mgr,
		key:             key,
		sharedWorkspace: opts.SharedWorkspace,
		supersededDone:  supersededDone,
	}
	go s.runLaunch(runCtx, rec, lc)
}
