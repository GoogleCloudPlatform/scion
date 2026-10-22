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
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"
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
// Envelope.AnalyticsEnvelope method plus the type's own Validate (which
// should call Envelope.Validate first).
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

// MarshalJSON writes a nil label set as [] rather than null: "no labels" is
// a known, empty set, not an unknown one.
func (l Labels) MarshalJSON() ([]byte, error) {
	if l == nil {
		return []byte("[]"), nil
	}
	// Not json.Marshal: it would HTML-escape "<", ">" and "&", and the
	// outer encoder's SetEscapeHTML(false) does not undo a Marshaler's
	// own escaping.
	return encodeNoHTMLEscape([]Label(l))
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

// Marshal serializes ev deterministically: it validates ev, then walks
// every serialized field and rejects non-integer numerics, integers beyond
// MaxSafeInteger, strings that are not valid UTF-8, and any Labels value
// that fails Labels.Validate (so label invariants hold even if an event
// type's own Validate forgets them). It
// encodes with encoding/json (struct fields in declaration order) without
// HTML escaping, so label punctuation such as "<" and "&" stays verbatim.
// The result has no trailing newline.
func Marshal(ev Event) ([]byte, error) {
	if ev == nil || isNilPointer(ev) {
		return nil, errors.New("telemetrycontract: nil event")
	}
	if err := ev.Validate(); err != nil {
		return nil, err
	}
	if err := checkValues(reflect.ValueOf(ev), ""); err != nil {
		return nil, err
	}
	data, err := encodeNoHTMLEscape(ev)
	if err != nil {
		return nil, fmt.Errorf("telemetrycontract: encoding event: %w", err)
	}
	return data, nil
}

// Unmarshal decodes data into ev and validates it. Unknown fields are
// ignored, so a consumer built against an older schema still decodes a
// newer, additively extended event. A non-integer or out-of-range number
// in an integer field is a decode error.
func Unmarshal(data []byte, ev Event) error {
	if ev == nil || isNilPointer(ev) {
		return errors.New("telemetrycontract: nil event")
	}
	if err := json.Unmarshal(data, ev); err != nil {
		return fmt.Errorf("telemetrycontract: decoding event: %w", err)
	}
	if err := checkValues(reflect.ValueOf(ev), ""); err != nil {
		return err
	}
	return ev.Validate()
}

func isNilPointer(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}

var (
	labelsType     = reflect.TypeOf(Labels(nil))
	jsonNumberType = reflect.TypeOf(json.Number(""))
)

// checkValues walks the fields encoding/json serializes (exported, not
// tagged "-") and reports the first float/complex value, json.Number
// (which can carry a fraction), integer outside ±MaxSafeInteger, string
// that is not valid UTF-8, or invalid Labels value. path names the
// offending field for the error message.
func checkValues(v reflect.Value, path string) error {
	if v.Type() == labelsType {
		if err := v.Interface().(Labels).Validate(); err != nil {
			return fmt.Errorf("field %s: %w", path, err)
		}
	}
	if v.Type() == jsonNumberType {
		return fmt.Errorf("telemetrycontract: field %s has type json.Number; analytics numerics must be Go integers", path)
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return checkValues(v.Elem(), path)
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() || f.Tag.Get("json") == "-" {
				continue
			}
			name := f.Name
			if path != "" {
				name = path + "." + name
			}
			if err := checkValues(v.Field(i), name); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if err := checkValues(v.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			elem := fmt.Sprintf("%s[%v]", path, iter.Key())
			if err := checkValues(iter.Key(), elem+"(key)"); err != nil {
				return err
			}
			if err := checkValues(iter.Value(), elem); err != nil {
				return err
			}
		}
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return fmt.Errorf("telemetrycontract: field %s is not valid UTF-8", path)
		}
	case reflect.Float32, reflect.Float64, reflect.Complex64, reflect.Complex128:
		return fmt.Errorf("telemetrycontract: field %s has non-integer type %s; analytics numerics must be integers", path, v.Type())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n := v.Int(); n > MaxSafeInteger || n < -MaxSafeInteger {
			return fmt.Errorf("telemetrycontract: field %s = %d exceeds the exactly representable range ±%d", path, n, int64(MaxSafeInteger))
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if n := v.Uint(); n > MaxSafeInteger {
			return fmt.Errorf("telemetrycontract: field %s = %d exceeds the exactly representable range ±%d", path, n, int64(MaxSafeInteger))
		}
	}
	return nil
}
