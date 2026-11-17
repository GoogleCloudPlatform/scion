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
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const passthroughProviderGrantDenialSubstr = "requires broker ownership, Hub admin, or the broker being provided to this project by its owner"

type capturedSlogEntry struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

type passthroughSlogCapture struct {
	mu      sync.Mutex
	base    []slog.Attr
	entries []capturedSlogEntry
}

func capturePassthroughSlog(t *testing.T) *passthroughSlogCapture {
	t.Helper()
	h := &passthroughSlogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

func (h *passthroughSlogCapture) Enabled(context.Context, slog.Level) bool { return true }

func (h *passthroughSlogCapture) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string, len(h.base)+r.NumAttrs())
	for _, a := range h.base {
		attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	h.entries = append(h.entries, capturedSlogEntry{level: r.Level, msg: r.Message, attrs: attrs})
	h.mu.Unlock()
	return nil
}

func (h *passthroughSlogCapture) WithAttrs(as []slog.Attr) slog.Handler {
	combined := make([]slog.Attr, 0, len(h.base)+len(as))
	combined = append(combined, h.base...)
	combined = append(combined, as...)
	return &passthroughSlogCapture{base: combined}
}

func (h *passthroughSlogCapture) WithGroup(string) slog.Handler { return h }

func (h *passthroughSlogCapture) reset() {
	h.mu.Lock()
	h.entries = nil
	h.mu.Unlock()
}

func (h *passthroughSlogCapture) findLast(msg, surface string) (capturedSlogEntry, bool) {
	return h.findLastForCaller(msg, surface, "")
}

func (h *passthroughSlogCapture) findLastForCaller(msg, surface, caller string) (capturedSlogEntry, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.entries) - 1; i >= 0; i-- {
		e := h.entries[i]
		if e.msg == msg && (surface == "" || e.attrs["surface"] == surface) && (caller == "" || e.attrs["caller"] == caller) {
			return e, true
		}
	}
	return capturedSlogEntry{}, false
}

// q5World sets up brokerOwner, projectMember (non-owner of the broker, owner of
// projectA so they hold agent create/update/settings rights on projectA),
// adminUser (super-admin), projectA, and projectB (separate project for
// cross-project link isolation checks).
type q5World struct {
	srv           *Server
	s             store.Store
	disp          *createAgentDispatcher
	brokerOwner   *store.User
	projectMember *store.User
	adminUser     *store.User
	otherMember   *store.User
	projectA      *store.Project
	projectB      *store.Project
}

func newQ5World(t *testing.T, prefix string) *q5World {
	t.Helper()
	srv, s := testServer(t)
	setFlatExperiments(t, srv, true, true)
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv.SetDispatcher(disp)
	ctx := context.Background()

	mkUser := func(suffix, role string) *store.User {
		u := &store.User{
			ID:          tid(prefix + "-" + suffix),
			Email:       prefix + "-" + suffix + "@example.com",
			DisplayName: suffix,
			Role:        role,
			Status:      store.UserStatusActive,
			Created:     time.Now(),
		}
		require.NoError(t, s.CreateUser(ctx, u))
		ensureHubMembership(ctx, s, u.ID)
		return u
	}

	w := &q5World{
		srv:           srv,
		s:             s,
		disp:          disp,
		brokerOwner:   mkUser("broker-owner", store.UserRoleMember),
		projectMember: mkUser("proj-member", store.UserRoleMember),
		adminUser:     mkUser("hub-admin", store.UserRoleAdmin),
		otherMember:   mkUser("other-member", store.UserRoleMember),
	}
	grantSuperAdminRole(t, s, w.adminUser.ID)

	mkProj := func(suffix string, owner *store.User) *store.Project {
		p := &store.Project{
			ID:        tid(prefix + "-proj-" + suffix),
			Name:      prefix + "-proj-" + suffix,
			Slug:      tidSlugSafe(prefix + "-proj-" + suffix),
			OwnerID:   owner.ID,
			CreatedBy: owner.ID,
			Created:   time.Now(),
			Updated:   time.Now(),
		}
		require.NoError(t, s.CreateProject(ctx, p))
		srv.seedProjectCreatorMembership(ctx, p)
		require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, p.ID, owner.ID))
		return p
	}

	w.projectA = mkProj("a", w.projectMember)
	addProjectMember(t, srv, s, w.projectA, w.brokerOwner)
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, w.projectA.ID, w.brokerOwner.ID))
	addProjectMember(t, srv, s, w.projectA, w.adminUser)

	w.projectB = mkProj("b", w.otherMember)
	return w
}

