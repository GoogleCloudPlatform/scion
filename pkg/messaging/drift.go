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

import "fmt"

// Drift states for conversations.
const (
	DriftStateActive       = "active"
	DriftStateOrphaned     = "orphaned"
	DriftStateUnresolvable = "unresolvable"
)

// Drift triggers describe events that can cause state transitions.
const (
	DriftTriggerSendFailedNotFound  = "send_failed_not_found"
	DriftTriggerSendFailedPermanent = "send_failed_permanent"
	DriftTriggerInboundMessage      = "inbound_message"
	DriftTriggerSendAttempt         = "send_attempt"
)

// DriftTransition describes a state transition in the drift state machine.
type DriftTransition struct {
	From    string // current state
	Trigger string // what happened
	To      string // new state
}

// ErrInvalidDriftTransition is returned when a drift state transition is not
// allowed by the state machine.
type ErrInvalidDriftTransition struct {
	From    string
	Trigger string
	Reason  string
}

func (e *ErrInvalidDriftTransition) Error() string {
	return fmt.Sprintf("invalid drift transition from %q on trigger %q: %s", e.From, e.Trigger, e.Reason)
}

// TransitionDriftState computes the next drift state given the current state
// and a trigger event. Returns the new state or an error if the transition is
// not permitted.
//
// State machine (§2.11):
//
//	active  + send_failed_not_found  → orphaned
//	active  + send_failed_permanent  → unresolvable
//	orphaned + inbound_message       → active (resurrection)
//	orphaned + send_attempt          → error (fail fast, do not fall back)
//	unresolvable + any               → error (terminal, needs manual intervention)
func TransitionDriftState(current string, trigger string) (string, error) {
	// Terminal state — nothing can leave unresolvable.
	if current == DriftStateUnresolvable {
		return "", &ErrInvalidDriftTransition{
			From:    current,
			Trigger: trigger,
			Reason:  "unresolvable is terminal; manual intervention required",
		}
	}

	switch current {
	case DriftStateActive:
		switch trigger {
		case DriftTriggerSendFailedNotFound:
			return DriftStateOrphaned, nil
		case DriftTriggerSendFailedPermanent:
			return DriftStateUnresolvable, nil
		default:
			return "", &ErrInvalidDriftTransition{
				From:    current,
				Trigger: trigger,
				Reason:  "no transition defined",
			}
		}

	case DriftStateOrphaned:
		switch trigger {
		case DriftTriggerInboundMessage:
			return DriftStateActive, nil // resurrection
		case DriftTriggerSendAttempt:
			return "", &ErrInvalidDriftTransition{
				From:    current,
				Trigger: trigger,
				Reason:  "cannot send to orphaned conversation; fail fast",
			}
		default:
			return "", &ErrInvalidDriftTransition{
				From:    current,
				Trigger: trigger,
				Reason:  "no transition defined",
			}
		}

	default:
		return "", &ErrInvalidDriftTransition{
			From:    current,
			Trigger: trigger,
			Reason:  "unknown drift state",
		}
	}
}
