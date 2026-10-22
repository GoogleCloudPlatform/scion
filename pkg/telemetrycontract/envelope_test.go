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
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testEvent is a minimal event type built on Envelope the way lifecycle
// and usage types are: embedded envelope, typed payload fields, nullable
// integers as *int64 with omitempty.
type testEvent struct {
	Envelope
	Count  int64  `json:"count"`
	Maybe  *int64 `json:"maybe,omitempty"`
	Labels Labels `json:"labels"`
}

func (e *testEvent) Validate() error {
	if err := e.Envelope.Validate(); err != nil {
		return err
	}
	return e.Labels.Validate()
}

type floatEvent struct {
	Envelope
	Ratio float64 `json:"ratio"`
}

func (e *floatEvent) Validate() error { return e.Envelope.Validate() }

func int64p(n int64) *int64 { return &n }

var (
	testEventTS    = time.Date(2026, 10, 22, 0, 5, 0, 0, time.UTC)
	testObservedTS = time.Date(2026, 10, 22, 0, 20, 0, 123456789, time.UTC)
)

func fullTestEvent() *testEvent {
	env := NewEnvelope(EventTypeUsage, "u1", "mock-hub/agent-a/inc-1", testEventTS, testObservedTS)
	env.HubID = "mock-hub"
	env.ProjectID = "mock-project"
	env.AgentID = "agent-a"
	env.IncarnationID = "inc-1"
	env.LaunchID = "launch-1"
	env.BrokerID = "mock-broker"
	env.ProducerBuild = "mock-v0"
	env.HarnessVersion = "1.2.3"
	return &testEvent{
		Envelope: env,
		Count:    3,
		Maybe:    int64p(0),
		Labels: LabelsFromMap(map[string]string{
			"stage":      "implement",
			"custom/key": "a<b>&c",
			"empty":      "",
			"空":          "値",
		}),
	}
}

// The golden form pins field order (envelope first, declaration order),
// microsecond truncation, unescaped HTML punctuation and raw UTF-8.
const fullTestEventGolden = `{"schema_version":1,"event_type":"agent.usage","event_id":"u1","producer_namespace":"mock-hub/agent-a/inc-1","event_ts":"2026-10-22T00:05:00Z","observed_ts":"2026-10-22T00:20:00.123456Z","hub_id":"mock-hub","project_id":"mock-project","agent_id":"agent-a","incarnation_id":"inc-1","launch_id":"launch-1","broker_id":"mock-broker","producer_build":"mock-v0","harness_version":"1.2.3","count":3,"maybe":0,"labels":[{"key":"custom/key","value":"a<b>&c"},{"key":"empty","value":""},{"key":"stage","value":"implement"},{"key":"空","value":"値"}]}`

