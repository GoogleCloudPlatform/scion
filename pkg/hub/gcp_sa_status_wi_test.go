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

package hub

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// wiReadStubAdmin implements GCPServiceAccountAdmin for status-view unit tests:
// only RoleMembers (and AddRoleMembers when testing cache invalidation) may be
// called. Any project-level or SA lifecycle call fails.
type wiReadStubAdmin struct {
	readCalls  []string
	readRoles  []string
	membersFn  func(call int, saEmail, role string) ([]string, error)
	writeCalls int
}

func (a *wiReadStubAdmin) CreateServiceAccount(context.Context, string, string, string, string) (string, string, error) {
	return "", "", errors.New("unexpected CreateServiceAccount")
}

func (a *wiReadStubAdmin) DeleteServiceAccount(context.Context, string) error {
	return errors.New("unexpected DeleteServiceAccount")
}

func (a *wiReadStubAdmin) SetIAMPolicy(context.Context, string, string, string) error {
	return errors.New("unexpected SetIAMPolicy")
}

func (a *wiReadStubAdmin) AddRoleMembers(context.Context, string, string, []string) error {
	a.writeCalls++
	return nil
}

func (a *wiReadStubAdmin) RoleMembers(_ context.Context, saEmail, role string) ([]string, error) {
	a.readCalls = append(a.readCalls, saEmail)
	a.readRoles = append(a.readRoles, role)
	if a.membersFn != nil {
		return a.membersFn(len(a.readCalls), saEmail, role)
	}
	return nil, nil
}

func withWIBindingClock(t *testing.T, start time.Time) *time.Time {
	t.Helper()
	now := start
	prev := gcpSAWIBindingNow
	gcpSAWIBindingNow = func() time.Time { return now }
	t.Cleanup(func() { gcpSAWIBindingNow = prev })
	return &now
}

