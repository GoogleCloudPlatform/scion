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

package agentcontrol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/tmuxdeliver"
)

// RouteDeliverMessage is the deliver-message control route: one message
// flush into the agent's tmux session (design v2.22 §1). It is the only
// route the hub's broker-facing /deliver endpoint ever calls.
const RouteDeliverMessage = "deliver-message"

// DeliverVersion is the only accepted request version.
const DeliverVersion = 1

// Bounds and timings of the deliver-message route (design v2.22 §1.2, §2.2).
const (
	// MaxDeliverText is the largest message text, in bytes. It leaves room
	// for JSON escaping inside the session's 768 KiB MaxRPCBody.
	MaxDeliverText = 512 * 1024
	// MaxDeliverMessageIDs bounds message_ids.
	MaxDeliverMessageIDs = 64
	// MaxDeliverRequestBytes bounds the JSON-encoded request as the broker
	// sends it: below the session's 768 KiB MaxRPCBody with room for the
	// fields the hub adds. JSON escaping can make text that fits
	// MaxDeliverText exceed it (for example '<' encodes as 6 bytes).
	MaxDeliverRequestBytes = 760 * 1024
	// MaxDeliverDeadline is the largest deadline_ms: sciontool never starts
	// the first visible step later than this after receipt.
	MaxDeliverDeadline = 92 * time.Second
	// DeliverNotAfter is how far after its send the hub sets
	// not_after_unix_ms.
	DeliverNotAfter = 92 * time.Second
	// DeliverSkewTolerance: a not_after earlier than receipt + deadline
	// minus this is treated as clock skew and ignored.
	DeliverSkewTolerance = 30 * time.Second
	// HubDeliverTimeout is the hub's call timeout for this route: above the
	// 92 s hard stop plus 8 s of paste and submits.
	HubDeliverTimeout = 110 * time.Second
	// BrokerDeliverTimeout is how long the broker waits for the hub's
	// /deliver answer; it outlasts the hub's own timeout.
	BrokerDeliverTimeout = 115 * time.Second
	// StaleDelivering is how long a message row may stay "delivering"
	// before the hub's stale sweep moves it to "partial" (2 × 110 s).
	StaleDelivering = 2 * HubDeliverTimeout
)

// DeliverOp values; they are tmuxdeliver's ops.
const (
	OpPaste           = string(tmuxdeliver.OpPaste)
	OpInterruptPaste  = string(tmuxdeliver.OpInterruptPaste)
	OpSubmitOnly      = string(tmuxdeliver.OpSubmitOnly)
	OpInterruptSubmit = string(tmuxdeliver.OpInterruptSubmit)
)

// ExtraSubmits is the request's extra-submit block.
type ExtraSubmits struct {
	Count   int `json:"count"`
	DelayMS int `json:"delay_ms"`
}

