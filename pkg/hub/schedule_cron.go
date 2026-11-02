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

	"github.com/GoogleCloudPlatform/scion/pkg/cronzone"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/robfig/cron/v3"
)

// cronZonePrefixMessage is the user-facing error for a cron expression that
// carries a zone prefix. The zone is a separate schedule field instead.
const cronZonePrefixMessage = "zone prefixes (CRON_TZ=, TZ=) are not supported in cron expressions — remove the prefix and set the schedule's time zone instead"

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
// A zone prefix is rejected with errCronZonePrefix; the zone is the
// schedule's separate time zone field. The returned schedule is pinned to
// UTC: cronzone.Next uses it as a naive wall clock and maps the result into
// the schedule's zone (scheduleNextRun). Pinning also keeps the result
// independent of the process zone: robfig/cron captures time.Local when no
// prefix is given, and a time.Local schedule evaluates in the zone of the
// time passed to Next.
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

// normalizeScheduleTimeZone validates a schedule time zone name and returns
// the value to store: "" for UTC (an empty name or "UTC"), otherwise the
// name as given. Every error matches cronzone.ErrInvalidZone.
func normalizeScheduleTimeZone(name string) (string, error) {
	loc, err := cronzone.ValidateZone(name)
	if err != nil {
		return "", err
	}
	if loc == time.UTC {
		return "", nil
	}
	return name, nil
}

// scheduleNextRun returns the first run of a recurring schedule strictly
// after `after`, as a UTC instant. The expression goes through
// parseScheduleCron (so zone prefixes stay refused) and is evaluated in the
// schedule's time zone; an empty zone is UTC and gives exactly the UTC-only
// result. The error is a parseScheduleCron error or a zone error matching
// cronzone.ErrInvalidZone; writeScheduleTimingError writes either.
func scheduleNextRun(expr, zone string, after time.Time) (time.Time, error) {
	sched, err := parseScheduleCron(expr)
	if err != nil {
		return time.Time{}, err
	}
	loc, err := cronzone.ValidateZone(zone)
	if err != nil {
		return time.Time{}, err
	}
	return cronzone.Next(sched, loc, after), nil
}

// zonePrefixPassBatchSize is the number of rows pauseZonePrefixedSchedules
// fetches per batch. It is a variable so tests can force several batches.
var zonePrefixPassBatchSize = 200

// pauseZonePrefixedSchedules pauses every active schedule whose cron
// expression carries a zone prefix (CRON_TZ=, TZ=). Such expressions are no
// longer supported; pausing makes the change visible and reversible (the user
// edits the expression to UTC and resumes it) instead of silently shifting
// the fire time. It runs once per process before the scheduler starts and
// logs one warning per paused schedule. It is idempotent: a second run finds
// no active prefixed rows.
//
// The pass does not page through ListSchedules: that keyset is on created,
// and on SQLite legacy timestamps written in a non-UTC zone can make it skip
// rows or stop advancing. Instead it repeatedly fetches a batch of active
// prefixed rows and pauses them, which removes them from the next fetch.
// Rows that are fetched but not paused (the pause failed, or the store's
// prefix match was looser than hasCronZonePrefix) are excluded from later
// fetches. Every ID is handled at most once: a batch that contains no ID
// the pass has not already handled (for example because a pause reported
// success without taking effect) stops the loop with an error. Each
// iteration therefore handles at least one new row or ends the pass, so it
// terminates after at most one iteration per candidate row. Errors are
// logged, not returned; the evaluator backstop in executeSchedule covers any
// row this pass misses.
func (s *Server) pauseZonePrefixedSchedules(ctx context.Context) {
	log := slog.With("subsystem", "scheduler")
	// seen holds every ID this pass has handled (paused, failed or not a
	// zone prefix). Only failed and non-prefix IDs go to the store as
	// excludeIDs, keeping its parameter list small; paused rows drop out
	// of the query on their own.
	seen := make(map[string]struct{})
	var excludeIDs []string
	paused, failed := 0, 0
	for {
		batch, err := s.store.ListActiveZonePrefixedSchedules(ctx, zonePrefixPassBatchSize, excludeIDs)
		if err != nil {
			log.Error("schedule zone-prefix check: failed to list active schedules", "error", err)
			return
		}
		if len(batch) == 0 {
			break
		}
		progressed := false
		for _, sched := range batch {
			if _, ok := seen[sched.ID]; ok {
				continue
			}
			progressed = true
			seen[sched.ID] = struct{}{}
			if !hasCronZonePrefix(sched.CronExpr) {
				excludeIDs = append(excludeIDs, sched.ID)
				continue
			}
			if err := s.store.UpdateScheduleStatus(ctx, sched.ID, store.ScheduleStatusPaused, store.ScheduleStatusReasonCronZoneUnsupported); err != nil {
				log.Error("schedule zone-prefix check: failed to pause schedule",
					"schedule_id", sched.ID, "project_id", sched.ProjectID,
					"cron_expr", sched.CronExpr, "error", err)
				failed++
				excludeIDs = append(excludeIDs, sched.ID)
				continue
			}
			paused++
			log.Warn("schedule paused: cron zone prefixes are not supported; remove the prefix, set the schedule's time zone, and resume",
				"schedule_id", sched.ID, "project_id", sched.ProjectID,
				"cron_expr", sched.CronExpr)
		}
		if !progressed {
			// The store returned only rows already handled, for example
			// a pause that reported success but did not take effect.
			// Stop rather than spin at startup.
			log.Error("schedule zone-prefix check: stopped, the store returned no new rows; "+
				"check store health, and on a SQLite hub upgraded from a non-UTC zone run the "+
				"utc-timestamp-normalize maintenance operation", "paused", paused)
			break
		}
	}
	if paused > 0 {
		log.Warn("schedule zone-prefix check: paused schedules with unsupported zone prefixes", "count", paused)
	}
	if failed > 0 {
		log.Error("schedule zone-prefix check: some prefixed schedules could not be paused; "+
			"the evaluator pauses them when they next fall due", "count", failed)
	}
}

// startScheduler pauses zone-prefixed schedules and then starts the
// scheduler, so the evaluator's first tick never sees an active prefixed row.
func (s *Server) startScheduler(ctx context.Context) {
	s.pauseZonePrefixedSchedules(ctx)
	s.scheduler.Start(ctx)
}

// writeScheduleTimingError writes the 400 response for an error from
// scheduleNextRun or normalizeScheduleTimeZone.
func writeScheduleTimingError(w http.ResponseWriter, err error) {
	if errors.Is(err, cronzone.ErrInvalidZone) {
		ValidationError(w, err.Error(), map[string]interface{}{"field": "timeZone"})
		return
	}
	writeCronParseError(w, err)
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
