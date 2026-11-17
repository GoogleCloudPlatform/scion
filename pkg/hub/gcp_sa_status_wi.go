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
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Workload Identity binding read for the per-account status view
// (ptone/scion#3329 phase 5, P4; ptone/scion#7968, design §3.7).
//
// Behind hub.k8s_workload_identity_setup:
//   - For GSAs minted by this hub (sa.Managed), the per-account status view
//     reads the GSA's resource-level IAM policy (roles/iam.workloadIdentityUser),
//     cached for 60s by email, and reports bound or not_bound per target from
//     the P1 target computation (workloadIdentityMintPlanFor).
//   - Bring-your-own GSAs stay unknown with reason "not minted by this hub".
//   - List views never read IAM policies.
//   - Stored broker targets are not presented as current when a broker has
//     stopped reporting wiTargetsV1 (for example after a downgrade) or its
//     heartbeat is stale.

const (
	// gcpSAWIBindingCacheTTL is how long a minted GSA's Workload Identity
	// role members are cached for the per-account status view (design §3.7).
	gcpSAWIBindingCacheTTL = 60 * time.Second

	// maxWIRoleMembersCacheEntries bounds the in-memory policy cache size.
	maxWIRoleMembersCacheEntries = 1024

	// GCPSABindingReasonNotMinted is the reason returned for bring-your-own
	// (registered, non-managed) GSAs when hub.k8s_workload_identity_setup is on.
	GCPSABindingReasonNotMinted = "not minted by this hub"

	// Staleness reasons for stored Workload Identity targets (FYI-4).
	GCPSABindingTargetStaleBrokerNoLongerReports = "broker_no_longer_reports"
	GCPSABindingTargetStaleBrokerHeartbeat       = "broker_heartbeat_stale"
)

// gcpSAWIBindingNow is the clock for the Workload Identity binding cache and
// target freshness checks; a variable so tests can advance it.
var gcpSAWIBindingNow = time.Now

// GCPServiceAccountBindingTarget is the Workload Identity binding state of one
// Kubernetes runtime target (pool and namespace) as last reported by the
// project's Runtime Brokers.
type GCPServiceAccountBindingTarget struct {
	Pool          string     `json:"pool,omitempty"`
	Namespace     string     `json:"namespace,omitempty"`
	KSA           string     `json:"ksa,omitempty"`
	Member        string     `json:"member,omitempty"`
	Profiles      []string   `json:"profiles,omitempty"`
	State         string     `json:"state"`
	Reason        string     `json:"reason,omitempty"`
	ManualStep    string     `json:"manualStep,omitempty"`
	LastHeartbeat *time.Time `json:"lastHeartbeat,omitempty"`
	Stale         bool       `json:"stale,omitempty"`
	StaleReason   string     `json:"staleReason,omitempty"`
}

// GCPServiceAccountProfileBinding is one Kubernetes broker profile's Workload
// Identity IAM binding state for an account.
type GCPServiceAccountProfileBinding struct {
	State       string `json:"state"`
	Reason      string `json:"reason,omitempty"`
	Pool        string `json:"pool,omitempty"`
	Member      string `json:"member,omitempty"`
	Stale       bool   `json:"stale,omitempty"`
	StaleReason string `json:"staleReason,omitempty"`
}

type wiRoleMembersCacheEntry struct {
	members map[string]bool
	expires time.Time
}

// wiRoleMembersCache caches roles/iam.workloadIdentityUser member sets per
// minted GSA email. mu guards the map only and is never held across an IAM RPC.
type wiRoleMembersCache struct {
	mu      sync.Mutex
	entries map[string]wiRoleMembersCacheEntry
}

func wiCacheKey(saEmail string) string {
	return strings.ToLower(strings.TrimSpace(saEmail))
}

func (c *wiRoleMembersCache) get(saEmail string, now time.Time) (map[string]bool, bool) {
	key := wiCacheKey(saEmail)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		return nil, false
	}
	entry, ok := c.entries[key]
	if !ok || !now.Before(entry.expires) {
		return nil, false
	}
	return entry.members, true
}

