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
	"strings"
	"sync"
	"time"
)

// Dedupe collapses repeats of the same violation. A page can report the
// same violation many times in one load (once per rendered element, and
// again on each re-render), and some browsers send every report as its own
// request. Two reports are the same violation when their directive, the
// origin of their blocked URI and their page path match.
//
// The first report of a violation is logged; the repeats within the next
// interval are counted and not logged, and the latest of them is kept.
// When the violation's interval is over and it had repeats, one summary
// record follows (RepeatsLogMessage): the count, when the first record was
// written, and the latest repeat with when it arrived. So the records show
// the first and the latest report of each violation, and every repeat is
// counted. Dedupe has no timer, so summaries are written while reports keep
// arriving: when the same violation comes back after its interval; when
// any report arrives and the last check for finished intervals is at least
// one interval old (so a summary can come up to about one more interval
// late, about two intervals after the first record at worst); and when a
// new violation arrives with the table full. The counts are kept in memory
// only: repeats not yet summarized are not kept across a restart.
//
// At most maxKeys violations are tracked: when a new one arrives with the
// table full, the entries whose interval is over are removed, and if none
// is, the entry with the oldest record is removed early; an entry removed
// with repeats still gets its summary. Memory stays bounded whatever the
// reports hold (each entry holds two bounded violations' worth of text,
// without samples).
// Summary records are not charged to the record limiter: there is at most
// one per record written (an entry starts with a written record and gives
// at most one summary).
//
// A Dedupe is safe for concurrent use. A nil *Dedupe collapses nothing.
type Dedupe struct {
	interval time.Duration
	maxKeys  int
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]*dedupeEntry
	swept   time.Time
}

// dedupeEntry is the state of one violation: when its record was written,
// and the repeats counted since, with the latest of them.
type dedupeEntry struct {
	logged     time.Time
	repeats    int
	latest     Violation
	latestSeen time.Time
}

// repeatSummary describes the repeats of one violation collapsed after its
// record: how many, when that record was written (First), and the latest
// repeat (Latest, without its sample) and when it arrived.
type repeatSummary struct {
	Count      int
	First      time.Time
	Latest     Violation
	LatestSeen time.Time
}

// NewDedupe returns a Dedupe that logs each violation at most once per
// interval and tracks at most maxKeys violations (at least 1).
func NewDedupe(interval time.Duration, maxKeys int) *Dedupe {
	if maxKeys < 1 {
		maxKeys = 1
	}
	return &Dedupe{
		interval: interval,
		maxKeys:  maxKeys,
		now:      time.Now,
		entries:  make(map[string]*dedupeEntry),
	}
}

// verdict is what admit decided for one report.
type verdict int

const (
	// verdictWrite: write the record.
	verdictWrite verdict = iota
	// verdictRepeat: a repeat within the interval; counted, not logged.
	verdictRepeat
	// verdictRefused: the record limiter refused; nothing is recorded, so
	// a later report of the same violation is still logged.
	verdictRefused
	// verdictDropped: not a repeat, but the request may write no more
	// records; nothing is recorded and the limiter is not asked.
	verdictDropped
)

// admit decides whether the report v is written. A report that is not a
// repeat is dropped when canWrite is false; otherwise allowRecord is asked
// (under the Dedupe lock: it must not block or call back into the handler).
// On verdictRefused, retry is the limiter's Retry-After. summaries holds
// the repeat summaries of the violations whose interval was found over, or
// that were removed, during this call; they are returned with every
// verdict.
func (d *Dedupe) admit(v Violation, canWrite bool, allowRecord func() (bool, int)) (vd verdict, summaries []repeatSummary, retry int) {
	if d == nil {
		if !canWrite {
			return verdictDropped, nil, 0
		}
		if ok, retry := allowRecord(); !ok {
			return verdictRefused, nil, retry
		}
		return verdictWrite, nil, 0
	}
	key := v.key()
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if now.Sub(d.swept) >= d.interval {
		d.swept = now
		summaries = d.removeFinished(now, summaries)
	}
	if e, seen := d.entries[key]; seen {
		if now.Sub(e.logged) < d.interval {
			e.repeats++
			e.latest, e.latestSeen = v, now
			// The sample is never kept: it is logged, at most, with the
			// report's own record.
			e.latest.Sample = ""
			return verdictRepeat, summaries, 0
		}
		summaries = d.remove(key, e, summaries)
	}
	if !canWrite {
		return verdictDropped, summaries, 0
	}
	if ok, retry := allowRecord(); !ok {
		return verdictRefused, summaries, retry
	}
	if len(d.entries) >= d.maxKeys {
		summaries = d.removeFinished(now, summaries)
	}
	if len(d.entries) >= d.maxKeys {
		summaries = d.removeOldest(summaries)
	}
	d.entries[key] = &dedupeEntry{logged: now}
	return verdictWrite, summaries, 0
}

