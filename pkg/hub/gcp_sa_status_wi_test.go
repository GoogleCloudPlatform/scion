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
	readCalls    []string
	readRoles    []string
	membersFn    func(call int, saEmail, role string) ([]string, error)
	membersCtxFn func(ctx context.Context, call int, saEmail, role string) ([]string, error)
	writeCalls   int
	writeErr     error
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
	return a.writeErr
}

func (a *wiReadStubAdmin) RoleMembers(ctx context.Context, saEmail, role string) ([]string, error) {
	a.readCalls = append(a.readCalls, saEmail)
	a.readRoles = append(a.readRoles, role)
	if a.membersCtxFn != nil {
		return a.membersCtxFn(ctx, len(a.readCalls), saEmail, role)
	}
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

	// O4: A failed or partially applied AddRoleMembers attempt also invalidates
	// the cached entry, so a partial write followed by an error cannot serve a
	// stale member set for 60s.
	admin.writeErr = &googleapi.Error{Code: http.StatusInternalServerError, Message: "partial failure"}
	current = []string{"serviceAccount:p.svc.id.goog[agents/after-partial-failure]"}
	require.Error(t, s.addWorkloadIdentityMembers(ctx, wiTestGSA, []string{"serviceAccount:p.svc.id.goog[agents/after-partial-failure]"}))
	got, err = s.cachedWorkloadIdentityMembers(ctx, wiTestGSA)
	require.NoError(t, err)
	assert.True(t, got["serviceAccount:p.svc.id.goog[agents/after-partial-failure]"])
	assert.Len(t, admin.readCalls, 5)

	// O4: SA deletion (via invalidateActAsCache) invalidates only that email's
	// cached Workload Identity role-members entry; scion-other (cached at t+59s,
	// still within its 60s TTL at t+61s) stays cached.
	current = []string{"serviceAccount:p.svc.id.goog[agents/reminted]"}
	s.invalidateActAsCache(wiTestGSA)
	_, err = s.cachedWorkloadIdentityMembers(ctx, "scion-other@hub-proj.iam.gserviceaccount.com")
	require.NoError(t, err)
	assert.Len(t, admin.readCalls, 5, "other SA remains cached")
	got, err = s.cachedWorkloadIdentityMembers(ctx, wiTestGSA)
	require.NoError(t, err)
	assert.True(t, got["serviceAccount:p.svc.id.goog[agents/reminted]"])
	assert.Len(t, admin.readCalls, 6, "deleted SA's cache entry was invalidated")
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
	assert.Contains(t, binding.Reason, "3 of 4 targets bound")
	assert.Contains(t, binding.Reason, "3 targets from stale broker reports")

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

	// O1: When every target is bound but comes from a stale broker report, the
	// summary carries the stale qualifier in Reason and NextStep does not claim
	// "Nothing missing".
	staleOnlyBrokers := []*store.RuntimeBroker{downgraded}
	staleOnlyMappings := []GCPServiceAccountProfileMapping{{BrokerID: downgraded.ID, BrokerName: downgraded.Name, Profile: "gke-old", State: GCPSAMappingMapped}}
	staleOnlyBinding := s.evaluateWorkloadIdentityStatus(context.Background(), sa, staleOnlyBrokers, staleOnlyMappings, base)
	assert.Equal(t, GCPSABindingBound, staleOnlyBinding.State)
	assert.Equal(t, "2 targets from stale broker reports", staleOnlyBinding.Reason)
	step := gcpSANextStepForBinding(true, staleOnlyMappings, nil, staleOnlyBinding)
	assert.Equal(t, GCPSANextStepNone, step.Code)
	assert.Contains(t, step.Message, "stale broker reports")
	assert.NotContains(t, step.Message, "Nothing missing")
}

