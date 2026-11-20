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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingResumeDispatcher wraps createAgentDispatcher to also record
// whether DispatchAgentCreate was called and the resume flag passed to
// DispatchAgentStart.
type recordingResumeDispatcher struct {
	createAgentDispatcher
	createCalled bool
	lastResume   bool
}

func (d *recordingResumeDispatcher) DispatchAgentCreate(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	d.createCalled = true
	return d.createAgentDispatcher.DispatchAgentCreate(ctx, agent)
}

func (d *recordingResumeDispatcher) DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error) {
	d.createCalled = true
	return d.createAgentDispatcher.DispatchAgentCreateWithGather(ctx, agent)
}

func (d *recordingResumeDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	d.lastResume = resume
	return d.createAgentDispatcher.DispatchAgentStart(ctx, agent, task, resume)
}

// TestClassifyExistingAgent_ResumableRequiresResume verifies ptone/scion#8584:
// suspended, stopped, halted, and active idle-suspend-claim agents are only
// classified as existingBranchStart when req.Resume is true. Created and
// provisioning agents keep being reused without Resume.
func TestClassifyExistingAgent_ResumableRequiresResume(t *testing.T) {
	plain := CreateAgentRequest{}
	resume := CreateAgentRequest{Resume: true}
	forceResume := CreateAgentRequest{Resume: true, ForceResume: true}
	provOnly := CreateAgentRequest{ProvisionOnly: true}
	gather := CreateAgentRequest{GatherEnv: true}

	cases := []struct {
		name string
		a    *store.Agent
		req  CreateAgentRequest
		want existingAgentBranch
	}{
		{"suspended without resume", &store.Agent{Phase: string(state.PhaseSuspended)}, plain, existingBranchNone},
		{"suspended with resume", &store.Agent{Phase: string(state.PhaseSuspended)}, resume, existingBranchStart},
		{"suspended provisionOnly", &store.Agent{Phase: string(state.PhaseSuspended)}, provOnly, existingBranchNone},
		{"running+idleClaim without resume", &store.Agent{Phase: string(state.PhaseRunning), IdleSuspendClaimActive: true}, plain, existingBranchNone},
		{"running+idleClaim with resume", &store.Agent{Phase: string(state.PhaseRunning), IdleSuspendClaimActive: true}, resume, existingBranchStart},
		{"suspended+idleClaim without resume", &store.Agent{Phase: string(state.PhaseSuspended), IdleSuspendClaimActive: true}, plain, existingBranchNone},
		{"suspended+idleClaim with resume", &store.Agent{Phase: string(state.PhaseSuspended), IdleSuspendClaimActive: true}, resume, existingBranchStart},
		{"stopped without resume", &store.Agent{Phase: string(state.PhaseStopped)}, plain, existingBranchNone},
		{"stopped with resume", &store.Agent{Phase: string(state.PhaseStopped)}, resume, existingBranchStart},
		{"halted without resume", &store.Agent{Phase: string(state.PhaseHalted)}, plain, existingBranchNone},
		{"halted with resume", &store.Agent{Phase: string(state.PhaseHalted)}, resume, existingBranchStart},
		{"error(resumePhase=suspended) without resume", &store.Agent{Phase: string(state.PhaseError), LaunchResumePhase: string(state.PhaseSuspended)}, plain, existingBranchNone},
		{"error(resumePhase=suspended) with resume", &store.Agent{Phase: string(state.PhaseError), LaunchResumePhase: string(state.PhaseSuspended)}, resume, existingBranchStart},
		{"error(resumePhase=stopped) without resume", &store.Agent{Phase: string(state.PhaseError), LaunchResumePhase: string(state.PhaseStopped)}, plain, existingBranchNone},
		{"error(resumePhase=stopped) with resume", &store.Agent{Phase: string(state.PhaseError), LaunchResumePhase: string(state.PhaseStopped)}, resume, existingBranchStart},
		{"error(resumePhase=halted) without resume", &store.Agent{Phase: string(state.PhaseError), LaunchResumePhase: string(state.PhaseHalted)}, plain, existingBranchNone},
		{"error(resumePhase=halted) with resume", &store.Agent{Phase: string(state.PhaseError), LaunchResumePhase: string(state.PhaseHalted)}, resume, existingBranchStart},
		{"error(no resumePhase) without resume", &store.Agent{Phase: string(state.PhaseError)}, plain, existingBranchNone},
		{"error(no resumePhase) with resume", &store.Agent{Phase: string(state.PhaseError)}, resume, existingBranchNone},
		{"error(no resumePhase) with force resume", &store.Agent{Phase: string(state.PhaseError)}, forceResume, existingBranchStart},
		{"running without resume", &store.Agent{Phase: string(state.PhaseRunning)}, plain, existingBranchNone},
		{"running with force resume", &store.Agent{Phase: string(state.PhaseRunning)}, forceResume, existingBranchNone},
		{"created without resume", &store.Agent{Phase: string(state.PhaseCreated)}, plain, existingBranchStart},
		{"created provisionOnly", &store.Agent{Phase: string(state.PhaseCreated)}, provOnly, existingBranchNone},
		{"provisioning without resume", &store.Agent{Phase: string(state.PhaseProvisioning)}, plain, existingBranchStart},
		{"provisioning with gatherEnv", &store.Agent{Phase: string(state.PhaseProvisioning)}, gather, existingBranchRecreate},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyExistingAgent(tc.a, tc.req))
		})
	}
}

