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
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The health summary's status and attention policy. deriveHealthSummaryStatus
// is the only place that decides the overall status and the "Needs
// attention" list; the handler only gathers data. Every message is a fixed
// template filled with server-side identifiers and counts, never raw error
// text.

// agentErrorDegradedRatio is the share of considered agents in error or
// crashed at which the overall status turns degraded. A constant, not
// configuration. Compared with integers (see agentErrorRatioReached), so
// exactly 5% counts.
const agentErrorDegradedRatio = 0.05

// agentErrorRatioDenominator is 1 / agentErrorDegradedRatio, for the integer
// comparison errored * 20 >= considered.
const agentErrorRatioDenominator = 20

// Attention item severities.
const (
	HealthAttentionCritical = "critical"
	HealthAttentionWarning  = "warning"
)

// Attention item kinds.
const (
	HealthAttentionHubCheck       = "hub_check"
	HealthAttentionHubInstance    = "hub_instance"
	HealthAttentionBrokerOffline  = "broker_offline"
	HealthAttentionBrokerDegraded = "broker_degraded"
	HealthAttentionBrokerNFS      = "broker_nfs"
	HealthAttentionIntegration    = "integration"
	HealthAttentionDispatch       = "dispatch"
	HealthAttentionAgents         = "agents"
)

// Attention subject types.
const (
	HealthSubjectHub         = "hub"
	HealthSubjectBroker      = "runtime_broker"
	HealthSubjectIntegration = "integration"
	HealthSubjectDispatch    = "dispatch"
	HealthSubjectAgent       = "agent"
	HealthSubjectAgents      = "agents"
)

// HealthAttentionItem is one entry of the health summary's ranked "Needs
// attention" list.
type HealthAttentionItem struct {
	// Severity is critical or warning.
	Severity string `json:"severity"`
	// Kind is hub_check, hub_instance, broker_offline, broker_degraded,
	// broker_nfs, integration, dispatch or agents.
	Kind    string                 `json:"kind"`
	Subject HealthAttentionSubject `json:"subject"`
	// Message is a fixed, server-composed sentence.
	Message string `json:"message"`
}

// HealthAttentionSubject is what an attention item is about, so the
// dashboard can link to it. Fields that do not apply are omitted.
type HealthAttentionSubject struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

