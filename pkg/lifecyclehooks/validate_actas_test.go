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

package lifecyclehooks

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The execution-identity actAs gate (svc-accnt item D).
//
// These tests are about PERMISSION. The structural-validation tests in
// validate_test.go deliberately pass the disabled checker; anything here that
// needs a verdict scripts store.NewFakeCallerPermissionChecker instead.

const hookSAEmail = "hooks@example.iam.gserviceaccount.com"

// hookWithIdentity returns a hook that is valid in every respect except
// possibly the caller's permission, so that a failure is attributable to the
// gate and not to some other field.
func hookWithIdentity() *store.LifecycleHook {
	return validHTTPHook()
}

func hasExecutionIdentityError(t *testing.T, err error) bool {
	t.Helper()
	if err == nil {
		return false
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected a *ValidationError, got %T: %v", err, err)
	}
	for _, fe := range ve.Errors {
		if fe.Field == "executionIdentity" {
			return true
		}
	}
	return false
}

func executionIdentityMessage(t *testing.T, err error) string {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected a *ValidationError, got %T: %v", err, err)
	}
	for _, fe := range ve.Errors {
		if fe.Field == "executionIdentity" {
			return fe.Message
		}
	}
	return ""
}

// A caller the checker has granted actAs on the target passes.
func TestExecutionIdentity_AllowedCallerPasses(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hookSAEmail)

	err := ValidateHook(context.Background(), hookWithIdentity(), defaultResolver(),
		defaultCaller(), checker, noAudit())
	if err != nil {
		t.Fatalf("expected the hook to validate for a permitted caller, got: %v", err)
	}
}

// A caller the checker denies is refused, and the message names the permission
// so the reader knows what to grant.
func TestExecutionIdentity_DeniedCallerRejected(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker().DenyTarget(hookSAEmail, "no actAs grant")

	err := ValidateHook(context.Background(), hookWithIdentity(), defaultResolver(),
		defaultCaller(), checker, noAudit())
	if !hasExecutionIdentityError(t, err) {
		t.Fatalf("a caller without actAs must not be able to set this execution identity; got: %v", err)
	}
	if msg := executionIdentityMessage(t, err); !strings.Contains(msg, store.PermissionActAs) {
		t.Errorf("denial message should name %q so the operator knows what to grant; got: %q",
			store.PermissionActAs, msg)
	}
}

// Indeterminate denies. This is the whole reason the outcome is three-valued:
// "the check could not reach an answer" must not read as permission.
func TestExecutionIdentity_IndeterminateDenies(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker().
		IndeterminateTarget(hookSAEmail, "troubleshooter returned UNKNOWN_INFO")

	err := ValidateHook(context.Background(), hookWithIdentity(), defaultResolver(),
		defaultCaller(), checker, noAudit())
	if !hasExecutionIdentityError(t, err) {
		t.Fatal("an indeterminate actAs result must deny, not pass")
	}
}

// A transport failure denies too, and specifically must not be turned into a
// pass by the error being handled separately from the outcome.
func TestExecutionIdentity_CheckerErrorDenies(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker().
		FailTarget(hookSAEmail, errors.New("IAM API timeout"))

	err := ValidateHook(context.Background(), hookWithIdentity(), defaultResolver(),
		defaultCaller(), checker, noAudit())
	if !hasExecutionIdentityError(t, err) {
		t.Fatal("a checker transport error must deny, not pass")
	}
}

// ⚠️ nil is a WIRING BUG, NOT A SETTING. This is the test that stops a future
// call site from switching the gate off by forgetting to pass a checker. If it
// ever starts failing, the fix is at the call site, never here.
func TestExecutionIdentity_NilCheckerDenies(t *testing.T) {
	err := ValidateHook(context.Background(), hookWithIdentity(), defaultResolver(),
		defaultCaller(), nil, noAudit())
	if !hasExecutionIdentityError(t, err) {
		t.Fatal("a nil checker must deny; switching the check off is done with " +
			"store.NewDisabledCallerPermissionChecker, not with nil")
	}
}

