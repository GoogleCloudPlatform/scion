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
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// The triggers section of the health summary (Stage 1 §6.5, ptone/scion#8033):
// active triggers whose most recent fire failed or was refused, and triggers
// the hub paused itself, with their stored status reason and paused-by-reason
// counts. Mirrors health_summary_schedules.go and is gated behind hub.triggers:
// when the experiment is off, the section is omitted (nil) and adds no
// attention items.

// healthSummaryTriggerLimit caps the failing and paused trigger rows returned.
// The visible totals and paused-by-reason counts are still reported.
var healthSummaryTriggerLimit = 50

// healthSummaryTriggerPageSize is the store page size used while listing
// triggers for the health summary.
var healthSummaryTriggerPageSize = 200

// healthSummaryTriggerScanLimit bounds how many trigger rows one summary reads
// from the store. Past it the list is marked scan_limited / truncated.
var healthSummaryTriggerScanLimit = 2000

const healthTriggerLastErrorMaxRunes = 300

// healthTriggerReasonOther is the reason code returned for a stored trigger
// status reason this hub does not recognize.
const healthTriggerReasonOther = "other"

// HealthSummaryTriggers is the triggers section of the health summary.
// For a user caller, failing and paused lists hold only triggers in projects
// where the caller holds trigger.read (decided once per project). For an agent
// caller, only triggers in the agent's own project that the agent holds are
// listed. Two callers can see different lists; the section never changes the
// overall health status.
type HealthSummaryTriggers struct {
	// Unavailable is true when the failing triggers list could not be read
	// while hub.triggers is enabled.
	Unavailable bool `json:"unavailable,omitempty"`

	// Failing lists active triggers whose last fire failed or was refused, most
	// recent failure first, capped at healthSummaryTriggerLimit. Never null
	// unless Unavailable is true.
	Failing []HealthFailingTrigger `json:"failing"`
	// Total is the number of failing triggers visible to the caller among the
	// rows read. When ScanLimited is true it is a lower bound.
	Total int `json:"total"`
	// Truncated is true when Total exceeds len(Failing), or when ScanLimited is true.
	Truncated bool `json:"truncated"`
	// ScanLimited is true when the store held more active triggers than one
	// summary reads (healthSummaryTriggerScanLimit).
	ScanLimited bool `json:"scan_limited"`

	// Paused lists triggers the hub paused itself (paused with a status reason),
	// most recently updated first, capped at healthSummaryTriggerLimit. A
	// trigger a user paused is not listed. Null only when PausedUnavailable.
	Paused []HealthPausedTrigger `json:"paused"`
	// PausedByReason counts visible hub-paused triggers by normalized reason
	// code (creator_deleted, target_deleted, source_deleted, other).
	PausedByReason map[string]int `json:"paused_by_reason"`
	// PausedUnavailable is true when hub-paused triggers could not be listed.
	PausedUnavailable bool `json:"paused_unavailable"`
	// PausedTotal is the number of hub-paused triggers visible to the caller
	// among the rows read; a lower bound when PausedScanLimited is true.
	PausedTotal int `json:"paused_total"`
	// PausedTruncated is true when PausedTotal exceeds len(Paused), or when
	// PausedScanLimited is true.
	PausedTruncated bool `json:"paused_truncated"`
	// PausedScanLimited is true when the store held more hub-paused triggers
	// than one summary reads (healthSummaryTriggerScanLimit).
	PausedScanLimited bool `json:"paused_scan_limited"`
}

// HealthFailingTrigger is one active trigger whose most recent fire failed or
// was refused.
type HealthFailingTrigger struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	ProjectID      string     `json:"project_id"`
	ProjectSlug    string     `json:"project_slug"`
	SourceKind     string     `json:"source_kind"`
	ActionType     string     `json:"action_type"`
	LastFireStatus string     `json:"last_fire_status"`
	FireCount      int        `json:"fire_count"`
	ErrorCount     int        `json:"error_count"`
	LastFailureAt  *time.Time `json:"last_failure_at"`
	LastError      string     `json:"last_error"`
}

