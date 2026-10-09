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
	"encoding/json"
	"time"
)

// StartDispatchArgs carries the parameters for a cross-node agent start.
// Only fields that the owner's DispatchAgentStart cannot re-derive are
// included. Env/secret resolution is performed by the OWNER via
// DispatchAgentStart (all hub instances share the same store + secret
// backend), so resolved env/secrets are NOT serialized here.
type StartDispatchArgs struct {
	Task   string `json:"task,omitempty"`
	Resume bool   `json:"resume,omitempty"`
}

// RestartDispatchArgs is intentionally empty — the owner's
// DispatchAgentRestart re-resolves auth tokens and identity vars from the
// shared store on the owning node.
type RestartDispatchArgs struct{}

// StopDispatchArgs carries the parameters for a queued stop. A stop needs
// nothing beyond what the dispatch row already carries (agentID, projectID),
// except for a stop queued while the broker was offline: IntentAt is then the
// run_intent_at of the stop intent the row was queued for, and the drain
// applies the row only if that intent is still the current one.
//
// RunID is the run the stop was dispatched for (ptone/scion#2550). The
// executing node sends it to the broker in place of the row's current run
// ID, so a queued stop never stops a run started after it was queued. Empty
// (a row queued by an older hub, or an agent with no run ID) stops with the
// row's run ID as before.
type StopDispatchArgs struct {
	IntentAt *time.Time `json:"intentAt,omitempty"`
	// SupersedesClaim is the start claim the agent held when the stop was
	// recorded; the drain releases it once the stop is applied.
	SupersedesClaim string `json:"supersedesClaim,omitempty"`
	RunID           string `json:"runId,omitempty"`
}

// DeleteDispatchArgs carries the parameters for a cross-node agent delete.
// RunID is the run the delete was dispatched for, and PreviousRunIDs are
// the agent's previous runs the delete also names (ptone/scion#3097), both
// from the requesting node's copy of the agent (the delete engine's claim
// snapshot) when the intent is written (ptone/scion#2550). The owning node
// sends exactly these, never its own later read of the row, so an intent
// written for run A never deletes a run B started since. An intent with no
// RunID (written by an older hub, or for an agent with no run ID) uses the
// re-read row's run and, when the intent lists none, its previous runs, as
// before.
// Claim, when non-zero, is the delete engine's deletion claim: the executing
// node sends the delete (the current run's and each previous run's) only
// while that claim is still the row's current, live one (ptone/scion#2906).
type DeleteDispatchArgs struct {
	DeleteFiles    bool      `json:"deleteFiles,omitempty"`
	RemoveBranch   bool      `json:"removeBranch,omitempty"`
	SoftDelete     bool      `json:"softDelete,omitempty"`
	DeletedAt      time.Time `json:"deletedAt,omitempty"`
	RunID          string    `json:"runId,omitempty"`
	PreviousRunIDs []string  `json:"previousRunIds,omitempty"`
	Claim          int64     `json:"claim,omitempty"`
}

// CheckPromptDispatchArgs is intentionally empty — the agent slug/ID in the
// dispatch row is sufficient for the owner to run the local check.
type CheckPromptDispatchArgs struct{}

// FinalizeEnvDispatchArgs carries the gathered env vars for cross-node finalize.
type FinalizeEnvDispatchArgs struct {
	Env map[string]string `json:"env,omitempty"`
}

// CreateWithGatherDispatchArgs is intentionally empty — the owner rebuilds the
// full RemoteCreateAgentRequest from the shared store (same pattern as start).
type CreateWithGatherDispatchArgs struct{}

// CheckPromptResult is serialized into broker_dispatch.result by the owner.
type CheckPromptResult struct {
	HasPrompt bool `json:"hasPrompt"`
}

// FinalizeEnvResult is serialized into broker_dispatch.result by the owner.
type FinalizeEnvResult struct {
	Success bool `json:"success"`
	// Launch is set when the owner's send was accepted for asynchronous
	// launch.
	Launch *LaunchAccepted `json:"launch,omitempty"`
	// Warnings are the owner's dispatch warnings, such as the outcome of
	// the compensating delete of a run that landed after a delete won
	// (ptone/scion#3456). The requester adds them to its own collector.
	Warnings []string `json:"warnings,omitempty"`
}

// CreateWithGatherResult is serialized into broker_dispatch.result by the owner.
type CreateWithGatherResult struct {
	EnvRequirements *RemoteEnvRequirementsResponse `json:"envRequirements,omitempty"`
	// Launch is set when the owner's send was accepted for asynchronous
	// launch.
	Launch *LaunchAccepted `json:"launch,omitempty"`
	// Warnings are the owner's dispatch warnings (see FinalizeEnvResult).
	Warnings []string `json:"warnings,omitempty"`
}

// LifecycleDispatchResult is serialized into broker_dispatch.result by the
// owner of a completed start or restart (ptone/scion#3456). Rows written by
// owners that predate it carry an empty result, which decodes to the zero
// value.
type LifecycleDispatchResult struct {
	// Warnings are the owner's dispatch warnings, such as the outcome of
	// the compensating delete of a run that landed after a delete won.
	Warnings []string `json:"warnings,omitempty"`
	// DeleteWon reports that the broker start landed but a delete won
	// while it was in flight (deleteWonAfterLanding on the owner). The
	// agent never reaches the start's success phase then, so the requester
	// stops waiting for it once the row is done.
	DeleteWon bool `json:"deleteWon,omitempty"`
}

// marshalLifecycleResult returns the result for a completed start or restart
// row: "" when there is nothing to carry, as before.
func marshalLifecycleResult(r LifecycleDispatchResult) string {
	if len(r.Warnings) == 0 && !r.DeleteWon {
		return ""
	}
	b, err := json.Marshal(r)
	if err != nil {
		return ""
	}
	return string(b)
}

// decodeLifecycleResult decodes a completed start or restart row's result;
// an empty or unreadable result is the zero value.
func decodeLifecycleResult(result string) LifecycleDispatchResult {
	var r LifecycleDispatchResult
	if result != "" {
		_ = json.Unmarshal([]byte(result), &r)
	}
	return r
}

// MarshalDispatchArgs serializes a dispatch args struct to JSON for storage in
// broker_dispatch.args.
func MarshalDispatchArgs(v interface{}) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// UnmarshalStartArgs deserializes start dispatch args from the broker_dispatch row.
func UnmarshalStartArgs(raw string) (*StartDispatchArgs, error) {
	var a StartDispatchArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// UnmarshalStopArgs deserializes stop dispatch args from the broker_dispatch row.
func UnmarshalStopArgs(raw string) (*StopDispatchArgs, error) {
	var a StopDispatchArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// UnmarshalDeleteArgs deserializes delete dispatch args from the broker_dispatch row.
func UnmarshalDeleteArgs(raw string) (*DeleteDispatchArgs, error) {
	var a DeleteDispatchArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// UnmarshalFinalizeEnvArgs deserializes finalize_env dispatch args.
func UnmarshalFinalizeEnvArgs(raw string) (*FinalizeEnvDispatchArgs, error) {
	var a FinalizeEnvDispatchArgs
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return nil, err
	}
	return &a, nil
}
