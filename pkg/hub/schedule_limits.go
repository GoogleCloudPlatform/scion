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
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/robfig/cron/v3"
)

// Recurring schedule limits (server.hub.schedules): a minimum interval
// between fires and a per-project cap on active recurring schedules. Both
// apply to every kind of recurring schedule (message, dispatch_agent, exec)
// and to every author, and are checked whenever a schedule is armed: on
// create, on a cron change, and on enable or resume. Schedules that are
// already active are not changed when the settings change.

// errScheduleQuota refuses another active recurring schedule in a project
// that is at the hub's limit.
var errScheduleQuota = errors.New("recurring schedule quota exceeded")

// cronMinGap returns a lower bound on the time between two consecutive
// fires of sched, from its minute and hour fields alone: the smallest gap
// between the times of day it can fire at, including the gap from the last
// one of a day to the first one of the next. Day-of-month, month and
// day-of-week restrictions only remove fires, so the real minimum interval
// is never shorter. The gap is on the schedule's wall clock; a daylight
// saving jump can shorten one fire's interval once. A schedule that is not
// a standard cron spec returns 0.
func cronMinGap(sched cron.Schedule) time.Duration {
	spec, ok := sched.(*cron.SpecSchedule)
	if !ok {
		return 0
	}
	var fires []int // minutes of the day, ascending
	for h := 0; h < 24; h++ {
		if spec.Hour&(1<<uint(h)) == 0 {
			continue
		}
		for m := 0; m < 60; m++ {
			if spec.Minute&(1<<uint(m)) != 0 {
				fires = append(fires, h*60+m)
			}
		}
	}
	if len(fires) == 0 {
		return 0
	}
	// Wrap-around: the last fire of one day to the first of the next.
	minGap := 24*60 - fires[len(fires)-1] + fires[0]
	for i := 1; i < len(fires); i++ {
		if g := fires[i] - fires[i-1]; g < minGap {
			minGap = g
		}
	}
	return time.Duration(minGap) * time.Minute
}

// checkScheduleFrequency refuses (400) a cron expression that can fire more
// often than the hub's minimum interval. The expression must already have
// been parsed successfully; a parse failure here is reported the same way.
func (s *Server) checkScheduleFrequency(w http.ResponseWriter, expr string) bool {
	minInterval := s.config.ScheduleMinInterval
	if minInterval <= 0 {
		return true
	}
	sched, err := parseScheduleCron(expr)
	if err != nil {
		writeCronParseError(w, err)
		return false
	}
	if gap := cronMinGap(sched); gap < minInterval {
		ValidationError(w, fmt.Sprintf("cron expression %q can fire every %s; this hub allows recurring schedules at most every %s (server.hub.schedules.min_interval)",
			expr, gap, minInterval), nil)
		return false
	}
	return true
}

// scheduleQuotaCheck refuses another active recurring schedule when the
// project already has the hub's maximum (0: no maximum). snap holds the
// project's other active recurring schedules of every kind
// (ActiveSchedulesAllKinds), read under the per-project quota lock.
func scheduleQuotaCheck(snap store.ScheduledExecQuotaSnapshot, maxActive int) error {
	if maxActive > 0 && snap.ActiveSchedulesAllKinds >= maxActive {
		return fmt.Errorf("%w: at most %d active recurring schedules per project (server.hub.schedules.max_active_per_project)", errScheduleQuota, maxActive)
	}
	return nil
}

// writeScheduleQuotaError writes the 429 for an exec or recurring schedule
// quota refusal and reports whether err was one.
func writeScheduleQuotaError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, errScheduledExecQuota) || errors.Is(err, errScheduleQuota) {
		writeError(w, http.StatusTooManyRequests, ErrCodeQuotaExceeded, err.Error(), nil)
		return true
	}
	return false
}
