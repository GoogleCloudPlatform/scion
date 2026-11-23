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
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkCapacityTestBroker creates a runtime broker for the usage-capacity
// tests below.
func mkCapacityTestBroker(t *testing.T, s store.Store, name string) *store.RuntimeBroker {
	t.Helper()
	b := &store.RuntimeBroker{
		ID:      uuid.New().String(),
		Name:    name,
		Slug:    uuid.New().String(),
		Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), b))
	return b
}

// reservationByScopeID finds a decoded usage reservation by its ScopeID
// (the broker ID, for max_agents_per_broker rows).
func reservationByScopeID(reservations []usageReservationView, scopeID string) *usageReservationView {
	for i := range reservations {
		if reservations[i].ScopeID == scopeID {
			return &reservations[i]
		}
	}
	return nil
}

// TestGetUsageByLimit_MaxAgentsPerBroker_PerBrokerSourceAndLimit is the
// central P2.2 test (ptone/scion#2061, ptone/scion#2177 "and/or the admin
// Quotas page"): the admin usage detail for max_agents_per_broker must show
// each broker's actual effective limit and precedence source — broker
// override, entitlement binding, or hub default — never the raw binding or
// the hub-wide default value substituted for a broker that has its own
// override (AC-P2-10). It also proves the underlying listing bug is fixed:
// before this change, getUsageByLimit only queried store.QuotaScopeSystem
// reservations, so a broker-scoped limit like max_agents_per_broker always
// came back with zero reservations regardless of real broker usage.
func TestGetUsageByLimit_MaxAgentsPerBroker_PerBrokerSourceAndLimit(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	brokerOverride := mkCapacityTestBroker(t, s, "usage-override-broker")
	brokerDefault := mkCapacityTestBroker(t, s, "usage-default-broker")
	brokerEntitlement := mkCapacityTestBroker(t, s, "usage-entitlement-broker")

	const overrideValue = 3
	const bindingValue = 7
	require.NotEqualValues(t, overrideValue, def.DefaultValue)
	require.NotEqualValues(t, bindingValue, def.DefaultValue)

	maxAgents := int64(overrideValue)
	_, err = s.PutBrokerSettings(ctx, brokerOverride.ID, store.BrokerSettings{MaxAgents: &maxAgents}, 0, "test")
	require.NoError(t, err)

	seedBinding(t, s, def.ID, store.EntitlementSubjectSystemDefault, "", store.QuotaScopeBroker, brokerEntitlement.ID, bindingValue)

	for _, b := range []*store.RuntimeBroker{brokerOverride, brokerDefault, brokerEntitlement} {
		_, err := s.CreateUsageReservation(ctx, &store.UsageReservation{
			LimitDefinitionID: def.ID,
			SubjectID:         b.ID,
			ScopeType:         store.QuotaScopeBroker,
			ScopeID:           b.ID,
			ResourceID:        "agent-on-" + b.ID,
			Reserved:          1,
		})
		require.NoError(t, err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp usageByLimitResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 3, resp.TotalActive, "reservations across every broker must be listed, not just system-scoped ones")
	require.Len(t, resp.Reservations, 3)

	override := reservationByScopeID(resp.Reservations, brokerOverride.ID)
	require.NotNil(t, override)
	require.NotNil(t, override.BrokerAgentLimit)
	assert.EqualValues(t, overrideValue, *override.BrokerAgentLimit)
	assert.Equal(t, BrokerLimitSourceBroker, override.BrokerAgentLimitSource)

	defaultView := reservationByScopeID(resp.Reservations, brokerDefault.ID)
	require.NotNil(t, defaultView)
	require.NotNil(t, defaultView.BrokerAgentLimit)
	assert.EqualValues(t, def.DefaultValue, *defaultView.BrokerAgentLimit)
	assert.Equal(t, BrokerLimitSourceHubDefault, defaultView.BrokerAgentLimitSource)

	entitlement := reservationByScopeID(resp.Reservations, brokerEntitlement.ID)
	require.NotNil(t, entitlement)
	require.NotNil(t, entitlement.BrokerAgentLimit)
	assert.EqualValues(t, bindingValue, *entitlement.BrokerAgentLimit)
	assert.Equal(t, BrokerLimitSourceEntitlement, entitlement.BrokerAgentLimitSource)
}

// TestGetUsageByLimit_NonBrokerLimit_Unaffected proves the broker-scoped
// enumeration in getUsageByLimit is opt-in by limit name: a system-scoped
// limit must keep using the original system-scope query and must never
// carry BrokerAgentLimit/BrokerAgentLimitSource.
func TestGetUsageByLimit_NonBrokerLimit_Unaffected(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	def := seedLimit(t, s, "usage_capacity_non_broker_limit", 5)
	_, err := s.CreateUsageReservation(ctx, &store.UsageReservation{
		LimitDefinitionID: def.ID,
		SubjectID:         "some-user",
		ScopeType:         store.QuotaScopeSystem,
		ScopeID:           "",
		ResourceID:        "agent-1",
		Reserved:          1,
	})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp usageByLimitResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Reservations, 1)
	assert.Nil(t, resp.Reservations[0].BrokerAgentLimit)
	assert.Empty(t, resp.Reservations[0].BrokerAgentLimitSource)

	raw := struct {
		Reservations []map[string]any `json:"reservations"`
	}{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.Len(t, raw.Reservations, 1)
	_, hasLimit := raw.Reservations[0]["brokerAgentLimit"]
	assert.False(t, hasLimit, "brokerAgentLimit key must be absent from the JSON for a non-broker limit")
	_, hasSource := raw.Reservations[0]["brokerAgentLimitSource"]
	assert.False(t, hasSource, "brokerAgentLimitSource key must be absent from the JSON for a non-broker limit")
}

// TestGetUsageByLimit_MaxAgentsPerBroker_NoActiveReservations proves the
// broker-scoped enumeration returns an empty, not nil, list and a 200 when
// no broker currently holds a reservation — mirroring the original
// no-reservations behaviour for other limits.
func TestGetUsageByLimit_MaxAgentsPerBroker_NoActiveReservations(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	mkCapacityTestBroker(t, s, "usage-idle-broker")

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp usageByLimitResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 0, resp.TotalActive)
	assert.Empty(t, resp.Reservations)
}