// The disabled checker allows — a toggle must not be an outage. Paired with the
// nil test above, these two pin the distinction the whole design rests on:
// absence denies, explicit-off allows.
func TestExecutionIdentity_DisabledCheckerAllows(t *testing.T) {
	err := ValidateHook(context.Background(), hookWithIdentity(), defaultResolver(),
		defaultCaller(), store.NewDisabledCallerPermissionChecker(), noAudit())
	if err != nil {
		t.Fatalf("the disabled checker must allow, so that turning the check off "+
			"is not an outage; got: %v", err)
	}
}

// A caller with no GCP identity is denied without the checker ever being asked.
// There is nothing that could have been granted actAs to a block-mode agent.
func TestExecutionIdentity_CallerWithoutGCPIdentityDenied(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hookSAEmail)
	blockModeAgent := store.Principal{Kind: store.PrincipalAgent, ID: "agent-001"}

	err := ValidateHook(context.Background(), hookWithIdentity(), defaultResolver(),
		blockModeAgent, checker, noAudit())
	if !hasExecutionIdentityError(t, err) {
		t.Fatal("an agent with no GCP identity has nothing that could hold actAs and must be denied")
	}
	// Scripted to ALLOW above: if the checker had been consulted the hook would
	// have validated. Its not being consulted is the assertion.
	if n := len(checker.Calls()); n != 0 {
		t.Errorf("a caller with no GCP identity must be denied without a round-trip; got %d call(s)", n)
	}
}

// The zero Principal is denied. A call site that forgets to populate the caller
// must fail closed rather than inherit whatever the checker says.
func TestExecutionIdentity_ZeroPrincipalDenied(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hookSAEmail)

	err := ValidateHook(context.Background(), hookWithIdentity(), defaultResolver(),
		store.Principal{}, checker, noAudit())
	if !hasExecutionIdentityError(t, err) {
		t.Fatal("the zero Principal is PrincipalUnknown and must be denied")
	}
}

// An agent already running as the target account may configure a hook to use
// it: that grants nothing it does not already have. Asserted behaviourally —
// the checker is scripted to DENY, so a pass can only have come from the
// same-account path.
func TestExecutionIdentity_SameAccountPropagationAllowed(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker().DenyTarget(hookSAEmail, "should not be consulted")
	sameSA := store.Principal{
		Kind:                store.PrincipalAgent,
		ID:                  "agent-001",
		ServiceAccountEmail: hookSAEmail,
	}

	err := ValidateHook(context.Background(), hookWithIdentity(), defaultResolver(), sameSA, checker, noAudit())
	if err != nil {
		t.Fatalf("an agent already acting as the target account escalates nothing by "+
			"naming it as a hook identity; got: %v", err)
	}
	if n := len(checker.Calls()); n != 0 {
		t.Errorf("same-account propagation must not make an IAM round-trip; got %d call(s)", n)
	}
}

// Case-insensitively, because GCP service account emails are not case
// sensitive and a case difference is not a different principal.
func TestExecutionIdentity_SameAccountIsCaseInsensitive(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker().DenyTarget(hookSAEmail, "should not be consulted")
	sameSA := store.Principal{
		Kind:                store.PrincipalAgent,
		ID:                  "agent-001",
		ServiceAccountEmail: strings.ToUpper(hookSAEmail),
	}

	err := ValidateHook(context.Background(), hookWithIdentity(), defaultResolver(), sameSA, checker, noAudit())
	if err != nil {
		t.Fatalf("a case difference is not a different service account; got: %v", err)
	}
}

// A DIFFERENT account is not propagation. The guard above must not degrade into
// "any agent with any GCP identity passes".
func TestExecutionIdentity_DifferentAccountIsNotPropagation(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker().DenyTarget(hookSAEmail, "no actAs grant")
	otherSA := store.Principal{
		Kind:                store.PrincipalAgent,
		ID:                  "agent-001",
		ServiceAccountEmail: "something-else@example.iam.gserviceaccount.com",
	}

	err := ValidateHook(context.Background(), hookWithIdentity(), defaultResolver(), otherSA, checker, noAudit())
	if !hasExecutionIdentityError(t, err) {
		t.Fatal("holding one service account must not confer the right to run as another")
	}
}