// HealthPausedTrigger is one trigger the hub paused itself, with its normalized
// reason code.
type HealthPausedTrigger struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ProjectID   string `json:"project_id"`
	ProjectSlug string `json:"project_slug"`
	SourceKind  string `json:"source_kind"`
	ActionType  string `json:"action_type"`
	// Reason is a known store.TriggerStatusReason* constant or "other".
	Reason    string     `json:"reason"`
	UpdatedAt *time.Time `json:"updated_at"`
}

// healthSummaryTriggerSection reads the triggers section when hub.triggers is
// enabled. Returns nil when the experiment is off (omitted from the response),
// or a section with Unavailable=true when enabled and the store read fails.
func (s *Server) healthSummaryTriggerSection(r *http.Request) *HealthSummaryTriggers {
	if !s.experimentEnabled(experiments.Triggers) {
		return nil
	}
	sec, err := s.healthSummaryTriggers(r)
	if err != nil {
		slog.Error("health summary: failed to list failing triggers", "error", err)
		return &HealthSummaryTriggers{Unavailable: true}
	}
	return sec
}

// healthSummaryTriggers builds the triggers section for the caller of r.
func (s *Server) healthSummaryTriggers(r *http.Request) (*HealthSummaryTriggers, error) {
	ctx := r.Context()
	out := &HealthSummaryTriggers{
		Failing:        []HealthFailingTrigger{},
		Paused:         []HealthPausedTrigger{},
		PausedByReason: map[string]int{},
	}
	allowed := map[string]bool{}
	activeFilter := store.TriggerFilter{Status: store.TriggerStatusActive}
	pausedFilter := store.TriggerFilter{Status: store.TriggerStatusPaused, SystemPausedOnly: true}
	if isAgentCaller(ctx) {
		agentID, ok := agentListFilter(ctx)
		if !ok {
			return out, nil
		}
		activeFilter.InitiatorAgentID = agentID
		pausedFilter.InitiatorAgentID = agentID
		if agent := GetAgentIdentityFromContext(ctx); agent != nil {
			if _, err := uuid.Parse(agent.ProjectID()); err != nil {
				return out, nil
			}
			activeFilter.ProjectID = agent.ProjectID()
			pausedFilter.ProjectID = agent.ProjectID()
		}
	}

	failing, scanLimited, err := s.healthSummaryScanTriggers(r, allowed, activeFilter, isFailingTrigger)
	if err != nil {
		return nil, err
	}

	paused, pausedScanLimited, pausedErr := s.healthSummaryScanTriggers(r, allowed, pausedFilter, isSystemPausedTrigger)
	if pausedErr != nil {
		slog.Error("health summary: failed to list hub-paused triggers", "error", pausedErr)
		paused, pausedScanLimited = nil, false
		out.Paused = nil
		out.PausedByReason = nil
		out.PausedUnavailable = true
	}

	sort.SliceStable(failing, func(i, j int) bool {
		ti, tj := triggerLastFireAt(failing[i]), triggerLastFireAt(failing[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return failing[i].ID < failing[j].ID
	})
	out.Total = len(failing)
	out.ScanLimited = scanLimited
	out.Truncated = scanLimited
	if len(failing) > healthSummaryTriggerLimit {
		failing = failing[:healthSummaryTriggerLimit]
		out.Truncated = true
	}

	sortPausedTriggers(paused)
	out.PausedTotal = len(paused)
	out.PausedScanLimited = pausedScanLimited
	out.PausedTruncated = pausedScanLimited
	if !out.PausedUnavailable {
		for _, tr := range paused {
			out.PausedByReason[healthTriggerReasonCode(tr.StatusReason)]++
		}
	}
	if len(paused) > healthSummaryTriggerLimit {
		paused = paused[:healthSummaryTriggerLimit]
		out.PausedTruncated = true
	}

	var projectIDs []string
	seenProject := map[string]bool{}
	for _, list := range [][]store.Trigger{failing, paused} {
		for _, tr := range list {
			if tr.ProjectID != "" && !seenProject[tr.ProjectID] {
				seenProject[tr.ProjectID] = true
				projectIDs = append(projectIDs, tr.ProjectID)
			}
		}
	}
	slugs := s.healthSummaryProjectSlugs(ctx, projectIDs)

	for _, tr := range paused {
		row := HealthPausedTrigger{
			ID:          tr.ID,
			Name:        tr.Name,
			ProjectID:   tr.ProjectID,
			ProjectSlug: slugs[tr.ProjectID],
			SourceKind:  tr.SourceKind,
			ActionType:  tr.ActionType,
			Reason:      healthTriggerReasonCode(tr.StatusReason),
		}
		if !tr.UpdatedAt.IsZero() {
			at := tr.UpdatedAt.UTC()
			row.UpdatedAt = &at
		}
		out.Paused = append(out.Paused, row)
	}
	for _, tr := range failing {
		row := HealthFailingTrigger{
			ID:             tr.ID,
			Name:           tr.Name,
			ProjectID:      tr.ProjectID,
			ProjectSlug:    slugs[tr.ProjectID],
			SourceKind:     tr.SourceKind,
			ActionType:     tr.ActionType,
			LastFireStatus: tr.LastFireStatus,
			FireCount:      tr.FireCount,
			ErrorCount:     tr.ErrorCount,
			LastError:      capRunes(tr.LastFireError, healthTriggerLastErrorMaxRunes),
		}
		if tr.LastFireAt != nil {
			at := tr.LastFireAt.UTC()
			row.LastFailureAt = &at
		}
		out.Failing = append(out.Failing, row)
	}
	return out, nil
}

func (s *Server) healthSummaryScanTriggers(
	r *http.Request,
	allowed map[string]bool,
	filter store.TriggerFilter,
	keep func(store.Trigger) bool,
) (visible []store.Trigger, scanLimited bool, err error) {
	agentCaller := isAgentCaller(r.Context())
	scanned := 0
	opts := store.ListOptions{Limit: healthSummaryTriggerPageSize}
	for {
		page, err := s.store.ListTriggers(r.Context(), filter, opts)
		if err != nil {
			return nil, false, err
		}
		for _, tr := range page.Items {
			if scanned >= healthSummaryTriggerScanLimit {
				return visible, true, nil
			}
			scanned++
			if !keep(tr) {
				continue
			}
			ok, seen := allowed[tr.ProjectID]
			if agentCaller {
				ok = s.healthSummaryCanReadTrigger(r, &tr)
			} else if !seen {
				ok = s.healthSummaryCanReadTrigger(r, &tr)
				allowed[tr.ProjectID] = ok
			}
			if ok {
				visible = append(visible, tr)
			}
		}
		if page.NextCursor == "" || page.NextCursor == opts.Cursor {
			return visible, false, nil
		}
		opts.Cursor = page.NextCursor
	}
}

func (s *Server) healthSummaryCanReadTrigger(r *http.Request, tr *store.Trigger) bool {
	if tr == nil || tr.ProjectID == "" {
		return false
	}
	if agent := GetAgentIdentityFromContext(r.Context()); agent != nil {
		if !agent.HasScope(ScopeProjectRead) || agent.ProjectID() != tr.ProjectID {
			return false
		}
	} else if id := GetIdentityFromContext(r.Context()); id != nil && id.Type() == "agent" {
		return false
	}
	return s.triggerAccessDecision(r.Context(), tr.ProjectID, ActionRead, &triggerRow{attribution: tr.InitiatorAttribution}).allowed
}

func isFailingTrigger(tr store.Trigger) bool {
	return tr.Status == store.TriggerStatusActive &&
		(tr.LastFireStatus == store.TriggerFireStatusFailed ||
			tr.LastFireStatus == store.TriggerFireStatusRefused ||
			tr.LastFireStatus == store.TriggerFireStatusExpired)
}

func isSystemPausedTrigger(tr store.Trigger) bool {
	return tr.Status == store.TriggerStatusPaused && tr.StatusReason != store.TriggerStatusReasonNone
}

func triggerLastFireAt(tr store.Trigger) time.Time {
	if tr.LastFireAt == nil {
		return time.Time{}
	}
	return *tr.LastFireAt
}

func sortPausedTriggers(list []store.Trigger) {
	sort.SliceStable(list, func(i, j int) bool {
		if !list[i].UpdatedAt.Equal(list[j].UpdatedAt) {
			return list[i].UpdatedAt.After(list[j].UpdatedAt)
		}
		return list[i].ID < list[j].ID
	})
}

func healthTriggerReasonCode(reason string) string {
	switch reason {
	case store.TriggerStatusReasonCreatorDeleted,
		store.TriggerStatusReasonTargetDeleted,
		store.TriggerStatusReasonSourceDeleted:
		return reason
	default:
		return healthTriggerReasonOther
	}
}

func healthTriggerReasonText(code string) string {
	switch code {
	case store.TriggerStatusReasonCreatorDeleted:
		return "the agent or user it runs under was deleted"
	case store.TriggerStatusReasonTargetDeleted:
		return "its target agent was deleted"
	case store.TriggerStatusReasonSourceDeleted:
		return "its webhook source was deleted"
	default:
		return "a reason this hub does not recognize"
	}
}

// triggerAttention returns warning attention items for failing and hub-paused
// triggers when hub.triggers is enabled (sec != nil). Triggers never change the
// overall health status because the section is caller-scoped.
func triggerAttention(sec *HealthSummaryTriggers) []HealthAttentionItem {
	if sec == nil {
		return nil
	}
	if sec.Unavailable {
		return []HealthAttentionItem{{
			Severity: HealthAttentionWarning, Kind: HealthAttentionTrigger,
			Subject: HealthAttentionSubject{Type: HealthSubjectTriggers},
			Message: "Trigger data not available",
		}}
	}
	out := make([]HealthAttentionItem, 0, len(sec.Failing)+1)
	for _, tr := range sec.Failing {
		label := tr.Name
		if tr.ProjectSlug != "" {
			label = tr.ProjectSlug + " / " + tr.Name
		}
		verb := "failed"
		if tr.LastFireStatus == store.TriggerFireStatusRefused {
			verb = "was refused on"
		}
		out = append(out, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionTrigger,
			Subject: HealthAttentionSubject{Type: HealthSubjectTrigger, ID: tr.ID, Name: tr.Name, ProjectID: tr.ProjectID},
			Message: "Trigger " + label + " " + verb + " its last fire",
		})
	}
	more := sec.Total - len(sec.Failing)
	msg := ""
	switch {
	case more > 0 && sec.ScanLimited:
		msg = "At least " + pluralCount(more, "more trigger failed its", "more triggers failed their") + " last fire"
	case more > 0:
		msg = pluralCount(more, "more trigger failed its", "more triggers failed their") + " last fire"
	case sec.ScanLimited:
		msg = "More triggers may have failed their last fire"
	}
	if msg != "" {
		out = append(out, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionTrigger,
			Subject: HealthAttentionSubject{Type: HealthSubjectTriggers},
			Message: msg,
		})
	}
	return append(out, pausedTriggerAttention(sec)...)
}

