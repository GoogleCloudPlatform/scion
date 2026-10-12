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
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Save-time warnings for per-profile default GCP service accounts
// (ptone/scion#4630).
//
// A per-profile default is accepted for any well-formed profile name and
// any reachable, verified account, because a broker may be offline when the
// setting is saved. A default for a profile no provider broker reports, or
// for an account that profile does not map, is then inert or fails at agent
// create time. The settings PUT still saves it, and returns a warning naming
// the problem so the user learns about it at save time. Warnings never fail
// the request, and an unknown state never warns.

// projectSettingsWithWarnings is a project settings response plus advisory
// warnings. Embedding keeps the JSON identical to a bare
// hubclient.ProjectSettings when there are no warnings, so clients that
// decode the settings fields are unaffected.
type projectSettingsWithWarnings struct {
	*hubclient.ProjectSettings
	Warnings []string `json:"warnings,omitempty"`
}

// profileDefaultSAWarnings returns the warnings for the per-profile default
// entries in changed (profile name to service account ID; the entries a
// PUT added or changed), checked against the provider brokers of projectID.
// For each entry, in profile order:
//
//   - When no provider broker reports the profile, it warns that the
//     profile is unreported. This is skipped when the provider brokers
//     cannot all be read, or when a provider has no stored profile list
//     (such a broker's profiles are unknown, not absent).
//   - Otherwise, when no provider profile of that name maps the account
//     and at least one decides not_mapped (profileSAMappingDecisionOn, the
//     rule behind the dispatch refusal), it warns that the account is not
//     mapped. unknown and not_required decisions never warn.
//
// Nil when there is nothing to warn about.
func (s *Server) profileDefaultSAWarnings(ctx context.Context, projectID string, changed map[string]string) []string {
	if len(changed) == 0 {
		return nil
	}
	brokers, complete := s.projectProviderBrokers(ctx, projectID)
	rows := s.brokerKubernetesProfileMappings(brokers)
	now := time.Now()

	var out []string
	for _, profile := range slices.Sorted(maps.Keys(changed)) {
		reported, known := profileReportedBy(brokers, complete, profile)
		if !reported {
			if known {
				out = append(out, fmt.Sprintf(
					"defaultGCPIdentityServiceAccountIDByProfile[%s]: no broker providing this project reports profile %q; "+
						"the default applies only to agents that run under that profile",
					profile, profile))
			}
			// An unreported profile has no rows to check a mapping on.
			continue
		}

		sa, err := s.store.GetGCPServiceAccount(ctx, changed[profile])
		if err != nil || sa == nil {
			// Validated just before the save; a failed read here only
			// loses the warning.
			slog.Debug("per-profile default warning: reading service account failed",
				"project_id", projectID, "profile", profile, "error", err)
			continue
		}
		var profileRows []kubernetesProfileMappings
		for _, row := range rows {
			if row.profile == profile {
				profileRows = append(profileRows, row)
			}
		}
		c := projectSAMappingViewFrom(profileRows).check(sa.Email, now)
		if !c.warns() {
			continue
		}
		out = append(out, fmt.Sprintf(
			"defaultGCPIdentityServiceAccountIDByProfile[%s]: GCP service account %s is not mapped to a Kubernetes ServiceAccount on profile %q (checked: %s); "+
				"agents that get this default on that profile fail to start until a broker operator adds it to kubernetes_service_account_mappings",
			profile, strings.ToLower(sa.Email), profile, strings.Join(c.notMapped, ", ")))
	}
	return out
}

// profileReportedBy reports whether any of brokers has a stored profile
// named profile. known is false when the answer "not reported" cannot be
// trusted: the broker list is incomplete, or a broker has no stored profile
// list (flat Runtime Broker rows never persist profiles, so the profiles
// such a broker serves are unknown).
func profileReportedBy(brokers []*store.RuntimeBroker, complete bool, profile string) (reported, known bool) {
	known = complete
	for _, b := range brokers {
		if len(b.Profiles) == 0 {
			known = false
			continue
		}
		for _, p := range b.Profiles {
			if p.Name == profile {
				return true, true
			}
		}
	}
	return false, known
}
