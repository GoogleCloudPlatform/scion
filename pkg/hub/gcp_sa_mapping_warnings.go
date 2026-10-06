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

// isKubernetesBrokerProfile reports whether p is a Kubernetes profile for
// this check: its runtime key names Kubernetes (isKubernetesRuntimeType), or
// it reports GSA mappings, which only the Kubernetes runtime uses.
func isKubernetesBrokerProfile(p store.BrokerProfile) bool {
	return isKubernetesRuntimeType(p.Type) || len(p.ServiceAccountMappings) > 0
}

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
		var live *config.VersionedSettings
		liveKnown := false
		if embeddedID != "" && broker.ID == embeddedID {
			vs, err := loadEmbeddedBrokerMappingSettings()
			if err != nil {
				slog.Debug("SA mapping warning: loading embedded broker settings failed", "broker", broker.ID, "error", err)
			} else {
				live, liveKnown = vs, true
			}
		}
		for _, p := range broker.Profiles {
			if !isKubernetesBrokerProfile(p) {
				continue
			}
			label := broker.Name + "/" + p.Name
			switch {
			case liveKnown:
				// The profile's Type is its runtime entry key.
				for _, gsa := range live.KubernetesServiceAccountMappingGSAs(p.Name, p.Type) {
					view.mapped[gsa] = true
				}
				view.reported = append(view.reported, label)
			case p.MappingsReported:
				for _, m := range p.ServiceAccountMappings {
					view.mapped[strings.ToLower(m.GSA)] = true
				}
				view.reported = append(view.reported, label)
			default:
				view.unreported++
			}
		}
	}
	sort.Strings(view.reported)
	return view
}

// warningFor returns the warning for gsaEmail, or "" when no Kubernetes
// profile reported its mappings (nothing to compare against) or one maps it.
func (v projectSAMappingView) warningFor(gsaEmail string) string {
	if len(v.reported) == 0 {
		return ""
	}
	gsa := strings.ToLower(gsaEmail)
	if v.mapped[gsa] {
		return ""
	}
	msg := fmt.Sprintf(
		"GCP service account %s is not mapped to a Kubernetes ServiceAccount on any Kubernetes broker profile of this project (%s); "+
			"agents assigned it on those profiles fail to start until a broker operator adds it to kubernetes_service_account_mappings",
		gsa, strings.Join(v.reported, ", "))
	if v.unreported > 0 {
		msg += fmt.Sprintf(" (%d Kubernetes profile(s) did not report their mappings)", v.unreported)
	}
	return msg
}

// projectSAMappingWarnings returns one warning per project-scoped service
// account in sas (of projectID) that no Kubernetes broker profile of the
// project maps. Hub-scoped accounts get no warning. Nil when there is
// nothing to warn about.
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
		if w := view.warningFor(sa.Email); w != "" {
			warnings = append(warnings, w)
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
