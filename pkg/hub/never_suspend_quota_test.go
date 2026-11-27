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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func setNeverSuspendProjectCap(t *testing.T, s store.Store, capVal int64) *store.LimitDefinition {
	t.Helper()
	ctx := context.Background()
	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxNeverSuspendAgentsPerProject)
	require.NoError(t, err, "max_never_suspend_agents_per_project must be seeded")
	def.DefaultValue = capVal
	updated, err := s.UpdateLimitDefinition(ctx, def)
	require.NoError(t, err)
	return updated
}

type nsQuotaFixture struct {
	srv       *Server
	s         store.Store
	owner     *store.User
	projectID string
	brokerID  string
	disp      *createAgentDispatcher
}

func newNSQuotaFixture(t *testing.T, prefix string) *nsQuotaFixture {
	t.Helper()
	srv, s := testServer(t)
	return initNSQuotaFixture(t, srv, s, prefix)
}

func initNSQuotaFixture(t *testing.T, srv *Server, s store.Store, prefix string) *nsQuotaFixture {
	t.Helper()
	ctx := context.Background()
	ownerID := tid(prefix + "-owner")
	createTestUserWithRole(t, s, ownerID, prefix+"-owner@test.com", store.UserRoleMember, store.SystemRoleHubMember)
	owner, err := s.GetUser(ctx, ownerID)
	require.NoError(t, err)

	projectID := tid(prefix + "-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: prefix + " Project", Slug: prefix + "-project",
		GitRemote: "https://github.com/example/" + prefix + ".git",
		OwnerID:   ownerID, CreatedBy: ownerID,
	}))
	createTestUserWithProjectRole(t, s, ownerID, owner.Email, projectID, store.ProjectRoleOwner)

	brokerID := tid(prefix + "-broker")
	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: brokerID, Name: prefix + " Broker", Slug: prefix + "-broker",
		Status: store.BrokerStatusOnline, CreatedBy: ownerID,
	}))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: projectID, BrokerID: brokerID, BrokerName: prefix + " Broker",
		Status: store.BrokerStatusOnline,
	}))
	disp := &createAgentDispatcher{}
	srv.SetDispatcher(disp)
	srv.config.SoftDeleteRetention = time.Hour
	setNeverSuspendExperiment(t, srv, true)

	return &nsQuotaFixture{
		srv:       srv,
		s:         s,
		owner:     owner,
		projectID: projectID,
		brokerID:  brokerID,
		disp:      disp,
	}
}

