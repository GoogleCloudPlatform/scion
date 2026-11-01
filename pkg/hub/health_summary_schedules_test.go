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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

func TestCapRunes(t *testing.T) {
	assert.Equal(t, "", capRunes("", 3))
	assert.Equal(t, "abc", capRunes("abc", 3))
	assert.Equal(t, "abc…", capRunes("abcd", 3))
	// Runes, not bytes: a multi-byte character is never split.
	assert.Equal(t, "ééé…", capRunes("éééé", 3))
	// Invalid UTF-8 is replaced, so the JSON string is always valid.
	assert.Equal(t, "a�b", capRunes("a\xffb", 10))
}

func TestScheduleTarget(t *testing.T) {
	assert.Equal(t, "worker", scheduleTarget(`{"agentName":"worker","agentId":"id-1","message":"body"}`))
	assert.Equal(t, "id-1", scheduleTarget(`{"agentId":"id-1","message":"body"}`))
	assert.Equal(t, "", scheduleTarget(`{"message":"body"}`))
	assert.Equal(t, "", scheduleTarget(`not json`))
	assert.Equal(t, "", scheduleTarget(``))
	long := strings.Repeat("x", healthScheduleLastErrorMaxRunes+5)
	assert.Equal(t, strings.Repeat("x", healthScheduleLastErrorMaxRunes)+"…",
		scheduleTarget(`{"agentName":"`+long+`"}`))
}

func TestScheduleAttention_Items(t *testing.T) {
	sec := &HealthSummarySchedules{
		Failing: []HealthFailingSchedule{
			{ID: "s1", Name: "nightly", ProjectID: "p1", ProjectSlug: "alpha", LastError: "refused <b>x</b>"},
			{ID: "s2", Name: "hourly", ProjectID: "p2"},
		},
		Total:     4,
		Truncated: true,
	}
	assert.Equal(t, []HealthAttentionItem{
		{
			Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
			Subject: HealthAttentionSubject{Type: HealthSubjectSchedule, ID: "s1", Name: "nightly", ProjectID: "p1"},
			Message: "Scheduled event alpha / nightly failed its last run",
		},
		{
			Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
			Subject: HealthAttentionSubject{Type: HealthSubjectSchedule, ID: "s2", Name: "hourly", ProjectID: "p2"},
			Message: "Scheduled event hourly failed its last run",
		},
		{
			Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
			Subject: HealthAttentionSubject{Type: HealthSubjectSchedules},
			Message: "2 more scheduled events failed their last run",
		},
	}, scheduleAttention(sec))
}

// The stored error text never reaches an attention message.
func TestScheduleAttention_MessageOmitsLastError(t *testing.T) {
	sec := &HealthSummarySchedules{
		Failing: []HealthFailingSchedule{{ID: "s1", Name: "n", ProjectID: "p", LastError: "stored-error-text"}},
		Total:   1,
	}
	for _, it := range scheduleAttention(sec) {
		assert.NotContains(t, it.Message, "stored-error-text")
	}
}

// When the scan was limited, Total is a lower bound and the extra item
// says so.
func TestScheduleAttention_ScanLimited(t *testing.T) {
	more := HealthAttentionItem{
		Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
		Subject: HealthAttentionSubject{Type: HealthSubjectSchedules},
	}
	sec := &HealthSummarySchedules{
		Failing: []HealthFailingSchedule{{ID: "s1", Name: "n", ProjectID: "p"}},
		Total:   2, Truncated: true, ScanLimited: true,
	}
	items := scheduleAttention(sec)
	assert.Len(t, items, 2)
	more.Message = "At least 1 more scheduled event failed its last run"
	assert.Equal(t, more, items[1])

	sec.Total = 1
	items = scheduleAttention(sec)
	assert.Len(t, items, 2)
	more.Message = "More scheduled events may have failed their last run"
	assert.Equal(t, more, items[1])
}

func TestScheduleAttention_NotReported(t *testing.T) {
	assert.Equal(t, []HealthAttentionItem{{
		Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
		Subject: HealthAttentionSubject{Type: HealthSubjectSchedules},
		Message: "Scheduled event data not available",
	}}, scheduleAttention(nil))
	assert.Empty(t, scheduleAttention(&HealthSummarySchedules{Failing: []HealthFailingSchedule{}}))
}

func TestHealthScheduleReasonCode(t *testing.T) {
	assert.Equal(t, "creator_deleted", healthScheduleReasonCode(store.ScheduleStatusReasonCreatorDeleted))
	assert.Equal(t, "cron_zone_unsupported", healthScheduleReasonCode(store.ScheduleStatusReasonCronZoneUnsupported))
	assert.Equal(t, "other", healthScheduleReasonCode("some_later_reason"))
	assert.Equal(t, "other", healthScheduleReasonCode("<b>markup</b>"))
	assert.Equal(t, "other", healthScheduleReasonCode(""))
}

func TestSortPausedSchedules(t *testing.T) {
	t1 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	list := []store.Schedule{
		{ID: "b", UpdatedAt: t1},
		{ID: "c", UpdatedAt: t2},
		{ID: "a", UpdatedAt: t1},
		{ID: "d"},
	}
	sortPausedSchedules(list)
	ids := []string{}
	for _, sc := range list {
		ids = append(ids, sc.ID)
	}
	assert.Equal(t, []string{"c", "a", "b", "d"}, ids)
}

