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

package telemetrycontract

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/labels"
)

var (
	lcEventTS    = time.Date(2026, 10, 22, 0, 0, 0, 1000, time.UTC)
	lcObservedTS = time.Date(2026, 10, 22, 0, 0, 0, 2000, time.UTC)
)

// lifecycleBoolPtr is a *bool helper for the lifecycle tests (named apart from
// the usage-event tests' boolp in the same package).
func lifecycleBoolPtr(b bool) *bool { return &b }

// lcBase returns an envelope-complete event with the given transition and
// revisions 1/1; it carries no other payload.
func lcBase(transition string) *LifecycleEvent {
	ev := &LifecycleEvent{
		Envelope:           NewEnvelope(EventTypeLifecycle, "6f1c0000-0000-4000-8000-000000000001", "scion-hub/hub-a", lcEventTS, lcObservedTS),
		Transition:         transition,
		TransitionRevision: 1,
		MetadataRevision:   1,
	}
	ev.HubID = "hub-a"
	ev.ProjectID = "p1"
	ev.AgentID = "a1"
	ev.IncarnationID = "1"
	ev.ProducerBuild = "v0.0.0-test"
	return ev
}

// minimalLifecycleEvent returns the smallest valid event of each
// transition (WP0 r7 F1): snapshot transitions get an empty snapshot, and
// transitions with a required field get just that.
func minimalLifecycleEvent(transition string) *LifecycleEvent {
	ev := lcBase(transition)
	if IsSnapshotTransition(transition) {
		ev.Metadata = &LifecycleMetadata{Labels: Labels{}}
	}
	switch transition {
	case TransitionLaunchFailed:
		ev.EndReason = EndReasonFailed
	case TransitionStopped:
		ev.StopKind = StopKindStopped
	case TransitionDeleted:
		ev.DeleteMode = DeleteModeHard
	case TransitionResynced:
		ev.Cause = CauseNoCursor
	}
	return ev
}

var allTransitions = []string{
	TransitionCreated, TransitionLaunchAccepted, TransitionConfigured, TransitionStarted,
	TransitionLaunchFailed, TransitionMetadataChanged, TransitionStopped, TransitionHalted,
	TransitionReincarnated, TransitionDeleted, TransitionRestored, TransitionResynced,
}

func TestLifecycleTransitionSetIsComplete(t *testing.T) {
	if len(allTransitions) != len(lifecycleTransitions) {
		t.Fatalf("test lists %d transitions, contract has %d", len(allTransitions), len(lifecycleTransitions))
	}
	for _, tr := range allTransitions {
		if !IsLifecycleTransition(tr) {
			t.Errorf("%q is not in the transition enum", tr)
		}
	}
}

// TestLifecycleMinimalEventOfEachTransitionMarshals marshals a minimally
// populated event of every transition (WP0 r7 F1) and round trips it.
func TestLifecycleMinimalEventOfEachTransitionMarshals(t *testing.T) {
	for _, tr := range allTransitions {
		t.Run(tr, func(t *testing.T) {
			ev := minimalLifecycleEvent(tr)
			data, err := Marshal(ev)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if IsSnapshotTransition(tr) && !strings.Contains(string(data), `"metadata":{"labels":[]}`) {
				t.Errorf("snapshot must write labels as []: %s", data)
			}
			if !IsSnapshotTransition(tr) && strings.Contains(string(data), `"metadata"`) {
				t.Errorf("non-snapshot event wrote metadata: %s", data)
			}
			var got LifecycleEvent
			if err := Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(&got, ev) {
				t.Errorf("round trip mismatch:\n got %+v\nwant %+v", got, *ev)
			}
			again, err := Marshal(&got)
			if err != nil || string(again) != string(data) {
				t.Errorf("re-marshal differs (err %v):\n%s\n%s", err, again, data)
			}
		})
	}
}