// TestPassthroughGate_EvaluateMatrixAndAuthPathPrecedence exercises Check 1's
// owner / admin / provider_grant arms, owner > admin > provider_grant audit
// precedence, all four brokerProviderHasOwnerConsent forms, r14 flat dispatch
// tightening (AutoProvide off vs on), cross-project link isolation, and
// Hub-own embedded broker exclusions directly on evaluatePassthroughIdentity.
func TestPassthroughGate_EvaluateMatrixAndAuthPathPrecedence(t *testing.T) {
	w := newQ5World(t, "q5-eval")
	ctx := context.Background()
	logs := capturePassthroughSlog(t)

	const hostSA = "broker-host@q5-proj.iam.gserviceaccount.com"
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hostSA)
	enforceSAAssign(w.srv, checker)

	legacyBroker := &store.RuntimeBroker{
		ID:                         tid("q5-eval-legacy"),
		Name:                       "q5-eval-legacy",
		Slug:                       "q5-eval-legacy",
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  w.brokerOwner.ID,
		GCPHostServiceAccountEmail: hostSA,
		GCPHostProjectID:           "q5-proj",
		Profiles:                   []store.BrokerProfile{{Name: "default", Type: "docker", Available: true}},
	}
	require.NoError(t, w.s.CreateRuntimeBroker(ctx, legacyBroker))

	ctxFor := func(u *store.User) context.Context {
		return contextWithIdentity(ctx, NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, ""))
	}

	// 1. Unlinked broker: non-owner denied at Check 1 with updated message and zero actAs calls.
	d := w.srv.evaluatePassthroughIdentity(ctxFor(w.projectMember), nil, legacyBroker, w.projectA.ID, SurfacePassthroughCreate)
	require.NotNil(t, d)
	assert.Contains(t, d.Error(), passthroughProviderGrantDenialSubstr)
	assert.Equal(t, 0, checker.CallCount())

	// 2. Linked ONLY to projectB (cross-project isolation): non-owner in projectA is still denied.
	require.NoError(t, w.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: w.projectB.ID, BrokerID: legacyBroker.ID, BrokerName: legacyBroker.Name,
		Status: store.BrokerStatusOnline, LinkedBy: w.brokerOwner.ID,
	}))
	d = w.srv.evaluatePassthroughIdentity(ctxFor(w.projectMember), nil, legacyBroker, w.projectA.ID, SurfacePassthroughCreate)
	require.NotNil(t, d)
	assert.Contains(t, d.Error(), passthroughProviderGrantDenialSubstr)
	assert.Equal(t, 0, checker.CallCount())

	// 3. Linked to projectA with empty LinkedBy or unprivileged placeholder LinkedBy: denied.
	for _, badLinker := range []string{"", w.otherMember.ID} {
		_ = w.s.RemoveProjectProvider(ctx, w.projectA.ID, legacyBroker.ID)
		require.NoError(t, w.s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: w.projectA.ID, BrokerID: legacyBroker.ID, BrokerName: legacyBroker.Name,
			Status: store.BrokerStatusOnline, LinkedBy: badLinker,
		}))
		d = w.srv.evaluatePassthroughIdentity(ctxFor(w.projectMember), nil, legacyBroker, w.projectA.ID, SurfacePassthroughCreate)
		require.NotNil(t, d, "badLinker=%q must be denied", badLinker)
		assert.Contains(t, d.Error(), passthroughProviderGrantDenialSubstr)
		assert.Equal(t, 0, checker.CallCount())
	}

	// 4. All three LinkedBy consent forms on an owned legacy broker admit projectMember with authPath=provider_grant.
	for _, tc := range []struct {
		name     string
		linkedBy string
	}{
		{"owner-linked", w.brokerOwner.ID},
		{"auto-provide-linked", autoProvideLinkedBy},
		{"super-admin-linked", w.adminUser.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = w.s.RemoveProjectProvider(ctx, w.projectA.ID, legacyBroker.ID)
			require.NoError(t, w.s.AddProjectProvider(ctx, &store.ProjectProvider{
				ProjectID: w.projectA.ID, BrokerID: legacyBroker.ID, BrokerName: legacyBroker.Name,
				Status: store.BrokerStatusOnline, LinkedBy: tc.linkedBy,
			}))
			logs.reset()
			d := w.srv.evaluatePassthroughIdentity(ctxFor(w.projectMember), nil, legacyBroker, w.projectA.ID, SurfacePassthroughCreate)
			require.Nil(t, d)
			entry, ok := logs.findLast("passthrough allowed", SurfacePassthroughCreate)
			require.True(t, ok)
			assert.Equal(t, "provider_grant", entry.attrs["authPath"])
		})
	}

	// 5. Empty projectID never admits via provider_grant even when linked.
	d = w.srv.evaluatePassthroughIdentity(ctxFor(w.projectMember), nil, legacyBroker, "", SurfacePassthroughCreate)
	require.NotNil(t, d)

	// 6. Owner and admin audit paths, plus owner > admin > provider_grant precedence.
	logs.reset()
	require.Nil(t, w.srv.evaluatePassthroughIdentity(ctxFor(w.brokerOwner), nil, legacyBroker, w.projectA.ID, SurfacePassthroughCreate))
	entry, ok := logs.findLast("passthrough allowed", SurfacePassthroughCreate)
	require.True(t, ok)
	assert.Equal(t, "owner", entry.attrs["authPath"])

	logs.reset()
	require.Nil(t, w.srv.evaluatePassthroughIdentity(ctxFor(w.adminUser), nil, legacyBroker, w.projectA.ID, SurfacePassthroughCreate))
	entry, ok = logs.findLast("passthrough allowed", SurfacePassthroughCreate)
	require.True(t, ok)
	assert.Equal(t, "admin", entry.attrs["authPath"])

	// Admin who ALSO owns the broker logs authPath="owner" (owner > admin).
	adminOwnedBroker := &store.RuntimeBroker{
		ID:                         tid("q5-eval-admin-owned"),
		Name:                       "q5-eval-admin-owned",
		Slug:                       "q5-eval-admin-owned",
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  w.adminUser.ID,
		GCPHostServiceAccountEmail: hostSA,
		GCPHostProjectID:           "q5-proj",
	}
	require.NoError(t, w.s.CreateRuntimeBroker(ctx, adminOwnedBroker))
	logs.reset()
	require.Nil(t, w.srv.evaluatePassthroughIdentity(ctxFor(w.adminUser), nil, adminOwnedBroker, w.projectA.ID, SurfacePassthroughCreate))
	entry, ok = logs.findLast("passthrough allowed", SurfacePassthroughCreate)
	require.True(t, ok)
	assert.Equal(t, "owner", entry.attrs["authPath"], "precedence must be owner > admin > provider_grant")

	// 7. Provider-grant caller lacking actAs is denied at Check 3 AND logs authPath="provider_grant" on denial.
	checker.DenyTarget(hostSA, "missing actAs")
	logs.reset()
	d = w.srv.evaluatePassthroughIdentity(ctxFor(w.projectMember), nil, legacyBroker, w.projectA.ID, SurfacePassthroughCreate)
	require.NotNil(t, d)
	assert.Contains(t, d.Error(), store.PermissionActAs)
	deniedEntry, ok := logs.findLast("passthrough denied", SurfacePassthroughCreate)
	require.True(t, ok)
	assert.Equal(t, "provider_grant", deniedEntry.attrs["authPath"])
	checker.AllowTarget(hostSA)

	// 8. r14 flat broker rule: consented link with AutoProvide=false denies non-owner; AutoProvide=true allows.
	flatBroker := &store.RuntimeBroker{
		ID:                         tid("q5-eval-flat-docker"),
		Name:                       "q5-eval-flat-docker",
		Slug:                       "q5-eval-flat-docker",
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  w.brokerOwner.ID,
		AutoProvide:                false,
		GCPHostServiceAccountEmail: hostSA,
		GCPHostProjectID:           "q5-proj",
		RuntimeTarget:              &api.RuntimeTargetDescriptor{ID: tid("q5-rt-docker"), Type: config.RuntimeTargetTypeDocker, DisplayName: "Docker"},
	}
	require.NoError(t, w.s.CreateRuntimeBroker(ctx, flatBroker))
	require.NoError(t, w.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: w.projectA.ID, BrokerID: flatBroker.ID, BrokerName: flatBroker.Name,
		Status: store.BrokerStatusOnline, LinkedBy: w.brokerOwner.ID,
	}))

	d = w.srv.evaluatePassthroughIdentity(ctxFor(w.projectMember), nil, flatBroker, w.projectA.ID, SurfacePassthroughPatch)
	require.NotNil(t, d, "r14: flat row with AutoProvide=false must deny non-owner without broker.dispatch")
	assert.Contains(t, d.Error(), passthroughProviderGrantDenialSubstr)

	flatBroker.AutoProvide = true
	require.NoError(t, w.s.UpdateRuntimeBroker(ctx, flatBroker))
	logs.reset()
	d = w.srv.evaluatePassthroughIdentity(ctxFor(w.projectMember), nil, flatBroker, w.projectA.ID, SurfacePassthroughPatch)
	require.Nil(t, d, "r14: flat row with consented link + AutoProvide=true + actAs must allow")
	entry, ok = logs.findLast("passthrough allowed", SurfacePassthroughPatch)
	require.True(t, ok)
	assert.Equal(t, "provider_grant", entry.attrs["authPath"])

	// 9. Hub-own embedded brokers (legacy embedded broker AND co-located flat Docker) keep admin-only,
	// whereas co-located flat Cloud Run (operator-provisioned CreatedBy=="") allows provider_grant!
	legacyEmbedded := &store.RuntimeBroker{
		ID:                         tid("q5-eval-legacy-embedded"),
		Name:                       "embedded",
		Slug:                       "embedded",
		Status:                     store.BrokerStatusOnline,
		AutoProvide:                true,
		GCPHostServiceAccountEmail: hostSA,
		GCPHostProjectID:           "q5-proj",
	}
	require.NoError(t, w.s.CreateRuntimeBroker(ctx, legacyEmbedded))
	w.srv.SetEmbeddedBrokerID(legacyEmbedded.ID)
	require.NoError(t, w.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: w.projectA.ID, BrokerID: legacyEmbedded.ID, BrokerName: legacyEmbedded.Name,
		Status: store.BrokerStatusOnline, LinkedBy: autoProvideLinkedBy,
	}))
	d = w.srv.evaluatePassthroughIdentity(ctxFor(w.projectMember), nil, legacyEmbedded, w.projectA.ID, SurfacePassthroughCreate)
	require.NotNil(t, d, "legacy embedded broker must stay admin-only")
	w.srv.SetEmbeddedBrokerID("")

	// Co-located flat Docker: also shares Hub host metadata identity -> stays admin-only.
	embDockerRow, err := w.srv.RegisterEmbeddedFlatRuntimeBroker(ctx, &brokeridentity.Identity{
		SchemaVersion:   brokeridentity.SchemaVersion,
		InstanceKey:     "emb-docker",
		RuntimeBrokerID: tid("q5-eval-emb-docker"),
		RuntimeTarget:   api.RuntimeTargetDescriptor{ID: tid("q5-emb-docker-rt"), Type: config.RuntimeTargetTypeDocker, DisplayName: "Emb Docker"},
		ExecutionScope: brokeridentity.ExecutionScope{
			Type:   config.RuntimeTargetTypeDocker,
			Docker: &brokeridentity.DockerScope{DaemonID: "d1"},
		},
	}, config.V1RuntimeBrokerInstanceConfig{
		Key: "emb-docker", Name: "emb-docker",
		RuntimeTarget: &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeDocker, DisplayName: "Emb Docker"},
	}, EmbeddedFlatRegistrationOptions{Endpoint: "http://localhost:9800", AutoProvide: true})
	require.NoError(t, err)
	_, err = w.s.SetRuntimeBrokerGCPHost(ctx, embDockerRow.ID, hostSA, "q5-proj")
	require.NoError(t, err)
	embDockerRow, err = w.s.GetRuntimeBroker(ctx, embDockerRow.ID)
	require.NoError(t, err)
	require.NoError(t, w.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: w.projectA.ID, BrokerID: embDockerRow.ID, BrokerName: embDockerRow.Name,
		Status: store.BrokerStatusOnline, LinkedBy: autoProvideLinkedBy,
	}))
	d = w.srv.evaluatePassthroughIdentity(ctxFor(w.projectMember), nil, embDockerRow, w.projectA.ID, SurfacePassthroughCreate)
	require.NotNil(t, d, "co-located flat Docker broker shares Hub host identity and must stay admin-only")

	// Co-located flat Cloud Run: operator-provisioned (CreatedBy=="") whose GCP host pair is runtime_target.service_account -> allows provider_grant!
	const crAgentSA = "cr-agents@cr-proj.iam.gserviceaccount.com"
	checker.AllowTarget(crAgentSA)
	embCRRow, err := w.srv.RegisterEmbeddedFlatRuntimeBroker(ctx, &brokeridentity.Identity{
		SchemaVersion:   brokeridentity.SchemaVersion,
		InstanceKey:     "emb-cr",
		RuntimeBrokerID: tid("q5-eval-emb-cr"),
		RuntimeTarget:   *cloudRunTargetFor("q5-eval-emb-cr"),
		ExecutionScope: brokeridentity.ExecutionScope{
			Type:     config.RuntimeTargetTypeCloudRun,
			CloudRun: &brokeridentity.CloudRunScope{ProjectNumber: "12345", Region: "us-central1"},
		},
	}, config.V1RuntimeBrokerInstanceConfig{
		Key: "emb-cr", Name: "emb-cr",
		RuntimeTarget: &config.V1RuntimeTargetConfig{
			Type: config.RuntimeTargetTypeCloudRun, DisplayName: "Emb CR",
			ProjectID: "cr-proj", Region: "us-central1", ServiceAccount: crAgentSA,
		},
	}, EmbeddedFlatRegistrationOptions{Endpoint: "http://localhost:9800", AutoProvide: true})
	require.NoError(t, err)
	require.NoError(t, w.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: w.projectA.ID, BrokerID: embCRRow.ID, BrokerName: embCRRow.Name,
		Status: store.BrokerStatusOnline, LinkedBy: autoProvideLinkedBy,
	}))
	logs.reset()
	d = w.srv.evaluatePassthroughIdentity(ctxFor(w.projectMember), nil, embCRRow, w.projectA.ID, SurfacePassthroughCreate)
	require.Nil(t, d, "co-located flat Cloud Run broker must admit via provider_grant + actAs on service_account")
	entry, ok = logs.findLast("passthrough allowed", SurfacePassthroughCreate)
	require.True(t, ok)
	assert.Equal(t, "provider_grant", entry.attrs["authPath"])
	assert.Equal(t, crAgentSA, entry.attrs["targetSA"])
}