// ReadySpec is the request's readiness block, for op=paste only. It holds
// the broker's resolved harness timings, used when the image writes no
// ready spec of its own (gap G4: there is no nonce; sciontool reads the
// init-run nonce locally).
type ReadySpec struct {
	Strategy  string `json:"strategy"`
	SettleMS  int    `json:"settle_ms"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// DeliverRequest is the deliver-message request body (design v2.22 §1.2).
// The broker sends it to the hub's /deliver endpoint; the hub overwrites
// BrokerID and NotAfterUnixMS and forwards it to the agent.
type DeliverRequest struct {
	V              int           `json:"v"`
	DeliveryID     string        `json:"delivery_id"`
	BrokerID       string        `json:"broker_id"`
	MessageIDs     []string      `json:"message_ids"`
	Op             string        `json:"op"`
	Text           *string       `json:"text,omitempty"`
	InterruptKeys  []string      `json:"interrupt_keys,omitempty"`
	DeadlineMS     int64         `json:"deadline_ms"`
	NotAfterUnixMS int64         `json:"not_after_unix_ms"`
	SubmitKeys     []string      `json:"submit_keys"`
	ExtraSubmits   *ExtraSubmits `json:"extra_submits,omitempty"`
	Ready          *ReadySpec    `json:"ready,omitempty"`
}

// Fields a FieldError names.
const (
	FieldBody          = "body"
	FieldV             = "v"
	FieldDeliveryID    = "delivery_id"
	FieldBrokerID      = "broker_id"
	FieldMessageIDs    = "message_ids"
	FieldOp            = "op"
	FieldText          = "text"
	FieldInterruptKeys = tmuxdeliver.FieldInterruptKeys
	FieldDeadlineMS    = "deadline_ms"
	FieldNotAfter      = "not_after_unix_ms"
	FieldSubmitKeys    = tmuxdeliver.FieldSubmitKeys
	FieldExtraSubmits  = tmuxdeliver.FieldExtraSubmits
	FieldExtraDelay    = tmuxdeliver.FieldExtraSubmitDelay
	FieldReady         = "ready"
	FieldReadyStrategy = tmuxdeliver.FieldReadyStrategy
	FieldReadySettle   = tmuxdeliver.FieldReadySettle
	FieldReadyTimeout  = tmuxdeliver.FieldReadyTimeout
)

// FieldError reports the first request field that is malformed or outside
// the route bounds. Its message names the field only, never a value.
type FieldError struct {
	Field string
}

func (e *FieldError) Error() string {
	return "deliver-message: invalid field " + e.Field
}

// SizeError reports a request that is well formed but larger than the
// route carries: text over MaxDeliverText or MaxDeliverRequestBytes once
// encoded, or more than MaxDeliverMessageIDs ids. Unlike a FieldError it is
// not permanent: the delivery can go over exec instead (design v2.22 §1.6;
// review R3). The hub and the agent answer it with 413.
type SizeError struct {
	Field string
}

func (e *SizeError) Error() string {
	return "deliver-message: too large for the route: " + e.Field
}

// FitsRoute reports, as a *SizeError, whether r is within the route's size
// limits, including its encoded size. The broker checks it before choosing
// conduit transport.
func (r *DeliverRequest) FitsRoute() error {
	if len(r.MessageIDs) > MaxDeliverMessageIDs {
		return &SizeError{Field: FieldMessageIDs}
	}
	if r.Text != nil && len(*r.Text) > MaxDeliverText {
		return &SizeError{Field: FieldText}
	}
	b, err := json.Marshal(r)
	if err != nil || len(b) > MaxDeliverRequestBytes {
		return &SizeError{Field: FieldText}
	}
	return nil
}

// idPattern is the shape of delivery, broker and message ids.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// ValidID reports whether id has the shape of a delivery, broker or message
// id.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// DecodeDeliverRequest decodes body strictly: unknown fields, a second JSON
// value or trailing data are a FieldError on FieldBody. It does not
// validate; call Validate.
func DecodeDeliverRequest(body []byte) (*DeliverRequest, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var r DeliverRequest
	if err := dec.Decode(&r); err != nil {
		return nil, &FieldError{Field: FieldBody}
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, &FieldError{Field: FieldBody}
	}
	return &r, nil
}

// Validate checks r against the fixed schema and the route bounds (design
// v2.22 §1.2, §1.7, §5.3) and returns a *FieldError naming the first field
// that fails, in a fixed order, or a *SizeError for a size limit. No value
// is ever clamped. NotAfterUnixMS
// and BrokerID must be set; the hub sets both before forwarding.
func (r *DeliverRequest) Validate() error {
	if err := r.validateCommon(); err != nil {
		return err
	}
	if !ValidID(r.BrokerID) {
		return &FieldError{Field: FieldBrokerID}
	}
	if r.NotAfterUnixMS <= 0 {
		return &FieldError{Field: FieldNotAfter}
	}
	return nil
}

// ValidateFromBroker is Validate for the request as the broker sends it to
// the hub: BrokerID and NotAfterUnixMS are ignored, since the hub
// overwrites both.
func (r *DeliverRequest) ValidateFromBroker() error {
	return r.validateCommon()
}

func (r *DeliverRequest) validateCommon() error {
	if r.V != DeliverVersion {
		return &FieldError{Field: FieldV}
	}
	if !ValidID(r.DeliveryID) {
		return &FieldError{Field: FieldDeliveryID}
	}
	if len(r.MessageIDs) == 0 {
		return &FieldError{Field: FieldMessageIDs}
	}
	if len(r.MessageIDs) > MaxDeliverMessageIDs {
		return &SizeError{Field: FieldMessageIDs}
	}
	seen := make(map[string]struct{}, len(r.MessageIDs))
	for _, id := range r.MessageIDs {
		if !ValidID(id) {
			return &FieldError{Field: FieldMessageIDs}
		}
		if _, dup := seen[id]; dup {
			return &FieldError{Field: FieldMessageIDs}
		}
		seen[id] = struct{}{}
	}
	op := tmuxdeliver.Op(r.Op)
	if !op.Valid() {
		return &FieldError{Field: FieldOp}
	}
	pastes := op == tmuxdeliver.OpPaste || op == tmuxdeliver.OpInterruptPaste
	interrupts := op == tmuxdeliver.OpInterruptPaste || op == tmuxdeliver.OpInterruptSubmit
	if pastes {
		if r.Text == nil || *r.Text == "" {
			return &FieldError{Field: FieldText}
		}
		if len(*r.Text) > MaxDeliverText {
			return &SizeError{Field: FieldText}
		}
	} else if r.Text != nil {
		return &FieldError{Field: FieldText}
	}
	if interrupts {
		if len(r.InterruptKeys) == 0 {
			return &FieldError{Field: FieldInterruptKeys}
		}
	} else if r.InterruptKeys != nil {
		return &FieldError{Field: FieldInterruptKeys}
	}
	if err := tmuxdeliver.CheckKeys(FieldInterruptKeys, r.InterruptKeys); err != nil {
		return &FieldError{Field: FieldInterruptKeys}
	}
	if r.DeadlineMS <= 0 || time.Duration(r.DeadlineMS)*time.Millisecond > MaxDeliverDeadline {
		return &FieldError{Field: FieldDeadlineMS}
	}
	if len(r.SubmitKeys) == 0 {
		return &FieldError{Field: FieldSubmitKeys}
	}
	if err := tmuxdeliver.CheckKeys(FieldSubmitKeys, r.SubmitKeys); err != nil {
		return &FieldError{Field: FieldSubmitKeys}
	}
	if r.ExtraSubmits != nil {
		if !pastes {
			return &FieldError{Field: FieldExtraSubmits}
		}
		if r.ExtraSubmits.Count < 0 || r.ExtraSubmits.Count > tmuxdeliver.MaxExtraSubmits {
			return &FieldError{Field: FieldExtraSubmits}
		}
		if r.ExtraSubmits.DelayMS < 0 || time.Duration(r.ExtraSubmits.DelayMS)*time.Millisecond > tmuxdeliver.MaxExtraSubmitDelay {
			return &FieldError{Field: FieldExtraDelay}
		}
	}
	if r.Ready != nil {
		if op != tmuxdeliver.OpPaste {
			return &FieldError{Field: FieldReady}
		}
		switch r.Ready.Strategy {
		case "hook", "delay":
		default:
			return &FieldError{Field: FieldReadyStrategy}
		}
		if r.Ready.SettleMS < 0 || time.Duration(r.Ready.SettleMS)*time.Millisecond > tmuxdeliver.MaxReadySettle {
			return &FieldError{Field: FieldReadySettle}
		}
		if r.Ready.TimeoutMS < 0 || time.Duration(r.Ready.TimeoutMS)*time.Millisecond > tmuxdeliver.MaxReadyTimeout {
			return &FieldError{Field: FieldReadyTimeout}
		}
	}
	return nil
}

// Outcomes in a deliver-message answer (design v2.22 §1.3).
const (
	OutcomeDelivered          = "delivered"
	OutcomePartial            = "partial"
	OutcomeInProgress         = "in_progress"
	OutcomeBusy               = "busy"
	OutcomeExpiredBeforePaste = "expired_before_paste"
	OutcomeInvalid            = "invalid"
	// OutcomeTooLarge is a 413: the request is over a size limit; nothing
	// was done, and the delivery may use exec.
	OutcomeTooLarge = "too_large"
	// OutcomeNotDelivered is a 422: nothing visible was done (the tmux
	// target is missing, or staging the text failed).
	OutcomeNotDelivered = "not_delivered"
	// OutcomeOutOfBounds is a 422: the agent image's own readiness spec is
	// outside the route bounds (Field names it). Nothing was done; the
	// delivery should use exec, which applies the spec in full.
	OutcomeOutOfBounds = "out_of_bounds"
	// OutcomeInFlight is the hub's own 409: the same delivery is already
	// in flight under another call, which owns it and reports its outcome.
	// Never forwarded to the agent; the caller neither retries, falls back
	// to exec, nor reports a failure.
	OutcomeInFlight = "in_flight"
	// OutcomeBound is the hub's own 409: a row is bound to another
	// delivery, or past delivering. Never forwarded to the agent.
	OutcomeBound = "bound"
)

// Stages, diagnostic only (design v2.22 §1.3, r3.1).
const (
	StageTarget      = "target"
	StageInterrupt   = "interrupt"
	StageLoadBuffer  = "load_buffer"
	StagePaste       = "paste"
	StageSubmit      = "submit"
	StageExtraSubmit = "extra_submit"
)

// DeliverResponse is every deliver-message answer body.
type DeliverResponse struct {
	Outcome  string `json:"outcome"`
	Stage    string `json:"stage,omitempty"`
	Field    string `json:"field,omitempty"`
	Replayed bool   `json:"replayed,omitempty"`
}

// DecodeDeliverResponse decodes an answer body. A body that does not decode
// or has no outcome is an error: callers must never read it as success.
func DecodeDeliverResponse(body []byte) (DeliverResponse, error) {
	var r DeliverResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return DeliverResponse{}, fmt.Errorf("deliver-message: malformed answer: %w", err)
	}
	if r.Outcome == "" {
		return DeliverResponse{}, errors.New("deliver-message: answer has no outcome")
	}
	return r, nil
}
