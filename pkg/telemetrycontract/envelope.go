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

// This file holds the shared analytics event envelope: the fields every
// structured analytics event (agent.lifecycle, agent.usage,
// telemetry.health) carries, and the one serializer they all go through.
// Event-specific types live in their own files and embed Envelope, so the
// wire object stays flat (jsonPayload.event_type, jsonPayload.input, ...).
//
// Frozen conventions (contract envelope freeze, 2026-10-22):
//   - Cloud Logging log ID AnalyticsLogID, structured jsonPayload;
//   - schema_version is the integer SchemaVersion;
//   - event_type is one of the EventType* constants;
//   - the primary key is (producer_namespace, event_id);
//   - every numeric payload field is an integer;
//   - timestamps are RFC 3339 strings;
//   - "unknown values omitted or null, never sentinel numbers".
//
// Unknown policy (this package's choice within the freeze, agreed with the
// WP1 lifecycle design): an unknown value is OMITTED (omitempty), never null and
// never a sentinel. A string field that may be unknown is a plain string
// with omitempty (empty means unknown). An integer that may be unknown is
// *int64 with omitempty, so a known zero (non-nil pointer to 0) is still
// written and stays distinguishable from unknown (nil, omitted).

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// AnalyticsLogID is the Cloud Logging log ID analytics events are written
// to as structured jsonPayload. A Logging sink routes this log to BigQuery;
// Scion never writes to BigQuery itself.
const AnalyticsLogID = "scion-analytics"

// SchemaVersion is the envelope schema version every event carries.
const SchemaVersion = 1

// Analytics event types (the closed event_type enum).
const (
	EventTypeLifecycle = "agent.lifecycle"
	EventTypeUsage     = "agent.usage"
	EventTypeHealth    = "telemetry.health"
)

// ValidEventType reports whether v is a member of the closed event_type
// enum.
func ValidEventType(v string) bool {
	switch v {
	case EventTypeLifecycle, EventTypeUsage, EventTypeHealth:
		return true
	default:
		return false
	}
}

// MaxSafeInteger is the largest integer magnitude Marshal accepts.
// Cloud Logging stores jsonPayload as a protobuf Struct, whose numbers are
// IEEE-754 doubles: a larger integer would silently lose precision on the
// way to BigQuery, so it is rejected at the producer instead.
const MaxSafeInteger = 1<<53 - 1