// TestGetUsageSummary_MaxAgentsPerBroker_SumsAcrossBrokers proves the admin
// usage summary (GET /api/v1/admin/usage) reports the real active count for
// max_agents_per_broker by summing reservations across every runtime broker,
// the same way getUsageByLimit and ReconcileStaleBrokerQuotaReservations
// enumerate brokers (ptone/scion#2061 P2.2). Before this fix, the summary's
// activeCount for this limit always queried store.QuotaScopeSystem — a scope
// max_agents_per_broker reservations are never stored at — so the admin
// quotas page always showed 0 active agents for it regardless of real usage.
func TestGetUsageSummary_MaxAgentsPerBroker_SumsAcrossBrokers(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	brokerA := mkCapacityTestBroker(t, s, "summary-broker-a")
	brokerB := mkCapacityTestBroker(t, s, "summary-broker-b")

	// Two reservations on brokerA, one on brokerB: the summary must report 3
	// total, not 0 (the pre-fix behaviour) and not just one broker's count.
	for _, res := range []*store.UsageReservation{
		{LimitDefinitionID: def.ID, SubjectID: brokerA.ID, ScopeType: store.QuotaScopeBroker, ScopeID: brokerA.ID, ResourceID: "agent-a1", Reserved: 1},
		{LimitDefinitionID: def.ID, SubjectID: brokerA.ID, ScopeType: store.QuotaScopeBroker, ScopeID: brokerA.ID, ResourceID: "agent-a2", Reserved: 1},
		{LimitDefinitionID: def.ID, SubjectID: brokerB.ID, ScopeType: store.QuotaScopeBroker, ScopeID: brokerB.ID, ResourceID: "agent-b1", Reserved: 1},
	} {
		_, err := s.CreateUsageReservation(ctx, res)
		require.NoError(t, err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp usageSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	var found *usageSummaryEntry
	for i := range resp.Items {
		if resp.Items[i].LimitDefinition.ID == def.ID {
			found = &resp.Items[i]
			break
		}
	}
	require.NotNil(t, found, "summary must include the max_agents_per_broker limit")
	assert.Equal(t, 3, found.ActiveCount, "activeCount must sum reservations across every broker, not just query system scope")
}

// TestGetUsageSummary_NonBrokerLimit_Unaffected proves the broker-scoped
// enumeration in getUsageSummary is opt-in by limit name: a system-scoped
// limit must keep using the original system-scope query.
func TestGetUsageSummary_NonBrokerLimit_Unaffected(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	def := seedLimit(t, s, "usage_summary_non_broker_limit", 5)
	_, err := s.CreateUsageReservation(ctx, &store.UsageReservation{
		LimitDefinitionID: def.ID,
		SubjectID:         "some-user",
		ScopeType:         store.QuotaScopeSystem,
		ScopeID:           "",
		ResourceID:        "agent-1",
		Reserved:          1,
	})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp usageSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	var found *usageSummaryEntry
	for i := range resp.Items {
		if resp.Items[i].LimitDefinition.ID == def.ID {
			found = &resp.Items[i]
			break
		}
	}
	require.NotNil(t, found)
	assert.Equal(t, 1, found.ActiveCount)
}

// TestQuotaUsage_MaxProjectsPerUser_AdminAndMyUsageMatchEnforcement proves the
// fix for ptone/scion#6036 for max_projects_per_user: project creation reserves
// at ("system", "system") (handlers_projects_core.go), so admin usage summary,
// admin usage-by-limit, and per-user /usage/me must all query that exact scope
// instead of ("system", "").
func TestQuotaUsage_MaxProjectsPerUser_AdminAndMyUsageMatchEnforcement(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Set a finite default ceiling so project creation records reservations
	// and enforces the cap.
	setUserProjectQuotaCeiling(t, s, 2)

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxProjectsPerUser)
	require.NoError(t, err)

	userA := seedHubMemberNoProjects(t, s, "quota-proj-a")
	userB := seedHubMemberNoProjects(t, s, "quota-proj-b")

	createProjectAs := func(u *store.User, name string, wantStatus int) string {
		t.Helper()
		rec := doRequestAsUser(t, srv, u, http.MethodPost, "/api/v1/projects", map[string]string{
			"name": name,
		})
		require.Equal(t, wantStatus, rec.Code, "body: %s", rec.Body.String())
		if wantStatus != http.StatusCreated {
			return ""
		}
		var created store.Project
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		return created.ID
	}

	// userA creates 2 projects (at limit); a 3rd must be refused with 429.
	projA1 := createProjectAs(userA, "proj-a1", http.StatusCreated)
	createProjectAs(userA, "proj-a2", http.StatusCreated)
	createProjectAs(userA, "proj-a3-blocked", http.StatusTooManyRequests)

	// userB creates 1 project.
	createProjectAs(userB, "proj-b1", http.StatusCreated)

	// Sanity check: enforcement sees 2 for userA and 1 for userB.
	require.Equal(t, int64(2), countProjectQuotaReservations(t, s, userA.ID))
	require.Equal(t, int64(1), countProjectQuotaReservations(t, s, userB.ID))

	// 1. Admin usage summary must report 3 active reservations across both users.
	summaryRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
	require.Equal(t, http.StatusOK, summaryRec.Code, "body: %s", summaryRec.Body.String())
	var summary usageSummaryResponse
	require.NoError(t, json.Unmarshal(summaryRec.Body.Bytes(), &summary))
	var summaryEntry *usageSummaryEntry
	for i := range summary.Items {
		if summary.Items[i].LimitDefinition.ID == def.ID {
			summaryEntry = &summary.Items[i]
			break
		}
	}
	require.NotNil(t, summaryEntry, "summary must include max_projects_per_user")
	assert.Equal(t, 3, summaryEntry.ActiveCount)

	// 2. Admin usage-by-limit must list all 3 reservations at ("system", "system").
	detailRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
	require.Equal(t, http.StatusOK, detailRec.Code, "body: %s", detailRec.Body.String())
	var detail usageByLimitResponse
	require.NoError(t, json.Unmarshal(detailRec.Body.Bytes(), &detail))
	assert.Equal(t, 3, detail.TotalActive)
	require.Len(t, detail.Reservations, 3)
	for _, r := range detail.Reservations {
		assert.Equal(t, store.QuotaScopeSystem, r.ScopeType)
		assert.Equal(t, store.QuotaScopeSystem, r.ScopeID)
	}

	// 3. Per-user /usage/me must report each user's own count and effective limit.
	myProjectUsage := func(u *store.User) myUsageEntry {
		t.Helper()
		rec := doRequestAsUser(t, srv, u, http.MethodGet, "/api/v1/usage/me", nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var resp myUsageResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		for _, entry := range resp.Items {
			if entry.LimitDefinition.ID == def.ID {
				return entry
			}
		}
		t.Fatalf("max_projects_per_user missing from /usage/me for %s", u.ID)
		return myUsageEntry{}
	}

	entryA := myProjectUsage(userA)
	assert.Equal(t, int64(2), entryA.Current)
	assert.Equal(t, int64(2), entryA.Max)

	entryB := myProjectUsage(userB)
	assert.Equal(t, int64(1), entryB.Current)
	assert.Equal(t, int64(2), entryB.Max)

	// 4. Raising userA's effective limit via an entitlement binding must be
	// reflected in /usage/me and allow another project.
	seedBinding(t, s, def.ID, store.EntitlementSubjectUser, userA.ID, store.QuotaScopeSystem, store.QuotaScopeSystem, 4)
	entryA = myProjectUsage(userA)
	assert.Equal(t, int64(2), entryA.Current)
	assert.Equal(t, int64(4), entryA.Max)

	createProjectAs(userA, "proj-a3-allowed", http.StatusCreated)
	entryA = myProjectUsage(userA)
	assert.Equal(t, int64(3), entryA.Current)

	// 5. Deleting a project releases its reservation across admin and /usage/me.
	delRec := doRequestAsUser(t, srv, userA, http.MethodDelete, "/api/v1/projects/"+projA1, nil)
	require.Equal(t, http.StatusNoContent, delRec.Code, "body: %s", delRec.Body.String())

	entryA = myProjectUsage(userA)
	assert.Equal(t, int64(2), entryA.Current)

	summaryRec = doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
	require.Equal(t, http.StatusOK, summaryRec.Code)
	require.NoError(t, json.Unmarshal(summaryRec.Body.Bytes(), &summary))
	for i := range summary.Items {
		if summary.Items[i].LimitDefinition.ID == def.ID {
			assert.Equal(t, 3, summary.Items[i].ActiveCount)
		}
	}
}

// groupReservationStore wraps store.Store to keep group-scoped
// UsageReservations in memory while delegating everything else. The SQLite Ent
// schema does not include "group" in usage_reservation.scope_type yet
// (ptone/scion#3082), so tests that create real max_members_per_group
// reservations through the HTTP handlers use this wrapper for both srv.store
// and srv.quotaService.
type groupReservationStore struct {
	store.Store
	mu     sync.Mutex
	active map[string]*store.UsageReservation
}

func (g *groupReservationStore) TryAdvisoryLock(ctx context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	return true, func() error { return nil }, nil
}

func (g *groupReservationStore) TryAdvisoryLockObject(ctx context.Context, class store.AdvisoryLockKey, obj int32) (bool, func() error, error) {
	return true, func() error { return nil }, nil
}

func (g *groupReservationStore) CreateUsageReservation(ctx context.Context, r *store.UsageReservation) (*store.UsageReservation, error) {
	if r.ScopeType != store.QuotaScopeGroup {
		return g.Store.CreateUsageReservation(ctx, r)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := r.LimitDefinitionID + "\x00" + r.ResourceID
	if _, exists := g.active[key]; exists {
		return nil, store.ErrActiveReservationExists
	}
	cp := *r
	if cp.ID == "" {
		cp.ID = uuid.New().String()
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}
	g.active[key] = &cp
	return &cp, nil
}

func (g *groupReservationStore) CountActiveReservations(ctx context.Context, limitDefinitionID, subjectID, scopeType, scopeID string) (int64, error) {
	if scopeType != store.QuotaScopeGroup {
		return g.Store.CountActiveReservations(ctx, limitDefinitionID, subjectID, scopeType, scopeID)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var n int64
	for _, r := range g.active {
		if r.LimitDefinitionID == limitDefinitionID && r.SubjectID == subjectID &&
			r.ScopeType == scopeType && r.ScopeID == scopeID {
			n++
		}
	}
	return n, nil
}

func (g *groupReservationStore) CountActiveReservationsByScope(ctx context.Context, limitDefinitionID, scopeType string) (map[string]int64, error) {
	if scopeType != store.QuotaScopeGroup {
		return g.Store.CountActiveReservationsByScope(ctx, limitDefinitionID, scopeType)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	counts := make(map[string]int64)
	for _, r := range g.active {
		if r.LimitDefinitionID == limitDefinitionID && r.ScopeType == scopeType {
			counts[r.ScopeID]++
		}
	}
	return counts, nil
}

func (g *groupReservationStore) ListActiveReservationsByScopeType(ctx context.Context, limitDefinitionID, scopeType string) ([]*store.UsageReservation, error) {
	if scopeType != store.QuotaScopeGroup {
		return g.Store.ListActiveReservationsByScopeType(ctx, limitDefinitionID, scopeType)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []*store.UsageReservation
	for _, r := range g.active {
		if r.LimitDefinitionID == limitDefinitionID && r.ScopeType == scopeType {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (g *groupReservationStore) GetActiveReservation(ctx context.Context, limitDefinitionID, resourceID string) (*store.UsageReservation, error) {
	g.mu.Lock()
	if r, ok := g.active[limitDefinitionID+"\x00"+resourceID]; ok {
		cp := *r
		g.mu.Unlock()
		return &cp, nil
	}
	g.mu.Unlock()
	return g.Store.GetActiveReservation(ctx, limitDefinitionID, resourceID)
}

func (g *groupReservationStore) ReleaseReservation(ctx context.Context, limitDefinitionID, resourceID string) error {
	g.mu.Lock()
	key := limitDefinitionID + "\x00" + resourceID
	if _, ok := g.active[key]; ok {
		delete(g.active, key)
		g.mu.Unlock()
		return nil
	}
	g.mu.Unlock()
	return g.Store.ReleaseReservation(ctx, limitDefinitionID, resourceID)
}

// TestQuotaUsage_MaxMembersPerGroup_AdminUsageMatchesEnforcement proves the fix
// for ptone/scion#6036 for max_members_per_group: group member additions
// reserve at scope_type="group", scope_id=groupID (handlers_groups.go), so
// admin usage summary and usage-by-limit must aggregate across group scope
// instead of querying ("system", "").
func TestQuotaUsage_MaxMembersPerGroup_AdminUsageMatchesEnforcement(t *testing.T) {
	ctx := context.Background()

	// Verify first against the real SQLite store that querying group scope
	// in admin usage endpoints succeeds cleanly with 0 active reservations.
	t.Run("sqlite store read succeeds with zero active", func(t *testing.T) {
		srv, s := testServer(t)
		def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxMembersPerGroup)
		require.NoError(t, err)

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		rec = doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var detail usageByLimitResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &detail))
		assert.Equal(t, 0, detail.TotalActive)
	})

	// Exercise the real add-member/remove-member HTTP paths with a finite
	// max_members_per_group ceiling and assert admin usage matches enforcement.
	t.Run("admin summary and detail match group member enforcement", func(t *testing.T) {
		srv, s, wrapped, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) *groupReservationStore {
			return &groupReservationStore{
				Store:  inner,
				active: make(map[string]*store.UsageReservation),
			}
		})
		srv.quotaService = &QuotaService{store: wrapped, logger: slog.Default()}

		def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxMembersPerGroup)
		require.NoError(t, err)
		def.DefaultValue = 2
		_, err = s.UpdateLimitDefinition(ctx, def)
		require.NoError(t, err)

		groupA := &store.Group{
			ID: uuid.New().String(), Name: "Quota Group A", Slug: "quota-group-a",
			GroupType: store.GroupTypeExplicit, OwnerID: DevUserID, CreatedBy: DevUserID,
		}
		groupB := &store.Group{
			ID: uuid.New().String(), Name: "Quota Group B", Slug: "quota-group-b",
			GroupType: store.GroupTypeExplicit, OwnerID: DevUserID, CreatedBy: DevUserID,
		}
		require.NoError(t, s.CreateGroup(ctx, groupA))
		require.NoError(t, s.CreateGroup(ctx, groupB))

		u1 := seedHubMemberNoProjects(t, s, "grp-mem-1")
		u2 := seedHubMemberNoProjects(t, s, "grp-mem-2")
		u3 := seedHubMemberNoProjects(t, s, "grp-mem-3")

		addMember := func(groupID, userID string, wantStatus int) {
			t.Helper()
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/groups/"+groupID+"/members", map[string]string{
				"memberType": store.GroupMemberTypeUser,
				"memberId":   userID,
			})
			require.Equal(t, wantStatus, rec.Code, "body: %s", rec.Body.String())
		}

		// Add 2 members to groupA (hits limit); a 3rd must be refused with 429.
		addMember(groupA.ID, u1.ID, http.StatusCreated)
		addMember(groupA.ID, u2.ID, http.StatusCreated)
		addMember(groupA.ID, u3.ID, http.StatusTooManyRequests)

		// Add 1 member to groupB.
		addMember(groupB.ID, u3.ID, http.StatusCreated)

		// 1. Admin usage summary must report 3 active reservations across both groups.
		summaryRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
		require.Equal(t, http.StatusOK, summaryRec.Code, "body: %s", summaryRec.Body.String())
		var summary usageSummaryResponse
		require.NoError(t, json.Unmarshal(summaryRec.Body.Bytes(), &summary))
		var summaryEntry *usageSummaryEntry
		for i := range summary.Items {
			if summary.Items[i].LimitDefinition.ID == def.ID {
				summaryEntry = &summary.Items[i]
				break
			}
		}
		require.NotNil(t, summaryEntry, "summary must include max_members_per_group")
		assert.Equal(t, 3, summaryEntry.ActiveCount)

		// 2. Admin usage-by-limit must list all 3 reservations across groupA and groupB.
		detailRec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
		require.Equal(t, http.StatusOK, detailRec.Code, "body: %s", detailRec.Body.String())
		var detail usageByLimitResponse
		require.NoError(t, json.Unmarshal(detailRec.Body.Bytes(), &detail))
		assert.Equal(t, 3, detail.TotalActive)
		require.Len(t, detail.Reservations, 3)

		perGroup := make(map[string]int)
		for _, r := range detail.Reservations {
			assert.Equal(t, store.QuotaScopeGroup, r.ScopeType)
			perGroup[r.ScopeID]++
		}
		assert.Equal(t, 2, perGroup[groupA.ID])
		assert.Equal(t, 1, perGroup[groupB.ID])

		// 3. Removing a member releases its reservation and drops admin counts to 2.
		delRec := doRequest(t, srv, http.MethodDelete, "/api/v1/groups/"+groupA.ID+"/members/"+store.GroupMemberTypeUser+"/"+u1.ID, nil)
		require.Equal(t, http.StatusNoContent, delRec.Code, "body: %s", delRec.Body.String())

		detailRec = doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+def.ID, nil)
		require.Equal(t, http.StatusOK, detailRec.Code)
		require.NoError(t, json.Unmarshal(detailRec.Body.Bytes(), &detail))
		assert.Equal(t, 2, detail.TotalActive)
		assert.Len(t, detail.Reservations, 2)
	})
}