// TestLifecycleGoldenCreated pins the exact bytes of the design 3.4 golden
// (with a full event ID): key order is declaration order, unknown values
// are omitted, labels are bytewise sorted and not HTML-escaped.
func TestLifecycleGoldenCreated(t *testing.T) {
	ev := lcBase(TransitionCreated)
	ev.Metadata = &LifecycleMetadata{
		Name:                  "a1",
		Slug:                  "a1",
		RequestedTemplate:     "dev",
		RequestedTemplateID:   "t1",
		RequestedTemplateHash: "sha256:ab",
		RequestedModel:        "m-large-1",
		RequestedModelAlias:   "large",
		Harness:               "claude",
		CreatorPrincipalType:  "user",
		CreatorPrincipalID:    "u1",
		Labels:                LabelsFromMap(map[string]string{"stage": "implement", "x/y.z-1": "a<b>&c", "empty": ""}),
	}
	const want = `{"schema_version":1,"event_type":"agent.lifecycle","event_id":"6f1c0000-0000-4000-8000-000000000001",` +
		`"producer_namespace":"scion-hub/hub-a","event_ts":"2026-10-22T00:00:00.000001Z","observed_ts":"2026-10-22T00:00:00.000002Z",` +
		`"hub_id":"hub-a","project_id":"p1","agent_id":"a1","incarnation_id":"1","producer_build":"v0.0.0-test",` +
		`"transition":"created","transition_revision":1,"metadata_revision":1,` +
		`"metadata":{"name":"a1","slug":"a1","requested_template":"dev","requested_template_id":"t1",` +
		`"requested_template_hash":"sha256:ab","requested_model":"m-large-1","requested_model_alias":"large",` +
		`"harness":"claude","creator_principal_type":"user","creator_principal_id":"u1",` +
		`"labels":[{"key":"empty","value":""},{"key":"stage","value":"implement"},{"key":"x/y.z-1","value":"a<b>&c"}]}}`
	data, err := Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(data) != want {
		t.Errorf("golden mismatch:\n got %s\nwant %s", data, want)
	}
}