func TestNeverSuspend_QuotaEnforcementLifecycleAndAdminUsage(t *testing.T) {
	f := newNSQuotaFixture(t, "ns-q-life")
	ctx := context.Background()

	// Seeded unlimited by default.
	def, err := f.s.GetLimitDefinitionByName(ctx, store.LimitMaxNeverSuspendAgentsPerProject)
	require.NoError(t, err)
	assert.Equal(t, int64(0), def.DefaultValue, "must be seeded unlimited")
	assert.True(t, def.System)

	// Seed N=2 flagged rows BEFORE any quota cap exists (simulating Phase-1
	// flagged rows with zero usage_reservations).
	pre1 := &store.Agent{
		ID: tid("ns-q-pre1"), Slug: "ns-q-pre1", Name: "ns-q-pre1",
		ProjectID: f.projectID, OwnerID: f.owner.ID, CreatedBy: f.owner.ID,
		RuntimeBrokerID: f.brokerID, Phase: string(state.PhaseRunning),
		SuspendPolicy: store.SuspendPolicyNever,
	}
	pre2 := &store.Agent{
		ID: tid("ns-q-pre2"), Slug: "ns-q-pre2", Name: "ns-q-pre2",
		ProjectID: f.projectID, OwnerID: f.owner.ID, CreatedBy: f.owner.ID,
		RuntimeBrokerID: f.brokerID, Phase: string(state.PhaseStopped),
		SuspendPolicy: store.SuspendPolicyNever,
	}
	require.NoError(t, f.s.CreateAgent(ctx, pre1))
	require.NoError(t, f.s.CreateAgent(ctx, pre2))

	// Also seed an auto agent and a soft-deleted flagged agent.
	autoAgent := &store.Agent{
		ID: tid("ns-q-auto"), Slug: "ns-q-auto", Name: "ns-q-auto",
		ProjectID: f.projectID, OwnerID: f.owner.ID, CreatedBy: f.owner.ID,
		RuntimeBrokerID: f.brokerID, Phase: string(state.PhaseRunning),
		SuspendPolicy: store.SuspendPolicyAuto,
	}
	require.NoError(t, f.s.CreateAgent(ctx, autoAgent))

	// Create a flagged agent via API while still unlimited, then soft-delete it.
	recSoft := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]any{
		"name":          "ns-q-soft",
		"projectId":     f.projectID,
		"provisionOnly": true,
		"suspendPolicy": store.SuspendPolicyNever,
	})
	require.Equal(t, http.StatusCreated, recSoft.Code, "body: %s", recSoft.Body.String())
	var createdSoft CreateAgentResponse
	require.NoError(t, json.Unmarshal(recSoft.Body.Bytes(), &createdSoft))
	softAgentID := createdSoft.Agent.ID

	delRec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, "/api/v1/agents/"+softAgentID, nil)
	require.Equal(t, http.StatusNoContent, delRec.Code, "body: %s", delRec.Body.String())

	// Enforce cap = 2 (currently 2 live flagged rows: pre1 running, pre2 stopped).
	setNeverSuspendProjectCap(t, f.s, 2)

	t.Run("create over cap returns 429 quota_exceeded with details and leaks no row or reservations", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-q-over-create",
			"projectId":     f.projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusTooManyRequests, rec.Code, "body: %s", rec.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeQuotaExceeded, errResp.Error.Code)
		assert.Equal(t, f.projectID, errResp.Error.Details["projectId"])
		assert.Equal(t, float64(2), errResp.Error.Details["count"])
		assert.Equal(t, float64(2), errResp.Error.Details["limit"])

		_, err := f.s.GetAgentBySlug(ctx, f.projectID, "ns-q-over-create")
		assert.ErrorIs(t, err, store.ErrNotFound)

		// Verify zero reservations exist for the row-only never-suspend limit.
		resList, err := f.s.ListActiveReservationsByScopeType(ctx, def.ID, store.QuotaScopeProject)
		require.NoError(t, err)
		assert.Empty(t, resList, "never-suspend quota must not create usage_reservations")
	})

	t.Run("PATCH auto->never over cap returns 429 and rolls back combined field edits", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+autoAgent.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
			"labels":        map[string]string{"should-rollback": "yes"},
		})
		require.Equal(t, http.StatusTooManyRequests, rec.Code, "body: %s", rec.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeQuotaExceeded, errResp.Error.Code)
		assert.Equal(t, f.projectID, errResp.Error.Details["projectId"])
		assert.Equal(t, float64(2), errResp.Error.Details["count"])
		assert.Equal(t, float64(2), errResp.Error.Details["limit"])

		got, err := f.s.GetAgent(ctx, autoAgent.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyAuto, got.SuspendPolicy)
		assert.Empty(t, got.Labels["should-rollback"], "combined edit must roll back on quota refusal")
	})

	t.Run("restore flagged agent over cap returns 409 quota_exceeded", func(t *testing.T) {
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents/"+softAgentID+"/restore", nil)
		require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeQuotaExceeded, errResp.Error.Code)
		assert.Equal(t, f.projectID, errResp.Error.Details["projectId"])
		assert.Equal(t, float64(2), errResp.Error.Details["count"])
		assert.Equal(t, float64(2), errResp.Error.Details["limit"])

		got, err := f.s.GetAgent(ctx, softAgentID)
		require.NoError(t, err)
		assert.False(t, got.DeletedAt.IsZero(), "refused restore must keep agent soft-deleted")
	})

	t.Run("no-op never PATCH and labels+never PATCH on existing flagged agent succeed at cap", func(t *testing.T) {
		recNoop := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+pre1.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusOK, recNoop.Code, "body: %s", recNoop.Body.String())

		recLabels := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+pre1.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
			"labels":        map[string]string{"env": "prod"},
		})
		require.Equal(t, http.StatusOK, recLabels.Code, "body: %s", recLabels.Body.String())

		got, err := f.s.GetAgent(ctx, pre1.ID)
		require.NoError(t, err)
		assert.Equal(t, store.SuspendPolicyNever, got.SuspendPolicy)
		assert.Equal(t, "prod", got.Labels["env"])
	})

	t.Run("admin usage summary and usage-by-limit agree with row count", func(t *testing.T) {
		sumRec := doRequest(t, f.srv, http.MethodGet, "/api/v1/admin/usage", nil)
		require.Equal(t, http.StatusOK, sumRec.Code, "body: %s", sumRec.Body.String())
		var sumResp usageSummaryResponse
		require.NoError(t, json.Unmarshal(sumRec.Body.Bytes(), &sumResp))
		var found *usageSummaryEntry
		for i := range sumResp.Items {
			if sumResp.Items[i].LimitDefinition.Name == store.LimitMaxNeverSuspendAgentsPerProject {
				found = &sumResp.Items[i]
				break
			}
		}
		require.NotNil(t, found, "usage summary must include max_never_suspend_agents_per_project")
		assert.Equal(t, 2, found.ActiveCount)

		byLimitRec := doRequest(t, f.srv, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
		require.Equal(t, http.StatusOK, byLimitRec.Code, "body: %s", byLimitRec.Body.String())
		var byLimitResp usageByLimitResponse
		require.NoError(t, json.Unmarshal(byLimitRec.Body.Bytes(), &byLimitResp))
		assert.Equal(t, 2, byLimitResp.TotalActive)
		require.Len(t, byLimitResp.Reservations, 2)
		gotIDs := []string{byLimitResp.Reservations[0].ResourceID, byLimitResp.Reservations[1].ResourceID}
		assert.ElementsMatch(t, []string{pre1.ID, pre2.ID}, gotIDs)
	})

	t.Run("clearing flag, soft delete, and hard delete each free a slot immediately", func(t *testing.T) {
		// 1. Clear pre2 -> auto frees 1 slot -> restore softAgentID succeeds (2/2).
		recClear := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+pre2.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyAuto,
		})
		require.Equal(t, http.StatusOK, recClear.Code, "body: %s", recClear.Body.String())

		recRestore := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents/"+softAgentID+"/restore", nil)
		require.Equal(t, http.StatusOK, recRestore.Code, "body: %s", recRestore.Body.String())

		// 2. Soft-delete softAgentID frees 1 slot -> PATCH autoAgent->never succeeds (2/2).
		recDel := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, "/api/v1/agents/"+softAgentID, nil)
		require.Equal(t, http.StatusNoContent, recDel.Code, "body: %s", recDel.Body.String())

		recPatch := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+autoAgent.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusOK, recPatch.Code, "body: %s", recPatch.Body.String())

		// 3. Hard-delete pre1 frees 1 slot -> create with never succeeds (2/2).
		f.srv.config.SoftDeleteRetention = 0
		recHardDel := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, "/api/v1/agents/"+pre1.ID, nil)
		require.Equal(t, http.StatusNoContent, recHardDel.Code, "body: %s", recHardDel.Body.String())
		_, err := f.s.GetAgent(ctx, pre1.ID)
		assert.ErrorIs(t, err, store.ErrNotFound, "pre1 must be hard-deleted")

		recCreate := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-q-after-hard-del",
			"projectId":     f.projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, recCreate.Code, "body: %s", recCreate.Body.String())

		// Cap is now full again (2/2): another create must fail with 429.
		recOver := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-q-full-again",
			"projectId":     f.projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		assert.Equal(t, http.StatusTooManyRequests, recOver.Code)
	})
}