func TestMarshalGolden(t *testing.T) {
	got, err := Marshal(fullTestEvent())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != fullTestEventGolden {
		t.Errorf("Marshal mismatch\n got: %s\nwant: %s", got, fullTestEventGolden)
	}
	// Deterministic: map iteration order in LabelsFromMap must not leak.
	for i := 0; i < 20; i++ {
		again, err := Marshal(fullTestEvent())
		if err != nil || string(again) != fullTestEventGolden {
			t.Fatalf("Marshal not deterministic on run %d: %s (%v)", i, again, err)
		}
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	want := fullTestEvent()
	data, err := Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got testEvent
	if err := Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(&got, want) {
		t.Errorf("round trip mismatch\n got: %+v\nwant: %+v", got, *want)
	}
	again, err := Marshal(&got)
	if err != nil || string(again) != string(data) {
		t.Errorf("re-marshal differs: %s (%v)", again, err)
	}
}

// Unknown identity fields and unknown integers are omitted; a known zero
// integer is written; an empty label set is [] (known, empty), never null.
func TestMarshalOmitsUnknowns(t *testing.T) {
	ev := &testEvent{Envelope: NewEnvelope(EventTypeHealth, "h1", "ns", testEventTS, testEventTS)}
	got, err := Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `{"schema_version":1,"event_type":"telemetry.health","event_id":"h1","producer_namespace":"ns","event_ts":"2026-10-22T00:05:00Z","observed_ts":"2026-10-22T00:05:00Z","count":0,"labels":[]}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if strings.Contains(string(got), "null") {
		t.Errorf("unknowns must be omitted, never null: %s", got)
	}

	var back testEvent
	if err := Unmarshal(got, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Maybe != nil {
		t.Errorf("omitted integer decoded as %d, want nil (unknown)", *back.Maybe)
	}
	ev.Maybe = int64p(0)
	withZero, _ := Marshal(ev)
	if !strings.Contains(string(withZero), `"maybe":0`) {
		t.Errorf("known zero must be written: %s", withZero)
	}
}

func TestEnvelopeValidate(t *testing.T) {
	valid := func() Envelope {
		return NewEnvelope(EventTypeLifecycle, "e", "ns", testEventTS, testEventTS)
	}
	cases := map[string]func(*Envelope){
		"schema version 0":  func(e *Envelope) { e.SchemaVersion = 0 },
		"schema version 2":  func(e *Envelope) { e.SchemaVersion = 2 },
		"unknown type":      func(e *Envelope) { e.EventType = "agent.other" },
		"empty event id":    func(e *Envelope) { e.EventID = "" },
		"empty namespace":   func(e *Envelope) { e.ProducerNamespace = "" },
		"empty event_ts":    func(e *Envelope) { e.EventTS = "" },
		"bad observed_ts":   func(e *Envelope) { e.ObservedTS = "2026-10-22 00:00:00" },
		"epoch number time": func(e *Envelope) { e.EventTS = "1761091200" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := valid()
			mutate(&e)
			if err := e.Validate(); err == nil {
				t.Error("Validate() = nil, want error")
			}
			if _, err := Marshal(&testEvent{Envelope: e}); err == nil {
				t.Error("Marshal accepted an invalid envelope")
			}
		})
	}
	e := valid()
	if err := e.Validate(); err != nil {
		t.Errorf("valid envelope: %v", err)
	}
	for _, et := range []string{EventTypeLifecycle, EventTypeUsage, EventTypeHealth} {
		if !ValidEventType(et) {
			t.Errorf("ValidEventType(%q) = false", et)
		}
	}
}

func TestMarshalRejectsNonIntegerAndUnsafeNumerics(t *testing.T) {
	env := NewEnvelope(EventTypeUsage, "u", "ns", testEventTS, testEventTS)
	if _, err := Marshal(&floatEvent{Envelope: env, Ratio: 1}); err == nil {
		t.Error("Marshal accepted a float field")
	}
	if _, err := Marshal(&testEvent{Envelope: env, Count: MaxSafeInteger}); err != nil {
		t.Errorf("Marshal rejected MaxSafeInteger: %v", err)
	}
	if _, err := Marshal(&testEvent{Envelope: env, Count: MaxSafeInteger + 1}); err == nil {
		t.Error("Marshal accepted an integer beyond MaxSafeInteger")
	}
	if _, err := Marshal(&testEvent{Envelope: env, Maybe: int64p(-MaxSafeInteger - 1)}); err == nil {
		t.Error("Marshal accepted a negative integer beyond -MaxSafeInteger")
	}
	var nilEvent *testEvent
	if _, err := Marshal(nilEvent); err == nil {
		t.Error("Marshal accepted a nil event")
	}
}

func TestUnmarshalRejectsFractionalIntegers(t *testing.T) {
	base := `{"schema_version":1,"event_type":"agent.usage","event_id":"u","producer_namespace":"ns","event_ts":"2026-10-22T00:00:00Z","observed_ts":"2026-10-22T00:00:00Z","labels":[],`
	for _, tail := range []string{`"count":1.5}`, `"count":1e3}`, `"maybe":"7"}`, `"count":9007199254740992}`} {
		var ev testEvent
		if err := Unmarshal([]byte(base+tail), &ev); err == nil {
			t.Errorf("Unmarshal accepted %s", tail)
		}
	}
	var ev testEvent
	if err := Unmarshal([]byte(base+`"count":7,"future_field":{"x":1}}`), &ev); err != nil || ev.Count != 7 {
		t.Errorf("Unmarshal must ignore unknown fields: count=%d err=%v", ev.Count, err)
	}
}

func TestFormatTS(t *testing.T) {
	cases := []struct {
		in   time.Time
		want string
	}{
		{time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), "2026-10-22T00:00:00Z"},
		{time.Date(2026, 10, 22, 0, 0, 0, 999, time.UTC), "2026-10-22T00:00:00Z"},
		{time.Date(2026, 10, 22, 0, 0, 0, 1500, time.UTC), "2026-10-22T00:00:00.000001Z"},
		{time.Date(2026, 10, 22, 0, 0, 0, 120000000, time.UTC), "2026-10-22T00:00:00.12Z"},
		{time.Date(2026, 10, 22, 2, 0, 0, 0, time.FixedZone("x", 2*3600)), "2026-10-22T00:00:00Z"},
	}
	for _, tc := range cases {
		got := FormatTS(tc.in)
		if got != tc.want {
			t.Errorf("FormatTS(%v) = %q, want %q", tc.in, got, tc.want)
		}
		back, err := ParseTS(got)
		if err != nil || !back.Equal(tc.in.Truncate(time.Microsecond)) {
			t.Errorf("ParseTS(%q) = %v, %v", got, back, err)
		}
	}
	if _, err := ParseTS("2026-10-22T02:00:00+02:00"); err != nil {
		t.Errorf("ParseTS must accept an offset: %v", err)
	}
}

func TestLabelsOrderingAndRoundTrip(t *testing.T) {
	in := map[string]string{
		"b":            "2",
		"a":            "1",
		"B":            "upper sorts before lower",
		"a.b/c-d_e:f":  "punct !\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~",
		"":             "empty key",
		"empty":        "",
		"ü":            "two-byte",
		"空":            "three-byte 値",
		"😀":            "four-byte",
		"line\nbreak":  "tab\there",
		"trailing   ":  "  spaces  ",
		"quote\"key\\": "esc sep",
	}
	labels := LabelsFromMap(in)
	if err := labels.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	wantOrder := []string{"", "B", "a", "a.b/c-d_e:f", "b", "empty", "line\nbreak", "quote\"key\\", "trailing   ", "ü", "空", "😀"}
	var gotOrder []string
	for _, l := range labels {
		gotOrder = append(gotOrder, l.Key)
	}
	if !reflect.DeepEqual(gotOrder, wantOrder) {
		t.Errorf("order = %q\nwant    %q", gotOrder, wantOrder)
	}
	if !reflect.DeepEqual(labels.Map(), in) {
		t.Errorf("Map() did not round trip")
	}

	ev := &testEvent{Envelope: NewEnvelope(EventTypeLifecycle, "l", "ns", testEventTS, testEventTS), Labels: labels}
	data, err := Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"value":"three-byte 値"`) || !strings.Contains(string(data), `<=>`) {
		t.Errorf("Unicode and punctuation must be written verbatim: %s", data)
	}
	var back testEvent
	if err := Unmarshal(data, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(back.Labels, labels) {
		t.Errorf("labels round trip mismatch\n got %q\nwant %q", back.Labels, labels)
	}
	// Also independent of this package's decoder.
	var generic struct{ Labels []map[string]string }
	if err := json.Unmarshal(data, &generic); err != nil || len(generic.Labels) != len(in) {
		t.Fatalf("generic decode: %v (%d labels)", err, len(generic.Labels))
	}
	for _, kv := range generic.Labels {
		if in[kv["key"]] != kv["value"] {
			t.Errorf("label %q = %q, want %q", kv["key"], kv["value"], in[kv["key"]])
		}
	}
}