func TestLifecycleValidateRules(t *testing.T) {
	i64 := func(n int64) *int64 { return &n }
	cases := []struct {
		name    string
		mutate  func(e *LifecycleEvent)
		tr      string
		wantErr string // "" = valid
	}{
		{"wrong event type", func(e *LifecycleEvent) { e.EventType = EventTypeUsage }, TransitionStarted, "event_type"},
		{"unknown transition", func(e *LifecycleEvent) { e.Transition = "exploded" }, TransitionStarted, "not a known lifecycle transition"},
		{"zero transition revision", func(e *LifecycleEvent) { e.TransitionRevision = 0 }, TransitionStarted, "transition_revision"},
		{"zero metadata revision", func(e *LifecycleEvent) { e.MetadataRevision = 0 }, TransitionStarted, "metadata_revision"},
		{"snapshot without metadata", func(e *LifecycleEvent) { e.Metadata = nil }, TransitionCreated, "metadata is required"},
		{"deleted without metadata", func(e *LifecycleEvent) { e.Metadata = nil }, TransitionDeleted, "metadata is required"},
		{"metadata on non-snapshot", func(e *LifecycleEvent) { e.Metadata = &LifecycleMetadata{Labels: Labels{}} }, TransitionStarted, "only carried by snapshot"},
		{"unsorted labels", func(e *LifecycleEvent) {
			e.Metadata.Labels = Labels{{Key: "b"}, {Key: "a"}}
		}, TransitionCreated, "out of order"},
		{"duplicate labels", func(e *LifecycleEvent) {
			e.Metadata.Labels = Labels{{Key: "a"}, {Key: "a"}}
		}, TransitionCreated, "duplicate label key"},
		{"stopped without stop_kind", func(e *LifecycleEvent) { e.StopKind = "" }, TransitionStopped, "stop_kind is required"},
		{"unknown stop_kind", func(e *LifecycleEvent) { e.StopKind = "melted" }, TransitionStopped, "not a known value"},
		{"stop_kind elsewhere", func(e *LifecycleEvent) { e.StopKind = StopKindStopped }, TransitionStarted, "stop_kind is only allowed"},
		{"launch_failed without end_reason", func(e *LifecycleEvent) { e.EndReason = "" }, TransitionLaunchFailed, "end_reason is required"},
		{"unknown end_reason", func(e *LifecycleEvent) { e.EndReason = "bored" }, TransitionLaunchFailed, "not a known value"},
		{"deleted without delete_mode", func(e *LifecycleEvent) { e.DeleteMode = "" }, TransitionDeleted, "delete_mode is required"},
		{"unknown delete_mode", func(e *LifecycleEvent) { e.DeleteMode = "shredded" }, TransitionDeleted, "not a known value"},
		{"purged delete_mode", func(e *LifecycleEvent) { e.DeleteMode = DeleteModePurged }, TransitionDeleted, ""},
		{"delete_mode elsewhere", func(e *LifecycleEvent) { e.DeleteMode = DeleteModeSoft }, TransitionCreated, "delete_mode is only allowed"},
		{"resynced without cause", func(e *LifecycleEvent) { e.Cause = "" }, TransitionResynced, "not a resynced cause"},
		{"resynced unobserved without gap", func(e *LifecycleEvent) { e.Cause = CauseUnobservedChange }, TransitionResynced, "gap must be true"},
		{"resynced unobserved gap false", func(e *LifecycleEvent) { e.Cause = CauseUnobservedChange; e.Gap = lifecycleBoolPtr(false) }, TransitionResynced, "gap must be true"},
		{"resynced unobserved gap true", func(e *LifecycleEvent) { e.Cause = CauseUnobservedChange; e.Gap = lifecycleBoolPtr(true) }, TransitionResynced, ""},
		{"gap on no_cursor", func(e *LifecycleEvent) { e.Gap = lifecycleBoolPtr(true) }, TransitionResynced, "gap is only allowed"},
		{"gap on deleted", func(e *LifecycleEvent) { e.Gap = lifecycleBoolPtr(true) }, TransitionDeleted, "gap is only allowed"},
		{"cause on created", func(e *LifecycleEvent) { e.Cause = "x" }, TransitionCreated, "cause is not allowed"},
		{"bad stopped cause", func(e *LifecycleEvent) { e.Cause = CauseNoCursor }, TransitionStopped, "not a stopped cause"},
		{"started run_uncorrelated", func(e *LifecycleEvent) { e.Cause = CauseRunUncorrelated }, TransitionStarted, ""},
		{"run_uncorrelated on stopped", func(e *LifecycleEvent) { e.Cause = CauseRunUncorrelated }, TransitionStopped, "not a stopped cause"},
		{"broker cause on launch_failed", func(e *LifecycleEvent) { e.Cause = "image_pull_backoff" }, TransitionLaunchFailed, ""},
		{"superseded needs segment revision", func(e *LifecycleEvent) { e.StopKind = StopKindSuperseded }, TransitionStopped, "segment_metadata_revision is required"},
		{"superseded with segment revision", func(e *LifecycleEvent) {
			e.StopKind = StopKindSuperseded
			e.SegmentMetadataRevision = i64(1)
			e.RunChangedAt = "2026-10-22T00:00:00.5Z"
		}, TransitionStopped, ""},
		{"stop leg needs segment revision", func(e *LifecycleEvent) { e.Cause = CauseRestartStopLeg }, TransitionStopped, "segment_metadata_revision is required"},
		{"segment revision zero", func(e *LifecycleEvent) { e.SegmentMetadataRevision = i64(0) }, TransitionDeleted, "segment_metadata_revision = 0"},
		{"segment revision on deleted", func(e *LifecycleEvent) { e.SegmentMetadataRevision = i64(2) }, TransitionDeleted, ""},
		{"segment revision on start_unobserved", func(e *LifecycleEvent) {
			e.Cause = CauseStartUnobserved
			e.SegmentMetadataRevision = i64(1)
		}, TransitionStopped, "segment_metadata_revision is only allowed"},
		{"segment revision on started", func(e *LifecycleEvent) { e.SegmentMetadataRevision = i64(1) }, TransitionStarted, "segment_metadata_revision is only allowed"},
		{"run_changed_at on plain stop", func(e *LifecycleEvent) { e.RunChangedAt = "2026-10-22T00:00:00Z" }, TransitionStopped, "run_changed_at is only allowed"},
		{"run_changed_at non-canonical", func(e *LifecycleEvent) {
			e.StopKind = StopKindSuperseded
			e.SegmentMetadataRevision = i64(1)
			e.RunChangedAt = "2026-10-22T00:00:00.500Z"
		}, TransitionStopped, "canonical"},
		{"adoption complete", func(e *LifecycleEvent) {
			e.AdoptedFromLaunchID = "r1"
			e.AdoptedAt = "2026-10-22T00:00:00Z"
			e.AdoptionSource = AdoptionSourceCAS
		}, TransitionStarted, ""},
		{"adoption missing source", func(e *LifecycleEvent) {
			e.AdoptedFromLaunchID = "r1"
			e.AdoptedAt = "2026-10-22T00:00:00Z"
		}, TransitionStarted, "are required with adopted_from_launch_id"},
		{"adoption fields without id", func(e *LifecycleEvent) { e.AdoptionSource = AdoptionSourceCAS }, TransitionStarted, "only allowed with adopted_from_launch_id"},
		{"adoption unknown source", func(e *LifecycleEvent) {
			e.AdoptedFromLaunchID = "r1"
			e.AdoptedAt = "2026-10-22T00:00:00Z"
			e.AdoptionSource = "guess"
		}, TransitionStarted, "adoption_source"},
		{"adopted_at non-canonical", func(e *LifecycleEvent) {
			e.AdoptedFromLaunchID = "r1"
			e.AdoptedAt = "2026-10-22T01:00:00+01:00"
			e.AdoptionSource = AdoptionSourceCAS
		}, TransitionStarted, "adopted_at"},
		{"dispatch_failed names a launch", func(e *LifecycleEvent) {
			e.EndReason = EndReasonDispatchFailed
			e.LaunchID = "r1"
		}, TransitionLaunchFailed, "launch_id must be omitted"},
		{"cancelled without launch", func(e *LifecycleEvent) { e.EndReason = EndReasonCancelled }, TransitionLaunchFailed, ""},
		{"create_rolled_back with launch", func(e *LifecycleEvent) {
			e.EndReason = EndReasonCreateRolledBack
			e.LaunchID = "r1"
		}, TransitionLaunchFailed, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := minimalLifecycleEvent(tc.tr)
			tc.mutate(ev)
			err := ev.Validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("Validate: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("Validate passed, want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("Validate error %q does not contain %q", err, tc.wantErr)
			}
			if tc.wantErr != "" {
				if _, mErr := Marshal(ev); mErr == nil {
					t.Fatalf("Marshal accepted an invalid event")
				}
			}
		})
	}
}