func TestNeverSuspend_QuotaConcurrency(t *testing.T) {
	ctx := context.Background()

	t.Run("parallel creates at cap N-1 admit exactly one", func(t *testing.T) {
		f := newNSQuotaFixture(t, "ns-qc-cr")
		setNeverSuspendProjectCap(t, f.s, 2)

		// 1 existing flagged row (cap N-1 = 1 slot left).
		require.NoError(t, f.s.CreateAgent(ctx, &store.Agent{
			ID: tid("ns-qc-cr-seed"), Slug: "ns-qc-cr-seed", Name: "ns-qc-cr-seed",
			ProjectID: f.projectID, OwnerID: f.owner.ID, CreatedBy: f.owner.ID,
			RuntimeBrokerID: f.brokerID, Phase: string(state.PhaseRunning),
			SuspendPolicy: store.SuspendPolicyNever,
		}))

		const K = 6
		start := make(chan struct{})
		var wg sync.WaitGroup
		var created, rejected atomic.Int32
		for i := 0; i < K; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]any{
					"name":          fmt.Sprintf("ns-qc-cr-%d", idx),
					"projectId":     f.projectID,
					"provisionOnly": true,
					"suspendPolicy": store.SuspendPolicyNever,
				})
				switch rec.Code {
				case http.StatusCreated:
					created.Add(1)
				case http.StatusTooManyRequests:
					rejected.Add(1)
				default:
					t.Errorf("unexpected status %d: %s", rec.Code, rec.Body.String())
				}
			}(i)
		}
		close(start)
		wg.Wait()

		assert.Equal(t, int32(1), created.Load(), "exactly one parallel create must be admitted")
		assert.Equal(t, int32(K-1), rejected.Load(), "remaining parallel creates must get 429")
		cnt, err := f.s.CountNeverSuspendAgentsInProject(ctx, f.projectID)
		require.NoError(t, err)
		assert.Equal(t, int64(2), cnt)
	})

	t.Run("parallel PATCH auto->never at cap N-1 admit exactly one", func(t *testing.T) {
		f := newNSQuotaFixture(t, "ns-qc-pa")
		setNeverSuspendProjectCap(t, f.s, 2)

		require.NoError(t, f.s.CreateAgent(ctx, &store.Agent{
			ID: tid("ns-qc-pa-seed"), Slug: "ns-qc-pa-seed", Name: "ns-qc-pa-seed",
			ProjectID: f.projectID, OwnerID: f.owner.ID, CreatedBy: f.owner.ID,
			RuntimeBrokerID: f.brokerID, Phase: string(state.PhaseRunning),
			SuspendPolicy: store.SuspendPolicyNever,
		}))

		const K = 6
		agentIDs := make([]string, K)
		for i := 0; i < K; i++ {
			a := &store.Agent{
				ID: tid(fmt.Sprintf("ns-qc-pa-%d", i)), Slug: fmt.Sprintf("ns-qc-pa-%d", i),
				Name: fmt.Sprintf("ns-qc-pa-%d", i), ProjectID: f.projectID,
				OwnerID: f.owner.ID, CreatedBy: f.owner.ID,
				RuntimeBrokerID: f.brokerID, Phase: string(state.PhaseRunning),
				SuspendPolicy: store.SuspendPolicyAuto,
			}
			require.NoError(t, f.s.CreateAgent(ctx, a))
			agentIDs[i] = a.ID
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		var admitted, rejected atomic.Int32
		for i := 0; i < K; i++ {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				<-start
				rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+id, map[string]any{
					"suspendPolicy": store.SuspendPolicyNever,
				})
				switch rec.Code {
				case http.StatusOK:
					admitted.Add(1)
				case http.StatusTooManyRequests:
					rejected.Add(1)
				default:
					t.Errorf("unexpected status %d: %s", rec.Code, rec.Body.String())
				}
			}(agentIDs[i])
		}
		close(start)
		wg.Wait()

		assert.Equal(t, int32(1), admitted.Load(), "exactly one parallel PATCH must be admitted")
		assert.Equal(t, int32(K-1), rejected.Load(), "remaining parallel PATCHes must get 429")
		cnt, err := f.s.CountNeverSuspendAgentsInProject(ctx, f.projectID)
		require.NoError(t, err)
		assert.Equal(t, int64(2), cnt)
	})

	t.Run("parallel restores at cap N-1 admit exactly one", func(t *testing.T) {
		f := newNSQuotaFixture(t, "ns-qc-rs")

		const K = 4
		softIDs := make([]string, K)
		for i := 0; i < K; i++ {
			rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]any{
				"name":          fmt.Sprintf("ns-qc-rs-%d", i),
				"projectId":     f.projectID,
				"provisionOnly": true,
				"suspendPolicy": store.SuspendPolicyNever,
			})
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
			var created CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
			softIDs[i] = created.Agent.ID

			delRec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, "/api/v1/agents/"+softIDs[i], nil)
			require.Equal(t, http.StatusNoContent, delRec.Code, "body: %s", delRec.Body.String())
		}

		// Seed 1 live flagged row and set cap=2 (1 slot left).
		require.NoError(t, f.s.CreateAgent(ctx, &store.Agent{
			ID: tid("ns-qc-rs-seed"), Slug: "ns-qc-rs-seed", Name: "ns-qc-rs-seed",
			ProjectID: f.projectID, OwnerID: f.owner.ID, CreatedBy: f.owner.ID,
			RuntimeBrokerID: f.brokerID, Phase: string(state.PhaseRunning),
			SuspendPolicy: store.SuspendPolicyNever,
		}))
		setNeverSuspendProjectCap(t, f.s, 2)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var admitted, rejected atomic.Int32
		for i := 0; i < K; i++ {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				<-start
				rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents/"+id+"/restore", nil)
				switch rec.Code {
				case http.StatusOK:
					admitted.Add(1)
				case http.StatusConflict:
					rejected.Add(1)
				default:
					t.Errorf("unexpected status %d: %s", rec.Code, rec.Body.String())
				}
			}(softIDs[i])
		}
		close(start)
		wg.Wait()

		assert.Equal(t, int32(1), admitted.Load(), "exactly one parallel restore must be admitted")
		assert.Equal(t, int32(K-1), rejected.Load(), "remaining parallel restores must get 409")
		cnt, err := f.s.CountNeverSuspendAgentsInProject(ctx, f.projectID)
		require.NoError(t, err)
		assert.Equal(t, int64(2), cnt)
	})
}