// TestPassthroughGate_Surfaces_CreateAndPatch exercises Surfaces 1 (create) and
// 2 (PATCH) end-to-end over HTTP, including flat Cloud Run passthrough checking
// actAs on runtime_target.service_account (not the Hub host account), r14
// AutoProvide gating, pre-dispatch denial when actAs is absent (zero dispatch /
// zero DB mutation), cross-project link isolation, and empty LinkedBy refusal.
func TestPassthroughGate_Surfaces_CreateAndPatch(t *testing.T) {
	w := newQ5World(t, "q5-cp")
	ctx := context.Background()
	logs := capturePassthroughSlog(t)

	const (
		crRuntimeSA = "cr-runtime-sa@cr-proj.iam.gserviceaccount.com"
		hubHostSA   = "hub-vm-sa@hub-proj.iam.gserviceaccount.com"
	)

	// Register a remote-style flat Cloud Run broker owned by brokerOwner whose
	// GCP host pair is crRuntimeSA.
	flatCR := &store.RuntimeBroker{
		ID:                         tid("q5-cp-flat-cr"),
		Name:                       "q5-cp-flat-cr",
		Slug:                       "q5-cp-flat-cr",
		Status:                     store.BrokerStatusOnline,
		CreatedBy:                  w.brokerOwner.ID,
		AutoProvide:                false,
		GCPHostServiceAccountEmail: crRuntimeSA,
		GCPHostProjectID:           "cr-proj",
		RuntimeTarget:              cloudRunTargetFor("q5-cp"),
	}
	require.NoError(t, w.s.CreateRuntimeBroker(ctx, flatCR))

	// Checker allows crRuntimeSA ONLY, and explicitly denies hubHostSA — proving
	// Check 3 tests runtime_target.service_account rather than any Hub host SA.
	checker := store.NewFakeCallerPermissionChecker().
		AllowTarget(crRuntimeSA).
		DenyTarget(hubHostSA, "hub host SA is not the Cloud Run workload SA")
	enforceSAAssign(w.srv, checker)

	// Seed an existingcreated-phase agent on flatCR so PATCH can be tested before AutoProvide is enabled.
	patchAgent := &store.Agent{
		ID:              tid("q5-cp-patch-agent"),
		Slug:            "q5-cp-patch-agent",
		Name:            "q5-cp-patch-agent",
		ProjectID:       w.projectA.ID,
		Phase:           string(state.PhaseCreated),
		CreatedBy:       w.projectMember.ID,
		OwnerID:         w.projectMember.ID,
		RuntimeBrokerID: flatCR.ID,
	}
	require.NoError(t, w.s.CreateAgent(ctx, patchAgent))

	// A) Linked only to projectB with AutoProvide=true -> both create and PATCH in projectA are denied!
	//    (On flatCR, unlinked create is refused even earlier with 422 runtime_broker_not_linked;
	//    on a legacy AutoProvide=true broker linked only to projectB, create reaches the passthrough
	//    gate directly and is refused with 403 passthroughProviderGrantDenialSubstr.)
	flatCR.AutoProvide = true
	require.NoError(t, w.s.UpdateRuntimeBroker(ctx, flatCR))
	require.NoError(t, w.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: w.projectB.ID, BrokerID: flatCR.ID, BrokerName: flatCR.Name,
		LocalPath: fixtureProviderCheckout(t), Status: store.BrokerStatusOnline, LinkedBy: w.brokerOwner.ID,
	}))

	rec := doRequestAsUser(t, w.srv, w.projectMember, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "denied-unlinked-flat-create", ProjectID: w.projectA.ID, RuntimeBrokerID: flatCR.ID, Task: "t",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModePassthrough},
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Nil(t, w.disp.capturedAgent, "denied create must not dispatch")

	legacyCrossProj := &store.RuntimeBroker{
		ID: tid("q5-cp-legacy-cross"), Name: "q5-cp-legacy-cross", Slug: "q5-cp-legacy-cross",
		Status: store.BrokerStatusOnline, CreatedBy: w.brokerOwner.ID, AutoProvide: true,
		GCPHostServiceAccountEmail: crRuntimeSA, GCPHostProjectID: "cr-proj",
	}
	require.NoError(t, w.s.CreateRuntimeBroker(ctx, legacyCrossProj))
	require.NoError(t, w.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: w.projectB.ID, BrokerID: legacyCrossProj.ID, BrokerName: legacyCrossProj.Name,
		LocalPath: fixtureProviderCheckout(t), Status: store.BrokerStatusOnline, LinkedBy: w.brokerOwner.ID,
	}))
	rec = doRequestAsUser(t, w.srv, w.projectMember, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "denied-crossproj-legacy-create", ProjectID: w.projectA.ID, RuntimeBrokerID: legacyCrossProj.ID, Task: "t",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModePassthrough},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Nil(t, w.disp.capturedAgent)

	rec = doRequestAsUser(t, w.srv, w.projectMember, http.MethodPatch, "/api/v1/agents/"+patchAgent.ID,
		map[string]interface{}{"gcp_identity": map[string]interface{}{"metadata_mode": store.GCPMetadataModePassthrough}})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), passthroughProviderGrantDenialSubstr)

	// B) Linked to projectA with empty LinkedBy -> both create and PATCH denied!
	require.NoError(t, w.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: w.projectA.ID, BrokerID: flatCR.ID, BrokerName: flatCR.Name,
		LocalPath: fixtureProviderCheckout(t), Status: store.BrokerStatusOnline, LinkedBy: "",
	}))
	rec = doRequestAsUser(t, w.srv, w.projectMember, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "denied-empty-linker-create", ProjectID: w.projectA.ID, RuntimeBrokerID: flatCR.ID, Task: "t",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModePassthrough},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), passthroughProviderGrantDenialSubstr)
	assert.Nil(t, w.disp.capturedAgent)

	// C) Linked to projectA by brokerOwner, but AutoProvide=false (r14): PATCH and create both denied for non-owner!
	_ = w.s.RemoveProjectProvider(ctx, w.projectA.ID, flatCR.ID)
	flatCR.AutoProvide = false
	require.NoError(t, w.s.UpdateRuntimeBroker(ctx, flatCR))
	require.NoError(t, w.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: w.projectA.ID, BrokerID: flatCR.ID, BrokerName: flatCR.Name,
		LocalPath: fixtureProviderCheckout(t), Status: store.BrokerStatusOnline, LinkedBy: w.brokerOwner.ID,
	}))
	rec = doRequestAsUser(t, w.srv, w.projectMember, http.MethodPatch, "/api/v1/agents/"+patchAgent.ID,
		map[string]interface{}{"gcp_identity": map[string]interface{}{"metadata_mode": store.GCPMetadataModePassthrough}})
	assert.Equal(t, http.StatusForbidden, rec.Code, "r14: PATCH passthrough on flat row with AutoProvide=false must be denied: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), passthroughProviderGrantDenialSubstr)

	// D) Enable AutoProvide=true, WITHOUT actAs -> both create and PATCH denied BEFORE dispatch!
	flatCR.AutoProvide = true
	require.NoError(t, w.s.UpdateRuntimeBroker(ctx, flatCR))
	checker.DenyTarget(crRuntimeSA, "no actAs on Cloud Run SA")
	checker.AllowTarget(hubHostSA) // Even if caller had actAs on Hub host SA, Cloud Run SA is checked!

	rec = doRequestAsUser(t, w.srv, w.projectMember, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "denied-no-actas-create", ProjectID: w.projectA.ID, RuntimeBrokerID: flatCR.ID, Task: "t",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModePassthrough},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), crRuntimeSA)
	assert.Nil(t, w.disp.capturedAgent, "no agent may be dispatched when actAs fails")
	_, err := w.s.GetAgentBySlug(ctx, w.projectA.ID, "denied-no-actas-create")
	assert.ErrorIs(t, err, store.ErrNotFound, "denied create must persist no agent")

	rec = doRequestAsUser(t, w.srv, w.projectMember, http.MethodPatch, "/api/v1/agents/"+patchAgent.ID,
		map[string]interface{}{"gcp_identity": map[string]interface{}{"metadata_mode": store.GCPMetadataModePassthrough}})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), crRuntimeSA)

	// E) Grant actAs on crRuntimeSA (while hubHostSA stays denied):
	//    non-owner (provider_grant), brokerOwner (owner), and adminUser (admin) all succeed!
	checker.AllowTarget(crRuntimeSA)
	checker.DenyTarget(hubHostSA, "still denied")

	logs.reset()
	rec = doRequestAsUser(t, w.srv, w.projectMember, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "allowed-provider-grant-create", ProjectID: w.projectA.ID, RuntimeBrokerID: flatCR.ID, Task: "t",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModePassthrough},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.NotNil(t, w.disp.capturedAgent)
	entry, ok := logs.findLast("passthrough allowed", SurfacePassthroughCreate)
	require.True(t, ok)
	assert.Equal(t, "provider_grant", entry.attrs["authPath"])
	assert.Equal(t, crRuntimeSA, entry.attrs["targetSA"])

	logs.reset()
	rec = doRequestAsUser(t, w.srv, w.projectMember, http.MethodPatch, "/api/v1/agents/"+patchAgent.ID,
		map[string]interface{}{"gcp_identity": map[string]interface{}{"metadata_mode": store.GCPMetadataModePassthrough}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	entry, ok = logs.findLast("passthrough allowed", SurfacePassthroughPatch)
	require.True(t, ok)
	assert.Equal(t, "provider_grant", entry.attrs["authPath"])
	assert.Equal(t, crRuntimeSA, entry.attrs["targetSA"])

	// Broker owner -> authPath="owner"
	logs.reset()
	rec = doRequestAsUser(t, w.srv, w.brokerOwner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "allowed-owner-create", ProjectID: w.projectA.ID, RuntimeBrokerID: flatCR.ID, Task: "t",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModePassthrough},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	entry, ok = logs.findLast("passthrough allowed", SurfacePassthroughCreate)
	require.True(t, ok)
	assert.Equal(t, "owner", entry.attrs["authPath"])

	// Hub admin -> authPath="admin"
	logs.reset()
	rec = doRequestAsUser(t, w.srv, w.adminUser, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "allowed-admin-create", ProjectID: w.projectA.ID, RuntimeBrokerID: flatCR.ID, Task: "t",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModePassthrough},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	entry, ok = logs.findLast("passthrough allowed", SurfacePassthroughCreate)
	require.True(t, ok)
	assert.Equal(t, "admin", entry.attrs["authPath"])

	// Start of an existing passthrough agent is not re-gated by evaluatePassthroughIdentity.
	createdByMember := agentBySlug(t, w.s, w.projectA.ID, "allowed-provider-grant-create")
	require.NoError(t, w.s.UpdateAgentStatus(ctx, createdByMember.ID, store.AgentStatusUpdate{Phase: string(state.PhaseStopped)}))
	checker.DenyTarget(crRuntimeSA, "revoked after creation")
	w.disp.startCalled = false
	rec = doRequestAsUser(t, w.srv, w.projectMember, http.MethodPost, "/api/v1/agents/"+createdByMember.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, w.disp.startCalled)
}

// TestPassthroughGate_Surface_ProjectDefault exercises Surface 3
// (SurfacePassthroughProjectDefault) on both project settings PUT (against a
// single co-located flat Cloud Run embedded broker vs legacy Hub-own embedded
// broker) and projectDefaultPassthroughAllowed apply.
func TestPassthroughGate_Surface_ProjectDefault(t *testing.T) {
	t.Run("put_colocated_flat_cloudrun_provider_grant_matrix", func(t *testing.T) {
		w := newQ5World(t, "q5-pd-cr")
		ctx := context.Background()
		logs := capturePassthroughSlog(t)

		const crSA = "pd-cr-sa@pd-cr-proj.iam.gserviceaccount.com"
		checker := store.NewFakeCallerPermissionChecker().AllowTarget(crSA)
		enforceSAAssign(w.srv, checker)

		embCR, err := w.srv.RegisterEmbeddedFlatRuntimeBroker(ctx, &brokeridentity.Identity{
			SchemaVersion:   brokeridentity.SchemaVersion,
			InstanceKey:     "pd-emb-cr",
			RuntimeBrokerID: tid("q5-pd-emb-cr"),
			RuntimeTarget:   *cloudRunTargetFor("q5-pd-emb-cr"),
			ExecutionScope: brokeridentity.ExecutionScope{
				Type:     config.RuntimeTargetTypeCloudRun,
				CloudRun: &brokeridentity.CloudRunScope{ProjectNumber: "999", Region: "us-central1"},
			},
		}, config.V1RuntimeBrokerInstanceConfig{
			Key: "pd-emb-cr", Name: "pd-emb-cr",
			RuntimeTarget: &config.V1RuntimeTargetConfig{
				Type: config.RuntimeTargetTypeCloudRun, DisplayName: "PD Emb CR",
				ProjectID: "pd-cr-proj", Region: "us-central1", ServiceAccount: crSA,
			},
		}, EmbeddedFlatRegistrationOptions{Endpoint: "http://localhost:9800", AutoProvide: false})
		require.NoError(t, err)

		putMode := func(as *store.User) (int, string) {
			rec := doRequestAsUser(t, w.srv, as, http.MethodPut,
				"/api/v1/projects/"+w.projectA.ID+"/settings",
				map[string]interface{}{"defaultGCPIdentityMode": store.GCPMetadataModePassthrough})
			return rec.Code, rec.Body.String()
		}

		// 1. Not linked -> denied.
		code, body := putMode(w.projectMember)
		assert.Equal(t, http.StatusForbidden, code, body)
		assert.Contains(t, body, passthroughProviderGrantDenialSubstr)

		// 2. Linked to projectA, but AutoProvide=false (r14) -> denied for non-admin project owner.
		require.NoError(t, w.s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: w.projectA.ID, BrokerID: embCR.ID, BrokerName: embCR.Name,
			Status: store.BrokerStatusOnline, LinkedBy: autoProvideLinkedBy,
		}))
		code, body = putMode(w.projectMember)
		assert.Equal(t, http.StatusForbidden, code, "r14: AutoProvide=false denies non-admin on flat row: %s", body)

		// 3. AutoProvide=true, but no actAs -> denied at Check 3.
		embCR.AutoProvide = true
		require.NoError(t, w.s.UpdateRuntimeBroker(ctx, embCR))
		checker.DenyTarget(crSA, "no actAs")
		code, body = putMode(w.projectMember)
		assert.Equal(t, http.StatusForbidden, code, body)
		assert.Contains(t, body, crSA)

		// 4. AutoProvide=true + actAs -> allowed with authPath=provider_grant!
		checker.AllowTarget(crSA)
		logs.reset()
		code, body = putMode(w.projectMember)
		require.Equal(t, http.StatusOK, code, body)
		entry, ok := logs.findLast("passthrough allowed", SurfacePassthroughProjectDefault)
		require.True(t, ok)
		assert.Equal(t, "provider_grant", entry.attrs["authPath"])
		assert.Equal(t, crSA, entry.attrs["targetSA"])

		// 5. Hub admin -> allowed with authPath=admin.
		logs.reset()
		code, body = putMode(w.adminUser)
		require.Equal(t, http.StatusOK, code, body)
		entry, ok = logs.findLast("passthrough allowed", SurfacePassthroughProjectDefault)
		require.True(t, ok)
		assert.Equal(t, "admin", entry.attrs["authPath"])
	})

	t.Run("legacy_embedded_broker_put_and_apply_keep_admin_only", func(t *testing.T) {
		f := pdptPutSetup(t, true, false)
		ctx := context.Background()
		// Even with an explicit auto-provide link on f.proj and actAs allowed,
		// the legacy embedded broker represents the Hub host's identity and must
		// stay admin-only on both PUT and apply!
		freshBroker, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
		require.NoError(t, err)
		freshBroker.AutoProvide = true
		require.NoError(t, f.store.UpdateRuntimeBroker(ctx, freshBroker))
		_ = f.store.RemoveProjectProvider(ctx, f.proj.ID, f.broker.ID)
		require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: f.proj.ID, BrokerID: f.broker.ID, BrokerName: f.broker.Name,
			Status: store.BrokerStatusOnline, LinkedBy: autoProvideLinkedBy,
		}))

		code, body := pdptPutMode(t, f, store.GCPMetadataModePassthrough)
		assert.Equal(t, http.StatusForbidden, code, body)
		assert.Contains(t, body, passthroughProviderGrantDenialSubstr)

		// Apply path (projectDefaultPassthroughAllowed) also refuses non-admin on legacy embedded broker,
		// and allows Hub admin with authPath="admin".
		userCtx := contextWithIdentity(ctx, NewAuthenticatedUser(f.owner.ID, f.owner.Email, f.owner.DisplayName, f.owner.Role, ""))
		allowed, _ := f.srv.projectDefaultPassthroughAllowed(userCtx, nil, f.broker.ID, f.proj.ID, "a1", "")
		assert.False(t, allowed)

		grantSuperAdminRole(t, f.store, f.owner.ID)
		logs := capturePassthroughSlog(t)
		allowed, _ = f.srv.projectDefaultPassthroughAllowed(userCtx, nil, f.broker.ID, f.proj.ID, "a1", "")
		assert.True(t, allowed)
		entry, ok := logs.findLast("passthrough allowed", SurfacePassthroughProjectDefault)
		require.True(t, ok)
		assert.Equal(t, "admin", entry.attrs["authPath"])
	})
}