// deriveHealthSummaryStatus computes the overall status and the ranked
// attention list from an assembled summary. It is a pure function of resp.
//
// Status is the worst of:
//   - the fleet hub status (fleetHubStatus over the live hub instances);
//   - degraded when a runtime broker is not online, reports its own health
//     as degraded or unhealthy, or reports an unhealthy NFS workspace share;
//   - degraded when dispatch has stuck messages or stuck broker dispatches;
//   - degraded when a managed integration reports unhealthy;
//   - degraded when at least agentErrorDegradedRatio of the considered
//     agents are in error or crashed;
//   - degraded when the service account assignment check cannot run
//     (ServiceAccountCheck is set).
//
// A section that could not be read (hub instances, agents or dispatch
// null, broker list not reported) adds a warning item and does not change
// the status. A stale hub instance is outside the fleet rule: it adds a
// warning for hubInstanceStoppedReportingWindow after its last write and
// does not change the status. A stopped hub instance adds nothing.
// Stalled agents are never counted. Offline agents, and error or crashed
// agents below the ratio, add warning items only.
//
// Hub check items, one per non-healthy check per live hub instance, are
// critical only when the fleet status is unhealthy, otherwise warnings.
//
// Order: the fleet item ("N of M hub instances healthy", "No hub instance
// is reporting" or "Hub instance data not available"), hub check items
// (critical checks first), the service account assignment check item, then
// broker warnings, "stopped reporting" hub instance warnings, integration
// and dispatch warnings, then the agent error ratio item, then agent items
// (errored, crashed, offline).
func deriveHealthSummaryStatus(resp *HealthSummaryResponse) (string, []HealthAttentionItem) {
	status := HealthStatusHealthy
	degrade := func() { status = worseHealthStatus(status, HealthStatusDegraded) }
	items := []HealthAttentionItem{}

	// The fleet hub status counts only when the registry was read; a
	// failed read is "not reported" and changes nothing.
	if resp.HubInstances != nil && resp.Hub.Instances != nil {
		status = worseHealthStatus(status, fleetHubStatus(*resp.Hub.Instances))
	}
	items = append(items, hubFleetAttention(resp)...)

	// Service account assignment check. A hub-level condition, so it is a
	// hub_check item about this hub instance.
	if resp.ServiceAccountCheck != nil {
		degrade()
		items = append(items, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionHubCheck,
			Subject: HealthAttentionSubject{Type: HealthSubjectHub, ID: resp.Hub.InstanceID},
			Message: "Service account assignment check cannot run",
		})
	}

	// Runtime brokers.
	if resp.Brokers.NotReported {
		items = append(items, HealthAttentionItem{
			// The broker list could not be read from the store: a hub
			// problem, not a broker state, so it is a hub_check item
			// about this hub instance (it links nowhere broker-specific).
			Severity: HealthAttentionWarning, Kind: HealthAttentionHubCheck,
			Subject: HealthAttentionSubject{Type: HealthSubjectHub, ID: resp.Hub.InstanceID},
			Message: "Runtime broker data not available",
		})
	}
	for _, b := range resp.Brokers.Items {
		subject := HealthAttentionSubject{Type: HealthSubjectBroker, ID: b.ID, Name: b.Name}
		label := healthBrokerLabel(b)
		if healthSummaryBrokerStatusIsProblem(b.Status) {
			degrade()
			msg := "Runtime broker " + label + " is not online"
			if b.Status == store.BrokerStatusOffline {
				msg = "Runtime broker " + label + " is offline"
			}
			items = append(items, HealthAttentionItem{
				Severity: HealthAttentionWarning, Kind: HealthAttentionBrokerOffline, Subject: subject, Message: msg,
			})
		}
		// A broker that is not online already degrades the status. Its
		// last self-report is stale, so it adds no further items.
		online := !healthSummaryBrokerStatusIsProblem(b.Status)
		if healthSummaryBrokerSelfIsProblem(b.Health) {
			degrade()
			if online {
				items = append(items, HealthAttentionItem{
					Severity: HealthAttentionWarning, Kind: HealthAttentionBrokerDegraded, Subject: subject,
					Message: "Runtime broker " + label + " reports " + b.Health.Status + brokerFailingChecks(b.Health),
				})
			}
		}
		if ws := b.WorkspaceStorage; ws != nil && ws.NFSHealthy != nil && !*ws.NFSHealthy {
			degrade()
			if online {
				items = append(items, HealthAttentionItem{
					Severity: HealthAttentionWarning, Kind: HealthAttentionBrokerNFS, Subject: subject,
					Message: "Runtime broker " + label + " reports its NFS workspace storage unhealthy",
				})
			}
		}
	}

	// Stale hub instances, after the broker items.
	items = append(items, hubInstanceStoppedReportingAttention(resp.HubInstances)...)

	// Integrations, in list order (sorted by name). Only unhealthy changes
	// the status; degraded is a warning; unknown (not reported, not run
	// by any live instance, or hub instance data not available) is
	// neutral.
	for _, it := range resp.Integrations {
		subject := HealthAttentionSubject{Type: HealthSubjectIntegration, ID: it.Name, Name: it.Name}
		switch it.Health {
		case HealthStatusUnhealthy:
			degrade()
			items = append(items, HealthAttentionItem{
				Severity: HealthAttentionWarning, Kind: HealthAttentionIntegration, Subject: subject,
				Message: "Integration " + it.Name + " is unhealthy",
			})
		case HealthStatusDegraded:
			items = append(items, HealthAttentionItem{
				Severity: HealthAttentionWarning, Kind: HealthAttentionIntegration, Subject: subject,
				Message: "Integration " + it.Name + " is degraded",
			})
		}
	}

	// Dispatch.
	dispatchSubject := HealthAttentionSubject{Type: HealthSubjectDispatch}
	if d := resp.Dispatch; d == nil {
		items = append(items, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionDispatch, Subject: dispatchSubject,
			Message: "Dispatch data not available",
		})
	} else {
		if d.StuckMessages > 0 {
			degrade()
			items = append(items, HealthAttentionItem{
				Severity: HealthAttentionWarning, Kind: HealthAttentionDispatch, Subject: dispatchSubject,
				Message: pluralCount(d.StuckMessages, "agent message", "agent messages") + " stuck pending delivery",
			})
		}
		if d.StuckBrokerDispatch > 0 {
			degrade()
			items = append(items, HealthAttentionItem{
				Severity: HealthAttentionWarning, Kind: HealthAttentionDispatch, Subject: dispatchSubject,
				Message: pluralCount(d.StuckBrokerDispatch, "broker dispatch", "broker dispatches") + " stuck in progress",
			})
		}
	}

	// Agents.
	agentsSubject := HealthAttentionSubject{Type: HealthSubjectAgents}
	if a := resp.Agents; a == nil {
		items = append(items, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionAgents, Subject: agentsSubject,
			Message: "Agent data not available",
		})
	} else {
		if agentErrorRatioReached(a.Errored, a.Considered) {
			degrade()
			items = append(items, HealthAttentionItem{
				Severity: HealthAttentionWarning, Kind: HealthAttentionAgents, Subject: agentsSubject,
				Message: fmt.Sprintf("Error or crashed: %d of %d agents (%s%%)",
					a.Errored, a.Considered, formatPercent(a.Errored, a.Considered)),
			})
		}
		for _, g := range a.Problems {
			items = append(items, agentGroupAttention(g)...)
		}
	}

	return status, items
}

