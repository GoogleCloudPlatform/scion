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

package store

import "time"

// AgentEffectiveConfig is the broker's verified effective configuration for
// one run of an agent (WP1 slice 3 addendum v12): what the broker actually
// resolved when it started the run, as opposed to the Hub-recorded request
// in AgentAppliedConfig. It is stored in agents.effective_config, written
// only by SetAgentEffectiveConfig, and fenced on the row's run, broker and
// liveness. Store-internal JSON; not a wire contract.
type AgentEffectiveConfig struct {
	// RunID is the run the echo belongs to (required).
	RunID string `json:"runId"`
	// LaunchID is the async launch the echo came from, when it came from a
	// launch report.
	LaunchID string `json:"launchId,omitempty"`

	Template              string `json:"template,omitempty"`
	TemplateHash          string `json:"templateHash,omitempty"`
	Harness               string `json:"harness,omitempty"`
	Model                 string `json:"model,omitempty"`
	HarnessConfig         string `json:"harnessConfig,omitempty"`
	HarnessConfigRevision string `json:"harnessConfigRevision,omitempty"`
	HarnessConfigSource   string `json:"harnessConfigSource,omitempty"`
	Image                 string `json:"image,omitempty"`
	Profile               string `json:"profile,omitempty"`
	Runtime               string `json:"runtime,omitempty"`

	// BrokerID is the broker the echo provably came from: the broker the
	// dispatch attempt was sent to, or the HMAC-authenticated reporting
	// broker. Never a broker_id the broker reported about itself.
	BrokerID    string `json:"brokerId,omitempty"`
	BrokerBuild string `json:"brokerBuild,omitempty"`

	// Source is EffectiveConfigSourceSync or EffectiveConfigSourceLaunchReport.
	Source string `json:"source,omitempty"`
	// ReportedAt is the hub commit time of the echo.
	ReportedAt time.Time `json:"reportedAt,omitempty"`
}

// Sources of an AgentEffectiveConfig.
const (
	EffectiveConfigSourceSync         = "sync"
	EffectiveConfigSourceLaunchReport = "launch_report"
)

// HasEffectiveFields reports whether c carries at least one field only a
// slice-3 broker echoes (template hash, harness, model or broker build).
// An echo without one is from an older broker and is never recorded.
func (c AgentEffectiveConfig) HasEffectiveFields() bool {
	return c.TemplateHash != "" || c.Harness != "" || c.Model != "" || c.BrokerBuild != ""
}

// SameEcho reports whether c and o are the same echo, ignoring ReportedAt
// (an identical retry).
func (c AgentEffectiveConfig) SameEcho(o AgentEffectiveConfig) bool {
	c.ReportedAt, o.ReportedAt = time.Time{}, time.Time{}
	return c == o
}

// Stale-echo reasons returned by SetAgentEffectiveConfig, in precedence
// order (the first failing check names the reason).
const (
	StaleEchoGone           = "gone"
	StaleEchoDeletedOrHeld  = "deleted_or_held"
	StaleEchoBrokerMismatch = "broker_mismatch"
	StaleEchoRunMismatch    = "run_mismatch"
	// StaleEchoUnsettledAdoption is a hub-side refinement of run_mismatch
	// for a launch-report echo (addendum v12 section 3); the store never
	// returns it.
	StaleEchoUnsettledAdoption = "unsettled_adoption"
)

// EffectiveConfigResult is SetAgentEffectiveConfig's outcome.
type EffectiveConfigResult struct {
	// Written: the echo is stored (written now, or already stored).
	Written bool
	// StaleReason names why the echo was not stored ("" when Written).
	StaleReason string
	// RowRunID, RowLaunchID and RowLaunchRunID are the row's run_id,
	// launch_id and launch_run_id from the classifying read (empty when the
	// row is gone).
	RowRunID       string
	RowLaunchID    string
	RowLaunchRunID string
}