// O2 & O3: Missing-pool targets preserve P1's distinction (no_workload_identity_pool,
// broker_too_old, flat_broker_pool_unreported), and when some Kubernetes targets
// report no pool while reported pool targets are bound, overall State is unknown
// and NextStep does not claim "Nothing missing".
func TestGCPServiceAccountWIBinding_MissingReasonsAndPartialUnknown(t *testing.T) {
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	withWIBindingClock(t, base)

	current := wiLegacyBroker("a", true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "pool-one.svc.id.goog", Namespace: "agents"},
	}, wiK8sProfile("gke"), wiK8sProfile("gke-nopool"))
	current.LastHeartbeat = base.Add(-time.Minute)

	tooOld := wiLegacyBroker("old", false, nil, wiK8sProfile("k8s"))
	tooOld.LastHeartbeat = base.Add(-time.Minute)

	flatNoPool := wiFlatBroker("flat-nopool", "kubernetes", nil)
	flatNoPool.LastHeartbeat = base.Add(-time.Minute)

	admin := &wiReadStubAdmin{
		membersFn: func(int, string, string) ([]string, error) {
			return []string{"serviceAccount:pool-one.svc.id.goog[agents/scion-foo]"}, nil
		},
	}
	s := &Server{gcpIAMAdmin: admin}
	sa := &store.GCPServiceAccount{ID: "sa-1", Email: wiTestGSA, Managed: true}

	brokers := []*store.RuntimeBroker{current, tooOld, flatNoPool}
	mappings := []GCPServiceAccountProfileMapping{
		{BrokerID: current.ID, BrokerName: current.Name, Profile: "gke", State: GCPSAMappingMapped},
		{BrokerID: current.ID, BrokerName: current.Name, Profile: "gke-nopool", State: GCPSAMappingMapped},
		{BrokerID: tooOld.ID, BrokerName: tooOld.Name, Profile: "k8s", State: GCPSAMappingMapped},
	}
	binding := s.evaluateWorkloadIdentityStatus(context.Background(), sa, brokers, mappings, base)
	assert.Equal(t, GCPSABindingUnknown, binding.State, "O2: cannot be overall bound while Kubernetes targets report no pool")
	assert.Contains(t, binding.Reason, "1 reported target bound")
	assert.Contains(t, binding.Reason, "3 Kubernetes targets report no pool")
	require.Len(t, binding.Targets, 4)
	assert.Equal(t, GCPSABindingBound, binding.Targets[0].State)
	assert.Equal(t, "no_workload_identity_pool", binding.Targets[1].Reason)
	assert.Equal(t, []string{"a/gke-nopool"}, binding.Targets[1].Profiles)
	assert.Equal(t, "flat_broker_pool_unreported", binding.Targets[2].Reason)
	assert.Equal(t, []string{"flat-nopool"}, binding.Targets[2].Profiles)
	assert.Equal(t, "broker_too_old", binding.Targets[3].Reason)
	assert.Equal(t, []string{"old/k8s"}, binding.Targets[3].Profiles)

	// Per-profile rows also distinguish no_workload_identity_pool from broker_too_old.
	assert.Equal(t, GCPSABindingBound, mappings[0].WorkloadIdentityBinding.State)
	assert.Equal(t, "no_workload_identity_pool", mappings[1].WorkloadIdentityBinding.Reason)
	assert.Equal(t, "broker_too_old", mappings[2].WorkloadIdentityBinding.Reason)

	step := gcpSANextStepForBinding(true, mappings, nil, binding)
	assert.Equal(t, GCPSANextStepNone, step.Code)
	assert.Contains(t, step.Message, "could not be checked")
	assert.NotContains(t, step.Message, "Nothing missing")

	// Also when NO broker reported a pool (len(plan.targets) == 0), the missing
	// target reasons still distinguish all three causes.
	noPoolBrokers := []*store.RuntimeBroker{
		wiLegacyBroker("a", true, nil, wiK8sProfile("gke-nopool")),
		tooOld,
		flatNoPool,
	}
	noPoolMappings := []GCPServiceAccountProfileMapping{
		{BrokerID: "a", BrokerName: "a", Profile: "gke-nopool", State: GCPSAMappingMapped},
		{BrokerID: tooOld.ID, BrokerName: tooOld.Name, Profile: "k8s", State: GCPSAMappingMapped},
	}
	noPoolBinding := s.evaluateWorkloadIdentityStatus(context.Background(), sa, noPoolBrokers, noPoolMappings, base)
	assert.Equal(t, GCPSABindingUnknown, noPoolBinding.State)
	require.Len(t, noPoolBinding.Targets, 3)
	assert.Equal(t, "no_workload_identity_pool", noPoolBinding.Targets[0].Reason)
	assert.Equal(t, "flat_broker_pool_unreported", noPoolBinding.Targets[1].Reason)
	assert.Equal(t, "broker_too_old", noPoolBinding.Targets[2].Reason)
	assert.Equal(t, "no_workload_identity_pool", noPoolMappings[0].WorkloadIdentityBinding.Reason)
	assert.Equal(t, "broker_too_old", noPoolMappings[1].WorkloadIdentityBinding.Reason)
}