func TestLabelsValidateRejectsDuplicatesAndDisorder(t *testing.T) {
	if err := (Labels{{"a", "1"}, {"a", "2"}}).Validate(); err == nil {
		t.Error("duplicate keys accepted")
	}
	if err := (Labels{{"b", "1"}, {"a", "2"}}).Validate(); err == nil {
		t.Error("out-of-order keys accepted")
	}
	data := []byte(`{"schema_version":1,"event_type":"agent.lifecycle","event_id":"l","producer_namespace":"ns","event_ts":"2026-10-22T00:00:00Z","observed_ts":"2026-10-22T00:00:00Z","count":0,"labels":[{"key":"k","value":"1"},{"key":"k","value":"2"}]}`)
	var ev testEvent
	if err := Unmarshal(data, &ev); err == nil {
		t.Error("Unmarshal accepted duplicate label keys")
	}
}

func TestLabelsEmpty(t *testing.T) {
	for name, l := range map[string]Labels{"nil": nil, "from nil map": LabelsFromMap(nil), "empty": {}} {
		b, err := json.Marshal(l)
		if err != nil || string(b) != "[]" {
			t.Errorf("%s: marshal = %s, %v; want []", name, b, err)
		}
	}
	if LabelsFromMap(nil) == nil {
		t.Error("LabelsFromMap(nil) must be non-nil")
	}
}

func TestNewEventIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := NewEventID()
		if len(id) != 36 || seen[id] {
			t.Fatalf("NewEventID() = %q (dup=%v)", id, seen[id])
		}
		seen[id] = true
	}
}

// A row from the shared telemetry exchange's contract mock
// (mock-events.jsonl, first usage row; not a fixture in this repository) decodes into an Envelope-based type unchanged.
func TestMockSeedEnvelopeDecodes(t *testing.T) {
	row := `{"schema_version": 1, "event_id": "u1", "producer_namespace": "mock-hub/agent-a/inc-1", "event_type": "agent.usage", "event_ts": "2026-10-22T00:05:00Z", "observed_ts": "2026-10-22T00:20:00Z", "hub_id": "mock-hub", "project_id": "mock-project", "agent_id": "agent-a", "incarnation_id": "inc-1", "launch_id": "launch-1", "broker_id": "mock-broker", "producer_build": "mock-v0", "calls": 1}`
	var ev testEvent
	if err := Unmarshal([]byte(row), &ev); err != nil {
		t.Fatalf("Unmarshal mock row: %v", err)
	}
	if ev.EventID != "u1" || ev.BrokerID != "mock-broker" || ev.EventType != EventTypeUsage {
		t.Errorf("decoded envelope = %+v", ev.Envelope)
	}
}

// R1: the producer gate accepts only the canonical FormatTS form of a
// non-zero instant.
func TestEnvelopeValidateRejectsNonCanonicalTimestamps(t *testing.T) {
	for _, ts := range []string{
		"2026-10-22T02:00:00+02:00",      // offset, not UTC
		"2026-10-22T00:00:00.123456789Z", // sub-microsecond
		"2026-10-22T00:00:00.120Z",       // trailing fractional zero
		"2026-10-22T00:00:00.000Z",       // ditto
		"0001-01-01T00:00:00Z",           // zero time sentinel
	} {
		e := NewEnvelope(EventTypeUsage, "u", "ns", testEventTS, testEventTS)
		e.EventTS = ts
		if err := e.Validate(); err == nil {
			t.Errorf("Validate accepted event_ts %q", ts)
		}
		e.EventTS, e.ObservedTS = FormatTS(testEventTS), ts
		if err := e.Validate(); err == nil {
			t.Errorf("Validate accepted observed_ts %q", ts)
		}
	}
	zero := NewEnvelope(EventTypeUsage, "u", "ns", time.Time{}, testEventTS)
	if _, err := Marshal(&testEvent{Envelope: zero}); err == nil {
		t.Error("Marshal accepted a zero event time")
	}
	for _, ok := range []string{"2026-10-22T00:00:00Z", "2026-10-22T00:00:00.12Z", "2026-10-22T00:00:00.000001Z"} {
		if err := ValidateTS(ok); err != nil {
			t.Errorf("ValidateTS(%q) = %v, want nil", ok, err)
		}
	}
}

