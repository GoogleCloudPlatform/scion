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

// projectSAMappings collects the Kubernetes profile mappings of every
// provider broker of projectID. Errors reading providers or brokers are
// logged and skipped: the result feeds warnings only.
//
// A broker with no profiles contributes nothing. This includes flat
// Runtime Broker rows, which never persist profiles (see the flat Runtime
// Brokers contract), so they are never read as "nothing mapped".
func (s *Server) projectSAMappings(ctx context.Context, projectID string) projectSAMappingView {
	view := projectSAMappingView{mapped: map[string]bool{}}
	providers, err := s.store.GetProjectProviders(ctx, projectID)
	if err != nil {
		slog.Debug("SA mapping warning: listing project providers failed", "project_id", projectID, "error", err)
		return view
	}
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
			label := broker.Name + "/" + p.Name
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
							for _, gsa := range gsas {
								// Already lowercase (KubernetesServiceAccountMappingGSAs
								// skips other keys); normalized here as on the record path.
								view.mapped[strings.ToLower(gsa)] = true
							}
							view.reported = append(view.reported, label)
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
				for _, m := range p.ServiceAccountMappings {
					view.mapped[strings.ToLower(m.GSA)] = true
				}
				view.reported = append(view.reported, label)
			} else {
				view.unreported++
			}
		}
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
	var candidates []*store.GCPServiceAccount
	for _, sa := range sas {
		if sa != nil && sa.Scope == store.ScopeProject && sa.ScopeID == projectID {
			candidates = append(candidates, sa)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	view := s.projectSAMappings(ctx, projectID)
	var warnings []string
	for _, sa := range candidates {
		if view.unmapped(sa.Email) {
			warnings = append(warnings, view.warningFor(sa.Email, len(warnings) == 0))
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