func TestNeverSuspend_QuotaEnvGatherRecreate(t *testing.T) {
	f := newNSQuotaFixture(t, "ns-q-eg")
	ctx := context.Background()

	// Cap = 1, filled by one live flagged agent.
	require.NoError(t, f.s.CreateAgent(ctx, &store.Agent{
		ID: tid("ns-q-eg-filler"), Slug: "ns-q-eg-filler", Name: "ns-q-eg-filler",
		ProjectID: f.projectID, OwnerID: f.owner.ID, CreatedBy: f.owner.ID,
		RuntimeBrokerID: f.brokerID, Phase: string(state.PhaseRunning),
		SuspendPolicy: store.SuspendPolicyNever,
	}))
	setNeverSuspendProjectCap(t, f.s, 1)

	t.Run("auto->never env-gather recreate at cap is refused 429 and old provisioning row survives", func(t *testing.T) {
		prov := &store.Agent{
			ID: tid("ns-q-eg-prov-auto"), Slug: "ns-q-eg-prov-auto", Name: "ns-q-eg-prov-auto",
			ProjectID: f.projectID, OwnerID: f.owner.ID, CreatedBy: f.owner.ID,
			RuntimeBrokerID: f.brokerID, Phase: string(state.PhaseProvisioning),
			SuspendPolicy: store.SuspendPolicyAuto,
		}
		require.NoError(t, f.s.CreateAgent(ctx, prov))

		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-q-eg-prov-auto",
			"projectId":     f.projectID,
			"gatherEnv":     true,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusTooManyRequests, rec.Code, "body: %s", rec.Body.String())
		var errResp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeQuotaExceeded, errResp.Error.Code)

		// The existing provisioning row MUST survive untouched!
		survived, err := f.s.GetAgent(ctx, prov.ID)
		require.NoError(t, err, "existing provisioning row must not be destroyed when auto->never recreate is over cap")
		assert.True(t, survived.DeletedAt.IsZero())
		assert.Equal(t, store.SuspendPolicyAuto, survived.SuspendPolicy)
		assert.Equal(t, string(state.PhaseProvisioning), survived.Phase)
	})

	t.Run("never->never env-gather recreate at cap succeeds because old row frees its slot", func(t *testing.T) {
		// Replace the filler with a provisioning never row at cap 1.
		require.NoError(t, f.s.DeleteAgent(ctx, tid("ns-q-eg-filler")))

		provNever := &store.Agent{
			ID: tid("ns-q-eg-prov-never"), Slug: "ns-q-eg-prov-never", Name: "ns-q-eg-prov-never",
			ProjectID: f.projectID, OwnerID: f.owner.ID, CreatedBy: f.owner.ID,
			RuntimeBrokerID: f.brokerID, Phase: string(state.PhaseProvisioning),
			SuspendPolicy: store.SuspendPolicyNever,
		}
		require.NoError(t, f.s.CreateAgent(ctx, provNever))

		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-q-eg-prov-never",
			"projectId":     f.projectID,
			"gatherEnv":     true,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())

		cnt, err := f.s.CountNeverSuspendAgentsInProject(ctx, f.projectID)
		require.NoError(t, err)
		assert.Equal(t, int64(1), cnt)
	})
}

