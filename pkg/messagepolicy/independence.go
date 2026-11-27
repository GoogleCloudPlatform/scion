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

package messagepolicy

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

// VaryingCopyCELIdents are the top-level CEL variables that can vary across
// copy targets sharing the same (surface, input) context.
var VaryingCopyCELIdents = []string{"recipient", "conversation"}

// VaryingCopySystemOneInputs are the System One content-environment input
// fields that can vary across copy targets sharing the same (surface, input).
var VaryingCopySystemOneInputs = []string{
	FieldRecipientSlug,
	FieldRecipientKind,
	FieldConversationUnmentionedAgentSlug,
}

// SystemOneRuleReferencesAnyInput compiles rawRule against the message_policy
// v1 ContentEnvironment and reports whether any of fields appears in the
// compiled selected-input list. It is conservative: any compile error returns
// (true, err).
func SystemOneRuleReferencesAnyInput(rawRule string, fields ...string) (bool, error) {
	trimmed := strings.TrimSpace(rawRule)
	if trimmed == "" || len(fields) == 0 {
		return false, nil
	}
	contentEnv := SystemOneContentEnv()
	compiled, err := spec.Compile([]byte(trimmed), spec.CompileOptions{
		Kind:        spec.Kind(KindJudgments),
		Environment: &contentEnv,
	})
	if err != nil {
		return true, err
	}
	for _, f := range fields {
		if slices.Contains(compiled.Inputs, f) {
			return true, nil
		}
	}
	return false, nil
}

// IsRecipientIndependent reports whether a policy (violationRule + optional
// systemOneRule) is proven independent of recipient-varying facts across copy
// units sharing the same (surface, input) context.
//
// It is conservative (R1): any compile error, unknown identifier, missing
// checked AST, or reference to VaryingCopyCELIdents / VaryingCopySystemOneInputs
// returns (false, errOrNil).
func (e *Evaluator) IsRecipientIndependent(ctx context.Context, violationRule, systemOneRule string) (bool, error) {
	return e.isRecipientIndependent(ctx, "", 0, false, violationRule, systemOneRule)
}

// IsRecipientIndependentCached is like IsRecipientIndependent but uses the
// evaluator's content-addressed CEL compile cache for policyID@revision.
func (e *Evaluator) IsRecipientIndependentCached(ctx context.Context, policyID string, revision int64, violationRule, systemOneRule string) (bool, error) {
	return e.isRecipientIndependent(ctx, policyID, revision, true, violationRule, systemOneRule)
}

func (e *Evaluator) isRecipientIndependent(
	ctx context.Context,
	policyID string,
	revision int64,
	useCache bool,
	violationRule string,
	systemOneRule string,
) (bool, error) {
	if e == nil || e.engine == nil {
		return false, errors.New("messagepolicy: nil evaluator")
	}
	if strings.TrimSpace(violationRule) == "" {
		return false, errors.New("messagepolicy: empty violation_rule")
	}
	var (
		compiled ruleengine.Compiled
		err      error
	)
	if useCache && policyID != "" {
		compiled, err = e.engine.CachedCompile(
			ctx,
			ruleengine.DialectCEL,
			ScopeForPolicy(policyID, revision),
			violationRule,
			KindViolation,
			ViolationEnvironmentRef(),
		)
	} else {
		compiled, err = e.engine.Compile(
			ctx,
			ruleengine.DialectCEL,
			violationRule,
			KindViolation,
			ViolationEnvironmentRef(),
		)
	}
	if err != nil {
		return false, err
	}
	prog, ok := compiled.Rep().(celengine.Program)
	if !ok || prog.IsZero() {
		return false, errors.New("messagepolicy: invalid compiled CEL program")
	}
	refsVarying, err := prog.ReferencesIdent(VaryingCopyCELIdents...)
	if err != nil {
		return false, err
	}
	if refsVarying {
		return false, nil
	}

	if strings.TrimSpace(systemOneRule) != "" {
		s1Refs, s1Err := SystemOneRuleReferencesAnyInput(systemOneRule, VaryingCopySystemOneInputs...)
		if s1Err != nil {
			return false, s1Err
		}
		if s1Refs {
			return false, nil
		}
	}

	return true, nil
}
