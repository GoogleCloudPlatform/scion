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
	Evaluation       = runtime.Evaluation
	EvaluationError  = runtime.EvaluationError
	AuthProvenance   = runtime.AuthProvenance
	RuleRef          = runtime.RuleRef
	AnswerRecord     = runtime.AnswerRecord
	ThresholdCheck   = runtime.ThresholdCheck
	Attempt          = runtime.Attempt
	FieldFingerprint = runtime.FieldFingerprint
	InputFingerprint = runtime.InputFingerprint
	Input            = runtime.Input
	Decision         = runtime.Decision
)

// ProbeInput constructs a single-field Input for the built-in P1 synthetic
// probe environment ("probe.text").
func ProbeInput(text string) Input {
	return runtime.ProbeInput(text)
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