// TestPassthroughGate_Surfaces_TranslationAndReview exercises Surfaces 4
// (SurfacePassthroughTranslation on cloudrun-sandbox agent create & PATCH) and
// 5 (SurfacePassthroughTranslationReview maintenance review) across the Q5
// provider-grant matrix.
func TestPassthroughGate_Surfaces_TranslationAndReview(t *testing.T) {
	f := setupPTG(t, "q5-trans", ptgHostSA)
	ctx := context.Background()
	logs := capturePassthroughSlog(t)

	member := f.nonOwnerCreator(t, "q5-trans")
	admin := addExtraUser(t, f.store, tid("q5-trans-admin"), "q5-trans-admin@test.com", store.UserRoleAdmin)
	grantSuperAdminRole(t, f.store, admin.ID)
	addProjectMember(t, f.srv, f.store, f.project, admin)

	checker := store.NewFakeCallerPermissionChecker().AllowTarget(ptgHostSA)
	enforceSAAssign(f.srv, checker)

	// 1. Default seedPassthroughSandboxServer link has LinkedBy=="" -> member is denied!
	rec := doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "trans-denied-nolinker", ProjectID: f.project.ID, Task: "t",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModePassthrough},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), passthroughProviderGrantDenialSubstr)
	assert.Empty(t, f.hostSARecords(t, ptgHostSA))

	// 2. Link with owner consent (LinkedBy = f.owner.ID), but deny actAs -> denied before dispatch, no SA record!
	_ = f.store.RemoveProjectProvider(ctx, f.project.ID, f.broker.ID)
	require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: f.project.ID, BrokerID: f.broker.ID, BrokerName: f.broker.Name,
		LocalPath: fixtureProviderCheckout(t), Status: store.BrokerStatusOnline,
		LinkedBy: f.owner.ID,
	}))
	checker.DenyTarget(ptgHostSA, "no actAs")
	rec = doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "trans-denied-noactas", ProjectID: f.project.ID, Task: "t",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModePassthrough},
	})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	f.agentMissing(t, "trans-denied-noactas")
	assert.Empty(t, f.hostSARecords(t, ptgHostSA))

	// 3. Allow actAs -> member create translates passthrough to assign with authPath=provider_grant on BOTH
	//    SurfacePassthroughCreate and SurfacePassthroughTranslation!
	checker.AllowTarget(ptgHostSA)
	logs.reset()
	rec = doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "trans-allowed-member", ProjectID: f.project.ID, Task: "t",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModePassthrough},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	createEntry, ok := logs.findLast("passthrough allowed", SurfacePassthroughCreate)
	require.True(t, ok)
	assert.Equal(t, "provider_grant", createEntry.attrs["authPath"])
	transEntry, ok := logs.findLast("passthrough allowed", SurfacePassthroughTranslation)
	require.True(t, ok)
	assert.Equal(t, "provider_grant", transEntry.attrs["authPath"])

	createdAgent := agentBySlug(t, f.store, f.project.ID, "trans-allowed-member")
	require.NotNil(t, createdAgent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeAssign, createdAgent.AppliedConfig.GCPIdentity.MetadataMode)

	// 4. Surface 5 (SurfacePassthroughTranslationReview):
	//    - With consented provider link + actAs -> review of member's translated agent PASSES (authPath=provider_grant)!
	//    - Without actAs -> review FAILS!
	//    - With unconsented link (LinkedBy=="") -> review FAILS with new denial message!
	reviewOf := func(agentID string) translationReviewEntry {
		entries, err := f.srv.reviewTranslatedAgents(ctx, false)
		require.NoError(t, err)
		for _, en := range entries {
			if en.AgentID == agentID {
				return en
			}
		}
		t.Fatalf("agent %s not reviewed", agentID)
		return translationReviewEntry{}
	}

	logs.reset()
	en := reviewOf(createdAgent.ID)
	assert.Equal(t, translationReviewPass, en.Outcome, en.Reason)
	revEntry, ok := logs.findLastForCaller("passthrough allowed", SurfacePassthroughTranslationReview, member.ID)
	require.True(t, ok)
	assert.Equal(t, "provider_grant", revEntry.attrs["authPath"])

	// Admin-created translated agent passes review with authPath=admin; owner-created with authPath=owner.
	rec = doRequestAsUser(t, f.srv, admin, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "trans-allowed-admin", ProjectID: f.project.ID, Task: "t",
		GCPIdentity: &GCPIdentityAssignment{MetadataMode: store.GCPMetadataModePassthrough},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	adminAgent := agentBySlug(t, f.store, f.project.ID, "trans-allowed-admin")
	logs.reset()
	en = reviewOf(adminAgent.ID)
	assert.Equal(t, translationReviewPass, en.Outcome, en.Reason)
	revEntry, ok = logs.findLastForCaller("passthrough allowed", SurfacePassthroughTranslationReview, admin.ID)
	require.True(t, ok)
	assert.Equal(t, "admin", revEntry.attrs["authPath"])

	// Revoke actAs -> review fails for member's agent and logs authPath=provider_grant on denial.
	checker.DenyTarget(ptgHostSA, "revoked")
	logs.reset()
	en = reviewOf(createdAgent.ID)
	assert.Equal(t, translationReviewFail, en.Outcome)
	assert.Contains(t, en.Reason, store.PermissionActAs)
	revDenied, ok := logs.findLast("passthrough denied", SurfacePassthroughTranslationReview)
	require.True(t, ok)
	assert.Equal(t, "provider_grant", revDenied.attrs["authPath"])
	checker.AllowTarget(ptgHostSA)

	// Remove owner consent (LinkedBy == "") -> review fails at Check 1!
	_ = f.store.RemoveProjectProvider(ctx, f.project.ID, f.broker.ID)
	require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: f.project.ID, BrokerID: f.broker.ID, BrokerName: f.broker.Name,
		LocalPath: fixtureProviderCheckout(t), Status: store.BrokerStatusOnline,
		LinkedBy: "",
	}))
	en = reviewOf(createdAgent.ID)
	assert.Equal(t, translationReviewFail, en.Outcome)
	assert.Contains(t, en.Reason, passthroughProviderGrantDenialSubstr)

	// Agent creator on translatePassthroughForSandbox directly remains denied even with consented link.
	_ = f.store.RemoveProjectProvider(ctx, f.project.ID, f.broker.ID)
	require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: f.project.ID, BrokerID: f.broker.ID, BrokerName: f.broker.Name,
		LocalPath: fixtureProviderCheckout(t), Status: store.BrokerStatusOnline,
		LinkedBy: f.owner.ID,
	}))
	rawAgent := &store.Agent{
		ID: tid("q5-trans-agent-caller"), Name: "q5-trans-agent-caller", ProjectID: f.project.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			GCPIdentity: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough},
		},
	}
	err := f.srv.translatePassthroughForSandbox(
		contextWithIdentity(ctx, agentIdentityFor(tid("parent-agent"), f.project.ID, ScopeAgentCreate)),
		nil, rawAgent, f.broker.ID, f.project)
	var denied *passthroughTranslationDenied
	require.True(t, errors.As(err, &denied))
	assert.Equal(t, http.StatusForbidden, denied.status)
}