// A hook with no execution identity needs no permission. Webhooks legitimately
// have none, and the gate must not invent a requirement where there is no
// identity to authorize.
func TestExecutionIdentity_EmptyIdentityNeedsNoPermission(t *testing.T) {
	h := validHTTPHook()
	h.Action.Type = store.LifecycleHookActionWebhook
	h.ExecutionIdentity = ""

	// nil checker: if the gate ran at all this would deny.
	err := ValidateHook(context.Background(), h, defaultResolver(), store.Principal{}, nil, noAudit())
	if err != nil {
		t.Fatalf("a hook with no execution identity has nothing to authorize; got: %v", err)
	}
}

// The permission check is independent of the other execution-identity checks.
// An unverified account denied to a permitted caller still reports the
// verification problem — the gate must not mask unrelated validation.
func TestExecutionIdentity_PermissionIsIndependentOfVerification(t *testing.T) {
	h := validHTTPHook()
	h.ExecutionIdentity = "sa-002" // unverified

	checker := store.NewFakeCallerPermissionChecker().
		AllowTarget("pending@example.iam.gserviceaccount.com")

	err := ValidateHook(context.Background(), h, defaultResolver(), defaultCaller(), checker, noAudit())
	if !hasExecutionIdentityError(t, err) {
		t.Fatal("an unverified account must still be rejected for a permitted caller")
	}
	if msg := executionIdentityMessage(t, err); !strings.Contains(msg, "not verified") {
		t.Errorf("the verification failure must still be reported; got: %q", msg)
	}
}

// ⚠️ THE SCOPE ASSERTIONS ARE NOT THE PERMISSION CHECK, and item D must not
// have quietly turned one into the other. These two sites answer a SAME-SCOPE
// question, not a reachability or permission one, and svc-accnt is under a hard
// constraint not to modify them. This test pins them from the outside: a caller
// with full actAs permission is still refused an out-of-scope account.
func TestExecutionIdentity_PermissionDoesNotBypassScope(t *testing.T) {
	h := validHTTPHook() // hub-scoped hook
	h.ExecutionIdentity = "sa-003"

	checker := store.NewFakeCallerPermissionChecker().
		AllowTarget("proj@example.iam.gserviceaccount.com")

	err := ValidateHook(context.Background(), h, defaultResolver(), defaultCaller(), checker, noAudit())
	if !hasExecutionIdentityError(t, err) {
		t.Fatal("actAs permission must not admit a project-scoped account into a hub-scoped hook")
	}
	if msg := executionIdentityMessage(t, err); !strings.Contains(msg, "hub-scoped hook requires") {
		t.Errorf("the scope failure must still be the reported reason; got: %q", msg)
	}
}

// The caller reaching the checker is the one passed in, unmodified. Ancestry is
// deliberately not consulted, so nothing here should be substituting a
// different principal.
func TestExecutionIdentity_ChecksTheImmediateCaller(t *testing.T) {
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(hookSAEmail)
	caller := store.Principal{
		Kind:                store.PrincipalAgent,
		ID:                  "agent-042",
		ServiceAccountEmail: "agent-own-sa@example.iam.gserviceaccount.com",
	}

	if err := ValidateHook(context.Background(), hookWithIdentity(), defaultResolver(),
		caller, checker, noAudit()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	calls := checker.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one actAs check, got %d", len(calls))
	}
	if calls[0].Caller.ID != "agent-042" {
		t.Errorf("the immediate caller must be the one checked; got %q", calls[0].Caller.ID)
	}
	if calls[0].TargetSAEmail != hookSAEmail {
		t.Errorf("expected the target to be the hook's execution identity; got %q", calls[0].TargetSAEmail)
	}
}
