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
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// The schedules section of the health summary (ptone/scion#5404): active
// recurring schedules whose most recent run failed, read from the fields
// every schedule already records (last run status, time and error, run
// and error counts), and schedules the hub paused itself, with their
// stored status reason (ptone/scion#5403). It is read-only: nothing here
// changes how schedules run, pause or are authorized.

// healthSummaryScheduleLimit caps the failing schedule rows returned. The
// visible total is still reported. A variable so tests can lower it.
var healthSummaryScheduleLimit = 50

// healthSummarySchedulePageSize is the store page size used while listing
// failing schedules. A variable so tests can lower it.
var healthSummarySchedulePageSize = 200

// healthSummaryScheduleScanLimit bounds how many failing schedule rows one
// summary reads from the store. Past it the section is marked truncated.
var healthSummaryScheduleScanLimit = 2000

// healthScheduleLastErrorMaxRunes caps the stored last error text a row
// carries. The text is stored data, returned as a JSON string only; the
// dashboard renders it as text and shortens it further.
const healthScheduleLastErrorMaxRunes = 300

// HealthSummarySchedules is the schedules section of the health summary.
// For a user caller, the failing and paused lists each hold only schedules
// in projects where the caller holds scheduled_event.read (decided once per
// project). For an agent caller, each row is decided on its own: only
// schedules in the agent's own project that the agent holds are listed. Two
// callers can see different lists; the section never changes the overall
// status.
type HealthSummarySchedules struct {
	// Failing lists active schedules whose last run failed, most recent
	// failure first, capped at healthSummaryScheduleLimit. Never null.
	Failing []HealthFailingSchedule `json:"failing"`
	// Total is the number of failing schedules visible to the caller among
	// the rows read. When ScanLimited is true it is a lower bound.
	Total int `json:"total"`
	// Truncated is true when Total exceeds len(Failing), or when
	// ScanLimited is true.
	Truncated bool `json:"truncated"`
	// ScanLimited is true when the store held more failing schedules than
	// one summary reads (healthSummaryScheduleScanLimit). The rows read are
	// the first in the store's listing order (newest schedule first, then
	// ID), so the result is deterministic; Total then counts only those.
	ScanLimited bool `json:"scan_limited"`

	// Paused lists schedules the hub paused itself (paused with a status
	// reason), most recently updated first, capped at
	// healthSummaryScheduleLimit. A schedule a user paused is not listed.
	// Null only when PausedUnavailable is true.
	Paused []HealthPausedSchedule `json:"paused"`
	// PausedUnavailable is true when the hub-paused schedules could not be
	// listed. The failing list above is still reported.
	PausedUnavailable bool `json:"paused_unavailable"`
	// PausedTotal is the number of hub-paused schedules visible to the
	// caller among the rows read; a lower bound when PausedScanLimited.
	PausedTotal int `json:"paused_total"`
	// PausedTruncated is true when PausedTotal exceeds len(Paused), or
	// when PausedScanLimited is true.
	PausedTruncated bool `json:"paused_truncated"`
	// PausedScanLimited is true when the store held more hub-paused
	// schedules than one summary reads (healthSummaryScheduleScanLimit).
	PausedScanLimited bool `json:"paused_scan_limited"`
}

// HealthPausedSchedule is one schedule the hub paused, with its reason.
type HealthPausedSchedule struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProjectID string `json:"project_id"`
	// ProjectSlug is empty when the project could not be resolved.
	ProjectSlug string `json:"project_slug"`
	EventType   string `json:"event_type"`
	// Target is the agent the schedule addresses, as for a failing row.
	Target string `json:"target"`
	// Reason is the stored status reason when it is one this hub knows
	// (store.ScheduleStatusReason*), otherwise "other". The stored text
	// itself is never returned.
	Reason string `json:"reason"`
	// UpdatedAt is the schedule's last update, normally the pause; null
	// when not recorded.
	UpdatedAt *time.Time `json:"updated_at"`
}

// HealthFailingSchedule is one active schedule whose last run failed.
type HealthFailingSchedule struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProjectID string `json:"project_id"`
	// ProjectSlug is empty when the project could not be resolved.
	ProjectSlug string `json:"project_slug"`
	// EventType is the schedule's event type, for example "message".
	EventType string `json:"event_type"`
	// Target is the agent the schedule addresses (agent name, else agent
	// ID), or empty when the payload names none. No other payload field is
	// returned.
	Target     string `json:"target"`
	RunCount   int    `json:"run_count"`
	ErrorCount int    `json:"error_count"`
	// LastFailureAt is when the failed last run happened; null when not
	// recorded.
	LastFailureAt *time.Time `json:"last_failure_at"`
	// LastError is the stored error of the last run, capped at
	// healthScheduleLastErrorMaxRunes runes.
	LastError string `json:"last_error"`
}

