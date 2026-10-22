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
// (mock-events.jsonl, first usage row; not a fixture in this repository)
// decodes into an Envelope-based type unchanged.
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

}

// --- allowlist (WP0 review r4) -------------------------------------------

// Nested is an allowed nested object.
type Nested struct {
	Name   string   `json:"name"`
	Counts []int64  `json:"counts,omitempty"`
	Flag   *bool    `json:"flag,omitempty"`
	Labels *Labels  `json:"labels,omitempty"`
	Tags   []string `json:"tags,omitempty"`
}

// allowedEvent uses every allowlisted shape.
type allowedEvent struct {
	Envelope
	S      string   `json:"s"`
	B      bool     `json:"b"`
	I      int64    `json:"i"`
	U      uint32   `json:"u"`
	P      *int64   `json:"p,omitempty"`
	L      Labels   `json:"l"`
	N      Nested   `json:"n"`
	NP     *Nested  `json:"np,omitempty"`
	NS     []Nested `json:"ns,omitempty"`
	Strs   []string `json:"strs,omitempty"`
	Ints16 []int16  `json:"ints16,omitempty"`
}

func (e *allowedEvent) Validate() error { return nil }

type selfEncodingRoot struct {
	Envelope
	X int64 `json:"x"`
}

func (e *selfEncodingRoot) Validate() error { return nil }
func (e selfEncodingRoot) MarshalJSON() ([]byte, error) {
	return []byte(`{"x":1.5}`), nil
}

type Recursive struct {
	Next *Recursive `json:"next,omitempty"`
}

type TextOnly string

func (TextOnly) MarshalText() ([]byte, error) { return []byte("x"), nil }

type PtrMarshaler struct {
	A string `json:"a"`
}

func (*PtrMarshaler) MarshalJSON() ([]byte, error) { return []byte(`{}`), nil }

type CaseA struct {
	Lower string `json:"agent_ref"`
	Upper string `json:"Agent_Ref"`
}

type rejectEvent[T any] struct {
	Envelope
	V T `json:"v"`
}

func (e *rejectEvent[T]) Validate() error { return nil }

// rejectPtrEvent is rejectEvent for pointer types (pointers must be
// omitempty, so the rule under test is the one that fires).
type rejectPtrEvent[T any] struct {
	Envelope
	V T `json:"v,omitempty"`
}

func (e *rejectPtrEvent[T]) Validate() error { return nil }

func TestCheckStructureAllowlist(t *testing.T) {
	env := NewEnvelope(EventTypeUsage, "u", "ns", testEventTS, testEventTS)
	empty := Labels{}
	ok := &allowedEvent{Envelope: env, L: Labels{{"k", "v"}}, NP: &Nested{Name: "x", Labels: &empty}, NS: []Nested{{Name: "y"}}}
	if _, err := Marshal(ok); err != nil {
		t.Fatalf("allowlisted event rejected: %v", err)
	}

	rejected := map[string]reflect.Type{
		"float":                reflect.TypeOf(&rejectEvent[float64]{}),
		"float pointer":        reflect.TypeOf(&rejectPtrEvent[*float32]{}),
		"interface":            reflect.TypeOf(&rejectEvent[any]{}),
		"slice of interface":   reflect.TypeOf(&rejectEvent[[]any]{}),
		"map":                  reflect.TypeOf(&rejectEvent[map[string]string]{}),
		"array":                reflect.TypeOf(&rejectEvent[[2]int64]{}),
		"byte slice":           reflect.TypeOf(&rejectEvent[[]byte]{}),
		"raw message":          reflect.TypeOf(&rejectEvent[json.RawMessage]{}),
		"json.Number":          reflect.TypeOf(&rejectEvent[json.Number]{}),
		"time.Time":            reflect.TypeOf(&rejectEvent[time.Time]{}),
		"text marshaler":       reflect.TypeOf(&rejectEvent[TextOnly]{}),
		"pointer marshaler":    reflect.TypeOf(&rejectEvent[PtrMarshaler]{}),
		"anonymous struct":     reflect.TypeOf(&rejectEvent[struct{ A string }]{}),
		"unexported struct":    reflect.TypeOf(&rejectEvent[idsC]{}),
		"recursive":            reflect.TypeOf(&rejectEvent[Recursive]{}),
		"envelope as field":    reflect.TypeOf(&rejectEvent[Envelope]{}),
		"case-insensitive dup": reflect.TypeOf(&rejectEvent[CaseA]{}),
		"nested slice":         reflect.TypeOf(&rejectEvent[[][]string]{}),
		"double pointer":       reflect.TypeOf(&rejectPtrEvent[**int64]{}),
		"self-encoding root":   reflect.TypeOf(&selfEncodingRoot{}),
		"non-pointer root":     reflect.TypeOf(allowedEvent{}),
	}
	for name, typ := range rejected {
		if err := checkStructure(typ); err == nil {
			t.Errorf("%s: checkStructure accepted %s", name, typ)
		}
	}
	if _, err := Marshal(&selfEncodingRoot{Envelope: env}); err == nil {
		t.Error("Marshal accepted a root type with its own MarshalJSON")
	}
}