func TestWIRoleMembersCache_TTLKeyAndInvalidation(t *testing.T) {
	now := withWIBindingClock(t, time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	current := []string{"serviceAccount:p.svc.id.goog[agents/scion-foo]"}
	admin := &wiReadStubAdmin{
		membersFn: func(_ int, saEmail, _ string) ([]string, error) {
			if strings.EqualFold(saEmail, wiTestGSA) {
				return append([]string(nil), current...), nil
			}
			return []string{"serviceAccount:p.svc.id.goog[agents/scion-other]"}, nil
		},
	}
	s := &Server{gcpIAMAdmin: admin}
	ctx := context.Background()

	got, err := s.cachedWorkloadIdentityMembers(ctx, wiTestGSA)
	require.NoError(t, err)
	assert.True(t, got["serviceAccount:p.svc.id.goog[agents/scion-foo]"])
	require.Len(t, admin.readCalls, 1)
	assert.Equal(t, []string{roleWorkloadIdentityUser}, admin.readRoles)

	// Within 60s and case-insensitive: served from cache without calling IAM.
	*now = now.Add(59 * time.Second)
	current = []string{"serviceAccount:p.svc.id.goog[agents/changed-not-yet-seen]"}
	got, err = s.cachedWorkloadIdentityMembers(ctx, strings.ToUpper(wiTestGSA))
	require.NoError(t, err)
	assert.True(t, got["serviceAccount:p.svc.id.goog[agents/scion-foo]"])
	assert.Len(t, admin.readCalls, 1)

	// Another email does not share the entry.
	otherGot, err := s.cachedWorkloadIdentityMembers(ctx, "scion-other@hub-proj.iam.gserviceaccount.com")
	require.NoError(t, err)
	assert.True(t, otherGot["serviceAccount:p.svc.id.goog[agents/scion-other]"])
	assert.Len(t, admin.readCalls, 2)

	// At 60s the entry expires and IAM is read again.
	*now = now.Add(2 * time.Second)
	got, err = s.cachedWorkloadIdentityMembers(ctx, wiTestGSA)
	require.NoError(t, err)
	assert.True(t, got["serviceAccount:p.svc.id.goog[agents/changed-not-yet-seen]"])
	assert.Len(t, admin.readCalls, 3)

	// A policy write through addWorkloadIdentityMembers invalidates that email's cache entry.
	current = []string{"serviceAccount:p.svc.id.goog[agents/after-grant]"}
	require.NoError(t, s.addWorkloadIdentityMembers(ctx, wiTestGSA, []string{"serviceAccount:p.svc.id.goog[agents/after-grant]"}))
	got, err = s.cachedWorkloadIdentityMembers(ctx, wiTestGSA)
	require.NoError(t, err)
	assert.True(t, got["serviceAccount:p.svc.id.goog[agents/after-grant]"])
	assert.Len(t, admin.readCalls, 4)
}

func TestWIRoleMembersCache_ErrorNotCachedAndExpiredNotServed(t *testing.T) {
	now := withWIBindingClock(t, time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC))
	fail := true
	admin := &wiReadStubAdmin{
		membersFn: func(int, string, string) ([]string, error) {
			if fail {
				return nil, &googleapi.Error{Code: http.StatusForbidden, Message: "denied"}
			}
			return []string{"serviceAccount:p.svc.id.goog[agents/scion-foo]"}, nil
		},
	}
	s := &Server{gcpIAMAdmin: admin}
	ctx := context.Background()

	_, err := s.cachedWorkloadIdentityMembers(ctx, wiTestGSA)
	require.Error(t, err)
	assert.Len(t, admin.readCalls, 1)

	// Immediate retry after fixing permissions succeeds without waiting 60s.
	fail = false
	got, err := s.cachedWorkloadIdentityMembers(ctx, wiTestGSA)
	require.NoError(t, err)
	assert.True(t, got["serviceAccount:p.svc.id.goog[agents/scion-foo]"])
	assert.Len(t, admin.readCalls, 2)

	// Past 60s, if IAM now fails (for example SA deleted or access revoked),
	// the expired entry is NOT served as still bound.
	*now = now.Add(gcpSAWIBindingCacheTTL + time.Second)
	fail = true
	_, err = s.cachedWorkloadIdentityMembers(ctx, wiTestGSA)
	require.Error(t, err)
	assert.Len(t, admin.readCalls, 3)
}