// R2: invalid UTF-8 is rejected, never rewritten to U+FFFD.
func TestMarshalRejectsInvalidUTF8(t *testing.T) {
	env := func() Envelope { return NewEnvelope(EventTypeLifecycle, "l", "ns", testEventTS, testEventTS) }
	cases := map[string]*testEvent{
		"invalid label key":                {Envelope: env(), Labels: Labels{{"a\xff", "v"}}},
		"invalid label value":              {Envelope: env(), Labels: Labels{{"k", "v\xfe"}}},
		"keys colliding after replacement": {Envelope: env(), Labels: Labels{{"k\xfe", "1"}, {"k\xff", "2"}}},
	}
	for name, ev := range cases {
		if _, err := Marshal(ev); err == nil {
			t.Errorf("%s: Marshal accepted invalid UTF-8", name)
		}
		if err := ev.Labels.Validate(); err == nil {
			t.Errorf("%s: Labels.Validate accepted invalid UTF-8", name)
		}
	}
	bad := env()
	bad.AgentID = "agent\xc3"
	if _, err := Marshal(&testEvent{Envelope: bad}); err == nil {
		t.Error("Marshal accepted an invalid UTF-8 envelope string")
	}
}

// laxEvent's Validate deliberately skips Labels.Validate (O1): Marshal's
// walk must still enforce label invariants.
type laxEvent struct {
	Envelope
	Labels Labels `json:"labels"`
}

func (e *laxEvent) Validate() error { return e.Envelope.Validate() }

func TestMarshalEnforcesLabelsWithoutEventValidate(t *testing.T) {
	env := NewEnvelope(EventTypeLifecycle, "l", "ns", testEventTS, testEventTS)
	for name, l := range map[string]Labels{
		"duplicate":    {{"a", "1"}, {"a", "2"}},
		"out of order": {{"b", "1"}, {"a", "2"}},
	} {
		if _, err := Marshal(&laxEvent{Envelope: env, Labels: l}); err == nil {
			t.Errorf("%s labels marshaled through an event whose Validate skips them", name)
		}
	}
	nested := struct {
		laxEvent
		More []Labels `json:"more"`
	}{laxEvent: laxEvent{Envelope: env}, More: []Labels{{{"z", ""}, {"a", ""}}}}
	if _, err := Marshal(&nested); err == nil {
		t.Error("nested invalid Labels marshaled")
	}
}

type walkEvent struct {
	Envelope
	Nested   *struct{ F float32 } `json:"nested,omitempty"`
	Slice    []any                `json:"slice"`
	Map      map[string]any       `json:"map"`
	Unsigned uint64               `json:"unsigned"`
	Number   *json.Number         `json:"number,omitempty"`
	Internal float64              `json:"-"`
}

func (e *walkEvent) Validate() error { return e.Envelope.Validate() }

// O3/O4: every recursive branch of the walk, json:"-" skipped, json.Number
// rejected.
func TestMarshalWalksNestedValues(t *testing.T) {
	env := NewEnvelope(EventTypeHealth, "h", "ns", testEventTS, testEventTS)
	ok := &walkEvent{Envelope: env, Slice: []any{int64(1), "s"}, Map: map[string]any{"n": int64(2)}, Internal: 0.5}
	if _, err := Marshal(ok); err != nil {
		t.Fatalf("valid nested event rejected (json:\"-\" float must be skipped): %v", err)
	}
	cases := map[string]func(*walkEvent){
		"float in nested struct":   func(e *walkEvent) { e.Nested = &struct{ F float32 }{F: 1} },
		"float slice element":      func(e *walkEvent) { e.Slice = []any{1.0} },
		"float map value":          func(e *walkEvent) { e.Map = map[string]any{"x": float64(1)} },
		"invalid UTF-8 map key":    func(e *walkEvent) { e.Map = map[string]any{"\xff": int64(1)} },
		"uint64 beyond safe range": func(e *walkEvent) { e.Unsigned = MaxSafeInteger + 1 },
		"json.Number":              func(e *walkEvent) { n := json.Number("1.5"); e.Number = &n },
	}
	for name, mutate := range cases {
		ev := &walkEvent{Envelope: env}
		mutate(ev)
		if _, err := Marshal(ev); err == nil {
			t.Errorf("%s: Marshal accepted it", name)
		}
	}
}

