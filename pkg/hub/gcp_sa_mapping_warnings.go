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

// projectSAMappingView is what the project's providers report about their
// Kubernetes profiles' GSA mappings.
type projectSAMappingView struct {
	// mapped is the union of GSAs mapped on reported Kubernetes profiles.
	mapped map[string]bool
	// reported names the Kubernetes profiles whose mappings are known, as
	// "broker/profile", sorted.
	reported []string
	// unreported counts Kubernetes profiles whose mappings are unknown (an
	// older broker, or a broker that could not read its settings).
	unreported int
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
	// gsas holds the mapped GSAs, lowercased. Empty when not reported.
	gsas map[string]bool
	// entries holds the reported mapping per GSA (lowercased key), with the
	// Kubernetes ServiceAccount, namespace and source when the broker sent
	// them (ptone/scion#3329 phase 4). A GSA in gsas may have no entry.
	entries map[string]store.BrokerProfileSAMapping
	// storedReport is true when the broker's stored report backs the row's
	// completeness, version and report time. False for the embedded
	// broker's live settings without a stored report.
	storedReport bool
	// complete is the stored report's MappingsComplete: it lists every GSA
	// the profile can serve.
	complete bool
	// version is the stored report's MappingsReportVersion; zero from a
	// broker that predates api.BrokerSAReportVersion.
	version int
	// incompleteReason is the broker's code for why the report may not list
	// every usable GSA. Empty when complete, and for a report from a broker
	// that predates completeness reporting (which reads as before).
	incompleteReason string
	// ambiguous holds the GSAs, lowercased, that more than one
	// ServiceAccount in the namespace is annotated with.
	ambiguous map[string]bool
	// reportedAt is when the hub last stored or confirmed the report. Nil
	// when never reported.
	reportedAt *time.Time
}

// label names the profile as "broker/profile".
func (p kubernetesProfileMappings) label() string {
	return p.brokerName + "/" + p.profile
}