// FYI-4: stored targets from a broker downgraded below wiTargetsV1 or with a
// stale heartbeat are marked stale rather than presented as current.
func TestGCPServiceAccountWIBinding_TargetStaleness(t *testing.T) {
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	withWIBindingClock(t, base)

	freshHB := base.Add(-2 * time.Minute)
	recentHB := base.Add(-10 * time.Second)
	oldHB := base.Add(-profileSAReportFreshFor - time.Minute)

	freshBroker := wiLegacyBroker("fresh", true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "pool-one.svc.id.goog", Namespace: "agents"},
		{Profile: "shared", Pool: "p-shared.svc.id.goog", Namespace: "shared-ns"},
	}, wiK8sProfile("gke"), wiK8sProfile("shared"))
	freshBroker.LastHeartbeat = freshHB

	// Downgraded below wiTargetsV1: heartbeats continue (recentHB), so
	// LastHeartbeat is recent, yet WorkloadIdentityTargets is frozen from
	// before the downgrade.
	downgraded := wiLegacyBroker("downgraded", false, []store.WorkloadIdentityTarget{
		{Profile: "gke-old", Pool: "pool-two.svc.id.goog", Namespace: "legacy"},
		{Profile: "shared", Pool: "p-shared.svc.id.goog", Namespace: "shared-ns"},
	}, wiK8sProfile("gke-old"), wiK8sProfile("shared"))
	downgraded.LastHeartbeat = recentHB

	staleHBBroker := wiLegacyBroker("offline-ish", true, []store.WorkloadIdentityTarget{
		{Profile: "gke-stale", Pool: "pool-three.svc.id.goog", Namespace: "stale-ns"},
	}, wiK8sProfile("gke-stale"))
	staleHBBroker.LastHeartbeat = oldHB

	admin := &wiReadStubAdmin{
		membersFn: func(int, string, string) ([]string, error) {
			return []string{
				"serviceAccount:pool-one.svc.id.goog[agents/scion-foo]",
				"serviceAccount:pool-two.svc.id.goog[legacy/scion-foo]",
				"serviceAccount:p-shared.svc.id.goog[shared-ns/scion-foo]",
			}, nil
		},
	}
	s := &Server{gcpIAMAdmin: admin}
	sa := &store.GCPServiceAccount{ID: "sa-1", Email: wiTestGSA, Managed: true}
	brokers := []*store.RuntimeBroker{freshBroker, downgraded, staleHBBroker}
	mappings := gcpSAProfileMappings(sa.Email, s.brokerKubernetesProfileMappings(brokers), base)

	binding := s.evaluateWorkloadIdentityStatus(context.Background(), sa, brokers, mappings, base)
	assert.Equal(t, GCPSABindingNotBound, binding.State)

	byPool := map[string]GCPServiceAccountBindingTarget{}
	for _, tgt := range binding.Targets {
		byPool[tgt.Pool] = tgt
	}
	require.Len(t, byPool, 4)

	t1 := byPool["pool-one.svc.id.goog"]
	assert.Equal(t, GCPSABindingBound, t1.State)
	assert.False(t, t1.Stale)
	assert.Empty(t, t1.StaleReason)
	require.NotNil(t, t1.LastHeartbeat)
	assert.True(t, t1.LastHeartbeat.Equal(freshHB))

	t2 := byPool["pool-two.svc.id.goog"]
	assert.Equal(t, GCPSABindingBound, t2.State)
	assert.True(t, t2.Stale, "downgraded broker's stored target must be flagged stale")
	assert.Equal(t, GCPSABindingTargetStaleBrokerNoLongerReports, t2.StaleReason)
	assert.Nil(t, t2.LastHeartbeat, "recent post-downgrade LastHeartbeat must not be shown as target report age")

	t3 := byPool["pool-three.svc.id.goog"]
	assert.Equal(t, GCPSABindingNotBound, t3.State)
	assert.True(t, t3.Stale)
	assert.Equal(t, GCPSABindingTargetStaleBrokerHeartbeat, t3.StaleReason)
	require.NotNil(t, t3.LastHeartbeat)
	assert.True(t, t3.LastHeartbeat.Equal(oldHB))
	assert.Equal(t, workloadIdentityManualStep(wiTestGSA, "serviceAccount:pool-three.svc.id.goog[stale-ns/scion-foo]"), t3.ManualStep)

	// Shared target across one fresh broker and one downgraded broker: merged
	// target warns stale, while per-profile rows reflect their own broker.
	tShared := byPool["p-shared.svc.id.goog"]
	assert.Equal(t, GCPSABindingBound, tShared.State)
	assert.True(t, tShared.Stale)
	assert.Equal(t, GCPSABindingTargetStaleBrokerNoLongerReports, tShared.StaleReason)

	byBrokerProfile := map[string]*GCPServiceAccountProfileBinding{}
	for i := range mappings {
		byBrokerProfile[mappings[i].BrokerName+"/"+mappings[i].Profile] = mappings[i].WorkloadIdentityBinding
	}
	require.NotNil(t, byBrokerProfile["fresh/shared"])
	assert.False(t, byBrokerProfile["fresh/shared"].Stale)
	require.NotNil(t, byBrokerProfile["downgraded/shared"])
	assert.True(t, byBrokerProfile["downgraded/shared"].Stale)
	assert.Equal(t, GCPSABindingTargetStaleBrokerNoLongerReports, byBrokerProfile["downgraded/shared"].StaleReason)
}
