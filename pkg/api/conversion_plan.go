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

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ConversionPlanPresentationSchemaVersion is the schema version of
// ConversionPlanPresentation this build produces and accepts.
const ConversionPlanPresentationSchemaVersion = 1

// ConversionPlanPresentation is the publishable form of a legacy Runtime
// Broker's plan for moving to flat Runtime Broker instances. The Runtime
// Broker computes it from its own settings and reports it on every
// heartbeat; the Hub stores it read-only and serves it from
// GET /api/v1/runtime-brokers/{id}/conversion-plan.
//
// It carries nothing a flat instance does not already publish: instances are built
// only by config.PresentInstance, and a finding has no message, because the
// converter's message can quote settings values. Paths are settings key
// paths, never values.
type ConversionPlanPresentation struct {
	SchemaVersion int `json:"schemaVersion"`
	// Instances are the proposed instances, each as
	// config.PresentInstance(inst).API().
	Instances []RuntimeTargetPresentation `json:"instances"`
	// ProfileMap maps a profile name to its proposed instance key. A
	// profile with a blocking finding has no entry.
	ProfileMap map[string]string `json:"profileMap"`
	// Findings are the converter's findings, as code, severity, key path
	// and profiles only.
	Findings []ConversionPlanFinding `json:"findings"`
	// ComputedAt is the Runtime Broker's time of the last recompute.
	ComputedAt time.Time `json:"computedAt"`
}

// ConversionPlanFinding is one converter finding without its message.
type ConversionPlanFinding struct {
	Code     string   `json:"code"`
	Severity string   `json:"severity"`
	Path     string   `json:"path"`
	Profiles []string `json:"profiles"`
}

// ConversionPlanTruncatedCode is the finding a Runtime Broker adds when it
// had to drop part of the plan to fit the caps. It is blocking: a truncated
// plan may have lost a blocking finding or an instance and must not look
// convertible.
const ConversionPlanTruncatedCode = "plan_truncated"

// Caps on a reported ConversionPlanPresentation. The Runtime Broker trims
// its plan to fit them before sending; the Hub drops a snapshot that does
// not fit, keeping the stored one.
const (
	ConversionPlanMaxBytes       = 64 << 10
	ConversionPlanMaxInstances   = 64
	ConversionPlanMaxFindings    = 512
	ConversionPlanMaxProfileMap  = 256
	ConversionPlanMaxStringBytes = 256
)

// ErrConversionPlanOverCap reports a snapshot outside the caps. Its text
// never includes snapshot content.
var ErrConversionPlanOverCap = errors.New("conversion plan presentation is over a cap")

// CheckConversionPlanCaps checks an encoded ConversionPlanPresentation
// against the caps: the encoded size first, before any decode, then the
// element counts and the length of every string, map keys included,
// anywhere in the document. It returns ErrConversionPlanOverCap (wrapped
// with the name of the cap, never content) or a decode error.
func CheckConversionPlanCaps(encoded []byte) error {
	if len(encoded) > ConversionPlanMaxBytes {
		return fmt.Errorf("%w: encoded size", ErrConversionPlanOverCap)
	}
	var counts struct {
		Instances  []json.RawMessage          `json:"instances"`
		ProfileMap map[string]json.RawMessage `json:"profileMap"`
		Findings   []json.RawMessage          `json:"findings"`
	}
	if err := json.Unmarshal(encoded, &counts); err != nil {
		return errors.New("conversion plan presentation does not decode")
	}
	switch {
	case len(counts.Instances) > ConversionPlanMaxInstances:
		return fmt.Errorf("%w: instances", ErrConversionPlanOverCap)
	case len(counts.Findings) > ConversionPlanMaxFindings:
		return fmt.Errorf("%w: findings", ErrConversionPlanOverCap)
	case len(counts.ProfileMap) > ConversionPlanMaxProfileMap:
		return fmt.Errorf("%w: profileMap", ErrConversionPlanOverCap)
	}
	if !ConversionPlanStringsFit(encoded) {
		return fmt.Errorf("%w: string length", ErrConversionPlanOverCap)
	}
	return nil
}