type nsCountSpyStore struct {
	store.Store
	fault *storeFaultSwitch
	calls *atomic.Int64
}

func (w *nsCountSpyStore) CountNeverSuspendAgentsInProject(ctx context.Context, projectID string) (int64, error) {
	if w.fault.Active() {
		w.calls.Add(1)
	}
	return w.Store.CountNeverSuspendAgentsInProject(ctx, projectID)
}

func (w *nsCountSpyStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return w.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&nsCountSpyStore{Store: tx, fault: w.fault, calls: w.calls})
	})
}

func TestNeverSuspend_QuotaExperimentOffAndUnlimitedShortCircuit(t *testing.T) {
	calls := &atomic.Int64{}
	srv, rawStore, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *nsCountSpyStore {
		return &nsCountSpyStore{Store: inner, fault: f, calls: calls}
	})
	f := initNSQuotaFixture(t, srv, rawStore, "ns-q-sc")
	ctx := context.Background()

	t.Run("unlimited limit short-circuits before running any count query", func(t *testing.T) {
		setNeverSuspendExperiment(t, f.srv, true)
		setNeverSuspendProjectCap(t, f.s, 0) // unlimited
		fault.Arm()
		calls.Store(0)

		// Create with never.
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-q-sc-1",
			"projectId":     f.projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		var created CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

		// PATCH auto -> never.
		autoA := &store.Agent{
			ID: tid("ns-q-sc-auto"), Slug: "ns-q-sc-auto", Name: "ns-q-sc-auto",
			ProjectID: f.projectID, OwnerID: f.owner.ID, CreatedBy: f.owner.ID,
			RuntimeBrokerID: f.brokerID, Phase: string(state.PhaseRunning),
			SuspendPolicy: store.SuspendPolicyAuto,
		}
		require.NoError(t, f.s.CreateAgent(ctx, autoA))
		recPatch := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch, "/api/v1/agents/"+autoA.ID, map[string]any{
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusOK, recPatch.Code, "body: %s", recPatch.Body.String())

		// Soft-delete and restore.
		recDel := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, "/api/v1/agents/"+created.Agent.ID, nil)
		require.Equal(t, http.StatusNoContent, recDel.Code, "body: %s", recDel.Body.String())
		recRes := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents/"+created.Agent.ID+"/restore", nil)
		require.Equal(t, http.StatusOK, recRes.Code, "body: %s", recRes.Body.String())

		assert.Equal(t, int64(0), calls.Load(), "unlimited quota must not execute any count query")

		// Sanity check: once a finite cap is set, the count query IS called.
		setNeverSuspendProjectCap(t, f.s, 10)
		recCreate2 := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-q-sc-2",
			"projectId":     f.projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, recCreate2.Code, "body: %s", recCreate2.Body.String())
		assert.Equal(t, int64(1), calls.Load(), "finite cap must execute the count query")
	})

	t.Run("experiment off disables enforcement on restore even when over cap", func(t *testing.T) {
		setNeverSuspendExperiment(t, f.srv, true)
		setNeverSuspendProjectCap(t, f.s, 0)

		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]any{
			"name":          "ns-q-expoff",
			"projectId":     f.projectID,
			"provisionOnly": true,
			"suspendPolicy": store.SuspendPolicyNever,
		})
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		var created CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

		recDel := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, "/api/v1/agents/"+created.Agent.ID, nil)
		require.Equal(t, http.StatusNoContent, recDel.Code, "body: %s", recDel.Body.String())

		// Cap is 1 (already exceeded by live flagged agents above), but experiment is OFF.
		setNeverSuspendProjectCap(t, f.s, 1)
		setNeverSuspendExperiment(t, f.srv, false)

		fault.Arm()
		calls.Store(0)

		recRes := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents/"+created.Agent.ID+"/restore", nil)
		require.Equal(t, http.StatusOK, recRes.Code, "restore must not enforce never-suspend quota when experiment is off: %s", recRes.Body.String())
		assert.Equal(t, int64(0), calls.Load(), "experiment off must not execute count query")
	})
}