// TestPassthroughGate_Surface_ReincarnateMove exercises Surface 6
// (SurfacePassthroughMove) across owner, admin, provider_grant (with and
// without actAs, unlinked / cross-project link, empty LinkedBy, and legacy
// embedded destination broker).
func TestPassthroughGate_Surface_ReincarnateMove(t *testing.T) {
	const dstHostSA = "move-dst-host@move-proj.iam.gserviceaccount.com"
	brokerOwnerID := tid("q5-move-broker-owner")

	f := setupMoveFixture(t, false, func(dst *store.RuntimeBroker) {
		dst.CreatedBy = brokerOwnerID
		dst.AutoProvide = true
		dst.GCPHostServiceAccountEmail = dstHostSA
		dst.GCPHostProjectID = "move-proj"
	})
	ctx := context.Background()
	logs := capturePassthroughSlog(t)

	brokerOwner := addExtraUser(t, f.s, brokerOwnerID, "move-broker-owner@test.com", store.UserRoleMember)
	member := addExtraUser(t, f.s, tid("q5-move-member"), "move-member@test.com", store.UserRoleMember)
	admin := addExtraUser(t, f.s, tid("q5-move-admin"), "move-admin@test.com", store.UserRoleAdmin)
	grantSuperAdminRole(t, f.s, admin.ID)

	// Grant both member and brokerOwner project owner rights so both have reincarnate + provider rights on f.project.
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, f.project.ID, member.ID))
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, f.project.ID, brokerOwner.ID))
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, f.project.ID, admin.ID))

	// Configure f.agent with passthrough GCP identity.
	a, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	a.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough}
	require.NoError(t, f.s.UpdateAgent(ctx, a))
	f.agent = a

	checker := store.NewFakeCallerPermissionChecker().AllowTarget(dstHostSA)
	enforceSAAssign(f.srv, checker)

	moveDryRunAs := func(u *store.User, targetID string) (int, string) {
		rec := doRequestAsUser(t, f.srv, u, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/reincarnate",
			ReincarnateAgentRequest{DryRun: true, TargetBroker: targetID})
		return rec.Code, rec.Body.String()
	}

	// 1. Not linked to f.project (even if linked to another project!) -> member is denied at moveCheckAccess passthrough!
	otherProject := &store.Project{
		ID: tid("q5-move-other-proj"), Name: "other", Slug: "q5-move-other-proj",
		OwnerID: brokerOwner.ID, CreatedBy: brokerOwner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.s.CreateProject(ctx, otherProject))
	require.NoError(t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: otherProject.ID, BrokerID: f.dst.ID, BrokerName: f.dst.Name,
		Status: store.BrokerStatusOnline, LinkedBy: brokerOwner.ID,
	}))

	code, body := moveDryRunAs(member, f.dst.ID)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, passthroughProviderGrantDenialSubstr)

	// 2. Linked to f.project with empty LinkedBy -> member still denied!
	require.NoError(t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: f.project.ID, BrokerID: f.dst.ID, BrokerName: f.dst.Name,
		Status: store.BrokerStatusOnline, LinkedBy: "",
	}))
	code, body = moveDryRunAs(member, f.dst.ID)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, passthroughProviderGrantDenialSubstr)

	// 3. Linked to f.project by brokerOwner, WITHOUT actAs -> denied before dispatch!
	_ = f.s.RemoveProjectProvider(ctx, f.project.ID, f.dst.ID)
	require.NoError(t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: f.project.ID, BrokerID: f.dst.ID, BrokerName: f.dst.Name,
		Status: store.BrokerStatusOnline, LinkedBy: brokerOwner.ID,
	}))
	checker.DenyTarget(dstHostSA, "no actAs on destination host SA")
	logs.reset()
	code, body = moveDryRunAs(member, f.dst.ID)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, dstHostSA)
	deniedEntry, ok := logs.findLast("passthrough denied", SurfacePassthroughMove)
	require.True(t, ok)
	assert.Equal(t, "provider_grant", deniedEntry.attrs["authPath"])

	// 4. Linked by brokerOwner + actAs -> member (provider_grant), brokerOwner (owner), and admin (admin) all pass!
	checker.AllowTarget(dstHostSA)
	for _, tc := range []struct {
		user     *store.User
		wantPath string
	}{
		{member, "provider_grant"},
		{brokerOwner, "owner"},
		{admin, "admin"},
	} {
		logs.reset()
		code, body := moveDryRunAs(tc.user, f.dst.ID)
		require.Equal(t, http.StatusOK, code, "path=%s body=%s", tc.wantPath, body)
		var resp ReincarnateAgentResponse
		require.NoError(t, json.Unmarshal([]byte(body), &resp))
		require.NotNil(t, resp.MoveVerdict)
		assert.True(t, resp.MoveVerdict.Eligible)
		entry, ok := logs.findLast("passthrough allowed", SurfacePassthroughMove)
		require.True(t, ok)
		assert.Equal(t, tc.wantPath, entry.attrs["authPath"])
	}

	// 5. Legacy embedded destination broker -> non-admin denied even when linked with AutoProvide=true + actAs!
	f.srv.SetEmbeddedBrokerID(f.dst.ID)
	t.Cleanup(func() { f.srv.SetEmbeddedBrokerID("") })
	f.dst.CreatedBy = ""
	require.NoError(t, f.s.UpdateRuntimeBroker(ctx, f.dst))
	code, body = moveDryRunAs(member, f.dst.ID)
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, passthroughProviderGrantDenialSubstr)
}