func (c *wiRoleMembersCache) put(saEmail string, members map[string]bool, now time.Time) {
	key := wiCacheKey(saEmail)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]wiRoleMembersCacheEntry)
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= maxWIRoleMembersCacheEntries {
		for k, v := range c.entries {
			if !now.Before(v.expires) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) >= maxWIRoleMembersCacheEntries {
			var oldestKey string
			var oldestExp time.Time
			for k, v := range c.entries {
				if oldestKey == "" || v.expires.Before(oldestExp) {
					oldestKey, oldestExp = k, v.expires
				}
			}
			if oldestKey != "" {
				delete(c.entries, oldestKey)
			}
		}
	}
	c.entries[key] = wiRoleMembersCacheEntry{
		members: members,
		expires: now.Add(gcpSAWIBindingCacheTTL),
	}
}

func (c *wiRoleMembersCache) invalidate(saEmail string) {
	key := wiCacheKey(saEmail)
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

func (c *wiRoleMembersCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}

// cachedWorkloadIdentityMembers reads the unconditional members of
// roles/iam.workloadIdentityUser on saEmail, reusing a cached result for up to
// gcpSAWIBindingCacheTTL. Failed reads are never cached, and expired entries
// are never served.
func (s *Server) cachedWorkloadIdentityMembers(ctx context.Context, saEmail string) (map[string]bool, error) {
	if s.gcpIAMAdmin == nil {
		return nil, errors.New("GCP IAM admin client is not configured")
	}
	now := gcpSAWIBindingNow()
	if cached, ok := s.wiRoleMembersCache.get(saEmail, now); ok {
		return cached, nil
	}
	list, err := s.gcpIAMAdmin.RoleMembers(ctx, saEmail, roleWorkloadIdentityUser)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(list))
	for _, m := range list {
		set[m] = true
	}
	s.wiRoleMembersCache.put(saEmail, set, gcpSAWIBindingNow())
	return set, nil
}

type storedProfileWITarget struct {
	pool          string
	namespace     string
	lastHeartbeat *time.Time
	stale         bool
	staleReason   string
}

// brokerWITargetFreshness evaluates whether a broker's stored
// WorkloadIdentityTargets are actively reported or potentially stale.
func brokerWITargetFreshness(b *store.RuntimeBroker, now time.Time) (lastHeartbeat *time.Time, stale bool, staleReason string) {
	if b == nil || b.Capabilities == nil || !b.Capabilities.WITargetsV1 {
		// A broker downgraded below wiTargetsV1 stops reporting targets while
		// still advancing LastHeartbeat on subsequent heartbeats, so
		// LastHeartbeat is omitted rather than presented as the target report age.
		return nil, true, GCPSABindingTargetStaleBrokerNoLongerReports
	}
	if b.LastHeartbeat.IsZero() {
		return nil, true, GCPSABindingTargetStaleBrokerHeartbeat
	}
	hb := b.LastHeartbeat
	if now.Sub(hb) > profileSAReportFreshFor {
		return &hb, true, GCPSABindingTargetStaleBrokerHeartbeat
	}
	return &hb, false, ""
}

func mergeWITargetFreshness(existing *GCPServiceAccountBindingTarget, first bool, hb *time.Time, stale bool, staleReason string) {
	if first {
		existing.LastHeartbeat = hb
		existing.Stale = stale
		existing.StaleReason = staleReason
		return
	}
	if stale {
		existing.Stale = true
		if staleReason == GCPSABindingTargetStaleBrokerNoLongerReports || existing.StaleReason == "" {
			existing.StaleReason = staleReason
		}
	}
	if existing.StaleReason == GCPSABindingTargetStaleBrokerNoLongerReports {
		existing.LastHeartbeat = nil
		return
	}
	if hb == nil {
		existing.LastHeartbeat = nil
	} else if existing.LastHeartbeat != nil && hb.Before(*existing.LastHeartbeat) {
		t := *hb
		existing.LastHeartbeat = &t
	}
}

func validStoredWITarget(t store.WorkloadIdentityTarget) (store.WorkloadIdentityTarget, bool) {
	t.Profile = strings.TrimSpace(t.Profile)
	t.Pool = strings.TrimSpace(t.Pool)
	t.Namespace = strings.TrimSpace(t.Namespace)
	if t.Pool == "" {
		return t, false
	}
	if config.ValidateWorkloadIdentityPool(t.Pool) != nil ||
		len(validation.IsDNS1123Label(t.Namespace)) > 0 ||
		!validWorkloadIdentityProfile(t.Profile) {
		return t, false
	}
	return t, true
}

