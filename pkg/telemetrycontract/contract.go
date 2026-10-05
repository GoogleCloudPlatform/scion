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

// Package telemetrycontract holds the canonical usage-metrics contract
// (metric names, label keys and the token_type enum) shared by sciontool's
// telemetry receiver (the producer) and pkg/hub's metrics dashboard (the
// consumer). It has no dependency on either package, so the hub never
// depends on sciontool, and a rename on one side fails the contract test
// on the other (see ptone/scion#2053, design §3.6 "Contract pinning").
//
// Design references in this package (section N, Dn) are to
// .design/hosted/usage-telemetry.md.
package telemetrycontract

import (
	"strings"
	"unicode/utf8"
)

// Canonical metric names. Both are exported under the
// "workload.googleapis.com/" prefix by the GCP exporter, and unprefixed by
// the generic OTLP exporter.
const (
	// MetricAPICalls counts one completed model response (success) or a
	// failed request the source reports (error). It predates this contract
	// and is kept for Cloud descriptor compatibility (design D1).
	MetricAPICalls = "gen_ai.api.calls"

	// MetricUsageTokens counts tokens attributed to model requests, broken
	// down by TokenTypeLabel. It replaces the retired scion.hook.tokens.*
	// family (design D1).
	MetricUsageTokens = "scion.usage.tokens"

	// MetricSessionCount is unrelated to usage but is part of the dashboard
	// contract test's completeness check.
	MetricSessionCount = "agent.session.count"
)

// Canonical point label keys.
const (
	// ProjectLabel is the exporter-stamped canonical project identity label,
	// taken from authoritative resource identity, never from a producer
	// (design D3).
	ProjectLabel = "scion_project_id"

	// AgentLabel is the exporter-stamped canonical agent identity label.
	AgentLabel = "scion_agent_id"

	// AgentSlugLabel is the exporter-stamped display label for AgentLabel,
	// 1:1 with the agent ID (design D7).
	AgentSlugLabel = "scion_agent_slug"

	// ModelLabel is the producer-set model label, shared by both usage
	// metrics.
	ModelLabel = "model"

	// HarnessLabel identifies the producer harness (from SCION_HARNESS).
	HarnessLabel = "harness"

	// StatusLabel is StatusSuccess or StatusError, set on MetricAPICalls.
	StatusLabel = "status"

	// TokenTypeLabel is the closed enum label on MetricUsageTokens.
	TokenTypeLabel = "token_type"
)

// LabelKV is a plain string key/value pair. It exists so this package can
// hand callers ordered label sets without taking a dependency on any
// specific metrics client (this package has none, by design; see the
// package doc).
type LabelKV struct{ Key, Value string }

// UsageTokenPointAttrs returns the producer point-label set for
// MetricUsageTokens (design §3.2): harness and model only, each omitted if
// empty. Producers pass a model already resolved through ResolveModelLabel,
// so in practice the model label is always present (UnknownModel at
// worst). token_type is per-point (one of the TokenType* values) and added
// by the caller alongside these.
//
// Unlike MetricAPICalls, which keeps agent_id/project_id for Cloud
// descriptor compatibility, MetricUsageTokens's GCP allowlist does not
// permit them: a point carrying any key outside {harness, model, token_type}
// gets the whole OTLP request rejected, taking the rest of that flush (calls,
// tool and session metrics included) with it. This function is the single
// place the hook handler (pkg/sciontool/hooks/handlers) and its admission
// regression test (pkg/sciontool/telemetry) build this set from, so the two
// can't drift apart again.
func UsageTokenPointAttrs(harness, model string) []LabelKV {
	var attrs []LabelKV
	if harness != "" {
		attrs = append(attrs, LabelKV{HarnessLabel, harness})
	}
	if model != "" {
		attrs = append(attrs, LabelKV{ModelLabel, model})
	}
	return attrs
}

// MaxModelLabelBytes caps the ModelLabel value (design §3.2: "truncated to
// 128 characters"). The cut is made on a UTF-8 rune boundary, so a value is
// never split mid-character.
const MaxModelLabelBytes = 128

// UnknownModel is the ModelLabel value used when neither the source event
// nor SCION_MODEL names a model (design §3.2).
const UnknownModel = "unknown"

// ResolveModelLabel applies design §3.2's model label precedence: the
// source event's own model value when present (the actual model, including
// sub-agent models), then fallback (the agent's configured SCION_MODEL),
// then UnknownModel. Values are whitespace-trimmed before the emptiness
// check and truncated to MaxModelLabelBytes, so the result is always a
// non-empty, bounded string. Both usage producers -- the native
// UsageDeriver and the hook handler -- resolve the label through this one
// function so their precedence can't drift apart.
func ResolveModelLabel(native, fallback string) string {
	model := strings.TrimSpace(native)
	if model == "" {
		model = strings.TrimSpace(fallback)
	}
	if model == "" {
		return UnknownModel
	}
	return truncateUTF8(model, MaxModelLabelBytes)
}

// truncateUTF8 cuts s to at most maxBytes bytes without splitting a
// multi-byte rune.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

// Status values for StatusLabel.
const (
	StatusSuccess = "success"
	StatusError   = "error"
)

// Token type enum values for TokenTypeLabel (design §3.2). This is a closed
// set: any other value is an admission error.
const (
	// TokenTypeInput is non-cached prompt tokens.
	TokenTypeInput = "input"
	// TokenTypeOutput is generated tokens, including reasoning.
	TokenTypeOutput = "output"
	// TokenTypeCacheRead is tokens read from a prompt cache.
	TokenTypeCacheRead = "cache_read"
	// TokenTypeCacheWrite is tokens written to a prompt cache.
	TokenTypeCacheWrite = "cache_write"
	// TokenTypeReasoning is an informational subset of TokenTypeOutput,
	// never added to totals (it is already counted in "output").
	TokenTypeReasoning = "reasoning"
)

// TokenTypes lists the closed token_type enum in the order tokens are
// typically reported.
var TokenTypes = []string{TokenTypeInput, TokenTypeOutput, TokenTypeCacheRead, TokenTypeCacheWrite, TokenTypeReasoning}

// ValidTokenType reports whether v is a member of the closed token_type
// enum.
func ValidTokenType(v string) bool {
	switch v {
	case TokenTypeInput, TokenTypeOutput, TokenTypeCacheRead, TokenTypeCacheWrite, TokenTypeReasoning:
		return true
	default:
		return false
	}
}

// SummableTokenTypes are the token types that make up a usage total. Adding
// TokenTypeReasoning again would double count, since it already overlaps
// TokenTypeOutput (design §3.6 "DashboardSummary.TotalTokens").
var SummableTokenTypes = []string{TokenTypeInput, TokenTypeOutput, TokenTypeCacheRead, TokenTypeCacheWrite}