func TestUnmarshalNullLabels(t *testing.T) {
	data := []byte(`{"schema_version":1,"event_type":"agent.lifecycle","event_id":"l","producer_namespace":"ns","event_ts":"2026-10-22T00:00:00Z","observed_ts":"2026-10-22T00:00:00Z","count":0,"labels":null}`)
	var ev testEvent
	if err := Unmarshal(data, &ev); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if ev.Labels != nil {
		t.Errorf("labels:null decoded as %v, want nil", ev.Labels)
	}
	out, err := Marshal(&ev)
	if err != nil || !strings.Contains(string(out), `"labels":[]`) {
		t.Errorf("re-marshal of null labels = %s (%v), want []", out, err)
	}
}

// R2-1: fields promoted from an embedded struct of unexported type are
// serialized by encoding/json, so the walk must check them too.
type innerPayload struct {
	Ratio  *float64 `json:"ratio,omitempty"`
	Labels Labels   `json:"labels"`
	Note   string   `json:"note,omitempty"`
}

type embeddedUnexportedEvent struct {
	Envelope
	innerPayload
}

func (e *embeddedUnexportedEvent) Validate() error { return e.Envelope.Validate() }

type embeddedUnexportedPtrEvent struct {
	Envelope
	*innerPayload
}

func (e *embeddedUnexportedPtrEvent) Validate() error { return e.Envelope.Validate() }

func TestMarshalChecksEmbeddedUnexportedStructs(t *testing.T) {
	env := NewEnvelope(EventTypeUsage, "u", "ns", testEventTS, testEventTS)
	ok := &embeddedUnexportedEvent{Envelope: env, innerPayload: innerPayload{Labels: Labels{{"a", "1"}}, Note: "n"}}
	got, err := Marshal(ok)
	if err != nil {
		t.Fatalf("valid embedded payload rejected: %v", err)
	}
	if !strings.Contains(string(got), `"labels":[{"key":"a","value":"1"}],"note":"n"`) {
		t.Errorf("embedded fields must be written flat: %s", got)
	}
	bad := map[string]innerPayload{
		"float":          {Ratio: new(float64)},
		"invalid labels": {Labels: Labels{{"b", ""}, {"a", ""}}},
		"invalid utf-8":  {Note: "x\xff"},
	}
	for name, p := range bad {
		if _, err := Marshal(&embeddedUnexportedEvent{Envelope: env, innerPayload: p}); err == nil {
			t.Errorf("%s in embedded unexported struct: Marshal accepted it", name)
		}
		p := p
		if _, err := Marshal(&embeddedUnexportedPtrEvent{Envelope: env, innerPayload: &p}); err == nil {
			t.Errorf("%s in embedded unexported *struct: Marshal accepted it", name)
		}
	}
	if _, err := Marshal(&embeddedUnexportedPtrEvent{Envelope: env}); err != nil {
		t.Errorf("nil embedded pointer must be allowed: %v", err)
	}
}

type timeEvent struct {
	Envelope
	At time.Time `json:"at"`
}

func (e *timeEvent) Validate() error { return e.Envelope.Validate() }

type stringOptEvent struct {
	Envelope
	Count int64 `json:"count,string"`
}

func (e *stringOptEvent) Validate() error { return e.Envelope.Validate() }

// N1/N2: types with their own encoding and ",string" numerics are rejected.
func TestMarshalRejectsSelfEncodingTypesAndStringOption(t *testing.T) {
	env := NewEnvelope(EventTypeUsage, "u", "ns", testEventTS, testEventTS)
	if _, err := Marshal(&timeEvent{Envelope: env, At: testEventTS}); err == nil {
		t.Error("Marshal accepted a time.Time payload field")
	}
	if _, err := Marshal(&stringOptEvent{Envelope: env, Count: 3}); err == nil {
		t.Error(`Marshal accepted a ",string" integer field`)
	}
}

// N5: walk errors name wire fields.
func TestMarshalErrorsUseWireNames(t *testing.T) {
	env := NewEnvelope(EventTypeUsage, "u", "ns", testEventTS, testEventTS)
	env.AgentID = "a\xff"
	_, err := Marshal(&testEvent{Envelope: env})
	if err == nil || !strings.Contains(err.Error(), "field agent_id ") {
		t.Errorf("error = %v, want it to name agent_id", err)
	}
}