// evaluateWorkloadIdentityStatus computes WorkloadIdentityBinding for the
// status view and populates WorkloadIdentityBinding on each entry of mappings
// (called only when hub.k8s_workload_identity_setup is enabled).
func (s *Server) evaluateWorkloadIdentityStatus(
	ctx context.Context,
	sa *store.GCPServiceAccount,
	brokers []*store.RuntimeBroker,
	mappings []GCPServiceAccountProfileMapping,
	now time.Time,
) GCPServiceAccountBinding {
	if !sa.Managed {
		for i := range mappings {
			mappings[i].WorkloadIdentityBinding = &GCPServiceAccountProfileBinding{
				State:  GCPSABindingUnknown,
				Reason: GCPSABindingReasonNotMinted,
			}
		}
		return GCPServiceAccountBinding{
			State:  GCPSABindingUnknown,
			Reason: GCPSABindingReasonNotMinted,
		}
	}

	// Sanitize stored targets as defence in depth and index freshness per
	// (pool, namespace) and per (brokerID, profile).
	sanitized := make([]*store.RuntimeBroker, 0, len(brokers))
	targetMeta := map[string]*GCPServiceAccountBindingTarget{}
	profileTargets := map[string]storedProfileWITarget{}

	for _, b := range brokers {
		if b == nil {
			continue
		}
		hb, stale, staleReason := brokerWITargetFreshness(b, now)
		cp := *b
		if len(b.WorkloadIdentityTargets) > 0 {
			clean := make([]store.WorkloadIdentityTarget, 0, len(b.WorkloadIdentityTargets))
			for _, raw := range b.WorkloadIdentityTargets {
				t, ok := validStoredWITarget(raw)
				if !ok {
					continue
				}
				if b.IsFlat() {
					t.Profile = ""
				}
				clean = append(clean, t)
				key := t.Pool + "\x00" + t.Namespace
				meta, seen := targetMeta[key]
				if !seen {
					meta = &GCPServiceAccountBindingTarget{}
					targetMeta[key] = meta
				}
				var hbCopy *time.Time
				if hb != nil {
					v := *hb
					hbCopy = &v
				}
				mergeWITargetFreshness(meta, !seen, hbCopy, stale, staleReason)
				if !b.IsFlat() && t.Profile != "" {
					var profHB *time.Time
					if hb != nil {
						v := *hb
						profHB = &v
					}
					profileTargets[b.ID+"\x00"+t.Profile] = storedProfileWITarget{
						pool: t.Pool, namespace: t.Namespace,
						lastHeartbeat: profHB, stale: stale, staleReason: staleReason,
					}
				}
			}
			cp.WorkloadIdentityTargets = clean
		}
		sanitized = append(sanitized, &cp)
	}

	plan := workloadIdentityMintPlanFor(sanitized)
	ksa := gcpAccountID(sa.Email)

	if plan.empty() {
		for i := range mappings {
			mappings[i].WorkloadIdentityBinding = &GCPServiceAccountProfileBinding{
				State:  GCPSABindingUnknown,
				Reason: api.WorkloadIdentityReasonNoPool,
			}
		}
		return GCPServiceAccountBinding{
			State:  GCPSABindingUnknown,
			Reason: "no Kubernetes targets in this project",
		}
	}

	if len(plan.targets) == 0 {
		for i := range mappings {
			mappings[i].WorkloadIdentityBinding = &GCPServiceAccountProfileBinding{
				State:  GCPSABindingUnknown,
				Reason: api.WorkloadIdentityReasonNoPool,
			}
		}
		targets := make([]GCPServiceAccountBindingTarget, 0, len(plan.missing))
		for _, m := range plan.missing {
			targets = append(targets, GCPServiceAccountBindingTarget{
				KSA:      ksa,
				Profiles: m.profiles,
				State:    GCPSABindingUnknown,
				Reason:   api.WorkloadIdentityReasonNoPool,
			})
		}
		return GCPServiceAccountBinding{
			State:   GCPSABindingUnknown,
			Reason:  "no Workload Identity pool reported",
			Targets: targets,
		}
	}

	if s.gcpIAMAdmin == nil {
		const reason = "IAM policy client not configured"
		setProfileBindingsUnknown(mappings, profileTargets, reason)
		return GCPServiceAccountBinding{
			State:  GCPSABindingUnknown,
			Reason: reason,
		}
	}

	members, err := s.cachedWorkloadIdentityMembers(ctx, sa.Email)
	if err != nil {
		slog.WarnContext(ctx, "GCP SA status: reading IAM policy failed", "sa_email", sa.Email, "error", err)
		reason := "IAM policy read failed"
		if workloadIdentityErrorReason(err) == api.WorkloadIdentityReasonPermissionDenied {
			reason = "permission denied reading IAM policy"
		}
		setProfileBindingsUnknown(mappings, profileTargets, reason)
		return GCPServiceAccountBinding{
			State:  GCPSABindingUnknown,
			Reason: reason,
		}
	}

	targets := make([]GCPServiceAccountBindingTarget, 0, len(plan.targets)+len(plan.missing))
	boundCount := 0
	for _, t := range plan.targets {
		member := workloadIdentityMember(t.pool, t.namespace, ksa)
		entry := GCPServiceAccountBindingTarget{
			Pool:      t.pool,
			Namespace: t.namespace,
			KSA:       ksa,
			Member:    member,
			Profiles:  t.profiles,
			State:     GCPSABindingNotBound,
		}
		if members[member] {
			entry.State = GCPSABindingBound
			boundCount++
		} else {
			entry.ManualStep = workloadIdentityManualStep(sa.Email, member)
		}
		if meta := targetMeta[t.pool+"\x00"+t.namespace]; meta != nil {
			entry.LastHeartbeat = meta.LastHeartbeat
			entry.Stale = meta.Stale
			entry.StaleReason = meta.StaleReason
		}
		targets = append(targets, entry)
	}
	for _, m := range plan.missing {
		targets = append(targets, GCPServiceAccountBindingTarget{
			KSA:      ksa,
			Profiles: m.profiles,
			State:    GCPSABindingUnknown,
			Reason:   api.WorkloadIdentityReasonNoPool,
		})
	}

	for i := range mappings {
		pt, ok := profileTargets[mappings[i].BrokerID+"\x00"+mappings[i].Profile]
		if !ok {
			mappings[i].WorkloadIdentityBinding = &GCPServiceAccountProfileBinding{
				State:  GCPSABindingUnknown,
				Reason: api.WorkloadIdentityReasonNoPool,
			}
			continue
		}
		profKSA := ksa
		profNS := pt.namespace
		if mappings[i].State == GCPSAMappingMapped {
			if mappings[i].KubernetesServiceAccount != "" {
				profKSA = mappings[i].KubernetesServiceAccount
			}
			if mappings[i].Namespace != "" && len(validation.IsDNS1123Label(mappings[i].Namespace)) == 0 {
				profNS = mappings[i].Namespace
			}
		}
		profMember := workloadIdentityMember(pt.pool, profNS, profKSA)
		profState := GCPSABindingNotBound
		if members[profMember] {
			profState = GCPSABindingBound
		}
		mappings[i].WorkloadIdentityBinding = &GCPServiceAccountProfileBinding{
			State:       profState,
			Pool:        pt.pool,
			Member:      profMember,
			Stale:       pt.stale,
			StaleReason: pt.staleReason,
		}
	}

	missingProfiles := countMissingTargetProfiles(plan.missing)
	state := GCPSABindingNotBound
	var reasons []string
	if boundCount == len(plan.targets) {
		state = GCPSABindingBound
	} else if boundCount > 0 {
		reasons = append(reasons, fmt.Sprintf("%d of %d targets bound", boundCount, len(plan.targets)))
	}
	if missingProfiles > 0 {
		if missingProfiles == 1 {
			reasons = append(reasons, "1 Kubernetes target reports no pool")
		} else {
			reasons = append(reasons, fmt.Sprintf("%d Kubernetes targets report no pool", missingProfiles))
		}
	}

	return GCPServiceAccountBinding{
		State:   state,
		Reason:  strings.Join(reasons, "; "),
		Targets: targets,
	}
}

func setProfileBindingsUnknown(mappings []GCPServiceAccountProfileMapping, profileTargets map[string]storedProfileWITarget, reason string) {
	for i := range mappings {
		pt, ok := profileTargets[mappings[i].BrokerID+"\x00"+mappings[i].Profile]
		if !ok {
			mappings[i].WorkloadIdentityBinding = &GCPServiceAccountProfileBinding{
				State:  GCPSABindingUnknown,
				Reason: api.WorkloadIdentityReasonNoPool,
			}
			continue
		}
		mappings[i].WorkloadIdentityBinding = &GCPServiceAccountProfileBinding{
			State:       GCPSABindingUnknown,
			Reason:      reason,
			Pool:        pt.pool,
			Stale:       pt.stale,
			StaleReason: pt.staleReason,
		}
	}
}

func countMissingTargetProfiles(missing []workloadIdentityMissing) int {
	n := 0
	for _, m := range missing {
		n += len(m.profiles)
	}
	return n
}
