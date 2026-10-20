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
