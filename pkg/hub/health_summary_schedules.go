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
)

// The schedules section of the health summary (ptone/scion#5404): active
// recurring schedules whose most recent run failed, read from the fields
// every schedule already records (last run status, time and error, run
// and error counts). It is read-only: nothing here changes how schedules
// run, pause or are authorized.

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
// It lists only schedules in projects where the caller holds
// scheduled_event.read, so two callers can see different lists; it never
// changes the overall status.
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
// It returns nil and the error when the store read fails ("not reported").
func (s *Server) healthSummarySchedules(r *http.Request) (*HealthSummarySchedules, error) {
	ctx := r.Context()
	out := &HealthSummarySchedules{Failing: []HealthFailingSchedule{}}
	// A user caller's read decision depends on the project only, so it is
	// made once per project; an agent caller's depends on each row.
	agentCaller := isAgentCaller(ctx)
	allowed := map[string]bool{}
	var visible []store.Schedule
	scanned := 0
	opts := store.ListOptions{Limit: healthSummarySchedulePageSize}
	filter := store.ScheduleFilter{Status: store.ScheduleStatusActive, LastRunStatus: store.ScheduleRunError}
	for {
		page, err := s.store.ListSchedules(ctx, filter, opts)
		if err != nil {
			return nil, err
		}
		for _, sc := range page.Items {
			if scanned >= healthSummaryScheduleScanLimit {
				out.Truncated = true
				out.ScanLimited = true
				break
			}
			scanned++
			// Re-check the filter, so a store that ignores a field never
			// widens the list.
			if sc.Status != store.ScheduleStatusActive || sc.LastRunStatus != store.ScheduleRunError {
				continue
			}
			ok, seen := allowed[sc.ProjectID]
			if agentCaller || !seen {
				ok = s.healthSummaryCanReadSchedules(r, &sc)
				allowed[sc.ProjectID] = ok
			}
			if ok {
				visible = append(visible, sc)
			}
		}
		if out.Truncated || page.NextCursor == "" || page.NextCursor == opts.Cursor {
			break
		}
		opts.Cursor = page.NextCursor
	}

	sort.SliceStable(visible, func(i, j int) bool {
		ti, tj := scheduleLastRunAt(visible[i]), scheduleLastRunAt(visible[j])
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return visible[i].ID < visible[j].ID
	})
	out.Total = len(visible)
	if len(visible) > healthSummaryScheduleLimit {
		visible = visible[:healthSummaryScheduleLimit]
		out.Truncated = true
	}

	var projectIDs []string
	seenProject := map[string]bool{}
	for _, sc := range visible {
		if sc.ProjectID != "" && !seenProject[sc.ProjectID] {
			seenProject[sc.ProjectID] = true
			projectIDs = append(projectIDs, sc.ProjectID)
		}
	}
	slugs := s.healthSummaryProjectSlugs(ctx, projectIDs)
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
