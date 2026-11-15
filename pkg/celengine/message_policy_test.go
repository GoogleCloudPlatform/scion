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

package celengine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
)

// messagePolicyEnv builds the provisional second-consumer environment fixture
// described in stage1-design.md §7.0 (system-one-mode message policy shape) to
// prove consumer neutrality without importing any messaging or hub package.
func messagePolicyEnv() celengine.Environment {
	return celengine.Environment{
		Name:    "message_policy",
		Version: 1,
		Variables: []celengine.Variable{
			{Name: "sender_id", Type: celengine.String()},
			{Name: "recipient_id", Type: celengine.String()},
			{Name: "project_id", Type: celengine.String()},
			{Name: "has_choices", Type: celengine.Bool()},
			{Name: "mentioned_agent_ids", Type: celengine.List(celengine.String())},
			{Name: "relation_permissions", Type: celengine.Map(celengine.String(), celengine.Bool())},
			{Name: "judgment_status", Type: celengine.Map(celengine.String(), celengine.String())},
			{Name: "judgment_flags", Type: celengine.Map(celengine.String(), celengine.Bool())},
			{Name: "judgment_labels", Type: celengine.Map(celengine.String(), celengine.String())},
			{Name: "judgment_scores", Type: celengine.Map(celengine.String(), celengine.Double())},
		},
		Extensions: []celengine.Extension{celengine.ExtensionStrings},
		Macros:     celengine.MacrosStandard,
	}
}

func TestMessagePolicyConsumerFixture(t *testing.T) {
	eng, err := celengine.New(messagePolicyEnv(), celengine.DefaultLimits(), celengine.DefaultCacheConfig())
	if err != nil {
		t.Fatalf("New(messagePolicyEnv): %v", err)
	}

	admitKind := celengine.Kind{Name: "admit", Result: celengine.Bool()}
	routeKind := celengine.Kind{Name: "route", Result: celengine.String()}

	admitProg, err := eng.Compile(
		`relation_permissions["can_interrupt"] && judgment_flags.urgent && judgment_scores.confidence >= 0.85`,
		admitKind,
	)
	if err != nil {
		t.Fatalf("Compile admit: %v", err)
	}

	routeProg, err := eng.Compile(
		`judgment_labels.topic == "security" ? "sec-reviewer" : "general-inbox"`,
		routeKind,
	)
	if err != nil {
		t.Fatalf("Compile route: %v", err)
	}

	baseAct := func() map[string]any {
		return map[string]any{
			"sender_id":            "agent:dev-1",
			"recipient_id":         "agent:lead",
			"project_id":           "proj-1",
			"has_choices":          false,
			"mentioned_agent_ids":  []string{"agent:lead"},
			"relation_permissions": map[string]bool{"can_interrupt": true},
			"judgment_status":      map[string]string{"urgent": "ok", "confidence": "ok", "topic": "ok"},
			"judgment_flags":       map[string]bool{"urgent": true},
			"judgment_labels":      map[string]string{"topic": "security"},
			"judgment_scores":      map[string]float64{"confidence": 0.92},
		}
	}

	ctx := context.Background()

	// Happy path: admit == true, route == "sec-reviewer".
	admitRes, _, err := admitProg.Eval(ctx, baseAct())
	if err != nil {
		t.Fatalf("Eval admit: %v", err)
	}
	if ok, err := admitRes.Bool(); err != nil || !ok {
		t.Fatalf("admit.Bool() = (%v, %v), want (true, nil)", ok, err)
	}

	routeRes, _, err := routeProg.Eval(ctx, baseAct())
	if err != nil {
		t.Fatalf("Eval route: %v", err)
	}
	if dest, err := routeRes.String(); err != nil || dest != "sec-reviewer" {
		t.Fatalf("route.String() = (%q, %v), want (\"sec-reviewer\", nil)", dest, err)
	}

	// Requirement: unguarded access to a missing judgment map key (judgment_flags.urgent
	// when "urgent" is absent from judgment_flags) MUST return EvalError{Reason: "missing_input"},
	// NEVER false.
	missingKeyAct := baseAct()
	missingKeyAct["judgment_flags"] = map[string]bool{} // "urgent" key absent
	res, _, err := admitProg.Eval(ctx, missingKeyAct)
	if res != nil {
		t.Fatalf("expected nil Result on missing judgment key, got %v", res)
	}
	var evalErr *celengine.EvalError
	if !errors.As(err, &evalErr) {
		t.Fatalf("expected *EvalError on missing judgment key, got %T: %v", err, err)
	}
	if evalErr.Reason != celengine.ReasonMissingInput {
		t.Fatalf("EvalError.Reason = %q, want %q", evalErr.Reason, celengine.ReasonMissingInput)
	}

	// Requirement: omitting a declared judgment variable altogether from the activation
	// ALSO returns EvalError{Reason: "missing_input"}, never false.
	missingVarAct := baseAct()
	delete(missingVarAct, "judgment_flags")
	res, _, err = admitProg.Eval(ctx, missingVarAct)
	if res != nil {
		t.Fatalf("expected nil Result on absent declared variable, got %v", res)
	}
	if !errors.As(err, &evalErr) || evalErr.Reason != celengine.ReasonMissingInput {
		t.Fatalf("absent variable got (%T, %v), want EvalError{Reason: %q}", err, err, celengine.ReasonMissingInput)
	}
}