type fakeNeverSuspendLockerStore struct {
	store.Store
	mu             sync.Mutex
	failAcquire    bool
	held           bool
	acquireCount   int
	releaseCount   int
	lastClassID    store.AdvisoryLockKey
	lastObjID      int32
	heldAtTxCommit bool
}

func (f *fakeNeverSuspendLockerStore) TryAdvisoryLock(_ context.Context, _ store.AdvisoryLockKey) (bool, func() error, error) {
	return true, func() error { return nil }, nil
}

func (f *fakeNeverSuspendLockerStore) TryAdvisoryLockObject(_ context.Context, classID store.AdvisoryLockKey, objID int32) (bool, func() error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acquireCount++
	f.lastClassID = classID
	f.lastObjID = objID
	if f.failAcquire {
		return false, func() error { return nil }, nil
	}
	f.held = true
	released := false
	return true, func() error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !released {
			released = true
			f.held = false
			f.releaseCount++
		}
		return nil
	}, nil
}

func (f *fakeNeverSuspendLockerStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	err := f.Store.WithTx(ctx, fn)
	if err == nil {
		// Record whether the advisory lock is STILL held immediately after the
		// underlying transaction commits.
		f.mu.Lock()
		f.heldAtTxCommit = f.held
		f.mu.Unlock()
	}
	return err
}

