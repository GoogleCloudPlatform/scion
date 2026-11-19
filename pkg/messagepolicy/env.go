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

// Package messagepolicy defines the message_policy CEL environment (v1),
// activation builder from typed outbound message facts, evaluation error
// classification, outcome mapping, content-free reason/remediation template
// rendering, and authoring validation helpers (ptone/scion#8047).
//
// It is a leaf consumer package: it imports only pkg/celengine, pkg/ruleengine,
// and Go standard library packages.
package messagepolicy

import (
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
)

// Consumer and environment identifiers (consumer design §5.4, brief §§1, 1a).
const (
	ConsumerName       = "message_policy"
	EnvironmentName    = "message_policy"
	EnvironmentVersion = 1

	// KindViolation is the boolean rule projection slot for violation_rule.
	KindViolation ruleengine.Kind = "violation"
)

// Size and budget bounds (consumer design §§5.3, 5.4; brief §1).
const (
	MaxViolationRuleBytes = 4096
	MaxSystemOneRuleBytes = 16384
	MaxBodyBytes          = 16 * 1024
	MaxTemplateBytes      = 1024
	MaxPolicyNameLen      = 63

	// DefaultSendBudget caps aggregate CEL evaluation time across matching
	// synchronous policies on a single outbound send (250 ms; brief §1).
	DefaultSendBudget = 250 * time.Millisecond
)

// Policy modes (consumer design §5.3).
const (
	ModeShadow  = "shadow"
	ModeEnforce = "enforce"
)

// Policy fallback outcomes (consumer design §§4.4, 5.3).
const (
	OutcomeAdmit  = "admit"
	OutcomeReject = "reject"
)

// Evaluation classes (consumer design §§4.3, 5.5, 5.8).
const (
	ClassPass      = "pass"
	ClassViolation = "violation"
	ClassUncertain = "uncertain"
	ClassFailed    = "failed"
)

// Failure reason codes produced or consumed by message_policy (consumer design §§5.3, 5.5, 5.8).
const (
	ReasonBodyTruncated          = "body_truncated"
	ReasonBudgetExhausted        = "budget_exhausted"
	ReasonPolicyCapExceeded      = "policy_cap_exceeded"
	ReasonFactsUnavailable       = "facts_unavailable"
	ReasonCompileError           = "compile_error"
	ReasonUninitializedProgram   = "uninitialized_program"
	ReasonInvalidCompiledProgram = "invalid_compiled_program"
	ReasonUnsupportedDialect     = "unsupported_dialect"
)

// Outbound message hook surfaces (consumer design §§5.2, 5.3).
// Only SurfaceAgentToUser and SurfaceAgentToConversation are enforced in M1 (H2).
const (
	SurfaceAgentToUser         = "agent_to_user"
	SurfaceAgentToConversation = "agent_to_conversation"
	SurfaceAgentDM             = "agent_dm"
	SurfaceGroupPost           = "group_post"
	SurfaceGroupSet            = "group_set"
	SurfaceMentionFanout       = "mention_fanout"
	SurfaceMentionDelivery     = "mention_delivery"
)

// Env returns the load-bearing message_policy v1 CEL Environment declaration
// (consumer design §5.4).
func Env() celengine.Environment {
	return celengine.Environment{
		Name:    EnvironmentName,
		Version: EnvironmentVersion,
		Variables: []celengine.Variable{
			{Name: "surface", Type: celengine.String()},
			{Name: "sender", Type: celengine.Map(celengine.String(), celengine.String())},
			{Name: "recipient", Type: celengine.Map(celengine.String(), celengine.String())},
			{Name: "message", Type: celengine.Map(celengine.String(), celengine.Dyn())},
			{Name: "mentioned_agent_ids", Type: celengine.List(celengine.String())},
			{Name: "mentioned_user_ids", Type: celengine.List(celengine.String())},
			{Name: "conversation", Type: celengine.Map(celengine.String(), celengine.Dyn())},
			{Name: "judgment_flags", Type: celengine.Map(celengine.String(), celengine.Bool())},
			{Name: "judgment_labels", Type: celengine.Map(celengine.String(), celengine.String())},
			{Name: "judgment_scores", Type: celengine.Map(celengine.String(), celengine.Double())},
		},
		Extensions: []celengine.Extension{celengine.ExtensionStrings},
		Macros:     celengine.MacrosStandard,
	}
}

// DefaultLimits returns the CEL ceiling Limits used by message_policy.
func DefaultLimits() celengine.Limits {
	return celengine.DefaultLimits()
}

// NewCELEngine constructs the message_policy *celengine.Engine with DefaultLimits
// and DefaultCacheConfig for registration on the Hub's shared CELDialect.
func NewCELEngine() (*celengine.Engine, error) {
	return celengine.New(Env(), DefaultLimits(), celengine.DefaultCacheConfig())
}

// ViolationEnvironmentRef returns the ruleengine.EnvironmentRef for compiling
// boolean violation_rule expressions against the message_policy environment.
func ViolationEnvironmentRef() ruleengine.EnvironmentRef {
	return ruleengine.EnvironmentRef{
		Consumer:     ConsumerName,
		Name:         EnvironmentName,
		MaxRuleBytes: MaxViolationRuleBytes,
		ResultType:   celengine.Bool(),
	}
}
