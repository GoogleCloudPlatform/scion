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
	"sync"
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
//  1. the envelope (ev.AnalyticsEnvelope().Validate), then the event
//     (ev.Validate);
//  2. the event type against checkStructure's allowlist, so encoding/json
//     has no field-selection, naming or custom-encoding rule left to apply
//     beyond plain tagged fields;
//  3. every string is valid UTF-8 (encoding/json would silently rewrite
//     invalid bytes to U+FFFD);
//  4. Labels validate themselves as they are encoded (Labels.MarshalJSON);
//  5. the produced bytes: every JSON number is an integer within
//     ±MaxSafeInteger, and the envelope rebuilt from the object's exact
//     top-level keys equals ev.AnalyticsEnvelope().
func Marshal(ev Event) ([]byte, error) {
	if err := validateEvent(ev); err != nil {
		return nil, err
	}
	if err := checkStructure(reflect.TypeOf(ev)); err != nil {
		return nil, err
	}
	if err := checkStrings(reflect.ValueOf(ev), ""); err != nil {
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
	if err := checkStrings(reflect.ValueOf(ev), ""); err != nil {
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
	jsonMarshalerType = reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	textMarshalerType = reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
)

// structureCache memoizes checkStructure per event type (reflect.Type ->
// error or nil); the result depends only on the type.
var structureCache sync.Map

type structureResult struct{ err error }

// checkStructure validates the event type against an ALLOWLIST (a type, not
// a value, so nil pointers hide nothing). An event type is a pointer to a
// named struct whose fields are:
//
//   - the embedded, untagged Envelope (root struct only), or
//   - exported, non-embedded fields with an explicit, valid json tag name
//     and no option other than omitempty (never omitempty on Labels),
//     whose type is one of: the predeclared string, bool or integer types
//     (not named types such as json.Number); Labels; an exported named
//     struct obeying the same field rules
//     (non-recursive); a pointer to any of these; a slice of any of these
//     except byte slices.
//
// No type other than Labels may implement json.Marshaler or
// encoding.TextMarshaler (the root included). Everything else -- floats,
// interfaces, maps, arrays, unexported or json:"-" fields, other
// embeddings -- is rejected, so encoding/json has no rule left to apply
// that these checks would have to model. Wire names must be unique per JSON
// object, compared case-insensitively (BigQuery columns and encoding/json
// decoding both fold case).
func checkStructure(t reflect.Type) error {
	if cached, ok := structureCache.Load(t); ok {
		return cached.(structureResult).err
	}
	err := checkRootType(t)
	structureCache.Store(t, structureResult{err})
	return err
}

func checkRootType(t reflect.Type) error {
	if t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct || t.Elem().Name() == "" {
		return fmt.Errorf("telemetrycontract: event type %s must be a pointer to a named struct", t)
	}
	return checkStructType(t.Elem(), true, map[reflect.Type]bool{})
}

// checkStructType checks one JSON object's struct type. ancestors holds the
// struct types on the current path, to reject recursive types (which would
// make value depth unbounded).
func checkStructType(t reflect.Type, root bool, ancestors map[reflect.Type]bool) error {
	if ancestors[t] {
		return fmt.Errorf("telemetrycontract: type %s is recursive", t)
	}
	if implementsEncoder(t) {
		return fmt.Errorf("telemetrycontract: type %s has its own JSON or text encoding; only Labels may", t)
	}
	ancestors[t] = true
	defer delete(ancestors, t)

	seen := map[string]string{} // lower-cased wire name -> Go field
	record := func(name, goName string) error {
		key := strings.ToLower(name)
		if prev, dup := seen[key]; dup {
			return fmt.Errorf("telemetrycontract: wire name %q of %s.%s collides with %s (case-insensitive)", name, t.Name(), goName, prev)
		}
		seen[key] = t.Name() + "." + goName
		return nil
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag, hasTag := f.Tag.Lookup("json")
		if f.Anonymous {
			if !root || f.Type != envelopeType || hasTag {
				return fmt.Errorf("telemetrycontract: field %s.%s: the only embedding allowed is an untagged Envelope in the event's root struct", t.Name(), f.Name)
			}
			for j := 0; j < envelopeType.NumField(); j++ {
				name, _, _ := strings.Cut(envelopeType.Field(j).Tag.Get("json"), ",")
				if err := record(name, "Envelope."+envelopeType.Field(j).Name); err != nil {
					return err
				}
			}
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		switch {
		case !f.IsExported():
			return fmt.Errorf("telemetrycontract: field %s.%s is unexported; event types hold wire fields only", t.Name(), f.Name)
		case !hasTag || name == "" || name == "-":
			return fmt.Errorf("telemetrycontract: field %s.%s needs an explicit json tag name", t.Name(), f.Name)
		case !isValidTagName(name):
			return fmt.Errorf("telemetrycontract: field %s.%s has an invalid json tag name %q", t.Name(), f.Name, name)
		case opts != "" && opts != "omitempty":
			return fmt.Errorf("telemetrycontract: field %s.%s: only the omitempty tag option is allowed, got %q", t.Name(), f.Name, opts)
		case opts == "omitempty" && f.Type == labelsType:
			return fmt.Errorf("telemetrycontract: Labels field %s.%s must not use omitempty (a known-empty set would be omitted); use *Labels for unknown", t.Name(), f.Name)
		}
		if err := record(name, f.Name); err != nil {
			return err
		}
		if err := checkFieldType(f.Type, t.Name()+"."+f.Name, ancestors); err != nil {
			return err
		}
	}
	return nil
}

// checkFieldType checks a field's type against the allowlist.
func checkFieldType(t reflect.Type, field string, ancestors map[reflect.Type]bool) error {
	if t == labelsType {
		return nil
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		// Predeclared types only: a named basic type can carry its own
		// encoding, and encoding/json special-cases some (json.Number is a
		// string written as a raw number).
		if t.PkgPath() != "" {
			return fmt.Errorf("telemetrycontract: field %s has named type %s; use the predeclared %s", field, t, t.Kind())
		}
		return nil
	case reflect.Pointer:
		if t.Elem().Kind() == reflect.Pointer || t.Elem().Kind() == reflect.Slice && t.Elem() != labelsType {
			return fmt.Errorf("telemetrycontract: field %s has unsupported type %s", field, t)
		}
		return checkFieldType(t.Elem(), field, ancestors)
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return fmt.Errorf("telemetrycontract: field %s is a byte slice (encoded as base64); use a string", field)
		}
		if k := t.Elem().Kind(); k == reflect.Slice || k == reflect.Pointer {
			return fmt.Errorf("telemetrycontract: field %s has unsupported type %s", field, t)
		}
		return checkFieldType(t.Elem(), field, ancestors)
	case reflect.Struct:
		if t == envelopeType {
			return fmt.Errorf("telemetrycontract: field %s holds Envelope as a named field; embed it in the root struct instead", field)
		}
		if t.Name() == "" || !isExportedName(t.Name()) {
			return fmt.Errorf("telemetrycontract: field %s has type %s; nested objects must be exported named structs", field, t)
		}
		return checkStructType(t, false, ancestors)
	default:
		return fmt.Errorf("telemetrycontract: field %s has type %s, which is not allowed in an analytics event (allowed: string, bool, integers, Labels, exported structs, pointers and slices of these)", field, t)
	}
}

func isExportedName(name string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}

func implementsEncoder(t reflect.Type) bool {
	return t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType) ||
		reflect.PointerTo(t).Implements(jsonMarshalerType) || reflect.PointerTo(t).Implements(textMarshalerType)
}

// isValidTagName mirrors encoding/json's isValidTag; names it would ignore
// are rejected.
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

// checkStrings walks a value of an allowlisted type and rejects strings
// that are not valid UTF-8 (encoding/json would rewrite them to U+FFFD,
// which cannot be detected in the output). checkStructure has already
// excluded every other kind, and recursive types, so the walk is bounded.
func checkStrings(v reflect.Value, path string) error {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return checkStrings(v.Elem(), path)
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			if err := checkStrings(v.Field(i), fieldPath(path, t.Field(i))); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if v.Type() == labelsType {
			for i := 0; i < v.Len(); i++ {
				if !utf8.ValidString(v.Index(i).Field(0).String()) || !utf8.ValidString(v.Index(i).Field(1).String()) {
					return fmt.Errorf("telemetrycontract: field %s[%d] is not valid UTF-8", path, i)
				}
			}
			return nil
		}
		for i := 0; i < v.Len(); i++ {
			if err := checkStrings(v.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return fmt.Errorf("telemetrycontract: field %s is not valid UTF-8", path)
		}
	}
	return nil
}

// maxWireDepth bounds the walk over decoded event bytes.
const maxWireDepth = 128

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
	// want was validated by validateEvent, and got equals it.
	return nil
}

func checkWireNumbers(node any, path string, depth int) error {
	if depth > maxWireDepth {
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