// TestLifecycleLabelRoundTrip covers the serializer for punctuation,
// empty values and Unicode (Unicode is contract-only: the API refuses
// non-ASCII labels).
func TestLifecycleLabelRoundTrip(t *testing.T) {
	in := map[string]string{
		`p/u.n-c_t`: `< > & " \ ' :`,
		"empty":     "",
		"ключ":      "значение",
		"emoji-🙂":   "日本語",
		"A-upper":   "Mixed Case",
		" lead":     "space ",
		"z sep":     "line sep",
	}
	ev := minimalLifecycleEvent(TransitionCreated)
	ev.Metadata.Labels = LabelsFromMap(in)
	data, err := Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), `\u003c`) || !strings.Contains(string(data), `< > &`) {
		t.Errorf("labels were HTML-escaped: %s", data)
	}
	var got LifecycleEvent
	if err := Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got.Metadata.Labels.Map(), in) {
		t.Errorf("labels changed in round trip:\n got %q\nwant %q", got.Metadata.Labels.Map(), in)
	}
	for i := 1; i < len(got.Metadata.Labels); i++ {
		if got.Metadata.Labels[i-1].Key >= got.Metadata.Labels[i].Key {
			t.Errorf("labels not in bytewise order at %d", i)
		}
	}
}

func TestLifecycleRejectsInvalidUTF8(t *testing.T) {
	ev := minimalLifecycleEvent(TransitionCreated)
	ev.Metadata.Name = "bad\xff"
	if _, err := Marshal(ev); err == nil {
		t.Fatal("Marshal accepted invalid UTF-8 in metadata")
	}
	ev = minimalLifecycleEvent(TransitionCreated)
	ev.Metadata.Labels = Labels{{Key: "k", Value: "bad\xfe"}}
	if _, err := Marshal(ev); err == nil {
		t.Fatal("Marshal accepted invalid UTF-8 in a label")
	}
}

