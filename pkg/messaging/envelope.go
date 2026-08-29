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

package messaging

import (
	"fmt"
	"strings"
	"time"
)

// ---------- MessageKind ----------

// MessageKind distinguishes text from lifecycle events.
type MessageKind string

const (
	KindText  MessageKind = "text"
	KindEvent MessageKind = "event"
)

// validMessageKinds enumerates all accepted MessageKind values.
var validMessageKinds = map[MessageKind]bool{
	KindText:  true,
	KindEvent: true,
}

// ValidateMessageKind returns an error if k is not a recognised kind.
func ValidateMessageKind(k MessageKind) error {
	if !validMessageKinds[k] {
		return fmt.Errorf("invalid message kind %q: must be one of: text, event", k)
	}
	return nil
}

// ---------- TextIntent ----------

// TextIntent captures what the sender wants from the recipient.
type TextIntent string

const (
	IntentInform   TextIntent = "inform"
	IntentRequest  TextIntent = "request"
	IntentQuestion TextIntent = "question"
)

// validTextIntents enumerates all accepted TextIntent values.
var validTextIntents = map[TextIntent]bool{
	IntentInform:   true,
	IntentRequest:  true,
	IntentQuestion: true,
}

// ValidateTextIntent returns an error if i is not a recognised intent.
func ValidateTextIntent(i TextIntent) error {
	if !validTextIntents[i] {
		return fmt.Errorf("invalid text intent %q: must be one of: inform, request, question", i)
	}
	return nil
}

// ---------- EventType ----------

// EventType is a closed enum of lifecycle events.
type EventType string

const (
	EventAgentStateChanged EventType = "agent.state-changed"
	EventAgentInputNeeded  EventType = "agent.input-needed"
	EventDeliveryFailed    EventType = "delivery.failed"
	EventScheduleFired     EventType = "schedule.fired"
	EventPortExposed       EventType = "port.exposed"
)

// validEventTypes enumerates all accepted EventType values.
var validEventTypes = map[EventType]bool{
	EventAgentStateChanged: true,
	EventAgentInputNeeded:  true,
	EventDeliveryFailed:    true,
	EventScheduleFired:     true,
	EventPortExposed:       true,
}

// ValidateEventType returns an error if t is not a recognised event type.
func ValidateEventType(t EventType) error {
	if !validEventTypes[t] {
		return fmt.Errorf("invalid event type %q: must be one of: agent.state-changed, agent.input-needed, delivery.failed, schedule.fired, port.exposed", t)
	}
	return nil
}

// ---------- PrincipalRef ----------

// PrincipalRef identifies the sender or an addressee.
// Format: "user:<id>", "agent:<id>", or "system:<component>".
type PrincipalRef string

// validPrincipalPrefixes lists the allowed prefix kinds for a PrincipalRef.
var validPrincipalPrefixes = map[string]bool{
	"user":   true,
	"agent":  true,
	"system": true,
}

// MaxPrincipalRefLength is the maximum allowed length for a PrincipalRef.
const MaxPrincipalRefLength = 512

// ValidatePrincipalRef returns an error if ref is not well-formed.
func ValidatePrincipalRef(ref PrincipalRef) error {
	s := string(ref)
	if len(s) > MaxPrincipalRefLength {
		return fmt.Errorf("principal ref exceeds maximum length of %d characters",
			MaxPrincipalRefLength)
	}
	idx := strings.IndexByte(s, ':')
	if idx < 1 {
		return fmt.Errorf("invalid principal ref %q: must be kind:id (e.g. user:alice)", ref)
	}
	prefix := s[:idx]
	id := s[idx+1:]
	if !validPrincipalPrefixes[prefix] {
		return fmt.Errorf("invalid principal ref prefix %q in %q: must be one of: user, agent, system", prefix, ref)
	}
	if id == "" {
		return fmt.Errorf("invalid principal ref %q: id part must not be empty", ref)
	}
	return nil
}

// PrincipalKind returns the kind portion of a PrincipalRef (e.g. "user").
func (p PrincipalRef) PrincipalKind() string {
	s := string(p)
	idx := strings.IndexByte(s, ':')
	if idx < 0 {
		return s
	}
	return s[:idx]
}

// PrincipalID returns the id portion of a PrincipalRef (e.g. "alice").
func (p PrincipalRef) PrincipalID() string {
	s := string(p)
	idx := strings.IndexByte(s, ':')
	if idx < 0 {
		return ""
	}
	return s[idx+1:]
}

// ---------- AddressedVia ----------

// AddressedVia records how an addressee was selected.
type AddressedVia string

const (
	ViaExplicit     AddressedVia = "explicit"      // --to flag
	ViaBodyMention  AddressedVia = "body-mention"  // @mention in body
	ViaDefaultAgent AddressedVia = "default-agent" // conversation's default agent
	ViaDirect       AddressedVia = "direct"        // other participant in DM
)

// validAddressedVia enumerates all accepted AddressedVia values.
var validAddressedVia = map[AddressedVia]bool{
	ViaExplicit:     true,
	ViaBodyMention:  true,
	ViaDefaultAgent: true,
	ViaDirect:       true,
}

// ValidateAddressedVia returns an error if v is not a recognised value.
func ValidateAddressedVia(v AddressedVia) error {
	if !validAddressedVia[v] {
		return fmt.Errorf("invalid addressed-via %q: must be one of: explicit, body-mention, default-agent, direct", v)
	}
	return nil
}