// removeFinished removes every entry whose interval is over, appending the
// summaries of those with repeats. d.mu is held.
func (d *Dedupe) removeFinished(now time.Time, summaries []repeatSummary) []repeatSummary {
	for k, e := range d.entries {
		if now.Sub(e.logged) >= d.interval {
			summaries = d.remove(k, e, summaries)
		}
	}
	return summaries
}

// removeOldest removes the entry whose record is the oldest, appending its
// summary when it has repeats. d.mu is held.
func (d *Dedupe) removeOldest(summaries []repeatSummary) []repeatSummary {
	var oldestKey string
	var oldest *dedupeEntry
	for k, e := range d.entries {
		if oldest == nil || e.logged.Before(oldest.logged) || (e.logged.Equal(oldest.logged) && k < oldestKey) {
			oldestKey, oldest = k, e
		}
	}
	if oldest == nil {
		return summaries
	}
	return d.remove(oldestKey, oldest, summaries)
}

// remove removes entry e (key k), appending its summary when it has
// repeats. d.mu is held.
func (d *Dedupe) remove(k string, e *dedupeEntry, summaries []repeatSummary) []repeatSummary {
	delete(d.entries, k)
	if e.repeats > 0 {
		summaries = append(summaries, repeatSummary{Count: e.repeats, First: e.logged, Latest: e.latest, LatestSeen: e.latestSeen})
	}
	return summaries
}

// size returns the number of tracked violations.
func (d *Dedupe) size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.entries)
}

// splitWebURL splits a URL reduced by reduceURL that has an authority
// ("scheme://host/path" or "//host/path") into its origin
// ("scheme://host" or "//host") and its path. ok is false for any other
// value.
func splitWebURL(reduced string) (origin, path string, ok bool) {
	prefix, rest := "", ""
	switch scheme, afterScheme, hasScheme := splitScheme(reduced); {
	case hasScheme && strings.HasPrefix(afterScheme, "//"):
		prefix, rest = scheme+"://", afterScheme[2:]
	case !hasScheme && strings.HasPrefix(reduced, "//"):
		prefix, rest = "//", reduced[2:]
	default:
		return "", "", false
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return prefix + rest[:i], rest[i:], true
	}
	return prefix + rest, "", true
}

// urlOrigin returns the origin of a URL reduced by reduceURL: scheme and
// host for a URL with an authority, and the value itself for a keyword
// (such as "inline" or "trusted-types-sink") or a bare scheme (such as
// "data:"). A path without a host gives "".
func urlOrigin(reduced string) string {
	if origin, _, ok := splitWebURL(reduced); ok {
		return origin
	}
	if strings.Contains(reduced, "/") {
		return ""
	}
	return reduced
}

// documentPath returns the path of the page a report is about: the URL is
// reduced by reduceURL (no user information, query or fragment), then the
// scheme and host are dropped. A URL with an authority and no path gives
// "/"; a bare scheme (such as "about:" or "data:") is kept as is.
func documentPath(documentURL string) string {
	reduced := reduceURL(documentURL)
	if _, path, ok := splitWebURL(reduced); ok {
		if path == "" {
			return "/"
		}
		return path
	}
	return reduced
}