type untaggedField struct {
	Envelope
	X int64
}

func (e *untaggedField) Validate() error { return nil }

type unexportedField struct {
	Envelope
	x int64 //nolint:unused // deliberately unexported
}

func (e *unexportedField) Validate() error { return nil }

type dashField struct {
	Envelope
	X int64 `json:"-"`
}

func (e *dashField) Validate() error { return nil }

type badTagName struct {
	Envelope
	X int64 `json:"a\\b"`
}

func (e *badTagName) Validate() error { return nil }

type badOption struct {
	Envelope
	X int64 `json:"x,inline"`
}

func (e *badOption) Validate() error { return nil }

// Field-level allowlist rules: explicit valid tag, exported, no json:"-",
// no option but omitempty.
func TestCheckStructureFieldRules(t *testing.T) {
	for name, ev := range map[string]Event{
		"untagged field":   &untaggedField{},
		"unexported field": &unexportedField{},
		"json dash field":  &dashField{},
		"invalid tag name": &badTagName{},
		"unknown option":   &badOption{},
	} {
		if err := checkStructure(reflect.TypeOf(ev)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// R4-1 and R2-1: every embedding other than the root Envelope is rejected,
// tagged or not, exported or not, by value or pointer.
type innerPayload struct {
	Note string `json:"note,omitempty"`
}

type embeddedUnexportedEvent struct {
	Envelope
	innerPayload
}

func (e *embeddedUnexportedEvent) Validate() error { return nil }

type embeddedUnexportedPtrEvent struct {
	Envelope
	*innerPayload
}

func (e *embeddedUnexportedPtrEvent) Validate() error { return nil }

type TimeHolder struct {
	At time.Time `json:"at"`
}

type taggedEmbedEvent struct {
	Envelope
	TimeHolder `json:"usage"`
	Usage      string `json:"usage_other"`
}

func (e *taggedEmbedEvent) Validate() error { return nil }

func TestCheckStructureRejectsOtherEmbeddings(t *testing.T) {
	for name, typ := range map[string]reflect.Type{
		"unexported embed":         reflect.TypeOf(&embeddedUnexportedEvent{}),
		"unexported pointer embed": reflect.TypeOf(&embeddedUnexportedPtrEvent{}),
		"tagged embed (R4-1)":      reflect.TypeOf(&taggedEmbedEvent{}),
	} {
		if err := checkStructure(typ); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCheckStructureIsCached(t *testing.T) {
	typ := reflect.TypeOf(&allowedEvent{})
	first := checkStructure(typ)
	if _, ok := structureCache.Load(typ); !ok || first != nil {
		t.Errorf("result not cached (err %v)", first)
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

// --- WP0 review r3: name resolution, envelope gate, wire validation -----

// (a) the same tagged wire name at equal depth from two embedded structs:
// encoding/json would drop it silently. Built with reflect.StructOf
// because go vet's structtag check rejects the literal declaration.
type IdsTagged1 struct {
	Ref1 string `json:"agent_ref"`
}

type IdsTagged2 struct {
	Ref2 string `json:"agent_ref"`
}

func equalDepthConflictType() reflect.Type {
	return reflect.StructOf([]reflect.StructField{
		{Name: "Envelope", Type: envelopeType, Anonymous: true},
		{Name: "IdsTagged1", Type: reflect.TypeOf(IdsTagged1{}), Anonymous: true},
		{Name: "IdsTagged2", Type: reflect.TypeOf(IdsTagged2{}), Anonymous: true},
	})
}

func TestCheckStructureEqualDepthTaggedConflict(t *testing.T) {
	typ := equalDepthConflictType()
	// Confirm the premise: encoding/json really drops the field.
	v := reflect.New(typ).Elem()
	v.Field(1).Field(0).SetString("x")
	b, _ := json.Marshal(v.Interface())
	if strings.Contains(string(b), "agent_ref") {
		t.Fatalf("premise: encoding/json was expected to drop agent_ref: %s", b)
	}
	if err := checkStructure(typ); err == nil {
		t.Error("checkStructure accepted an equal-depth tagged conflict")
	}
}

// (b) an outer field shadowing an Envelope field.
type shadowEvent struct {
	Envelope
	EventType string `json:"event_type"`
}

func (e *shadowEvent) Validate() error { return nil }

// (c) a conflict hidden behind a nil embedded pointer: the embedded
// field's tag and the outer field's untagged Go name are both "AgentRef".
type idsC struct {
	Ref string `json:"AgentRef"`
}

type nilPtrConflictEvent struct {
	Envelope
	*idsC
	AgentRef string
}

func (e *nilPtrConflictEvent) Validate() error { return nil }

// Envelope as a named field nests it instead of flattening.
type namedEnvelopeEvent struct {
	Env Envelope `json:"env"`
	Envelope
}

func (e *namedEnvelopeEvent) Validate() error { return nil }

func TestMarshalRejectsDuplicateAndShadowedWireNames(t *testing.T) {
	env := NewEnvelope(EventTypeUsage, "u", "ns", testEventTS, testEventTS)
	cases := map[string]Event{
		"shadows event_type":   &shadowEvent{Envelope: env, EventType: EventTypeUsage},
		"behind nil pointer":   &nilPtrConflictEvent{Envelope: env, AgentRef: "x"},
		"named envelope field": &namedEnvelopeEvent{Env: env, Envelope: env},
	}
	for name, ev := range cases {
		if _, err := Marshal(ev); err == nil {
			t.Errorf("%s: Marshal accepted it", name)
		}
	}
	var dst shadowEvent
	if err := Unmarshal([]byte(fullTestEventGolden), &dst); err == nil {
		t.Error("Unmarshal into a type with a shadowed envelope field accepted it")
	}
}

// R3-2: the envelope is validated even if the event's Validate skips it.
type laxEnvelopeEvent struct {
	Envelope
	Count int64 `json:"count"`
}

func (e *laxEnvelopeEvent) Validate() error { return nil }

func TestMarshalValidatesEnvelopeRegardlessOfEventValidate(t *testing.T) {
	bad := NewEnvelope(EventTypeUsage, "", "", testEventTS, testEventTS)
	if _, err := Marshal(&laxEnvelopeEvent{Envelope: bad}); err == nil {
		t.Error("Marshal accepted an invalid envelope behind a lax Validate")
	}
	data := []byte(`{"schema_version":2,"event_type":"agent.usage","event_id":"u","producer_namespace":"ns","event_ts":"2026-10-22T00:00:00Z","observed_ts":"2026-10-22T00:00:00Z","count":1}`)
	var ev laxEnvelopeEvent
	if err := Unmarshal(data, &ev); err == nil {
		t.Error("Unmarshal accepted schema_version 2 behind a lax Validate")
	}
}

type omitLabelsEvent struct {
	Envelope
	Labels Labels `json:"labels,omitempty"`
}

func (e *omitLabelsEvent) Validate() error { return nil }

type ptrLabelsEvent struct {
	Envelope
	Labels *Labels `json:"labels,omitempty"`
}

func (e *ptrLabelsEvent) Validate() error { return nil }

type stringOptStringEvent struct {
	Envelope
	Name string `json:"name,string"`
}

func (e *stringOptStringEvent) Validate() error { return nil }

type cycleNode struct {
	Next *cycleNode `json:"next,omitempty"`
}

type cycleEvent struct {
	Envelope
	Node *cycleNode `json:"node,omitempty"`
}

func (e *cycleEvent) Validate() error { return nil }

// N3-1, N3-2, N3-3.
func TestMarshalTagRulesAndCycles(t *testing.T) {
	env := NewEnvelope(EventTypeUsage, "u", "ns", testEventTS, testEventTS)
	if _, err := Marshal(&omitLabelsEvent{Envelope: env}); err == nil {
		t.Error("omitempty on a Labels field accepted")
	}
	empty := Labels{}
	got, err := Marshal(&ptrLabelsEvent{Envelope: env, Labels: &empty})
	if err != nil || !strings.Contains(string(got), `"labels":[]`) {
		t.Errorf("*Labels with omitempty: %s, %v", got, err)
	}
	if got, err := Marshal(&ptrLabelsEvent{Envelope: env}); err != nil || strings.Contains(string(got), "labels") {
		t.Errorf("nil *Labels must be omitted (unknown): %s, %v", got, err)
	}
	if _, err := Marshal(&stringOptStringEvent{Envelope: env, Name: "x"}); err == nil {
		t.Error(`",string" on a string field accepted`)
	}
	n := &cycleNode{}
	n.Next = n
	if _, err := Marshal(&cycleEvent{Envelope: env, Node: n}); err == nil {
		t.Error("a pointer cycle must be an error, not a stack overflow")
	}
}

func TestCheckWire(t *testing.T) {
	env := NewEnvelope(EventTypeUsage, "u", "ns", testEventTS, testEventTS)
	base := `{"schema_version":1,"event_type":"agent.usage","event_id":"u","producer_namespace":"ns","event_ts":"2026-10-22T00:05:00Z","observed_ts":"2026-10-22T00:05:00Z"`
	if err := checkWire([]byte(base+`,"n":{"deep":[1,2,-3]}}`), &env); err != nil {
		t.Errorf("valid wire rejected: %v", err)
	}
	for name, tail := range map[string]string{
		"fraction":         `,"n":1.5}`,
		"exponent":         `,"n":[1e2]}`,
		"beyond safe":      `,"n":{"x":9007199254740992}}`,
		"envelope differs": `,"event_id":"other"}`,
	} {
		if err := checkWire([]byte(base+tail), &env); err == nil {
			t.Errorf("%s: checkWire accepted it", name)
		}
	}
	noEventID := `{"schema_version":1,"event_type":"agent.usage","producer_namespace":"ns","event_ts":"2026-10-22T00:05:00Z","observed_ts":"2026-10-22T00:05:00Z"}`
	if err := checkWire([]byte(noEventID), &env); err == nil {
		t.Error("an omitted envelope key accepted")
	}
	if err := checkWire([]byte(`[1]`), &env); err == nil {
		t.Error("a non-object accepted")
	}
}

// Labels validate themselves when encoded, wherever they sit.
func TestLabelsMarshalJSONValidates(t *testing.T) {
	if _, err := json.Marshal(Labels{{"b", ""}, {"a", ""}}); err == nil {
		t.Error("out-of-order Labels encoded")
	}
	env := NewEnvelope(EventTypeHealth, "h", "ns", testEventTS, testEventTS)
	bad := Labels{{"k", ""}, {"k", ""}}
	ev := &allowedEvent{Envelope: env, NS: []Nested{{Name: "x", Labels: &bad}}}
	if _, err := Marshal(ev); err == nil {
		t.Error("invalid Labels nested in a slice of structs accepted")
	}
}

// --- WP0 review r5: self-audit of every allowlisted kind -----------------

type NamedString string
type NamedInt int64
type NamedBool bool
type NamedPtr *int64
type NamedSlice []string
type NamedLabels Labels
type NamedLabelSlice []Label

type MarshalString string

func (MarshalString) MarshalJSON() ([]byte, error) { return []byte(`1.5`), nil }

type MarshalInt int64

func (MarshalInt) MarshalJSON() ([]byte, error) { return []byte(`1.5`), nil }

type MarshalBool bool

func (*MarshalBool) MarshalJSON() ([]byte, error) { return []byte(`1.5`), nil }

type MarshalSlice []string

func (MarshalSlice) MarshalJSON() ([]byte, error) { return []byte(`[1.5]`), nil }

type TextSlice []string

func (TextSlice) MarshalText() ([]byte, error) { return []byte("x"), nil }

type MarshalStruct struct {
	A string `json:"a"`
}

func (MarshalStruct) MarshalJSON() ([]byte, error) { return []byte(`{"a":1.5}`), nil }

type PtrMarshalStruct struct {
	A string `json:"a"`
}

func (*PtrMarshalStruct) MarshalJSON() ([]byte, error) { return []byte(`{"a":1.5}`), nil }

type TextStruct struct {
	A string `json:"a"`
}

func (TextStruct) MarshalText() ([]byte, error) { return []byte("x"), nil }

// Every allowlisted kind (string, integer, bool, struct, pointer, slice)
// crossed with {named, value MarshalJSON, pointer MarshalJSON,
// MarshalText}: only the plain predeclared forms, plain exported structs
// and Labels pass. Positive controls guard against a rule that rejects
// everything.
func TestCheckStructureKindByNamedOrMarshaler(t *testing.T) {
	accepted := map[string]reflect.Type{
		"string":          reflect.TypeOf(&rejectEvent[string]{}),
		"int64":           reflect.TypeOf(&rejectEvent[int64]{}),
		"uint16":          reflect.TypeOf(&rejectEvent[uint16]{}),
		"bool":            reflect.TypeOf(&rejectEvent[bool]{}),
		"exported struct": reflect.TypeOf(&rejectEvent[Nested]{}),
		"slice of string": reflect.TypeOf(&rejectEvent[[]string]{}),
		"slice of struct": reflect.TypeOf(&rejectEvent[[]Nested]{}),
		"Labels":          reflect.TypeOf(&rejectEvent[Labels]{}),
		"*Labels":         reflect.TypeOf(&rejectPtrEvent[*Labels]{}),
		"*int64":          reflect.TypeOf(&rejectPtrEvent[*int64]{}),
		"*struct":         reflect.TypeOf(&rejectPtrEvent[*Nested]{}),
	}
	for name, typ := range accepted {
		if err := checkStructure(typ); err != nil {
			t.Errorf("positive control %s rejected: %v", name, err)
		}
	}
	rejected := map[string]struct {
		typ  reflect.Type
		want string
	}{
		"named string":            {reflect.TypeOf(&rejectEvent[NamedString]{}), "named type"},
		"named int":               {reflect.TypeOf(&rejectEvent[NamedInt]{}), "named type"},
		"named bool":              {reflect.TypeOf(&rejectEvent[NamedBool]{}), "named type"},
		"named pointer":           {reflect.TypeOf(&rejectPtrEvent[NamedPtr]{}), "named pointer"},
		"named slice":             {reflect.TypeOf(&rejectEvent[NamedSlice]{}), "named slice"},
		"named Labels":            {reflect.TypeOf(&rejectEvent[NamedLabels]{}), "named slice"},
		"named []Label":           {reflect.TypeOf(&rejectEvent[NamedLabelSlice]{}), "named slice"},
		"[]Label":                 {reflect.TypeOf(&rejectEvent[[]Label]{}), "Label outside Labels"},
		"Label":                   {reflect.TypeOf(&rejectEvent[Label]{}), "Label outside Labels"},
		"string MarshalJSON":      {reflect.TypeOf(&rejectEvent[MarshalString]{}), "named type"},
		"int MarshalJSON":         {reflect.TypeOf(&rejectEvent[MarshalInt]{}), "named type"},
		"bool ptr MarshalJSON":    {reflect.TypeOf(&rejectEvent[MarshalBool]{}), "named type"},
		"slice MarshalJSON":       {reflect.TypeOf(&rejectEvent[MarshalSlice]{}), "named slice"},
		"slice MarshalText":       {reflect.TypeOf(&rejectEvent[TextSlice]{}), "named slice"},
		"struct MarshalJSON":      {reflect.TypeOf(&rejectEvent[MarshalStruct]{}), "own JSON or text encoding"},
		"struct ptr MarshalJSON":  {reflect.TypeOf(&rejectEvent[PtrMarshalStruct]{}), "own JSON or text encoding"},
		"struct MarshalText":      {reflect.TypeOf(&rejectEvent[TextStruct]{}), "own JSON or text encoding"},
		"pointer to marshaler":    {reflect.TypeOf(&rejectPtrEvent[*MarshalStruct]{}), "own JSON or text encoding"},
		"slice of marshaler":      {reflect.TypeOf(&rejectEvent[[]MarshalStruct]{}), "own JSON or text encoding"},
		"pointer to named string": {reflect.TypeOf(&rejectPtrEvent[*NamedString]{}), "named type"},
		"slice of named int":      {reflect.TypeOf(&rejectEvent[[]NamedInt]{}), "named type"},
	}
	for name, tc := range rejected {
		err := checkStructure(tc.typ)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

type omitIntEvent struct {
	Envelope
	N int64 `json:"n,omitempty"`
}

func (e *omitIntEvent) Validate() error { return nil }

type omitBoolEvent struct {
	Envelope
	B bool `json:"b,omitempty"`
}

func (e *omitBoolEvent) Validate() error { return nil }

type nullPtrEvent struct {
	Envelope
	P *int64 `json:"p"`
}

func (e *nullPtrEvent) Validate() error { return nil }

type upperTagEvent struct {
	Envelope
	X string `json:"Agent_Ref"`
}

func (e *upperTagEvent) Validate() error { return nil }

// N5-1, N5-5: the omit-vs-null policy and wire-name charset are enforced.
func TestCheckStructureOmitPolicyAndWireNames(t *testing.T) {
	for name, tc := range map[string]struct {
		ev   Event
		want string
	}{
		"omitempty int":   {&omitIntEvent{}, "known zero"},
		"omitempty bool":  {&omitBoolEvent{}, "known zero"},
		"pointer no omit": {&nullPtrEvent{}, "never null"},
		"upper-case tag":  {&upperTagEvent{}, "[a-z0-9_]"},
	} {
		err := checkStructure(reflect.TypeOf(tc.ev))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	for _, bad := range []string{"", "A", "a-b", "a.b", "ſ", "a b"} {
		if isWireName(bad) {
			t.Errorf("isWireName(%q) = true", bad)
		}
	}
	if !isWireName("cache_read_2") {
		t.Error("isWireName rejects a snake_case name")
	}
}

// N5-4: Unmarshal rejects invalid UTF-8 in its input instead of letting
// json rewrite it.
func TestUnmarshalRejectsInvalidUTF8Input(t *testing.T) {
	data := []byte(`{"schema_version":1,"event_type":"agent.usage","event_id":"u","producer_namespace":"ns","event_ts":"2026-10-22T00:00:00Z","observed_ts":"2026-10-22T00:00:00Z","count":0,"labels":[{"key":"k` + "\xff" + `","value":""}]}`)
	var ev testEvent
	if err := Unmarshal(data, &ev); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Errorf("invalid UTF-8 input: err = %v", err)
	}
}