// TestHandleExistingAgent_RefusesResumableWithoutResume verifies ptone/scion#8584:
// a raw POST /api/v1/projects/{id}/agents without resume against a suspended,
// stopped, or halted agent — including an active idle-suspend claim, an error
// agent whose LaunchResumePhase is resumable, and ProvisionOnly creates —
// returns 409 agent_exists_resumable naming the phase and hinting `scion resume`,
// and dispatches nothing.
func TestHandleExistingAgent_RefusesResumableWithoutResume(t *testing.T) {
	cases := []struct {
		name           string
		phase          state.Phase
		resumePhase    string
		provisionOnly  bool
		wantPhaseInMsg string
	}{
		{"suspended", state.PhaseSuspended, "", false, "suspended"},
		{"suspended provisionOnly", state.PhaseSuspended, "", true, "suspended"},
		{"stopped", state.PhaseStopped, "", false, "stopped"},
		{"stopped provisionOnly", state.PhaseStopped, "", true, "stopped"},
		{"halted", state.PhaseHalted, "", false, "halted"},
		{"error remembering suspended", state.PhaseError, string(state.PhaseSuspended), false, "suspended"},
		{"error remembering halted", state.PhaseError, string(state.PhaseHalted), false, "halted"},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &recordingResumeDispatcher{createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)}}
			srv, s, project := setupCreateAgentServer(t, disp)
			ctx := context.Background()

			slug := fmt.Sprintf("resumable-refuse-%d", i)
			initialPhase := string(tc.phase)
			if tc.resumePhase != "" {
				initialPhase = tc.resumePhase
			}
			existing := &store.Agent{
				ID:              tid(slug),
				Slug:            slug,
				Name:            slug,
				ProjectID:       project.ID,
				RuntimeBrokerID: tid("broker-create"),
				Phase:           initialPhase,
			}
			require.NoError(t, s.CreateAgent(ctx, existing))
			if tc.resumePhase != "" {
				existing = seedResumeMemory(t, s, existing, tc.resumePhase)
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
				Name:          slug,
				Task:          "fresh task",
				ProvisionOnly: tc.provisionOnly,
			})

			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			var errResp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
			assert.Equal(t, ErrCodeAgentExistsResumable, errResp.Error.Code)
			assert.Contains(t, errResp.Error.Message, tc.wantPhaseInMsg)
			assert.Contains(t, errResp.Error.Message, "scion resume "+slug)
			assert.False(t, disp.startCalled, "must not dispatch start")
			assert.False(t, disp.createCalled, "must not dispatch create")
			assert.False(t, disp.deleteCalled, "must not dispatch delete")
		})
	}

	t.Run("active idle-suspend claim", func(t *testing.T) {
		disp := &recordingResumeDispatcher{createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)}}
		srv, s := testServer(t)
		srv.SetDispatcher(disp)
		proj, _, agent := isoSetupIdleAgent(t, s, "resumable-idle-claim", string(state.ActivityIdle))
		_, _, _ = isoClaimIdleDirectly(t, s, agent, 5*time.Minute)

		check, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		require.True(t, check.IdleSuspendClaimActive, "fixture must have an active idle-suspend claim")

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+proj.ID+"/agents", CreateAgentRequest{
			Name: agent.Slug,
			Task: "fresh task",
		})
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeAgentExistsResumable, errResp.Error.Code)
		assert.Contains(t, errResp.Error.Message, "suspending")
		assert.Contains(t, errResp.Error.Message, "scion resume "+agent.Slug)
		assert.False(t, disp.startCalled, "must not dispatch start")
		assert.False(t, disp.createCalled, "must not dispatch create")
		assert.False(t, disp.deleteCalled, "must not dispatch delete")
	})
}