// ---------- DeliveryState ----------

// DeliveryState tracks per-addressee delivery.
type DeliveryState string

const (
	DeliveryPending   DeliveryState = "pending"
	DeliveryDelivered DeliveryState = "delivered"
	DeliveryFailed    DeliveryState = "failed"
)

// validDeliveryStates enumerates all accepted DeliveryState values.
var validDeliveryStates = map[DeliveryState]bool{
	DeliveryPending:   true,
	DeliveryDelivered: true,
	DeliveryFailed:    true,
}

// ValidateDeliveryState returns an error if s is not a recognised state.
func ValidateDeliveryState(s DeliveryState) error {
	if !validDeliveryStates[s] {
		return fmt.Errorf("invalid delivery state %q: must be one of: pending, delivered, failed", s)
	}
	return nil
}

// ---------- Visibility ----------

// Visibility controls which consumers see a message.
type Visibility string

const (
	VisibilityNormal  Visibility = "normal"
	VisibilityVerbose Visibility = "verbose"
	VisibilityFull    Visibility = "full"
)

// validVisibilities enumerates all accepted Visibility values.
var validVisibilities = map[Visibility]bool{
	VisibilityNormal:  true,
	VisibilityVerbose: true,
	VisibilityFull:    true,
}

// ValidateVisibility returns an error if v is not a recognised visibility.
func ValidateVisibility(v Visibility) error {
	if v == "" {
		return nil // empty defaults to normal
	}
	if !validVisibilities[v] {
		return fmt.Errorf("invalid visibility %q: must be one of: normal, verbose, full", v)
	}
	return nil
}

// ---------- EventBody ----------

// EventBody carries the payload for an event-kind message.
type EventBody struct {
	Type    EventType `json:"type"`
	Subject string    `json:"subject,omitempty"` // e.g. "agent:builder"
	Status  string    `json:"status,omitempty"`  // e.g. "COMPLETED"
	Reason  string    `json:"reason,omitempty"`  // for delivery.failed
	URL     string    `json:"url,omitempty"`     // for port.exposed
}

// Validate checks that the EventBody has a valid Type.
func (e *EventBody) Validate() error {
	return ValidateEventType(e.Type)
}

// ---------- AttachmentRef ----------

// AttachmentRef identifies an attached file.
type AttachmentRef struct {
	Path string `json:"path"`
	Name string `json:"name,omitempty"`
}

// ---------- Message (new envelope) ----------

// Message is the new envelope format replacing StructuredMessage.
// It separates kind (text vs event) from intent (what the sender wants)
// and event type (what happened), fixing the mixed-concern type enum.
type Message struct {
	ID             string       `json:"id"`
	ConversationID string       `json:"conversation_id"`
	ReplyToID      *string      `json:"reply_to_id,omitempty"`
	From           PrincipalRef `json:"from"`
	Kind           MessageKind  `json:"kind"`

	// Exactly one populated per Kind.
	Intent *TextIntent `json:"intent,omitempty"` // Kind == text
	Event  *EventBody  `json:"event,omitempty"`  // Kind == event

	Body        string          `json:"body"`
	Attachments []AttachmentRef `json:"attachments,omitempty"`
	Visibility  Visibility      `json:"visibility,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Validate checks internal consistency of a Message.
func (m *Message) Validate() error {
	if m.ID == "" {
		return fmt.Errorf("message id is required")
	}
	if err := ValidatePrincipalRef(m.From); err != nil {
		return fmt.Errorf("invalid from: %w", err)
	}
	if err := ValidateMessageKind(m.Kind); err != nil {
		return err
	}
	if err := ValidateVisibility(m.Visibility); err != nil {
		return err
	}

	// Kind/intent mutual exclusivity.
	switch m.Kind {
	case KindText:
		if m.Intent == nil {
			return fmt.Errorf("text message must have intent set")
		}
		if err := ValidateTextIntent(*m.Intent); err != nil {
			return err
		}
		if m.Event != nil {
			return fmt.Errorf("text message must not have event body")
		}
	case KindEvent:
		if m.Event == nil {
			return fmt.Errorf("event message must have event body set")
		}
		if err := m.Event.Validate(); err != nil {
			return fmt.Errorf("invalid event body: %w", err)
		}
		if m.Intent != nil {
			return fmt.Errorf("event message must not have intent")
		}
	}

	return nil
}

// ---------- Addressee ----------

// Addressee records a resolved target for message delivery.
type Addressee struct {
	MessageID     string        `json:"message_id"`
	PrincipalKind string        `json:"principal_kind"` // "user" or "agent"
	PrincipalID   string        `json:"principal_id"`
	Via           AddressedVia  `json:"via"`
	DeliveryState DeliveryState `json:"delivery_state"`
	FailureReason *string       `json:"failure_reason,omitempty"`
}

// Validate checks that the Addressee has valid fields.
func (a *Addressee) Validate() error {
	if a.MessageID == "" {
		return fmt.Errorf("addressee message_id is required")
	}
	if a.PrincipalKind != "user" && a.PrincipalKind != "agent" {
		return fmt.Errorf("addressee principal_kind must be user or agent, got %q", a.PrincipalKind)
	}
	if a.PrincipalID == "" {
		return fmt.Errorf("addressee principal_id is required")
	}
	if err := ValidateAddressedVia(a.Via); err != nil {
		return err
	}
	if err := ValidateDeliveryState(a.DeliveryState); err != nil {
		return err
	}
	return nil
}
