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
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mutationTypeProjectSettingsLimitUnlimited is the audit event a project
// settings PUT records when it changes a default count limit to 0
// (Unlimited), so an accidental change can be found and reverted
// (ptone/scion#3925). A tab still running a page from before Unlimited
// existed sends 0 for an empty count limit on every save.
const mutationTypeProjectSettingsLimitUnlimited = "project_settings_limit_unlimited"

// projectCountLimitField is one project default count limit as the
// settings PUT names it, read from stored and merged settings.
type projectCountLimitField struct {
	name string
	get  func(*hubclient.ProjectSettings) *int
}

var projectCountLimitFields = []projectCountLimitField{
	{projectSettingsFieldMaxTurns, func(p *hubclient.ProjectSettings) *int { return p.DefaultMaxTurns }},
	{projectSettingsFieldMaxModelCalls, func(p *hubclient.ProjectSettings) *int { return p.DefaultMaxModelCalls }},
}

// recordProjectUnlimitedLimits records each default count limit a project
// settings PUT changed to 0 (Unlimited): an audit event for every such
// change, and also a warning in the log when the field had no value before
// (the change a stale settings page makes). It returns the names of the
// fields changed to 0, sorted, for projectUnlimitedBrokerWarnings. A field
// that already held 0 is no change and is not recorded.
func (s *Server) recordProjectUnlimitedLimits(ctx context.Context, project *store.Project, stored, merged *hubclient.ProjectSettings) []string {
	var changed []string
	for _, f := range projectCountLimitFields {
		before, after := f.get(stored), f.get(merged)
		if after == nil || *after != 0 || (before != nil && *before == 0) {
			continue
		}
		changed = append(changed, f.name)
		from := "unset"
		if before != nil {
			from = fmt.Sprint(*before)
		} else {
			slog.WarnContext(ctx, "project settings: a default count limit changed from unset to Unlimited (0); a page loaded before the upgrade sends 0 for an empty field",
				"project_id", project.ID, "field", f.name)
		}
		summary, err := json.Marshal(struct {
			Field string `json:"field"`
			From  string `json:"from"`
			To    string `json:"to"`
		}{Field: f.name, From: from, To: "0"})
		if err != nil {
			summary = nil
		}
		s.emitMutationAudit(ctx, &store.MutationAuditRecord{
			MutationType: mutationTypeProjectSettingsLimitUnlimited,
			TargetType:   "project",
			TargetID:     project.ID,
			AfterSummary: string(summary),
		})
	}
	return changed
}

// projectUnlimitedBrokerWarnings returns the PUT response warnings for
// fields, the default count limits the PUT changed to 0: when a Runtime
// Broker providing the project does not advertise unlimitedCountLimits, the
// agents it runs keep the inherited limit (the hub stamps 0 at create, but
// such a broker treats it as unset). It is best effort: a provider lookup
// that fails gives no warning and never fails the save.
func (s *Server) projectUnlimitedBrokerWarnings(ctx context.Context, projectID string, fields []string) []string {
	if len(fields) == 0 {
		return nil
	}
	brokers, _ := s.projectProviderBrokers(ctx, projectID)
	old := brokersWithoutUnlimitedCountLimits(brokers)
	if len(old) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s: Unlimited (0) applies only on Runtime Brokers that support it; agents on %s keep the inherited limit",
		strings.Join(fields, ", "), strings.Join(old, ", "))}
}

// brokersWithoutUnlimitedCountLimits returns the names (the ID when a
// broker has no name), sorted, of the brokers that do not advertise
// unlimitedCountLimits. A nil entry is skipped: projectProviderBrokers
// never returns one today, but the guard keeps a future caller from
// panicking on it.
func brokersWithoutUnlimitedCountLimits(brokers []*store.RuntimeBroker) []string {
	var out []string
	for _, b := range brokers {
		if b == nil {
			continue
		}
		if b.Capabilities == nil || !b.Capabilities.UnlimitedCountLimits {
			name := b.Name
			if name == "" {
				name = b.ID
			}
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