// projectKubernetesProfileMappings collects the Kubernetes profiles of every
// provider broker of projectID with their mappings, in provider then profile
// order. Errors reading providers or brokers are logged and skipped: the
// result feeds warnings and a read-only view only.
//
// A broker with no profiles contributes nothing. This includes flat
// Runtime Broker rows, which never persist profiles (see the flat Runtime
// Brokers contract), so they are never read as "nothing mapped".
func (s *Server) projectKubernetesProfileMappings(ctx context.Context, projectID string) []kubernetesProfileMappings {
	providers, err := s.store.GetProjectProviders(ctx, projectID)
	if err != nil {
		slog.Debug("SA mapping warning: listing project providers failed", "project_id", projectID, "error", err)
		return nil
	}
	var out []kubernetesProfileMappings
	embeddedID := s.embeddedBrokerSnapshot().id
	for _, provider := range providers {
		broker, err := s.store.GetRuntimeBroker(ctx, provider.BrokerID)
		if err != nil || broker == nil {
			slog.Debug("SA mapping warning: provider broker unavailable", "project_id", projectID, "broker", provider.BrokerID, "error", err)
			continue
		}
		embedded := embeddedID != "" && broker.ID == embeddedID
		// The embedded broker's live settings, loaded at most once and only
		// when a profile that could be Kubernetes is reached.
		var live *config.VersionedSettings
		liveLoaded, liveOK := false, false
		for _, p := range broker.Profiles {
			row := kubernetesProfileMappings{
				brokerID: broker.ID, brokerName: broker.Name, profile: p.Name,
				gsas: map[string]bool{}, entries: map[string]store.BrokerProfileSAMapping{},
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
							row.setStoredReport(p)
							for key, e := range stored {
								if e.Source == api.BrokerKSASourceDiscovered {
									row.gsas[key] = true
									row.entries[key] = e
								}
							}
							for _, gsa := range gsas {
								// Already lowercase (KubernetesServiceAccountMappingGSAs
								// skips other keys); normalized here as on the record path.
								key := strings.ToLower(gsa)
								row.gsas[key] = true
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
			if p.MappingsReported {
				for key, e := range storedProfileSAEntries(p) {
					row.gsas[key] = true
					row.entries[key] = e
				}
				row.setStoredReport(p)
				row.reported = true
			}
			out = append(out, row)
		}
	}
	return out
}

// setStoredReport copies the stored report's completeness, ambiguity and
// report time onto the row. A profile that never reported leaves them unset.
func (p *kubernetesProfileMappings) setStoredReport(bp store.BrokerProfile) {
	if !bp.MappingsReported {
		return
	}
	p.storedReport = true
	p.complete = bp.MappingsComplete
	p.version = bp.MappingsReportVersion
	if !bp.MappingsComplete {
		p.incompleteReason = bp.MappingsIncompleteReason
	}
	for _, gsa := range bp.AmbiguousGSAs {
		if p.ambiguous == nil {
			p.ambiguous = map[string]bool{}
		}
		p.ambiguous[strings.ToLower(gsa)] = true
	}
	p.reportedAt = bp.MappingsReportedAt
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

// projectSAMappingViewFrom aggregates per-profile rows into the union view.
func projectSAMappingViewFrom(profiles []kubernetesProfileMappings) projectSAMappingView {
	view := projectSAMappingView{mapped: map[string]bool{}}
	for _, p := range profiles {
		if !p.reported {
			view.unreported++
			continue
		}
		for gsa := range p.gsas {
			view.mapped[gsa] = true
		}
		view.reported = append(view.reported, p.label())
	}
	sort.Strings(view.reported)
	return view
}

// unmapped reports whether a warning applies to gsaEmail: at least one
// Kubernetes profile reported its mappings and none maps it.
func (v projectSAMappingView) unmapped(gsaEmail string) bool {
	return len(v.reported) > 0 && !v.mapped[strings.ToLower(gsaEmail)]
}

// warningFor returns the warning text for an unmapped gsaEmail. withContext
// adds the profiles checked (and the count of unreported ones), which
// projectSAMappingWarnings includes only once per response.
func (v projectSAMappingView) warningFor(gsaEmail string, withContext bool) string {
	msg := fmt.Sprintf(
		"GCP service account %s is not mapped to a Kubernetes ServiceAccount on any Kubernetes broker profile of this project; "+
			"agents assigned it on those profiles fail to start until a broker operator adds it to kubernetes_service_account_mappings",
		strings.ToLower(gsaEmail))
	if withContext {
		msg += fmt.Sprintf(" (profiles checked: %s", strings.Join(v.reported, ", "))
		if v.unreported > 0 {
			msg += fmt.Sprintf("; %d Kubernetes profile(s) did not report their mappings", v.unreported)
		}
		msg += ")"
	}
	return msg
}

// projectSAMappingWarnings returns one warning per project-scoped service
// account in sas (of projectID) that no Kubernetes broker profile of the
// project maps. The first warning also lists the profiles checked, so the
// set is not repeated per account. Hub-scoped accounts get no warning. Nil
// when there is nothing to warn about.
func (s *Server) projectSAMappingWarnings(ctx context.Context, projectID string, sas ...*store.GCPServiceAccount) []string {
	return projectSAMappingWarningsFrom(projectID, func() projectSAMappingView { return s.projectSAMappings(ctx, projectID) }, sas...)
}

// projectSAMappingWarningsFrom is projectSAMappingWarnings with the view
// supplied by the caller, so a handler that already read the profiles does
// not read them again. view is called only when an account needs checking.
func projectSAMappingWarningsFrom(projectID string, view func() projectSAMappingView, sas ...*store.GCPServiceAccount) []string {
	var candidates []*store.GCPServiceAccount
	for _, sa := range sas {
		if sa != nil && sa.Scope == store.ScopeProject && sa.ScopeID == projectID {
			candidates = append(candidates, sa)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	v := view()
	var warnings []string
	for _, sa := range candidates {
		if v.unmapped(sa.Email) {
			warnings = append(warnings, v.warningFor(sa.Email, len(warnings) == 0))
		}
	}
	return warnings
}

// verificationWarnings is projectSAMappingWarnings for the verify routes:
// none when projectID is "" (the parentless route).
func (s *Server) verificationWarnings(ctx context.Context, projectID string, sa *store.GCPServiceAccount) []string {
	if projectID == "" {
		return nil
	}
	return s.projectSAMappingWarnings(ctx, projectID, sa)
}