// Review 2: cachedWorkloadIdentityMembers bounds getIamPolicy with a short
// timeout and degrades to "unknown" ("IAM policy read failed") without caching.
func TestWIRoleMembersCache_PolicyReadTimeoutDegradesToUnknown(t *testing.T) {
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	withWIBindingClock(t, base)
	prevTimeout := gcpSAWIBindingReadTimeout
	gcpSAWIBindingReadTimeout = 25 * time.Millisecond
	t.Cleanup(func() { gcpSAWIBindingReadTimeout = prevTimeout })

	var sawDeadline bool
	admin := &wiReadStubAdmin{
		membersCtxFn: func(ctx context.Context, call int, _, _ string) ([]string, error) {
			_, sawDeadline = ctx.Deadline()
			if call == 1 {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(500 * time.Millisecond):
					return nil, errors.New("RoleMembers call had no read timeout")
				}
			}
			return []string{"serviceAccount:pool-one.svc.id.goog[agents/scion-foo]"}, nil
		},
	}
	s := &Server{gcpIAMAdmin: admin}
	b := wiLegacyBroker("a", true, []store.WorkloadIdentityTarget{
		{Profile: "gke", Pool: "pool-one.svc.id.goog", Namespace: "agents"},
	}, wiK8sProfile("gke"))
	b.LastHeartbeat = base.Add(-time.Minute)
	sa := &store.GCPServiceAccount{ID: "sa-1", Email: wiTestGSA, Managed: true}
	mappings := []GCPServiceAccountProfileMapping{{BrokerID: b.ID, BrokerName: b.Name, Profile: "gke", State: GCPSAMappingMapped}}

	binding := s.evaluateWorkloadIdentityStatus(context.Background(), sa, []*store.RuntimeBroker{b}, mappings, base)
	assert.True(t, sawDeadline, "RoleMembers must be called with a bounded context deadline")
	assert.Equal(t, GCPSABindingUnknown, binding.State)
	assert.Equal(t, GCPSABindingReasonIAMReadFailed, binding.Reason)
	require.NotNil(t, mappings[0].WorkloadIdentityBinding)
	assert.Equal(t, GCPSABindingUnknown, mappings[0].WorkloadIdentityBinding.State)
	assert.Equal(t, GCPSABindingReasonIAMReadFailed, mappings[0].WorkloadIdentityBinding.Reason)
	step := gcpSANextStepForBinding(true, mappings, nil, binding)
	assert.Equal(t, GCPSANextStepNone, step.Code)
	assert.Contains(t, step.Message, "could not be checked (IAM policy read failed)")

	// Timeout error is not cached: subsequent read within 60s retries and succeeds.
	bindingAfter := s.evaluateWorkloadIdentityStatus(context.Background(), sa, []*store.RuntimeBroker{b}, mappings, base)
	assert.Equal(t, GCPSABindingBound, bindingAfter.State)
	assert.Len(t, admin.readCalls, 2)
}

// Review 2 R1: When two profiles share a (pool, namespace) but one maps to a
// custom KSA while the other uses the conventional KSA, each profile's actual
// dispatch identity appears as its own target and drives both summary and nextStep.
func TestGCPServiceAccountWIBinding_MixedConventionalAndMappedKSAOnSamePool(t *testing.T) {
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	withWIBindingClock(t, base)

	b := wiLegacyBroker("a", true, []store.WorkloadIdentityTarget{
		{Profile: "gke-custom", Pool: "pool-one.svc.id.goog", Namespace: "agents"},
		{Profile: "gke-conv", Pool: "pool-one.svc.id.goog", Namespace: "agents"},
	}, wiK8sProfile("gke-custom"), wiK8sProfile("gke-conv"))
	b.LastHeartbeat = base.Add(-time.Minute)

	admin := &wiReadStubAdmin{
		membersFn: func(int, string, string) ([]string, error) {
			return []string{"serviceAccount:pool-one.svc.id.goog[agents/scion-foo]"}, nil
		},
	}
	s := &Server{gcpIAMAdmin: admin}
	sa := &store.GCPServiceAccount{ID: "sa-1", Email: wiTestGSA, Managed: true}
	mappings := []GCPServiceAccountProfileMapping{
		{BrokerID: b.ID, BrokerName: b.Name, Profile: "gke-conv", State: GCPSAMappingMapped},
		{BrokerID: b.ID, BrokerName: b.Name, Profile: "gke-custom", State: GCPSAMappingMapped, KubernetesServiceAccount: "custom-ksa", Namespace: "agents"},
	}

	binding := s.evaluateWorkloadIdentityStatus(context.Background(), sa, []*store.RuntimeBroker{b}, mappings, base)
	assert.Equal(t, GCPSABindingNotBound, binding.State)
	assert.Equal(t, "1 of 2 targets bound", binding.Reason)
	require.Len(t, binding.Targets, 2)
	assert.Equal(t, "custom-ksa", binding.Targets[0].KSA)
	assert.Equal(t, []string{"a/gke-custom"}, binding.Targets[0].Profiles)
	assert.Equal(t, GCPSABindingNotBound, binding.Targets[0].State)
	assert.Equal(t, "scion-foo", binding.Targets[1].KSA)
	assert.Equal(t, []string{"a/gke-conv"}, binding.Targets[1].Profiles)
	assert.Equal(t, GCPSABindingBound, binding.Targets[1].State)

	step := gcpSANextStepForBinding(true, mappings, nil, binding)
	assert.Equal(t, GCPSANextStepNotBound, step.Code)
	assert.Contains(t, step.Message, "serviceAccount:pool-one.svc.id.goog[agents/custom-ksa] (a/gke-custom)")
}
