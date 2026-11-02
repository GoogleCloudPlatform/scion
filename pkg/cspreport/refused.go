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

package cspreport

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// refusalLog counts the requests and reports the budgets refuse, and
// decides when a refusal summary record (RefusedLogMessage) is due: at
// most one per interval. The first refusal after a quiet interval is
// summarized at once, so the start of a refused stream shows right away.
// Later refusals are summarized when the interval since the previous
// summary is over: by the next request, or by a flush timer the handler
// arms when refusals are left pending, so the end of a refused stream is
// logged too. The totals since the handler was created are kept for
// Counts.
//
// The zero value is ready to use. A summary holds counts and times only:
// nothing about the client or its reports.
type refusalLog struct {
	mu sync.Mutex
	// pendingRequests and pendingReports are the refusals not yet in a
	// summary, the first of them at pendingSince and the latest at
	// pendingLatest.
	pendingRequests int
	pendingReports  int
	pendingSince    time.Time
	pendingLatest   time.Time
	// lastSummary is when the latest summary was written (zero before the
	// first).
	lastSummary time.Time
	// armed is set while a flush timer is pending.
	armed bool
	// totalRequests and totalReports are the refusals since creation.
	totalRequests int64
	totalReports  int64
}

// refusalSummary is the content of one refusal summary record: the
// refusals counted from Since to Until.
type refusalSummary struct {
	Requests int
	Reports  int
	Since    time.Time
	Until    time.Time
}

// add counts refused requests and reports at now. It returns a summary
// when one is due, and armIn > 0 when refusals are left pending and no
// flush timer is armed: the caller then arms one to call fire after armIn.
func (l *refusalLog) add(now time.Time, interval time.Duration, requests, reports int) (sum refusalSummary, ok bool, armIn time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pendingRequests == 0 && l.pendingReports == 0 {
		l.pendingSince = now
	}
	l.pendingLatest = now
	l.pendingRequests += requests
	l.pendingReports += reports
	l.totalRequests += int64(requests)
	l.totalReports += int64(reports)
	return l.dueLocked(now, interval)
}

// flush returns a summary of the pending refusals when one is due at now,
// as add does.
func (l *refusalLog) flush(now time.Time, interval time.Duration) (sum refusalSummary, ok bool, armIn time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.dueLocked(now, interval)
}

// fire is the flush timer's call: the timer is no longer armed, and the
// pending refusals are summarized when due, as add does.
func (l *refusalLog) fire(now time.Time, interval time.Duration) (sum refusalSummary, ok bool, armIn time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.armed = false
	return l.dueLocked(now, interval)
}

// drain returns, and clears, the pending refusals whatever the interval
// (for Handler.Close); the timer is no longer armed.
func (l *refusalLog) drain(now time.Time) (refusalSummary, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.armed = false
	if l.pendingRequests == 0 && l.pendingReports == 0 {
		return refusalSummary{}, false
	}
	sum := refusalSummary{Requests: l.pendingRequests, Reports: l.pendingReports, Since: l.pendingSince, Until: l.pendingLatest}
	l.pendingRequests, l.pendingReports = 0, 0
	l.lastSummary = now
	return sum, true
}

// dueLocked returns, and clears, the pending refusals when there are any
// and the interval since the latest summary is over. When refusals stay
// pending and no timer is armed, it marks one armed and returns when it
// should fire. l.mu is held.
func (l *refusalLog) dueLocked(now time.Time, interval time.Duration) (refusalSummary, bool, time.Duration) {
	if l.pendingRequests == 0 && l.pendingReports == 0 {
		return refusalSummary{}, false, 0
	}
	if since := now.Sub(l.lastSummary); !l.lastSummary.IsZero() && since < interval {
		if l.armed {
			return refusalSummary{}, false, 0
		}
		l.armed = true
		return refusalSummary{}, false, interval - since
	}
	sum := refusalSummary{Requests: l.pendingRequests, Reports: l.pendingReports, Since: l.pendingSince, Until: l.pendingLatest}
	l.pendingRequests, l.pendingReports = 0, 0
	l.lastSummary = now
	return sum, true, 0
}

// logRefusals writes a refusal summary record: "refused_requests"
// (requests refused before their reports were read: by the read ceiling,
// or an unreadable request by the per-client budget), "refused_reports"
// (reports of a read request refused by the per-client or the record
// budget), and "first_refused" and "latest_refused", the times the counts
// cover. It is not charged to the record limiter: there is at most one per
// interval.
func logRefusals(log *slog.Logger, s refusalSummary) {
	log.LogAttrs(context.Background(), slog.LevelInfo, RefusedLogMessage,
		slog.Int("refused_requests", s.Requests),
		slog.Int("refused_reports", s.Reports),
		slog.Time("first_refused", s.Since.UTC()),
		slog.Time("latest_refused", s.Until.UTC()),
	)
}