// The filter re-check keeps only paused schedules with a reason.
func TestIsSystemPausedSchedule(t *testing.T) {
	assert.True(t, isSystemPausedSchedule(store.Schedule{Status: store.ScheduleStatusPaused, StatusReason: store.ScheduleStatusReasonCreatorDeleted}))
	assert.True(t, isSystemPausedSchedule(store.Schedule{Status: store.ScheduleStatusPaused, StatusReason: "some_later_reason"}))
	assert.False(t, isSystemPausedSchedule(store.Schedule{Status: store.ScheduleStatusPaused}))
	assert.False(t, isSystemPausedSchedule(store.Schedule{Status: store.ScheduleStatusActive, StatusReason: store.ScheduleStatusReasonCreatorDeleted}))
}

// Each hub-paused schedule gets a fixed sentence for its reason, after the
// failing items; an unknown reason gets the generic sentence.
func TestScheduleAttention_PausedItems(t *testing.T) {
	sec := &HealthSummarySchedules{
		Failing: []HealthFailingSchedule{{ID: "f1", Name: "failing", ProjectID: "p1"}},
		Total:   1,
		Paused: []HealthPausedSchedule{
			{ID: "s1", Name: "nightly", ProjectID: "p1", ProjectSlug: "alpha", Reason: "creator_deleted"},
			{ID: "s2", Name: "zoned", ProjectID: "p2", Reason: "cron_zone_unsupported"},
			{ID: "s3", Name: "later", ProjectID: "p2", Reason: "other"},
			{ID: "s4", Name: "raw", ProjectID: "p2", Reason: "<b>stored</b>"},
		},
		PausedTotal: 6, PausedTruncated: true,
	}
	sub := func(id, name, project string) HealthAttentionSubject {
		return HealthAttentionSubject{Type: HealthSubjectSchedule, ID: id, Name: name, ProjectID: project}
	}
	w := func(s HealthAttentionSubject, msg string) HealthAttentionItem {
		return HealthAttentionItem{Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule, Subject: s, Message: msg}
	}
	assert.Equal(t, []HealthAttentionItem{
		w(sub("f1", "failing", "p1"), "Scheduled event failing failed its last run"),
		w(sub("s1", "nightly", "p1"), "Scheduled event alpha / nightly was paused by the hub: the agent or user it runs under was deleted"),
		w(sub("s2", "zoned", "p2"), "Scheduled event zoned was paused by the hub: its cron expression names a time zone, which is not supported"),
		w(sub("s3", "later", "p2"), "Scheduled event later was paused by the hub: a reason this hub does not recognize"),
		w(sub("s4", "raw", "p2"), "Scheduled event raw was paused by the hub: a reason this hub does not recognize"),
		w(HealthAttentionSubject{Type: HealthSubjectSchedules}, "2 more scheduled events were paused by the hub"),
	}, scheduleAttention(sec))
}

// When the paused scan was limited, PausedTotal is a lower bound and the
// extra item says so.
func TestScheduleAttention_PausedScanLimited(t *testing.T) {
	sec := &HealthSummarySchedules{
		Paused:      []HealthPausedSchedule{{ID: "s1", Name: "n", ProjectID: "p", Reason: "creator_deleted"}},
		PausedTotal: 2, PausedTruncated: true, PausedScanLimited: true,
	}
	items := scheduleAttention(sec)
	assert.Len(t, items, 2)
	assert.Equal(t, "At least 1 more scheduled event was paused by the hub", items[1].Message)
	assert.Equal(t, HealthSubjectSchedules, items[1].Subject.Type)

	sec.PausedTotal = 1
	items = scheduleAttention(sec)
	assert.Len(t, items, 2)
	assert.Equal(t, "More scheduled events may have been paused by the hub", items[1].Message)
}

// When only the paused list could not be read, the failing items stay and
// one warning replaces the paused items.
func TestScheduleAttention_PausedUnavailable(t *testing.T) {
	sec := &HealthSummarySchedules{
		Failing:           []HealthFailingSchedule{{ID: "f1", Name: "failing", ProjectID: "p1"}},
		Total:             1,
		PausedUnavailable: true,
	}
	assert.Equal(t, []HealthAttentionItem{
		{
			Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
			Subject: HealthAttentionSubject{Type: HealthSubjectSchedule, ID: "f1", Name: "failing", ProjectID: "p1"},
			Message: "Scheduled event failing failed its last run",
		},
		{
			Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
			Subject: HealthAttentionSubject{Type: HealthSubjectSchedules},
			Message: "Hub-paused scheduled event data not available",
		},
	}, scheduleAttention(sec))
}

// Failing schedules add attention items after the agent items and never
// change the overall status: the section depends on the caller.
func TestDeriveHealthSummaryStatus_FailingSchedulesWarnOnly(t *testing.T) {
	r := healthyPolicyResp()
	r.Schedules = &HealthSummarySchedules{
		Failing: []HealthFailingSchedule{{ID: "s1", Name: "nightly", ProjectID: "p1"}},
		Total:   1,
	}
	status, items := deriveHealthSummaryStatus(r)
	assert.Equal(t, HealthStatusHealthy, status)
	assert.Equal(t, []HealthAttentionItem{{
		Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
		Subject: HealthAttentionSubject{Type: HealthSubjectSchedule, ID: "s1", Name: "nightly", ProjectID: "p1"},
		Message: "Scheduled event nightly failed its last run",
	}}, items)

	r.Schedules = nil
	status, items = deriveHealthSummaryStatus(r)
	assert.Equal(t, HealthStatusHealthy, status)
	assert.Len(t, items, 1)
	assert.Equal(t, "Scheduled event data not available", items[0].Message)
}
