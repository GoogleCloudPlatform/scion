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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/robfig/cron/v3"
)

// cronZonePrefixMessage is the user-facing error for a cron expression that
// carries a zone prefix. Schedules are evaluated in UTC only.
const cronZonePrefixMessage = "cron expressions are evaluated in UTC; zone prefixes (CRON_TZ=, TZ=) are not supported — convert the time to UTC"

// errCronZonePrefix is returned by parseScheduleCron for an expression that
// begins with a CRON_TZ= or TZ= zone prefix.
var errCronZonePrefix = errors.New(cronZonePrefixMessage)

// scheduleCronParser is the standard 5-field parser used for every recurring
// schedule. Descriptors (@every, @daily, ...) are not enabled.
var scheduleCronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// hasCronZonePrefix reports whether expr begins with a zone prefix. The check
// mirrors robfig/cron v3.0.1 Parser.Parse exactly: a case-sensitive
// strings.HasPrefix on the untrimmed expression.
func hasCronZonePrefix(expr string) bool {
	return strings.HasPrefix(expr, "CRON_TZ=") || strings.HasPrefix(expr, "TZ=")
}

// parseScheduleCron parses a recurring schedule's cron expression. It is the
// only parser for schedule expressions in the hub: every create, update,
// enable, resume and evaluation goes through it.
//
// Schedules are UTC-only. A zone prefix is rejected with errCronZonePrefix,
// and the returned schedule is pinned to UTC explicitly: robfig/cron captures
// time.Local when no prefix is given, and a time.Local schedule evaluates in
// the zone of the time passed to Next, so pinning makes the result
// independent of both the process zone and the caller's time value.
func parseScheduleCron(expr string) (cron.Schedule, error) {
	if hasCronZonePrefix(expr) {
		return nil, errCronZonePrefix
	}
	sched, err := scheduleCronParser.Parse(expr)
	if err != nil {
		return nil, err
	}
	if spec, ok := sched.(*cron.SpecSchedule); ok {
		spec.Location = time.UTC
	}
	return sched, nil
}

// zonePrefixPassPageSize is the page size used by pauseZonePrefixedSchedules.
// It equals the store's maximum list limit.
const zonePrefixPassPageSize = 200

// pauseZonePrefixedSchedules pauses every active schedule whose cron
// expression carries a zone prefix (CRON_TZ=, TZ=). Such expressions are no
// longer supported; pausing makes the change visible and reversible (the user
// edits the expression to UTC and resumes it) instead of silently shifting
// the fire time. It runs once per process at scheduler start, pages through
// all active schedules across all projects to the end, and logs one warning
// per paused schedule. It is idempotent: a second run finds no active
// prefixed rows. Errors are logged, not returned; the evaluator backstop in
// executeSchedule covers any row this pass misses.
func (s *Server) pauseZonePrefixedSchedules(ctx context.Context) {
	log := slog.With("subsystem", "scheduler")
	filter := store.ScheduleFilter{Status: store.ScheduleStatusActive}
	cursor := ""
	paused := 0
	for {
		page, err := s.store.ListSchedules(ctx, filter, store.ListOptions{Limit: zonePrefixPassPageSize, Cursor: cursor})
		if err != nil {
			log.Error("schedule zone-prefix check: failed to list active schedules", "error", err)
			return
		}
		for _, sched := range page.Items {
			if !hasCronZonePrefix(sched.CronExpr) {
				continue
			}
			if err := s.store.UpdateScheduleStatus(ctx, sched.ID, store.ScheduleStatusPaused); err != nil {
				log.Error("schedule zone-prefix check: failed to pause schedule",
					"schedule_id", sched.ID, "project_id", sched.ProjectID,
					"cron_expr", sched.CronExpr, "error", err)
				continue
			}
			paused++
			log.Warn("schedule paused: cron zone prefixes are not supported; edit the expression to UTC and resume",
				"schedule_id", sched.ID, "project_id", sched.ProjectID,
				"cron_expr", sched.CronExpr)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if paused > 0 {
		log.Warn("schedule zone-prefix check: paused schedules with unsupported zone prefixes", "count", paused)
	}
}

// writeCronParseError writes the 400 response for a cron expression that
// parseScheduleCron rejected.
func writeCronParseError(w http.ResponseWriter, err error) {
	if errors.Is(err, errCronZonePrefix) {
		ValidationError(w, cronZonePrefixMessage, nil)
		return
	}
	ValidationError(w, fmt.Sprintf("invalid cron expression: %v", err), nil)
}