// TestHandleExistingAgent_ExplicitResumeAllowed verifies that POST with
// resume=true still resumes suspended agents (with harness resume=true) and
// restarts stopped/halted agents in place (with harness resume=false),
// including error agents with a resumable LaunchResumePhase.
func TestHandleExistingAgent_ExplicitResumeAllowed(t *testing.T) {
	cases := []struct {
		name              string
		phase             state.Phase
		resumePhase       string
		wantHarnessResume bool
	}{
		{"suspended", state.PhaseSuspended, "", true},
		{"stopped", state.PhaseStopped, "", false},
		{"halted", state.PhaseHalted, "", false},
		{"error remembering suspended", state.PhaseError, string(state.PhaseSuspended), true},
		{"error remembering halted", state.PhaseError, string(state.PhaseHalted), false},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &recordingResumeDispatcher{createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)}}
			srv, s, project := setupCreateAgentServer(t, disp)
			ctx := context.Background()

			slug := fmt.Sprintf("resumable-ok-%d", i)
			initialPhase := string(tc.phase)
			if tc.resumePhase != "" {
				initialPhase = tc.resumePhase
			}
			existing := &store.Agent{
				ID:              tid(slug),
				Slug:            slug,
				Name:            slug,
				ProjectID:       project.ID,
				RuntimeBrokerID: tid("broker-create"),
				Phase:           initialPhase,
			}
			require.NoError(t, s.CreateAgent(ctx, existing))
			if tc.resumePhase != "" {
				existing = seedResumeMemory(t, s, existing, tc.resumePhase)
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
				Name:   slug,
				Task:   "follow-up task",
				Resume: true,
			})

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.True(t, disp.startCalled, "explicit resume must dispatch start")
			assert.Equal(t, tc.wantHarnessResume, disp.lastResume)
		})
	}
}

// TestHandleExistingAgent_CreatedAndInFlightUnchanged verifies that a plain
// start without resume still starts a created agent and returns an in-flight
// launch without starting a second one, while running and plain error agents
// keep returning 409 conflict (not agent_exists_resumable).
func TestHandleExistingAgent_CreatedAndInFlightUnchanged(t *testing.T) {
	t.Run("created agent starts without resume", func(t *testing.T) {
		disp := &recordingResumeDispatcher{createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)}}
		srv, s, project := setupCreateAgentServer(t, disp)
		ctx := context.Background()

		existing := &store.Agent{
			ID:              tid("created-ok"),
			Slug:            "created-ok",
			Name:            "created-ok",
			ProjectID:       project.ID,
			RuntimeBrokerID: tid("broker-create"),
			Phase:           string(state.PhaseCreated),
		}
		require.NoError(t, s.CreateAgent(ctx, existing))

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
			Name: "created-ok",
			Task: "initial task",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.True(t, disp.startCalled)
		assert.False(t, disp.lastResume)
	})

	t.Run("in-flight launch returns 200 without dispatching", func(t *testing.T) {
		disp := &recordingResumeDispatcher{createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)}}
		srv, s, project := setupCreateAgentServer(t, disp)
		ctx := context.Background()

		existing := &store.Agent{
			ID:              tid("inflight-ok"),
			Slug:            "inflight-ok",
			Name:            "inflight-ok",
			ProjectID:       project.ID,
			RuntimeBrokerID: tid("broker-create"),
			Phase:           string(state.PhaseStarting),
		}
		require.NoError(t, s.CreateAgent(ctx, existing))
		beginTestStartLaunch(t, s, existing, store.LaunchKindStart, string(state.PhaseCreated), false)

		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
			Name: "inflight-ok",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.False(t, disp.startCalled, "in-flight launch must not dispatch again")
		assert.False(t, disp.createCalled, "in-flight launch must not dispatch create")
	})

	t.Run("running and plain error return plain conflict", func(t *testing.T) {
		for _, phase := range []state.Phase{state.PhaseRunning, state.PhaseError} {
			disp := &recordingResumeDispatcher{createAgentDispatcher: createAgentDispatcher{createPhase: string(state.PhaseRunning)}}
			srv, s, project := setupCreateAgentServer(t, disp)
			ctx := context.Background()

			slug := "plain-conflict-" + string(phase)
			existing := &store.Agent{
				ID:              tid(slug),
				Slug:            slug,
				Name:            slug,
				ProjectID:       project.ID,
				RuntimeBrokerID: tid("broker-create"),
				Phase:           string(phase),
			}
			require.NoError(t, s.CreateAgent(ctx, existing))

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
				Name: slug,
			})
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			var errResp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
			assert.Equal(t, ErrCodeConflict, errResp.Error.Code)
			assert.False(t, disp.startCalled)
		}
	})
}