// ConversionPlanStringsFit reports whether every string in an encoded JSON
// value, map keys included, is within ConversionPlanMaxStringBytes. A
// Runtime Broker uses it to drop whole elements that would fail the cap.
func ConversionPlanStringsFit(encoded []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return false
	}
	return stringsWithin(doc, ConversionPlanMaxStringBytes)
}

// stringsWithin reports whether every string in v, map keys included, is
// at most max bytes long.
func stringsWithin(v any, max int) bool {
	switch t := v.(type) {
	case string:
		return len(t) <= max
	case map[string]any:
		for k, e := range t {
			if len(k) > max || !stringsWithin(e, max) {
				return false
			}
		}
	case []any:
		for _, e := range t {
			if !stringsWithin(e, max) {
				return false
			}
		}
	}
	return true
}

// ConversionPlanResponse is the body of
// GET /api/v1/runtime-brokers/{id}/conversion-plan: the stored snapshot
// plus what the Hub computes itself.
type ConversionPlanResponse struct {
	BrokerID string `json:"brokerId"`
	// ObservedAt is the Hub time the snapshot was last written or
	// confirmed.
	ObservedAt *time.Time                 `json:"observedAt,omitempty"`
	Plan       ConversionPlanPresentation `json:"plan"`
	Agents     ConversionPlanAgents       `json:"agents"`
	Projects   ConversionPlanProjects     `json:"projects"`
	Capacity   ConversionPlanCapacity     `json:"capacity"`
}

// ConversionPlanProfileRef is a profile name and the proposed instance it
// maps to through the snapshot's profile map.
type ConversionPlanProfileRef struct {
	Profile string `json:"profile"`
	// Mapping is "mapped", "no_mapping" or "profile_unknown".
	Mapping  string `json:"mapping"`
	Instance string `json:"instance,omitempty"`
}

// ConversionPlanAgents lists the non-terminal agents bound to the legacy
// Runtime Broker that the caller can read.
type ConversionPlanAgents struct {
	Items []ConversionPlanAgent `json:"items"`
	// HiddenCount is the number of bound agents the caller cannot read.
	HiddenCount int `json:"hiddenCount"`
	// TerminalCount is the number of readable bound agents in a terminal
	// phase, which are not listed.
	TerminalCount int `json:"terminalCount"`
}

// ConversionPlanAgent is one bound agent.
type ConversionPlanAgent struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	ProjectID string `json:"projectId,omitempty"`
	Phase     string `json:"phase,omitempty"`
	ConversionPlanProfileRef
}

// ConversionPlanProjects lists the projects the legacy Runtime Broker
// provides that the caller can read.
type ConversionPlanProjects struct {
	Items []ConversionPlanProject `json:"items"`
	// HiddenCount is the number of providing projects the caller cannot
	// read; nothing else about them is returned.
	HiddenCount int `json:"hiddenCount"`
}

// ConversionPlanProject is one providing project and its profile-valued
// settings.
type ConversionPlanProject struct {
	ID            string                    `json:"id"`
	Name          string                    `json:"name"`
	ActiveProfile *ConversionPlanProfileRef `json:"activeProfile,omitempty"`
	// ServiceAccountProfiles are the profile keys of the project's
	// per-profile default GCP service accounts (keys only).
	ServiceAccountProfiles []ConversionPlanProfileRef `json:"serviceAccountProfiles,omitempty"`
}

// ConversionPlanCapacity is the proposed per-instance agent limit, planned
// by the Hub with its own values. Allocations are keyed by proposed
// instance key: the instances have no IDs before registration.
type ConversionPlanCapacity struct {
	Outcome     string                     `json:"outcome"`
	MaxAgents   *int64                     `json:"maxAgents,omitempty"`
	Allocations []ConversionPlanAllocation `json:"allocations,omitempty"`
	Reasons     []string                   `json:"reasons,omitempty"`
	Flags       []string                   `json:"flags,omitempty"`
}

// ConversionPlanAllocation is one proposed instance limit.
type ConversionPlanAllocation struct {
	Instance  string `json:"instance"`
	MaxAgents int64  `json:"maxAgents"`
}