// agentErrorRatioReached reports whether errored / considered is at least
// agentErrorDegradedRatio. Zero considered agents never reach it.
func agentErrorRatioReached(errored, considered int) bool {
	return considered > 0 && errored*agentErrorRatioDenominator >= considered
}

// formatPercent formats n / d as a percentage with at most one decimal,
// rounded down so a share below the threshold never reads as reaching it.
func formatPercent(n, d int) string {
	if d <= 0 {
		return "0"
	}
	tenths := n * 1000 / d
	s := strconv.Itoa(tenths / 10)
	if r := tenths % 10; r != 0 {
		s += "." + strconv.Itoa(r)
	}
	return s
}

// hubFleetAttention returns the hub items: one fleet item, then one item
// per non-healthy check per live hub instance (subject = that instance),
// in the order of hub.unhealthy_checks (critical checks first). Check
// items are critical only when the fleet status is unhealthy. A live
// instance that reports a non-healthy status without a non-healthy check
// still gets one item, so its status is always explained.
//
// The fleet item is "Hub instance data not available" (warning) when the
// registry could not be read, "No hub instance is reporting" (critical)
// when no instance is live, and "N of M hub instances healthy" (critical
// when the fleet is unhealthy, else a warning) when the fleet is not
// healthy.
func hubFleetAttention(resp *HealthSummaryResponse) []HealthAttentionItem {
	fleetSubject := HealthAttentionSubject{Type: HealthSubjectHub}
	if resp.HubInstances == nil || resp.Hub.Instances == nil {
		return []HealthAttentionItem{{
			Severity: HealthAttentionWarning, Kind: HealthAttentionHubInstance, Subject: fleetSubject,
			Message: "Hub instance data not available",
		}}
	}
	fleet := *resp.Hub.Instances
	fleetStatus := fleetHubStatus(fleet)
	sev := HealthAttentionWarning
	if fleetStatus == HealthStatusUnhealthy {
		sev = HealthAttentionCritical
	}
	var out []HealthAttentionItem
	switch {
	case fleet.Live == 0:
		out = append(out, HealthAttentionItem{
			Severity: HealthAttentionCritical, Kind: HealthAttentionHubInstance, Subject: fleetSubject,
			Message: "No hub instance is reporting",
		})
	case fleetStatus != HealthStatusHealthy:
		out = append(out, HealthAttentionItem{
			Severity: sev, Kind: HealthAttentionHubInstance, Subject: fleetSubject,
			Message: fmt.Sprintf("%d of %d hub instances healthy", fleet.Healthy, fleet.Live),
		})
	}

	explained := map[string]bool{}
	for _, c := range resp.Hub.UnhealthyChecks {
		explained[c.InstanceID] = true
		out = append(out, HealthAttentionItem{
			Severity: sev, Kind: HealthAttentionHubCheck,
			Subject: HealthAttentionSubject{Type: HealthSubjectHub, ID: c.InstanceID, Name: c.InstanceLabel},
			Message: "Hub check " + c.Name + " is not healthy on instance " + c.InstanceLabel,
		})
	}
	for _, it := range resp.HubInstances.Items {
		if it.State != HubInstanceStateLive || explained[it.ID] || healthStatusRank(it.Status) == 0 {
			continue
		}
		label := hubInstanceDisplayLabel(it.Label, it.ID)
		out = append(out, HealthAttentionItem{
			Severity: sev, Kind: HealthAttentionHubCheck,
			Subject: HealthAttentionSubject{Type: HealthSubjectHub, ID: it.ID, Name: label},
			Message: "Hub instance " + label + " is not healthy",
		})
	}
	return out
}