// healthSummarySchedules builds the schedules section for the caller of r.
// It returns nil and the error when the failing schedules cannot be read
// ("not reported"). A failed read of the hub-paused schedules only sets
// PausedUnavailable.
func (s *Server) healthSummarySchedules(r *http.Request) (*HealthSummarySchedules, error) {
	ctx := r.Context()
	out := &HealthSummarySchedules{Failing: []HealthFailingSchedule{}, Paused: []HealthPausedSchedule{}}
	allowed := map[string]bool{}
	failingFilter := store.ScheduleFilter{Status: store.ScheduleStatusActive, LastRunStatus: store.ScheduleRunError}
	pausedFilter := store.ScheduleFilter{Status: store.ScheduleStatusPaused, SystemPausedOnly: true}
	if isAgentCaller(ctx) {
		// An agent caller's scans read only the rows it holds, in its own
		// project, so rows it cannot see do not use up the scan limit.
		agentID, ok := agentListFilter(ctx)
		if !ok {
			return out, nil
		}
		failingFilter.InitiatorAgentID = agentID
		pausedFilter.InitiatorAgentID = agentID
		if agent := GetAgentIdentityFromContext(ctx); agent != nil {
			if _, err := uuid.Parse(agent.ProjectID()); err != nil {
				// A project claim that is not a project ID names no
				// schedules the agent may read.
				return out, nil
			}
			failingFilter.ProjectID = agent.ProjectID()
			pausedFilter.ProjectID = agent.ProjectID()
		}
	}

	visible, scanLimited, err := s.healthSummaryScanSchedules(r, allowed, failingFilter, isFailingSchedule)
	if err != nil {
		return nil, err
	}
	// The paused list is read on its own: a failed read leaves the failing
	// list in place and marks only the paused list as not reported.
	paused, pausedScanLimited, pausedErr := s.healthSummaryScanSchedules(r, allowed, pausedFilter, isSystemPausedSchedule)
	if pausedErr != nil {
		slog.Error("health summary: failed to list hub-paused schedules", "error", pausedErr)
		paused, pausedScanLimited = nil, false
		out.Paused = nil
		out.PausedUnavailable = true
	}

	sort.SliceStable(visible, func(i, j int) bool {
		ti, tj := scheduleLastRunAt(visible[i]), scheduleLastRunAt(visible[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return visible[i].ID < visible[j].ID
	})
	out.Total = len(visible)
	out.ScanLimited = scanLimited
	out.Truncated = scanLimited
	if len(visible) > healthSummaryScheduleLimit {
		visible = visible[:healthSummaryScheduleLimit]
		out.Truncated = true
	}

	sortPausedSchedules(paused)
	out.PausedTotal = len(paused)
	out.PausedScanLimited = pausedScanLimited
	out.PausedTruncated = pausedScanLimited
	if len(paused) > healthSummaryScheduleLimit {
		paused = paused[:healthSummaryScheduleLimit]
		out.PausedTruncated = true
	}

	var projectIDs []string
	seenProject := map[string]bool{}
	for _, list := range [][]store.Schedule{visible, paused} {
		for _, sc := range list {
			if sc.ProjectID != "" && !seenProject[sc.ProjectID] {
				seenProject[sc.ProjectID] = true
				projectIDs = append(projectIDs, sc.ProjectID)
			}
		}
	}
	slugs := s.healthSummaryProjectSlugs(ctx, projectIDs)
	for _, sc := range paused {
		row := HealthPausedSchedule{
			ID:          sc.ID,
			Name:        sc.Name,
			ProjectID:   sc.ProjectID,
			ProjectSlug: slugs[sc.ProjectID],
			EventType:   sc.EventType,
			Target:      scheduleTarget(sc.Payload),
			Reason:      healthScheduleReasonCode(sc.StatusReason),
		}
		if !sc.UpdatedAt.IsZero() {
			at := sc.UpdatedAt.UTC()
			row.UpdatedAt = &at
		}
		out.Paused = append(out.Paused, row)
	}
	for _, sc := range visible {
		row := HealthFailingSchedule{
			ID:          sc.ID,
			Name:        sc.Name,
			ProjectID:   sc.ProjectID,
			ProjectSlug: slugs[sc.ProjectID],
			EventType:   sc.EventType,
			Target:      scheduleTarget(sc.Payload),
			RunCount:    sc.RunCount,
			ErrorCount:  sc.ErrorCount,
			LastError:   capRunes(sc.LastRunError, healthScheduleLastErrorMaxRunes),
		}
		if sc.LastRunAt != nil {
			at := sc.LastRunAt.UTC()
			row.LastFailureAt = &at
		}
		out.Failing = append(out.Failing, row)
	}
	return out, nil
}

// healthSummaryScanSchedules pages through the schedules matching filter,
// reading at most healthSummaryScheduleScanLimit rows, and returns those
// that keep accepts and whose project the caller may read. keep re-checks
// the filter, so a store that ignores a field never widens the list.
// allowed caches a user caller's per-project read decision across calls;
// an agent caller's decision depends on each row, so it is made per row.
// scanLimited is true when rows were left unread.
func (s *Server) healthSummaryScanSchedules(r *http.Request, allowed map[string]bool, filter store.ScheduleFilter, keep func(store.Schedule) bool) (visible []store.Schedule, scanLimited bool, err error) {
	agentCaller := isAgentCaller(r.Context())
	scanned := 0
	opts := store.ListOptions{Limit: healthSummarySchedulePageSize}
	for {
		page, err := s.store.ListSchedules(r.Context(), filter, opts)
		if err != nil {
			return nil, false, err
		}
		for _, sc := range page.Items {
			if scanned >= healthSummaryScheduleScanLimit {
				return visible, true, nil
			}
			scanned++
			if !keep(sc) {
				continue
			}
			ok, seen := allowed[sc.ProjectID]
			if agentCaller {
				ok = s.healthSummaryCanReadSchedules(r, &sc)
			} else if !seen {
				ok = s.healthSummaryCanReadSchedules(r, &sc)
				allowed[sc.ProjectID] = ok
			}
			if ok {
				visible = append(visible, sc)
			}
		}
		if page.NextCursor == "" || page.NextCursor == opts.Cursor {
			return visible, false, nil
		}
		opts.Cursor = page.NextCursor
	}
}

// sortPausedSchedules orders hub-paused schedules most recently updated
// first (the pause is normally the schedule's last write), then by ID.
func sortPausedSchedules(list []store.Schedule) {
	sort.SliceStable(list, func(i, j int) bool {
		if !list[i].UpdatedAt.Equal(list[j].UpdatedAt) {
			return list[i].UpdatedAt.After(list[j].UpdatedAt)
		}
		return list[i].ID < list[j].ID
	})
}

// isFailingSchedule reports whether sc is active and its last run failed.
func isFailingSchedule(sc store.Schedule) bool {
	return sc.Status == store.ScheduleStatusActive && sc.LastRunStatus == store.ScheduleRunError
}

// isSystemPausedSchedule reports whether the hub itself paused sc: it is
// paused and carries a status reason. A user pause has no reason.
func isSystemPausedSchedule(sc store.Schedule) bool {
	return sc.Status == store.ScheduleStatusPaused && sc.StatusReason != store.ScheduleStatusReasonNone
}

// healthScheduleReasonOther is the reason code returned for a stored
// status reason this hub does not know.
const healthScheduleReasonOther = "other"

// healthScheduleReasonCode returns a known stored status reason unchanged
// and healthScheduleReasonOther for any other value, so the summary never
// carries arbitrary stored text as the reason.
func healthScheduleReasonCode(reason string) string {
	switch reason {
	case store.ScheduleStatusReasonCreatorDeleted, store.ScheduleStatusReasonCronZoneUnsupported:
		return reason
	default:
		return healthScheduleReasonOther
	}
}

// healthScheduleReasonText is the fixed sentence ending for a reason code.
func healthScheduleReasonText(code string) string {
	switch code {
	case store.ScheduleStatusReasonCreatorDeleted:
		return "the agent or user it runs under was deleted"
	case store.ScheduleStatusReasonCronZoneUnsupported:
		return "its cron expression has a zone prefix, which is not supported: set its time zone field instead"
	default:
		return "a reason this hub does not recognize"
	}
}

// healthSummaryCanReadSchedules reports whether the caller of r may read
// schedule sc: the decision the schedules API makes for a read of that row
// (scheduledEventAccessDecision), plus, for an agent, the project isolation
// and project:read scope that API's router checks first. An agent sees only
// the schedules it holds authority for. It fails closed and logs nothing,
// since it runs on every poll.
func (s *Server) healthSummaryCanReadSchedules(r *http.Request, sc *store.Schedule) bool {
	if sc == nil || sc.ProjectID == "" {
		return false
	}
	if agent := GetAgentIdentityFromContext(r.Context()); agent != nil {
		if !agent.HasScope(ScopeProjectRead) || agent.ProjectID() != sc.ProjectID {
			return false
		}
	} else if id := GetIdentityFromContext(r.Context()); id != nil && id.Type() == "agent" {
		// An agent-type identity without its agent context cannot be
		// checked for project isolation: deny.
		return false
	}
	return s.scheduledEventAccessDecision(r.Context(), sc.ProjectID, ActionRead, scheduleRow(sc)).allowed
}

// scheduleLastRunAt is the schedule's last run time, or the zero time.
func scheduleLastRunAt(sc store.Schedule) time.Time {
	if sc.LastRunAt == nil {
		return time.Time{}
	}
	return *sc.LastRunAt
}

// scheduleTarget returns the agent a schedule payload addresses: its
// agentName, else its agentId, else "". Every event type that names an
// agent uses these keys. A payload that is not a JSON object gives "".
func scheduleTarget(payload string) string {
	var p struct {
		AgentID   string `json:"agentId"`
		AgentName string `json:"agentName"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return ""
	}
	if p.AgentName != "" {
		return capRunes(p.AgentName, healthScheduleLastErrorMaxRunes)
	}
	return capRunes(p.AgentID, healthScheduleLastErrorMaxRunes)
}

// capRunes returns s as valid UTF-8, cut to at most limit runes with "…"
// appended when it was longer.
func capRunes(s string, limit int) string {
	s = strings.ToValidUTF8(s, "�")
	n := 0
	for i := range s {
		if n == limit {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

// scheduleAttention returns one warning per listed failing schedule, plus
// one for failing schedules beyond the cap ("at least" when the scan was
// limited, since Total is then a lower bound), or one "not available"
// warning when the section could not be read. Schedules never change the
// status.
func scheduleAttention(sec *HealthSummarySchedules) []HealthAttentionItem {
	if sec == nil {
		return []HealthAttentionItem{{
			Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
			Subject: HealthAttentionSubject{Type: HealthSubjectSchedules},
			Message: "Scheduled event data not available",
		}}
	}
	out := make([]HealthAttentionItem, 0, len(sec.Failing)+1)
	for _, sc := range sec.Failing {
		label := sc.Name
		if sc.ProjectSlug != "" {
			label = sc.ProjectSlug + " / " + sc.Name
		}
		out = append(out, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
			Subject: HealthAttentionSubject{Type: HealthSubjectSchedule, ID: sc.ID, Name: sc.Name, ProjectID: sc.ProjectID},
			Message: "Scheduled event " + label + " failed its last run",
		})
	}
	more := sec.Total - len(sec.Failing)
	msg := ""
	switch {
	case more > 0 && sec.ScanLimited:
		msg = "At least " + pluralCount(more, "more scheduled event failed its", "more scheduled events failed their") + " last run"
	case more > 0:
		msg = pluralCount(more, "more scheduled event failed its", "more scheduled events failed their") + " last run"
	case sec.ScanLimited:
		msg = "More scheduled events may have failed their last run"
	}
	if msg != "" {
		out = append(out, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
			Subject: HealthAttentionSubject{Type: HealthSubjectSchedules},
			Message: msg,
		})
	}
	return append(out, pausedScheduleAttention(sec)...)
}

// pausedScheduleAttention returns one warning per listed hub-paused
// schedule, with a fixed sentence for its reason, plus one for those
// beyond the cap or past the scan limit, or one "not available" warning
// when the paused list could not be read.
func pausedScheduleAttention(sec *HealthSummarySchedules) []HealthAttentionItem {
	if sec.PausedUnavailable {
		return []HealthAttentionItem{{
			Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
			Subject: HealthAttentionSubject{Type: HealthSubjectSchedules},
			Message: "Hub-paused scheduled event data not available",
		}}
	}
	out := make([]HealthAttentionItem, 0, len(sec.Paused)+1)
	for _, sc := range sec.Paused {
		label := sc.Name
		if sc.ProjectSlug != "" {
			label = sc.ProjectSlug + " / " + sc.Name
		}
		out = append(out, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
			Subject: HealthAttentionSubject{Type: HealthSubjectSchedule, ID: sc.ID, Name: sc.Name, ProjectID: sc.ProjectID},
			Message: "Scheduled event " + label + " was paused by the hub: " + healthScheduleReasonText(sc.Reason),
		})
	}
	more := sec.PausedTotal - len(sec.Paused)
	msg := ""
	switch {
	case more > 0 && sec.PausedScanLimited:
		msg = "At least " + pluralCount(more, "more scheduled event was", "more scheduled events were") + " paused by the hub"
	case more > 0:
		msg = pluralCount(more, "more scheduled event was", "more scheduled events were") + " paused by the hub"
	case sec.PausedScanLimited:
		msg = "More scheduled events may have been paused by the hub"
	}
	if msg != "" {
		out = append(out, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
			Subject: HealthAttentionSubject{Type: HealthSubjectSchedules},
			Message: msg,
		})
	}
	return out
}

// healthSummaryScheduleSection reads the schedules section, logging a
// failed read and returning nil ("not reported").
func (s *Server) healthSummaryScheduleSection(r *http.Request) *HealthSummarySchedules {
	sec, err := s.healthSummarySchedules(r)
	if err != nil {
		slog.Error("health summary: failed to list failing schedules", "error", err)
		return nil
	}
	return sec
}