// TestLifecycleWorstCaseEventFitsBudget is the design section 6 guard: a
// complete event with the API maximum of labels, each of maximum length
// and made of the characters that escape to two bytes, stays within
// 32 KiB. Raising the label limits fails this test and forces a chunking
// design instead of silent truncation.
func TestLifecycleWorstCaseEventFitsBudget(t *testing.T) {
	const budget = 32 << 10
	long := func(n int, prefix string) string {
		s := prefix + strings.Repeat(`"\`, n)
		return s[:n]
	}
	m := map[string]string{}
	for i := 0; i < labels.MaxLabels; i++ {
		m[long(labels.MaxKeyLen, string(rune('a'+i)))] = long(labels.MaxValueLen, "")
	}
	if err := labels.Validate(m); err != nil {
		t.Fatalf("worst-case labels are not API-valid: %v", err)
	}
	f := strings.Repeat("x", 256) // generous bound for every other string
	ev := lcBase(TransitionDeleted)
	ev.EventID = f
	ev.ProducerNamespace = "scion-hub/" + f
	ev.HubID, ev.ProjectID, ev.AgentID, ev.LaunchID, ev.BrokerID, ev.ProducerBuild, ev.HarnessVersion = f, f, f, f, f, f, f
	ev.IncarnationID = "9007199254740991"
	ev.TransitionRevision, ev.MetadataRevision = MaxSafeInteger, MaxSafeInteger
	ev.AsyncLaunchID, ev.LaunchOwner = f, f
	ev.DeleteMode = DeleteModeProjectDeleted
	ev.SegmentMetadataRevision = func(n int64) *int64 { return &n }(MaxSafeInteger)
	ev.AdoptedFromLaunchID, ev.AdoptedAt, ev.AdoptionSource = f, "2026-10-22T00:00:00.123456Z", AdoptionSourceHeartbeatAdopt
	ev.Metadata = &LifecycleMetadata{
		Name: f, Slug: f, RequestedTemplate: f, RequestedTemplateID: f, RequestedTemplateHash: f,
		RequestedModel: f, RequestedModelAlias: f, Harness: f, HarnessConfig: f, Profile: f, Runtime: f,
		CreatorPrincipalType: f, CreatorPrincipalID: f, ParentAgentID: f,
		HarnessConfigSource: f,
		Labels:              LabelsFromMap(m),
	}
	// The six broker-echoed effective fields (WP1 slice 3 addendum v12
	// section 4): the hub accepts up to 256 bytes of printable UTF-8 each,
	// so the worst case is 256 quotes and backslashes, every one escaped.
	esc := long(256, "")
	md := ev.Metadata
	md.EffectiveTemplate, md.EffectiveTemplateHash, md.EffectiveModel = esc, esc, esc
	md.EffectiveHarness, md.HarnessConfigRevision, md.EffectiveBrokerBuild = esc, esc, esc
	data, err := Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(data) > budget {
		t.Fatalf("worst-case lifecycle event is %d bytes, over the %d-byte budget", len(data), budget)
	}
	t.Logf("worst-case lifecycle event: %d bytes", len(data))
}

// TestLifecycleGoldenPerTransition pins the exact bytes of the minimal
// event of every transition (design section 8: a golden per transition).
func TestLifecycleGoldenPerTransition(t *testing.T) {
	const env = `{"schema_version":1,"event_type":"agent.lifecycle","event_id":"6f1c0000-0000-4000-8000-000000000001",` +
		`"producer_namespace":"scion-hub/hub-a","event_ts":"2026-10-22T00:00:00.000001Z","observed_ts":"2026-10-22T00:00:00.000002Z",` +
		`"hub_id":"hub-a","project_id":"p1","agent_id":"a1","incarnation_id":"1","producer_build":"v0.0.0-test",`
	const revs = `"transition_revision":1,"metadata_revision":1`
	const snap = `,"metadata":{"labels":[]}}`
	golden := map[string]string{
		TransitionCreated:         `"transition":"created",` + revs + snap,
		TransitionLaunchAccepted:  `"transition":"launch_accepted",` + revs + `}`,
		TransitionConfigured:      `"transition":"configured",` + revs + snap,
		TransitionStarted:         `"transition":"started",` + revs + `}`,
		TransitionLaunchFailed:    `"transition":"launch_failed",` + revs + `,"end_reason":"failed"}`,
		TransitionMetadataChanged: `"transition":"metadata_changed",` + revs + snap,
		TransitionStopped:         `"transition":"stopped",` + revs + `,"stop_kind":"stopped"}`,
		TransitionHalted:          `"transition":"halted",` + revs + `}`,
		TransitionReincarnated:    `"transition":"reincarnated",` + revs + snap,
		TransitionDeleted:         `"transition":"deleted",` + revs + `,"delete_mode":"hard"` + snap,
		TransitionRestored:        `"transition":"restored",` + revs + snap,
		TransitionResynced:        `"transition":"resynced",` + revs + `,"cause":"no_cursor"` + snap,
	}
	for _, tr := range allTransitions {
		want, ok := golden[tr]
		if !ok {
			t.Fatalf("no golden for %s", tr)
		}
		data, err := Marshal(minimalLifecycleEvent(tr))
		if err != nil {
			t.Fatalf("%s: Marshal: %v", tr, err)
		}
		if got := string(data); got != env+want {
			t.Errorf("%s golden mismatch:\n got %s\nwant %s", tr, got, env+want)
		}
	}
}
