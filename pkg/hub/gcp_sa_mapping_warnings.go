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
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Early warning for unmapped GCP service accounts (ptone/scion#3329 phase 2).
//
// GCP identity mode "assign" on the Kubernetes runtime needs an explicit
// kubernetes_service_account_mappings entry for the service account on the
// broker; without one the agent's start fails with a 400. These helpers let
// SA registration, the project SA list, and scion doctor say so up front.
// They only ever produce warnings: nothing here fails a request.

// loadEmbeddedBrokerMappingSettings loads the settings the embedded
// (co-located) broker resolves its mappings from at dispatch: the global
// settings plus the DB-backed overlay. The embedded broker runs in this
// process, so the Hub reads them live instead of using registration data.
// A variable so tests can substitute settings.
var loadEmbeddedBrokerMappingSettings = func() (*config.VersionedSettings, error) {
	vs, _, err := config.LoadGlobalSettingsWithOverlay()
	return vs, err
}

// projectSAMappingView is what a set of brokers (a project's providers, or
// every broker on the hub) reports about their Kubernetes profiles' GSA
// mappings.
type projectSAMappingView struct {
	// where names the broker set in warnings: "this project" or "this hub".
	where string
	// profiles are the Kubernetes profile rows of the broker set. Each
	// account is decided on them when warnings are asked for
	// (kubernetesProfileMappings.decision), so the warnings apply the same
	// rule as the status view and the dispatch refusal.
	profiles []kubernetesProfileMappings
}

// isKubernetesBrokerProfile reports whether a stored profile p is a
// Kubernetes profile for this check: its runtime key names Kubernetes
// (isKubernetesRuntimeType), or it reported mappings. Brokers report
// mappings only for Kubernetes profiles (by resolved runtime type), so a
// custom runtime key such as "gke" with type kubernetes counts once its
// broker reports.
func isKubernetesBrokerProfile(p store.BrokerProfile) bool {
	return isKubernetesRuntimeType(p.Type) || p.MappingsReported || len(p.ServiceAccountMappings) > 0
}

// localOnlyProfileTypes are runtime keys that are never Kubernetes. The
// embedded broker's live settings are not loaded for profiles keyed by them.
var localOnlyProfileTypes = map[string]bool{"docker": true, "podman": true, "container": true}

// kubernetesProfileMappings is what one Kubernetes broker profile of a
// project reports about its GSA mappings. It is the per-profile row the
// per-account status view shows; projectSAMappings aggregates the same rows
// for the warnings.
type kubernetesProfileMappings struct {
	brokerID   string
	brokerName string
	profile    string
	// reported is false when the profile's mappings are unknown (an older
	// broker, or a broker that could not read its settings).
	reported bool
	// entries holds the mapping details per GSA (lowercased key) that the
	// status view shows for a mapped account: the Kubernetes
	// ServiceAccount, namespace and source when the broker sent them
	// (ptone/scion#3329 phase 4). A mapped GSA may have no entry; whether a
	// GSA is mapped is the decision's (decision), not this map's.
	entries map[string]store.BrokerProfileSAMapping
	// report is the stored profile the mapping decision is made on
	// (profileSAMappingDecisionOn): its type, completeness, version, report
	// time, ambiguity and entries. For the embedded broker's live settings
	// its entries are the stored discovered ones only: the live settings,
	// not the stored report, decide explicit mappings (liveExplicit).
	report store.BrokerProfile
	// liveExplicit holds the GSAs, lowercased, that the embedded broker's
	// live settings map explicitly. Nil for every other row.
	liveExplicit map[string]bool
}

// decision decides account on the row with the dispatch refusal's rule
// (profileSAMappingDecisionOn) on its stored report. On the embedded
// broker, an explicit mapping in the live settings is mapped whatever the
// stored report says: the broker resolves explicit mappings from those
// settings at dispatch, and the stored report catches up on its next
// heartbeat.
//
// The live settings and the stored report can briefly disagree, and the
// view and warnings then differ from the dispatch refusal, which reads the
// stored report only, until that heartbeat:
//
//   - a mapping added to the live settings is mapped here while the refusal
//     can still refuse it;
//   - an explicit mapping dropped from the live settings is decided here
//     without the stored explicit entry (so not_mapped on a current report)
//     while the refusal still reads that entry as mapped and leaves the
//     dispatch to the broker, which refuses it. This is the safe direction:
//     the hub never refuses a dispatch the broker would accept.
func (p kubernetesProfileMappings) decision(account string, now time.Time) SAMappingDecision {
	if key := strings.ToLower(strings.TrimSpace(account)); key != "" && p.liveExplicit[key] {
		return SAMappingDecision{
			State: SAMappingStateMapped, Account: key, Profile: p.profile, Broker: p.brokerName,
			Namespace: p.entries[key].Namespace,
		}
	}
	return profileSAMappingDecisionOn(p.brokerName, &p.report, account, now)
}

// label names the profile as "broker/profile".
func (p kubernetesProfileMappings) label() string {
	return p.brokerName + "/" + p.profile
}

// projectKubernetesProfileMappings collects the Kubernetes profiles of every
// provider broker of projectID with their mappings, in provider then profile
// order. Errors reading providers or brokers are logged and skipped: the
// result feeds warnings and a read-only view only.
func (s *Server) projectKubernetesProfileMappings(ctx context.Context, projectID string) []kubernetesProfileMappings {
	providers, err := s.store.GetProjectProviders(ctx, projectID)
	if err != nil {
		slog.Debug("SA mapping warning: listing project providers failed", "project_id", projectID, "error", err)
		return nil
	}
	brokers := make([]*store.RuntimeBroker, 0, len(providers))
	for _, provider := range providers {
		broker, err := s.store.GetRuntimeBroker(ctx, provider.BrokerID)
		if err != nil || broker == nil {
			slog.Debug("SA mapping warning: provider broker unavailable", "project_id", projectID, "broker", provider.BrokerID, "error", err)
			continue
		}
		brokers = append(brokers, broker)
	}
	return s.brokerKubernetesProfileMappings(brokers)
}

// brokerKubernetesProfileMappings collects the Kubernetes profiles of brokers
// with their mappings, in broker then profile order. It serves both the
// project view (a project's providers) and the hub view (every broker).
//
// A broker with no profiles contributes nothing. This includes flat
// Runtime Broker rows, which never persist profiles (see the flat Runtime
// Brokers contract), so they are never read as "nothing mapped".
func (s *Server) brokerKubernetesProfileMappings(brokers []*store.RuntimeBroker) []kubernetesProfileMappings {
	var out []kubernetesProfileMappings
	embeddedID := s.embeddedBrokerSnapshot().id
	for _, broker := range brokers {
		embedded := embeddedID != "" && broker.ID == embeddedID
		// The embedded broker's live settings, loaded at most once and only
		// when a profile that could be Kubernetes is reached.
		var live *config.VersionedSettings
		liveLoaded, liveOK := false, false
		for _, p := range broker.Profiles {
			row := kubernetesProfileMappings{
				brokerID: broker.ID, brokerName: broker.Name, profile: p.Name,
				entries: map[string]store.BrokerProfileSAMapping{},
			}
			if embedded && !localOnlyProfileTypes[p.Type] {
				if !liveLoaded {
					liveLoaded = true
					if vs, err := loadEmbeddedBrokerMappingSettings(); err != nil {
						slog.Debug("SA mapping warning: loading embedded broker settings failed", "broker", broker.ID, "error", err)
					} else {
						live, liveOK = vs, true
					}
				}
				if liveOK {
					// The profile's own runtime entry and resolved type, from
					// the same settings the broker resolves at dispatch.
					gsas, isKubernetes, known := live.ProfileKubernetesSAMappings(p.Name)
					if known {
						if isKubernetes {
							stored := storedProfileSAEntries(p)
							// The live settings hold the explicit mappings only.
							// Discovery, completeness and the report time come
							// from the broker's stored report.
							row.report = p
							row.report.ServiceAccountMappings = nil
							for _, e := range p.ServiceAccountMappings {
								if e.Source == api.BrokerKSASourceDiscovered {
									row.report.ServiceAccountMappings = append(row.report.ServiceAccountMappings, e)
								}
							}
							for key, e := range stored {
								if e.Source == api.BrokerKSASourceDiscovered {
									row.entries[key] = e
								}
							}
							row.liveExplicit = map[string]bool{}
							for _, gsa := range gsas {
								// Already lowercase (KubernetesServiceAccountMappingGSAs
								// skips other keys); normalized here as on the record path.
								key := strings.ToLower(gsa)
								row.liveExplicit[key] = true
								// The live settings name only the GSA. The
								// KSA, namespace and source come from the
								// stored report only when it maps the same
								// GSA explicitly too: a stored discovered
								// entry predates the explicit mapping and
								// would show a KSA the broker no longer uses.
								if e, ok := stored[key]; ok && e.Source == api.BrokerKSASourceMapped {
									row.entries[key] = e
								} else {
									delete(row.entries, key)
								}
							}
							row.reported = true
							out = append(out, row)
						}
						continue
					}
					// Not in the live settings (the synthetic "default"
					// profile): fall back to the stored record below.
				}
			}
			if !isKubernetesBrokerProfile(p) {
				continue
			}
			row.report = p
			if p.MappingsReported {
				for key, e := range storedProfileSAEntries(p) {
					row.entries[key] = e
				}
				row.reported = true
			}
			out = append(out, row)
		}
	}
	return out
}

// storedProfileSAEntries indexes a profile's stored mappings by lowercased
// GSA. Nil when the profile has not reported.
func storedProfileSAEntries(p store.BrokerProfile) map[string]store.BrokerProfileSAMapping {
	if !p.MappingsReported || len(p.ServiceAccountMappings) == 0 {
		return nil
	}
	out := make(map[string]store.BrokerProfileSAMapping, len(p.ServiceAccountMappings))
	for _, m := range p.ServiceAccountMappings {
		out[strings.ToLower(m.GSA)] = m
	}
	return out
}

// projectSAMappings aggregates projectKubernetesProfileMappings for the
// warnings.
func (s *Server) projectSAMappings(ctx context.Context, projectID string) projectSAMappingView {
	return projectSAMappingViewFrom(s.projectKubernetesProfileMappings(ctx, projectID))
}

// projectSAMappingViewFrom aggregates a project's per-profile rows into the
// union view.
func projectSAMappingViewFrom(profiles []kubernetesProfileMappings) projectSAMappingView {
	return mappingViewFrom("this project", profiles)
}

// mappingViewFrom wraps per-profile rows in a view; where names the broker
// set in warnings.
func mappingViewFrom(where string, profiles []kubernetesProfileMappings) projectSAMappingView {
	return projectSAMappingView{where: where, profiles: profiles}
}

// hubSAMappingBrokerPageSize bounds each page of the broker listing behind
// hubSAMappings.
const hubSAMappingBrokerPageSize = 200

// hubSAMappingCacheTTL is how long a hub-wide mapping view is reused. The
// view feeds advisory warnings only, and brokers report mapping changes on
// a heartbeat measured in minutes, so a few seconds of reuse loses nothing
// a caller could act on while sparing repeated list, register and verify
// calls a walk over every broker each.
const hubSAMappingCacheTTL = 30 * time.Second

// hubSAMappingViewCache is the Server's single cached hub-wide view. mu
// guards the fields only; it is never held across the broker walk.
type hubSAMappingViewCache struct {
	mu      sync.Mutex
	view    projectSAMappingView
	loaded  bool // view holds a successfully loaded view
	expires time.Time
}

// hubSAMappingNow is the clock for the cache; a variable so tests can move it.
var hubSAMappingNow = time.Now

// hubSAMappings returns the Kubernetes profile mappings of every broker on
// the hub. A hub-scoped account is assignable from every project, so the
// question for it is whether any broker maps it, not one project's
// providers; the hub-scope routes have no project to narrow the brokers to.
//
// The view is cached on the Server for hubSAMappingCacheTTL, so the brokers
// are walked about once per window however many hub-scope requests arrive,
// and a request computes it at most once (hubSAMappingWarnings reads it
// once for all its accounts). Only the hub-scope routes (list, register,
// mint, verify-by-id) read it, and only when the response holds a
// hub-scoped account.
//
// The cache lock is released during the walk, so a slow store does not
// serialize concurrent requests behind it; requests that miss together may
// each walk, which is harmless for an advisory view. A failed listing is
// not cached: the last good view answers instead, or the partial view when
// none has loaded yet.
func (s *Server) hubSAMappings(ctx context.Context) projectSAMappingView {
	c := &s.hubSAMappingCache
	c.mu.Lock()
	if hubSAMappingNow().Before(c.expires) {
		view := c.view
		c.mu.Unlock()
		return view
	}
	c.mu.Unlock()

	view, ok := s.loadHubSAMappings(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	if ok {
		c.view, c.loaded, c.expires = view, true, hubSAMappingNow().Add(hubSAMappingCacheTTL)
		return view
	}
	if c.loaded {
		return c.view
	}
	return view
}

// loadHubSAMappings walks every broker on the hub. ok is false when a
// listing page failed; the partial view is still returned for this call.
func (s *Server) loadHubSAMappings(ctx context.Context) (projectSAMappingView, bool) {
	var brokers []*store.RuntimeBroker
	ok := true
	cursor := ""
	for {
		page, err := s.store.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{},
			store.ListOptions{Limit: hubSAMappingBrokerPageSize, Cursor: cursor, SkipTotalCount: true})
		if err != nil {
			slog.Debug("SA mapping warning: listing hub brokers failed", "error", err)
			ok = false
			break
		}
		for i := range page.Items {
			brokers = append(brokers, &page.Items[i])
		}
		if page.NextCursor == "" || len(page.Items) == 0 {
			break
		}
		cursor = page.NextCursor
	}
	return mappingViewFrom("this hub", s.brokerKubernetesProfileMappings(brokers)), ok
}

// saMappingCheck is the outcome of deciding one account on every profile
// of a view.
type saMappingCheck struct {
	// mapped is true when any profile's decision is mapped.
	mapped bool
	// notMapped names the profiles whose decision is not_mapped, as
	// "broker/profile", sorted.
	notMapped []string
	// unknown counts the profiles whose decision is unknown (no current,
	// complete report, an unrecognised runtime, or an ambiguous mapping).
	unknown int
}

// check decides gsaEmail on every profile of the view at now.
func (v projectSAMappingView) check(gsaEmail string, now time.Time) saMappingCheck {
	var c saMappingCheck
	for _, p := range v.profiles {
		switch p.decision(gsaEmail, now).State {
		case SAMappingStateMapped:
			c.mapped = true
		case SAMappingStateNotMapped:
			c.notMapped = append(c.notMapped, p.label())
		case SAMappingStateUnknown:
			c.unknown++
		}
		// not_required: the profile needs no mapping and never warns.
	}
	sort.Strings(c.notMapped)
	return c
}

// warns reports whether a warning applies: no profile maps the account and
// at least one profile's decision is not_mapped. A profile whose decision
// is unknown never makes a warning on its own.
func (c saMappingCheck) warns() bool {
	return !c.mapped && len(c.notMapped) > 0
}

// context is the parenthesised list of the profiles checked, and the count
// of profiles that could not be checked.
func (c saMappingCheck) context() string {
	msg := fmt.Sprintf(" (profiles checked: %s", strings.Join(c.notMapped, ", "))
	if c.unknown > 0 {
		msg += fmt.Sprintf("; %d other Kubernetes profile(s) could not be checked", c.unknown)
	}
	return msg + ")"
}

// warningFor returns the warning text for an unmapped gsaEmail, without
// the context (saMappingCheck.context).
func (v projectSAMappingView) warningFor(gsaEmail string) string {
	return fmt.Sprintf(
		"GCP service account %s is not mapped to a Kubernetes ServiceAccount on any checked Kubernetes broker profile of %s; "+
			"agents assigned it on those profiles fail to start until a broker operator adds it to kubernetes_service_account_mappings",
		strings.ToLower(gsaEmail), v.where)
}

// projectSAMappingWarnings returns one warning per service account in sas
// that projectID's agents can be assigned -- its own project-scoped accounts
// and every hub-scoped one -- that no Kubernetes broker profile of the
// project maps and at least one has decided not_mapped (saMappingCheck.warns).
// The first warning also lists the profiles checked, so the set is not
// repeated per account. Nil when there is nothing to warn about.
func (s *Server) projectSAMappingWarnings(ctx context.Context, projectID string, sas ...*store.GCPServiceAccount) []string {
	return projectSAMappingWarningsFrom(projectID, func() projectSAMappingView { return s.projectSAMappings(ctx, projectID) }, sas...)
}

// projectSAMappingWarningsFrom is projectSAMappingWarnings with the view
// supplied by the caller, so a handler that already read the profiles does
// not read them again. view is called only when an account needs checking.
func projectSAMappingWarningsFrom(projectID string, view func() projectSAMappingView, sas ...*store.GCPServiceAccount) []string {
	var candidates []*store.GCPServiceAccount
	for _, sa := range sas {
		if sa == nil {
			continue
		}
		if (sa.Scope == store.ScopeProject && sa.ScopeID == projectID) || sa.Scope == store.ScopeHub {
			candidates = append(candidates, sa)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	return view().warnings(candidates)
}

// hubSAMappingWarnings returns one warning per hub-scoped service account in
// sas that no Kubernetes broker profile on the hub maps and at least one has
// decided not_mapped (saMappingCheck.warns), for the hub-scope
// routes, which have no project to narrow the brokers to. Project-scoped
// accounts are skipped. Nil when there is nothing to warn about.
func (s *Server) hubSAMappingWarnings(ctx context.Context, sas ...*store.GCPServiceAccount) []string {
	var candidates []*store.GCPServiceAccount
	for _, sa := range sas {
		if sa != nil && sa.Scope == store.ScopeHub {
			candidates = append(candidates, sa)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	return s.hubSAMappings(ctx).warnings(candidates)
}

// warnings returns the warning for each account in sas whose check warns
// (saMappingCheck.warns). The profiles checked are added to the first
// warning, and to a later one only when they differ from the last listed.
func (v projectSAMappingView) warnings(sas []*store.GCPServiceAccount) []string {
	return v.warningsAt(sas, time.Now())
}

// warningsAt is warnings with the decisions made at now.
func (v projectSAMappingView) warningsAt(sas []*store.GCPServiceAccount, now time.Time) []string {
	var out []string
	lastContext := ""
	for _, sa := range sas {
		c := v.check(sa.Email, now)
		if !c.warns() {
			continue
		}
		msg := v.warningFor(sa.Email)
		if ctx := c.context(); ctx != lastContext {
			msg += ctx
			lastContext = ctx
		}
		out = append(out, msg)
	}
	return out
}

// verificationWarnings is the mapping warning for the verify routes: the
// project's when a project route verified sa, and for a hub-scoped account
// verified through the parentless route (projectID ""), the hub's.
func (s *Server) verificationWarnings(ctx context.Context, projectID string, sa *store.GCPServiceAccount) []string {
	if projectID == "" {
		return s.hubSAMappingWarnings(ctx, sa)
	}
	return s.projectSAMappingWarnings(ctx, projectID, sa)
}