func (f *fakeNeverSuspendLockerStore) isHeld() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held
}

func TestNeverSuspend_QuotaLockContract(t *testing.T) {
	ctx := context.Background()
	f := newNSQuotaFixture(t, "ns-qlock")
	setNeverSuspendProjectCap(t, f.s, 2)

	fake := &fakeNeverSuspendLockerStore{Store: f.s}
	srv := &Server{
		store: fake,
		quotaService: &QuotaService{
			store:  fake,
			logger: slog.Default(),
		},
	}
	setNeverSuspendExperiment(t, srv, true)

	t.Run("holds LockNeverSuspendQuota across fn and tx commit and releases after return", func(t *testing.T) {
		var heldInFn, heldInTx bool
		err := srv.withNeverSuspendQuotaLock(ctx, f.owner.ID, f.projectID, func(effectiveLimit int64) error {
			require.Equal(t, int64(2), effectiveLimit)
			heldInFn = fake.isHeld()
			return srv.store.WithTx(ctx, func(tx store.Store) error {
				heldInTx = fake.isHeld()
				_, cErr := tx.CountNeverSuspendAgentsInProject(ctx, f.projectID)
				return cErr
			})
		})
		require.NoError(t, err)
		assert.True(t, heldInFn, "advisory lock must be held while fn runs")
		assert.True(t, heldInTx, "advisory lock must be held inside tx")
		assert.True(t, fake.heldAtTxCommit, "advisory lock must remain held through tx commit")
		assert.False(t, fake.isHeld(), "advisory lock must be released after fn returns")
		assert.Equal(t, 1, fake.acquireCount)
		assert.Equal(t, 1, fake.releaseCount)
		assert.Equal(t, store.LockNeverSuspendQuota, fake.lastClassID,
			"must use dedicated LockNeverSuspendQuota class ID, not LockQuotaEnforcement")
		assert.NotEqual(t, store.LockQuotaEnforcement, fake.lastClassID)
		assert.Equal(t, store.StableProjectHash(f.projectID), fake.lastObjID)
	})

	t.Run("failed try-lock returns ErrQuotaLockContention and releases in-process mutex", func(t *testing.T) {
		fake.failAcquire = true
		fnCalled := false
		err := srv.withNeverSuspendQuotaLock(ctx, f.owner.ID, f.projectID, func(int64) error {
			fnCalled = true
			return nil
		})
		require.ErrorIs(t, err, ErrQuotaLockContention)
		assert.False(t, fnCalled, "fn must not run when advisory lock is not acquired")
		assert.False(t, fake.isHeld())

		// If the in-process scope mutex had leaked, the next call on the same
		// project would deadlock. Verify it acquires cleanly.
		fake.failAcquire = false
		done := make(chan error, 1)
		go func() {
			done <- srv.withNeverSuspendQuotaLock(ctx, f.owner.ID, f.projectID, func(int64) error {
				return nil
			})
		}()
		select {
		case nextErr := <-done:
			require.NoError(t, nextErr)
		case <-time.After(5 * time.Second):
			t.Fatal("in-process scope mutex leaked after ErrQuotaLockContention")
		}
	})

	t.Run("fn error releases both advisory lock and in-process mutex", func(t *testing.T) {
		boom := errors.New("injected tx error")
		err := srv.withNeverSuspendQuotaLock(ctx, f.owner.ID, f.projectID, func(int64) error {
			assert.True(t, fake.isHeld())
			return boom
		})
		require.ErrorIs(t, err, boom)
		assert.False(t, fake.isHeld())

		// Confirm in-process mutex was released.
		err = srv.withNeverSuspendQuotaLock(ctx, f.owner.ID, f.projectID, func(int64) error {
			return nil
		})
		require.NoError(t, err)
	})
}