// Envelope is the common part of every analytics event. Event types embed
// it (by value, not as a named field) so its fields serialize at the top
// level of the event object.
//
// Envelope deliberately has no MarshalJSON/UnmarshalJSON: an embedded
// type's JSON methods are promoted to the embedding type and would replace
// the whole flat object with only the envelope.
type Envelope struct {
	SchemaVersion int `json:"schema_version"`
	// EventType is one of the EventType* constants.
	EventType string `json:"event_type"`
	// EventID is minted once per logical event and kept across transport
	// retries and process restarts; together with ProducerNamespace it is
	// the primary key.
	EventID string `json:"event_id"`
	// ProducerNamespace is the stable logical producer identity (for
	// example a hub ID, or an agent ID plus incarnation), never a host name
	// or process ID, so a replay after restart keeps the same key.
	ProducerNamespace string `json:"producer_namespace"`
	// EventTS is when the event happened; ObservedTS is when the producer
	// observed it. Both are FormatTS strings.
	EventTS    string `json:"event_ts"`
	ObservedTS string `json:"observed_ts"`

	// Identity fields, omitted when genuinely unknown.
	HubID         string `json:"hub_id,omitempty"`
	ProjectID     string `json:"project_id,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	IncarnationID string `json:"incarnation_id,omitempty"`
	LaunchID      string `json:"launch_id,omitempty"`
	BrokerID      string `json:"broker_id,omitempty"`

	// ProducerBuild is the emitting binary's build identity;
	// HarnessVersion is the harness version, where applicable.
	ProducerBuild  string `json:"producer_build,omitempty"`
	HarnessVersion string `json:"harness_version,omitempty"`
}

// AnalyticsEnvelope returns e. It is promoted to every type that embeds
// Envelope, which is how such a type satisfies Event. (The method cannot be
// named Envelope: the embedded field of that name would shadow it.)
func (e *Envelope) AnalyticsEnvelope() *Envelope { return e }

// Validate checks the frozen envelope invariants: schema version, event
// type, non-empty primary key and RFC 3339 timestamps.
func (e *Envelope) Validate() error {
	if e == nil {
		return errors.New("telemetrycontract: nil envelope")
	}
	var errs []error
	if e.SchemaVersion != SchemaVersion {
		errs = append(errs, fmt.Errorf("schema_version = %d, want %d", e.SchemaVersion, SchemaVersion))
	}
	if !ValidEventType(e.EventType) {
		errs = append(errs, fmt.Errorf("event_type %q is not a known analytics event type", e.EventType))
	}
	if e.EventID == "" {
		errs = append(errs, errors.New("event_id is empty"))
	}
	if e.ProducerNamespace == "" {
		errs = append(errs, errors.New("producer_namespace is empty"))
	}
	if err := ValidateTS(e.EventTS); err != nil {
		errs = append(errs, fmt.Errorf("event_ts: %w", err))
	}
	if err := ValidateTS(e.ObservedTS); err != nil {
		errs = append(errs, fmt.Errorf("observed_ts: %w", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("telemetrycontract: invalid envelope: %w", errors.Join(errs...))
	}
	return nil
}

// NewEnvelope returns an Envelope with the schema version and event type
// set and the two timestamps formatted. The caller fills the identity
// fields it knows. A zero time.Time is not a valid timestamp (it would be
// a sentinel); Validate, and therefore Marshal, rejects it.
func NewEnvelope(eventType, eventID, producerNamespace string, eventTS, observedTS time.Time) Envelope {
	return Envelope{
		SchemaVersion:     SchemaVersion,
		EventType:         eventType,
		EventID:           eventID,
		ProducerNamespace: producerNamespace,
		EventTS:           FormatTS(eventTS),
		ObservedTS:        FormatTS(observedTS),
	}
}

// Event is implemented by every analytics event type, through the promoted
// Envelope.AnalyticsEnvelope method plus the type's own Validate for its
// payload. Marshal and Unmarshal always validate the envelope itself
// (AnalyticsEnvelope().Validate) before calling the event's Validate.
type Event interface {
	AnalyticsEnvelope() *Envelope
	Validate() error
}

// NewEventID returns a random (version 4) UUID. Producers that need replay
// idempotence must mint the ID once per logical event and persist it, not
// call this again on retry.
func NewEventID() string { return uuid.NewString() }

// FormatTS formats t as an RFC 3339 UTC timestamp truncated to
// microseconds, the precision of a BigQuery TIMESTAMP, so the value round
// trips through BigQuery exactly. Trailing fractional zeros are trimmed, so
// a whole second formats as "2026-10-22T00:00:00Z".
func FormatTS(t time.Time) string {
	return t.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
}

// ValidateTS reports whether s is a canonical producer timestamp: exactly
// FormatTS's output for a non-zero instant (UTC "Z", at most microsecond
// precision, no trailing fractional zeros). Producers write only this form,
// so one instant always serializes to the same bytes.
func ValidateTS(s string) error {
	t, err := ParseTS(s)
	if err != nil {
		return err
	}
	if t.IsZero() {
		return fmt.Errorf("timestamp %q is the zero time (a sentinel, not a known value)", s)
	}
	if canonical := FormatTS(t); canonical != s {
		return fmt.Errorf("timestamp %q is not in canonical form %q (UTC, microsecond precision)", s, canonical)
	}
	return nil
}

// ParseTS parses an RFC 3339 timestamp leniently (any offset and
// precision), for consumers. Producers must emit ValidateTS's canonical
// form.
func ParseTS(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("timestamp is empty")
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q is not RFC 3339: %w", s, err)
	}
	return t, nil
}

// Label is one user-supplied agent label. Value has no omitempty: an empty
// value is a real label value and must round trip.
type Label struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Labels is a label set in its wire form: an array of key/value pairs with
// unique keys in bytewise (UTF-8 byte) key order. Keys and values are
// carried verbatim: no key filter, no normalization, no truncation.
//
// Do not tag a Labels field with omitempty: encoding/json checks omitempty
// before calling MarshalJSON, so a known-empty set would be omitted (read
// as unknown) instead of written as [].
type Labels []Label

// LabelsFromMap returns m as Labels in canonical order. A nil or empty map
// yields an empty, non-nil set.
func LabelsFromMap(m map[string]string) Labels {
	out := make(Labels, 0, len(m))
	for k, v := range m {
		out = append(out, Label{Key: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Map returns the label set as a map.
func (l Labels) Map() map[string]string {
	m := make(map[string]string, len(l))
	for _, kv := range l {
		m[kv.Key] = kv.Value
	}
	return m
}

// Validate checks that keys are unique and in canonical (bytewise
// ascending) order, and that every key and value is valid UTF-8 (JSON
// encoding would otherwise silently replace invalid bytes with U+FFFD,
// which is lossy and can make distinct keys collide on the wire). Any
// valid string is otherwise accepted, including an empty key or value.
func (l Labels) Validate() error {
	for _, kv := range l {
		if !utf8.ValidString(kv.Key) {
			return fmt.Errorf("telemetrycontract: label key %q is not valid UTF-8", kv.Key)
		}
		if !utf8.ValidString(kv.Value) {
			return fmt.Errorf("telemetrycontract: value of label %q is not valid UTF-8", kv.Key)
		}
	}
	for i := 1; i < len(l); i++ {
		switch {
		case l[i-1].Key == l[i].Key:
			return fmt.Errorf("telemetrycontract: duplicate label key %q", l[i].Key)
		case l[i-1].Key > l[i].Key:
			return fmt.Errorf("telemetrycontract: label key %q is out of order after %q", l[i].Key, l[i-1].Key)
		}
	}
	return nil
}

// MarshalJSON validates the set (Labels.Validate) and writes it; a nil set
// is written as [] rather than null: "no labels" is a known, empty set, not
// an unknown one. Validating here means whatever label set encoding/json
// actually serializes -- wherever it sits in an event -- is checked.
func (l Labels) MarshalJSON() ([]byte, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	if l == nil {
		return []byte("[]"), nil
	}
	// Not json.Marshal: it would HTML-escape "<", ">" and "&", and the
	// outer encoder's SetEscapeHTML(false) does not undo a Marshaler's
	// own escaping.
	return encodeNoHTMLEscape([]Label(l))
}

// UnmarshalJSON decodes a label array and requires canonical form (unique,
// bytewise-ordered keys). JSON null decodes as a nil set.
func (l *Labels) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		*l = nil
		return nil
	}
	var raw []Label
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if err := Labels(raw).Validate(); err != nil {
		return err
	}
	*l = raw
	return nil
}

// encodeNoHTMLEscape is json.Marshal without HTML escaping and without the
// encoder's trailing newline. (U+2028/U+2029 are still escaped by
// encoding/json; they decode back to the same runes.)
func encodeNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// Marshal serializes ev deterministically (encoding/json, struct fields in
// declaration order, no HTML escaping, no trailing newline) and validates
// both the value and the bytes it produced:
//
//  1. the envelope (ev.AnalyticsEnvelope().Validate) and the event
//     (ev.Validate);
//  2. a structural check of the event type: no wire name may be declared
//     twice in one JSON object (encoding/json would silently drop or
//     shadow it), no ",string" tag option, no omitempty on a Labels field;
//  3. a walk over every reachable value -- every field except those tagged
//     exactly json:"-" -- rejecting floating-point and json.Number values and strings that are
//     not valid UTF-8 (encoding/json would rewrite them to U+FFFD);
//  4. Labels validate themselves as they are encoded (Labels.MarshalJSON);
//  5. the produced bytes: every JSON number must be an integer within
//     ±MaxSafeInteger, and the envelope rebuilt from the object's exact
//     top-level keys must equal ev.AnalyticsEnvelope().
//
// Checks 2 and 3 over-approximate on purpose: they reject anything
// encoding/json might handle differently instead of modelling its rules;
// check 5 validates what is actually written.
func Marshal(ev Event) ([]byte, error) {
	if err := validateEvent(ev); err != nil {
		return nil, err
	}
	if err := checkStructure(reflect.TypeOf(ev)); err != nil {
		return nil, err
	}
	if err := checkReachable(reflect.ValueOf(ev)); err != nil {
		return nil, err
	}
	data, err := encodeNoHTMLEscape(ev)
	if err != nil {
		return nil, fmt.Errorf("telemetrycontract: encoding event: %w", err)
	}
	if err := checkWire(data, ev.AnalyticsEnvelope()); err != nil {
		return nil, err
	}
	return data, nil
}

// Unmarshal decodes data into ev and validates the input bytes (integer
// numbers, envelope keys) and the decoded event. Unknown fields are
// ignored, so a consumer built against an older schema still decodes an
// additively extended event. Like encoding/json it matches keys
// case-insensitively and keeps the last duplicate key, so it is a decoder
// for producer output, not a strict validator of third-party input.
func Unmarshal(data []byte, ev Event) error {
	if ev == nil || isNilPointer(ev) {
		return errors.New("telemetrycontract: nil event")
	}
	if err := checkStructure(reflect.TypeOf(ev)); err != nil {
		return err
	}
	if err := json.Unmarshal(data, ev); err != nil {
		return fmt.Errorf("telemetrycontract: decoding event: %w", err)
	}
	if err := validateEvent(ev); err != nil {
		return err
	}
	if err := checkReachable(reflect.ValueOf(ev)); err != nil {
		return err
	}
	return checkWire(data, ev.AnalyticsEnvelope())
}

// validateEvent runs the envelope's own Validate (so the frozen invariants
// never depend on an event type remembering to call it) and then the
// event's.
func validateEvent(ev Event) error {
	if ev == nil || isNilPointer(ev) {
		return errors.New("telemetrycontract: nil event")
	}
	if err := ev.AnalyticsEnvelope().Validate(); err != nil {
		return err
	}
	return ev.Validate()
}

func isNilPointer(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

var (
	labelsType        = reflect.TypeOf(Labels(nil))
	envelopeType      = reflect.TypeOf(Envelope{})
	jsonNumberType    = reflect.TypeOf(json.Number(""))
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// maxWalkDepth bounds the reachable-value walk; deeper (or cyclic) values
// are rejected instead of overflowing the stack.
const maxWalkDepth = 64

// checkStructure inspects the event type (not a value, so a nil embedded
// pointer cannot hide anything). For each JSON object -- a struct plus the
// fields promoted into it from embedded structs -- every wire name must be
// declared once. It also rejects the ",string" tag option, omitempty on a
// Labels field, invalid tag names, and embedded types with their own
// encoding (whose promotion rules differ). Envelope must be embedded
// (promoted), never a named field.
func checkStructure(t reflect.Type) error {
	return checkStructureType(t, map[reflect.Type]bool{})
}

func checkStructureType(t reflect.Type, done map[reflect.Type]bool) error {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || done[t] || t == labelsType {
		return nil
	}
	done[t] = true
	seen := map[string]string{}
	var nested []reflect.Type
	if err := collectObjectFields(t, "", seen, &nested, 0); err != nil {
		return err
	}
	for _, n := range nested {
		if err := checkStructureType(n, done); err != nil {
			return err
		}
	}
	return nil
}

// collectObjectFields records the wire names of t's fields into seen,
// descending into promoted embedded structs, and queues every non-promoted
// struct-typed field type for its own object check.
func collectObjectFields(t reflect.Type, path string, seen map[string]string, nested *[]reflect.Type, depth int) error {
	if depth > maxWalkDepth {
		return fmt.Errorf("telemetrycontract: type %s embeds too deeply", t)
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		goPath := joinPath(path, f.Name)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name != "" && !isValidTagName(name) {
			return fmt.Errorf("telemetrycontract: field %s has an invalid json tag name %q", goPath, name)
		}
		if hasTagOption(opts, "string") {
			return fmt.Errorf("telemetrycontract: field %s uses the \",string\" option, which changes its wire type", goPath)
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if f.Type == labelsType && hasTagOption(opts, "omitempty") {
			return fmt.Errorf("telemetrycontract: Labels field %s must not use omitempty (a known-empty set would be omitted); use *Labels for unknown", goPath)
		}
		if f.Anonymous && name == "" && ft.Kind() == reflect.Struct {
			if implementsEncoder(ft) {
				return fmt.Errorf("telemetrycontract: embedded field %s has its own JSON encoding; embed plain structs only", goPath)
			}
			if err := collectObjectFields(ft, goPath, seen, nested, depth+1); err != nil {
				return err
			}
			continue
		}
		if !f.IsExported() {
			continue
		}
		if ft == envelopeType {
			return fmt.Errorf("telemetrycontract: field %s holds Envelope as a named field; embed it so its fields are top-level", goPath)
		}
		if elem := innermost(f.Type); elem != labelsType && implementsEncoder(elem) {
			return fmt.Errorf("telemetrycontract: field %s has type %s with its own encoding (time.Time included); use plain strings (FormatTS for timestamps) and integers", goPath, elem)
		}
		if name == "" {
			name = f.Name
		}
		if prev, dup := seen[name]; dup {
			return fmt.Errorf("telemetrycontract: wire name %q is declared by both %s and %s; encoding/json would drop or shadow it", name, prev, goPath)
		}
		seen[name] = goPath
		*nested = append(*nested, f.Type)
	}
	return nil
}

// innermost strips pointer, slice, array and map layers from t.
func innermost(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		if t == labelsType {
			return t
		}
		t = t.Elem()
	}
	return t
}

func implementsEncoder(t reflect.Type) bool {
	return t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType) ||
		reflect.PointerTo(t).Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType)
}

// isValidTagName mirrors encoding/json's isValidTag: names it would ignore
// (falling back to the Go name) are rejected here instead.
func isValidTagName(s string) bool {
	for _, c := range s {
		switch {
		case strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c):
		case !unicode.IsLetter(c) && !unicode.IsDigit(c):
			return false
		}
	}
	return true
}

// checkReachable walks every value reachable from v -- exported or not;
// only fields tagged exactly json:"-" are skipped -- and rejects floating-point and json.Number values
// and strings that are not valid UTF-8. Labels values are validated too.
// Pointer cycles and excessive depth are errors.
func checkReachable(v reflect.Value) error {
	return walkReachable(v, "", map[uintptr]bool{}, 0)
}

func walkReachable(v reflect.Value, path string, visited map[uintptr]bool, depth int) error {
	if depth > maxWalkDepth {
		return fmt.Errorf("telemetrycontract: value at %s is nested too deeply (or cyclic)", path)
	}
	if !v.IsValid() {
		return nil
	}
	t := v.Type()
	if t == jsonNumberType {
		return fmt.Errorf("telemetrycontract: field %s has type json.Number; analytics numerics must be Go integers", path)
	}
	if t == labelsType {
		if err := labelsFromValue(v).Validate(); err != nil {
			return fmt.Errorf("field %s: %w", path, err)
		}
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		if visited[v.Pointer()] {
			return fmt.Errorf("telemetrycontract: value at %s contains a pointer cycle", path)
		}
		visited[v.Pointer()] = true
		defer delete(visited, v.Pointer())
		return walkReachable(v.Elem(), path, visited, depth+1)
	case reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return walkReachable(v.Elem(), path, visited, depth+1)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).Tag.Get("json") == "-" {
				continue // never serialized (exactly "-"; "-," names a field "-")
			}
			if err := walkReachable(v.Field(i), fieldPath(path, t.Field(i)), visited, depth+1); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := walkReachable(v.Index(i), fmt.Sprintf("%s[%d]", path, i), visited, depth+1); err != nil {
				return err
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			elem := fmt.Sprintf("%s[%v]", path, iter.Key())
			if err := walkReachable(iter.Key(), elem+"(key)", visited, depth+1); err != nil {
				return err
			}
			if err := walkReachable(iter.Value(), elem, visited, depth+1); err != nil {
				return err
			}
		}
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return fmt.Errorf("telemetrycontract: field %s is not valid UTF-8", path)
		}
	case reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return fmt.Errorf("telemetrycontract: field %s has non-integer type %s; analytics numerics must be integers", path, t)
	}
	return nil
}

// checkWire validates encoded bytes: a JSON object whose numbers are all
// integers within ±MaxSafeInteger, and whose envelope, rebuilt from the
// exact top-level keys, equals want.
func checkWire(data []byte, want *Envelope) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return fmt.Errorf("telemetrycontract: event bytes are not JSON: %w", err)
	}
	top, ok := tree.(map[string]any)
	if !ok {
		return errors.New("telemetrycontract: event is not a JSON object")
	}
	if err := checkWireNumbers(tree, "", 0); err != nil {
		return err
	}
	var got Envelope
	gv := reflect.ValueOf(&got).Elem()
	for i := 0; i < envelopeType.NumField(); i++ {
		name, _, _ := strings.Cut(envelopeType.Field(i).Tag.Get("json"), ",")
		raw, present := top[name]
		if !present {
			continue
		}
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, gv.Field(i).Addr().Interface()); err != nil {
			return fmt.Errorf("telemetrycontract: wire field %s: %w", name, err)
		}
	}
	if !reflect.DeepEqual(got, *want) {
		return fmt.Errorf("telemetrycontract: envelope on the wire %+v differs from the event's envelope %+v", got, *want)
	}
	return got.Validate()
}

func checkWireNumbers(node any, path string, depth int) error {
	if depth > maxWalkDepth*2 {
		return fmt.Errorf("telemetrycontract: wire value at %s is nested too deeply", path)
	}
	switch n := node.(type) {
	case json.Number:
		s := string(n)
		if strings.ContainsAny(s, ".eE") {
			return fmt.Errorf("telemetrycontract: wire field %s = %s is not an integer", path, s)
		}
		i, err := strconv.ParseInt(s, 10, 64)
		if err != nil || i > MaxSafeInteger || i < -MaxSafeInteger {
			return fmt.Errorf("telemetrycontract: wire field %s = %s exceeds the exactly representable range ±%d", path, s, int64(MaxSafeInteger))
		}
	case map[string]any:
		for k, v := range n {
			if err := checkWireNumbers(v, joinPath(path, k), depth+1); err != nil {
				return err
			}
		}
	case []any:
		for i, v := range n {
			if err := checkWireNumbers(v, fmt.Sprintf("%s[%d]", path, i), depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// fieldPath names a struct field for error messages by its wire name: the
// json tag name if set, nothing for a promoted (anonymous, untagged)
// struct, else the Go name.
func fieldPath(path string, f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	switch {
	case name != "" && name != "-":
		return joinPath(path, name)
	case f.Anonymous && name == "":
		return path
	default:
		return joinPath(path, f.Name)
	}
}

func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

func hasTagOption(opts, want string) bool {
	for opts != "" {
		var o string
		o, opts, _ = strings.Cut(opts, ",")
		if o == want {
			return true
		}
	}
	return false
}

// labelsFromValue reads a Labels value through reflection, so it also works
// for a value reached via an unexported field, where
// reflect.Value.Interface is not allowed.
func labelsFromValue(v reflect.Value) Labels {
	if v.IsNil() {
		return nil
	}
	out := make(Labels, v.Len())
	for i := range out {
		e := v.Index(i)
		out[i] = Label{Key: e.Field(0).String(), Value: e.Field(1).String()}
	}
	return out
}