func pausedTriggerAttention(sec *HealthSummaryTriggers) []HealthAttentionItem {
	if sec.PausedUnavailable {
		return []HealthAttentionItem{{
			Severity: HealthAttentionWarning, Kind: HealthAttentionTrigger,
			Subject: HealthAttentionSubject{Type: HealthSubjectTriggers},
			Message: "Hub-paused trigger data not available",
		}}
	}
	out := make([]HealthAttentionItem, 0, len(sec.Paused)+1)
	for _, tr := range sec.Paused {
		label := tr.Name
		if tr.ProjectSlug != "" {
			label = tr.ProjectSlug + " / " + tr.Name
		}
		out = append(out, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionTrigger,
			Subject: HealthAttentionSubject{Type: HealthSubjectTrigger, ID: tr.ID, Name: tr.Name, ProjectID: tr.ProjectID},
			Message: "Trigger " + label + " was paused by the hub: " + healthTriggerReasonText(tr.Reason),
		})
	}
	more := sec.PausedTotal - len(sec.Paused)
	msg := ""
	switch {
	case more > 0 && sec.PausedScanLimited:
		msg = "At least " + pluralCount(more, "more trigger was", "more triggers were") + " paused by the hub"
	case more > 0:
		msg = pluralCount(more, "more trigger was", "more triggers were") + " paused by the hub"
	case sec.PausedScanLimited:
		msg = "More triggers may have been paused by the hub"
	}
	if msg != "" {
		out = append(out, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionTrigger,
			Subject: HealthAttentionSubject{Type: HealthSubjectTriggers},
			Message: msg,
		})
	}
	return out
}
