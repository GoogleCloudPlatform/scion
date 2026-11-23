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

package systemone

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
)

// DialectName is the ruleengine dialect identifier ("system_one").
const DialectName = runtime.DialectName

// Class aliases runtime.Class ("success" | "uncertain" | "failed").
type Class = runtime.Class

const (
	ClassSuccess   = runtime.ClassSuccess
	ClassUncertain = runtime.ClassUncertain
	ClassFailed    = runtime.ClassFailed
)

// Root provenance, input, and error type aliases re-exported from runtime so
// consumers and the ruleengine dialect (P1b/P1-reg) share one canonical type
// without circular imports (runtime-design §§4.1, 5, 6.3, 8.1; P1 brief §5).
type (
	Evaluation          = runtime.Evaluation
	EvaluationError     = runtime.EvaluationError
	AuthProvenance      = runtime.AuthProvenance
	SecretRefProvenance = runtime.SecretRefProvenance
	RuleRef             = runtime.RuleRef
	AnswerRecord        = runtime.AnswerRecord
	ThresholdCheck      = runtime.ThresholdCheck
	Attempt             = runtime.Attempt
	FieldFingerprint    = runtime.FieldFingerprint
	InputFingerprint    = runtime.InputFingerprint
	Input               = runtime.Input
	Decision            = runtime.Decision
)

// Request wraps a System One Input with consumer-authorized evaluation context
// for ruleengine.Input.Payload (t2-p2-trigger-binding-amendment §2.2, I3).
//
// Consumers evaluating rules on behalf of a project pass Request in
// ruleengine.Input.Payload so the dialect forwards ProjectID, RuleID,
// Revision, and optional ElementIndex into runtime.EvalOptions and evaluation
// provenance. Probes may continue passing a bare Input.
//
// Security contract: ProjectID MUST come from the consumer's authorized
// context (for example a Hub-owned trigger or message-policy record), never
// from untrusted caller input. The runtime trusts the ProjectID supplied here
// when resolving project-scoped named secrets and recording provenance.
type Request struct {
	Input Input
	// ProjectID is the consumer-authorized project ID for this evaluation.
	// Must come from the consumer's authorized context, never caller input.
	ProjectID string
	// RuleID identifies the consumer rule (for example a trigger or policy ID).
	RuleID string
	// Revision is the consumer rule's revision (for example authorization_revision).
	Revision int64
	// ElementIndex optionally records the 0-based element position inside an
	// ordered mixed-rule list for provenance only (never part of cache keys).
	ElementIndex *int
}

// ProbeInput constructs a single-field Input for the built-in P1 synthetic
// probe environment ("probe.text").
func ProbeInput(text string) Input {
	return runtime.ProbeInput(text)
}

// WithProjectID attaches a consumer-authorized project ID to ctx so authoring
// (Compile) and runtime calls can enforce per-project instance allowlists
// (t2-p2-trigger-binding-amendment D1) and project-scoped secret resolution.
func WithProjectID(ctx context.Context, projectID string) context.Context {
	return runtime.WithEvalProjectID(ctx, projectID)
}

// ProjectIDFromContext returns the validated project ID attached by
// WithProjectID or runtime.WithEvalProjectID, or "" when unset.
func ProjectIDFromContext(ctx context.Context) string {
	return runtime.EvalProjectIDFromContext(ctx)
}

// EvaluationFrom extracts the *Evaluation provenance record from err via
// errors.As, traversing any outer error wrapping (runtime-design §5, P1 brief §5).
func EvaluationFrom(err error) (*Evaluation, bool) {
	return runtime.EvaluationFrom(err)
}

// RetryableLater reports whether a consumer-level deferred retry (such as a
// durable trigger queue retry with backoff) may recover from reason
// (runtime-design §6.3).
func RetryableLater(reason provider.FailReason) bool {
	return reason.RetryableLater()
}