// hubInstanceStoppedReportingAttention returns one warning per stale hub
// instance whose last write is within hubInstanceStoppedReportingWindow of
// the section's store clock, in list order. A stopped instance, or a stale
// one past the window, adds nothing.
func hubInstanceStoppedReportingAttention(list *HealthSummaryHubInstances) []HealthAttentionItem {
	if list == nil {
		return nil
	}
	var out []HealthAttentionItem
	for _, it := range list.Items {
		if it.State != HubInstanceStateStale || list.AsOf.Sub(it.LastSeen) > hubInstanceStoppedReportingWindow {
			continue
		}
		label := hubInstanceDisplayLabel(it.Label, it.ID)
		out = append(out, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionHubInstance,
			Subject: HealthAttentionSubject{Type: HealthSubjectHub, ID: it.ID, Name: label},
			Message: "Hub instance " + label + " stopped reporting",
		})
	}
	return out
}

// brokerFailingChecks lists a broker's non-passing self-health checks as
// " (name: value, ...)", sorted, or "" when none. Values come from the
// fixed vocabulary of api.NormalizeBrokerHealthReport.
func brokerFailingChecks(h *HealthBrokerSelf) string {
	var parts []string
	for k, v := range h.Checks {
		if v != HealthStatusHealthy && v != "available" {
			parts = append(parts, k+": "+v)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return " (" + strings.Join(parts, ", ") + ")"
}

// healthBrokerLabel is the name a message uses for a broker.
func healthBrokerLabel(b HealthSummaryBroker) string {
	if b.Name != "" {
		return b.Name
	}
	return b.ID
}

// agentGroupAttention returns one item per listed agent of a problem group,
// plus one item for agents beyond the reference cap.
func agentGroupAttention(g HealthAgentGroup) []HealthAttentionItem {
	var suffix, moreSuffix string
	switch g.Kind {
	case HealthAgentGroupErrored:
		suffix, moreSuffix = " is in the error phase", " in the error phase"
	case HealthAgentGroupCrashed:
		suffix, moreSuffix = " has crashed", " crashed"
	case HealthAgentGroupOffline:
		suffix, moreSuffix = " is offline", " offline"
	default:
		return nil
	}
	out := make([]HealthAttentionItem, 0, len(g.Items)+1)
	for _, ref := range g.Items {
		out = append(out, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionAgents,
			Subject: HealthAttentionSubject{Type: HealthSubjectAgent, ID: ref.ID, Name: ref.Name, ProjectID: ref.ProjectID},
			Message: "Agent " + healthAgentLabel(ref) + suffix,
		})
	}
	if more := g.Count - len(g.Items); more > 0 {
		out = append(out, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionAgents,
			Subject: HealthAttentionSubject{Type: HealthSubjectAgents},
			Message: pluralCount(more, "more agent", "more agents") + moreSuffix,
		})
	}
	return out
}

// healthAgentLabel is "project / agent", or the agent name alone when the
// project slug could not be resolved.
func healthAgentLabel(ref HealthAgentRef) string {
	if ref.ProjectSlug != "" {
		return ref.ProjectSlug + " / " + ref.Name
	}
	return ref.Name
}

// plural formats "1 thing" or "N things".
func pluralCount(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
