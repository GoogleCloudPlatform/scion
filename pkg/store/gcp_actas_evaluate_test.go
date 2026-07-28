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

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// EvaluateActAs is the single shared decision sequence. Every surface that
// gates on actAs routes through it, so the ordering pinned here is the ordering
// everywhere.

const evalTargetEmail = "target@example.iam.gserviceaccount.com"

func evalTarget() *GCPServiceAccount {
	return &GCPServiceAccount{ID: "sa-eval", Email: evalTargetEmail, Scope: "hub"}
}

func evalUser() Principal {
	return Principal{Kind: PrincipalUser, ID: "user-1", Email: "human@example.com"}
}

func TestEvaluateActAs_NilTargetDenies(t *testing.T) {
	checker := NewFakeCallerPermissionChecker().AllowTarget(evalTargetEmail)

	got, err := EvaluateActAs(context.Background(), checker, evalUser(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Outcome != ActAsDenied {
		t.Errorf("a nil target must deny; got %s", got.Outcome)
	}
	if got.Mechanism != MechanismNoTarget {
		t.Errorf("expected mechanism %q, got %q", MechanismNoTarget, got.Mechanism)
	}
}

// ⚠️ nil is a wiring bug and must deny. This is the counterpart to
// TestEvaluateActAs_DisabledCheckerAllows: absence denies, explicit-off allows.
// If these two ever agree, the distinction the design rests on is gone.
func TestEvaluateActAs_NilCheckerDenies(t *testing.T) {
	got, err := EvaluateActAs(context.Background(), nil, evalUser(), evalTarget())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Outcome != ActAsDenied {
		t.Errorf("a nil checker must deny; got %s", got.Outcome)
	}
	if got.Mechanism != MechanismCheckUnwired {
		t.Errorf("expected mechanism %q, got %q", MechanismCheckUnwired, got.Mechanism)
	}
}

func TestEvaluateActAs_DisabledCheckerAllows(t *testing.T) {
	got, err := EvaluateActAs(context.Background(), NewDisabledCallerPermissionChecker(),
		evalUser(), evalTarget())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Outcome != ActAsAllowed {
		t.Errorf("the disabled checker must allow so a toggle is not an outage; got %s", got.Outcome)
	}
	if got.Mechanism != MechanismCheckDisabled {
		t.Errorf("expected mechanism %q, got %q", MechanismCheckDisabled, got.Mechanism)
	}
}

// ⚠️ THE NIL CHECK COMES BEFORE THE SAME-ACCOUNT FAST PATH. A fast path that
// succeeded while the gate was unwired would mask the wiring bug for exactly
// the callers most likely to hit it. Ordering test, not a behaviour preference.
func TestEvaluateActAs_NilCheckerDeniesEvenForSameAccount(t *testing.T) {
	sameSA := Principal{Kind: PrincipalAgent, ID: "agent-1", ServiceAccountEmail: evalTargetEmail}

	got, _ := EvaluateActAs(context.Background(), nil, sameSA, evalTarget())
	if got.Outcome != ActAsDenied {
		t.Errorf("an unwired checker must deny even on the same-account path; got %s", got.Outcome)
	}
	if got.Mechanism != MechanismCheckUnwired {
		t.Errorf("expected mechanism %q, got %q", MechanismCheckUnwired, got.Mechanism)
	}
}

func TestEvaluateActAs_SameAccountAllowsWithoutRoundTrip(t *testing.T) {
	// Scripted to DENY: a pass can only have come from the same-account path.
	checker := NewFakeCallerPermissionChecker().DenyTarget(evalTargetEmail, "should not be consulted")
	sameSA := Principal{Kind: PrincipalAgent, ID: "agent-1", ServiceAccountEmail: evalTargetEmail}

	got, _ := EvaluateActAs(context.Background(), checker, sameSA, evalTarget())
	if got.Outcome != ActAsAllowed {
		t.Errorf("a caller already acting as the target escalates nothing; got %s", got.Outcome)
	}
	if got.Mechanism != MechanismSameAccount {
		t.Errorf("expected mechanism %q, got %q", MechanismSameAccount, got.Mechanism)
	}
	if n := len(checker.Calls()); n != 0 {
		t.Errorf("same-account must not contact GCP; got %d call(s)", n)
	}
}

func TestEvaluateActAs_SameAccountIsCaseInsensitive(t *testing.T) {
	checker := NewFakeCallerPermissionChecker().DenyTarget(evalTargetEmail, "should not be consulted")
	sameSA := Principal{
		Kind:                PrincipalAgent,
		ID:                  "agent-1",
		ServiceAccountEmail: strings.ToUpper(evalTargetEmail),
	}

	got, _ := EvaluateActAs(context.Background(), checker, sameSA, evalTarget())
	if got.Outcome != ActAsAllowed {
		t.Errorf("a case difference is not a different service account; got %s", got.Outcome)
	}
}

// An empty caller email must never match an empty target email. Without the
// non-empty guards this would make every identity-less caller a permitted user
// of a malformed account.
func TestEvaluateActAs_EmptyEmailsDoNotMatch(t *testing.T) {
	checker := NewFakeCallerPermissionChecker()
	blockMode := Principal{Kind: PrincipalAgent, ID: "agent-1"} // no SA email
	malformed := &GCPServiceAccount{ID: "sa-bad", Email: ""}    // no email

	got, _ := EvaluateActAs(context.Background(), checker, blockMode, malformed)
	if got.Outcome == ActAsAllowed {
		t.Fatal("empty-equals-empty must not be treated as same-account propagation")
	}
	if got.Mechanism != MechanismNoCallerIdentity {
		t.Errorf("expected mechanism %q, got %q", MechanismNoCallerIdentity, got.Mechanism)
	}
}

func TestEvaluateActAs_NoCallerIdentityDeniesWithoutRoundTrip(t *testing.T) {
	checker := NewFakeCallerPermissionChecker().AllowTarget(evalTargetEmail)
	blockMode := Principal{Kind: PrincipalAgent, ID: "agent-1"}

	got, _ := EvaluateActAs(context.Background(), checker, blockMode, evalTarget())
	if got.Outcome != ActAsDenied {
		t.Errorf("a caller with no GCP identity must be denied; got %s", got.Outcome)
	}
	if got.Mechanism != MechanismNoCallerIdentity {
		t.Errorf("expected mechanism %q, got %q", MechanismNoCallerIdentity, got.Mechanism)
	}
	// The checker was scripted to allow: not consulting it is the assertion.
	if n := len(checker.Calls()); n != 0 {
		t.Errorf("nothing to check means no, without a round-trip; got %d call(s)", n)
	}
}

func TestEvaluateActAs_ZeroPrincipalDenies(t *testing.T) {
	checker := NewFakeCallerPermissionChecker().AllowTarget(evalTargetEmail)

	got, _ := EvaluateActAs(context.Background(), checker, Principal{}, evalTarget())
	if got.Outcome != ActAsDenied {
		t.Errorf("the zero Principal is PrincipalUnknown and must be denied; got %s", got.Outcome)
	}
}

func TestEvaluateActAs_DelegatesToChecker(t *testing.T) {
	t.Run("allow", func(t *testing.T) {
		checker := NewFakeCallerPermissionChecker().AllowTarget(evalTargetEmail)
		got, err := EvaluateActAs(context.Background(), checker, evalUser(), evalTarget())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Outcome != ActAsAllowed {
			t.Errorf("expected the checker's allow to be returned; got %s", got.Outcome)
		}
		if n := len(checker.Calls()); n != 1 {
			t.Errorf("expected exactly one round-trip; got %d", n)
		}
	})

	t.Run("deny", func(t *testing.T) {
		checker := NewFakeCallerPermissionChecker().DenyTarget(evalTargetEmail, "no grant")
		got, _ := EvaluateActAs(context.Background(), checker, evalUser(), evalTarget())
		if got.Outcome != ActAsDenied {
			t.Errorf("expected the checker's denial to be returned; got %s", got.Outcome)
		}
	})

	t.Run("indeterminate is not an allow", func(t *testing.T) {
		checker := NewFakeCallerPermissionChecker().IndeterminateTarget(evalTargetEmail, "UNKNOWN_INFO")
		got, _ := EvaluateActAs(context.Background(), checker, evalUser(), evalTarget())
		if got.Allowed() {
			t.Error("indeterminate must not read as permission")
		}
	})
}

// ⚠️ A checker returning (Allowed, err) is malformed, and this is the one place
// that can stop that becoming an allow. The contract says error carries no
// verdict; EvaluateActAs enforces it rather than trusting it.
func TestEvaluateActAs_ErrorForcesIndeterminateEvenIfCheckerSaidAllowed(t *testing.T) {
	boom := errors.New("IAM API timeout")
	checker := &malformedChecker{result: ActAsResult{Outcome: ActAsAllowed, Mechanism: "bogus"}, err: boom}

	got, err := EvaluateActAs(context.Background(), checker, evalUser(), evalTarget())
	if !errors.Is(err, boom) {
		t.Errorf("the error must be returned for diagnostics; got %v", err)
	}
	if got.Outcome != ActAsIndeterminate {
		t.Errorf("an error must force indeterminate regardless of the reported outcome; got %s", got.Outcome)
	}
	if got.Allowed() {
		t.Error("a transport failure must never read as permission")
	}
}

// ⚠️ A FAILED CHECK MUST NOT LEAVE BEHIND A VERDICT-SHAPED EXPLANATION. Forcing
// the outcome is not enough on its own: a record reading "indeterminate /
// iam-getIamPolicy / caller has roles/iam.serviceAccountUser" asserts that IAM
// was consulted and answered, which is the precise distinction the mechanism
// field exists to preserve. The reason from a call that produced no verdict is
// worse than no reason, because it reads as evidence.
func TestEvaluateActAs_ErrorReplacesMechanismAndReason(t *testing.T) {
	checker := &malformedChecker{
		result: ActAsResult{
			Outcome:   ActAsAllowed,
			Mechanism: "iam-getIamPolicy",
			Reason:    "caller has roles/iam.serviceAccountUser",
		},
		err: errors.New("IAM API timeout"),
	}

	got, _ := EvaluateActAs(context.Background(), checker, evalUser(), evalTarget())

	if got.Mechanism != MechanismCheckFailed {
		t.Errorf("expected mechanism %q, got %q", MechanismCheckFailed, got.Mechanism)
	}
	if strings.Contains(got.Reason, "roles/iam.serviceAccountUser") {
		t.Errorf("the checker's verdict-shaped reason must not survive a failed check; got: %q", got.Reason)
	}
	// The attempted mechanism is still useful for triage, so it is preserved
	// inside the reason rather than discarded outright.
	if !strings.Contains(got.Reason, "iam-getIamPolicy") {
		t.Errorf("the attempted mechanism should be recorded for triage; got: %q", got.Reason)
	}
}

// The raw error must not reach Reason: Reason is surfaced to callers on denial,
// and a remote IAM error is not a string this package can promise is free of
// policy content. Consumers log the error separately.
func TestEvaluateActAs_ErrorTextDoesNotLeakIntoReason(t *testing.T) {
	checker := &malformedChecker{
		result: ActAsResult{Outcome: ActAsIndeterminate, Mechanism: "iam-getIamPolicy"},
		err:    errors.New("rpc error: policy binding user:secret@example.com role roles/owner"),
	}

	got, _ := EvaluateActAs(context.Background(), checker, evalUser(), evalTarget())
	if strings.Contains(got.Reason, "secret@example.com") {
		t.Errorf("raw error text must not be folded into the caller-visible reason; got: %q", got.Reason)
	}
}

// ⚠️ AN UNATTRIBUTABLE ALLOW IS NOT AN ALLOW. A permit nobody can account for
// defeats the whole reason the mechanism field is mandatory, so it is
// downgraded rather than stamped with a neutral word and honoured.
func TestEvaluateActAs_AllowWithoutMechanismIsNotHonoured(t *testing.T) {
	checker := &malformedChecker{result: ActAsResult{Outcome: ActAsAllowed}} // no mechanism

	got, _ := EvaluateActAs(context.Background(), checker, evalUser(), evalTarget())
	if got.Allowed() {
		t.Fatal("an allow that names no check must not be honoured")
	}
	if got.Mechanism != MechanismUnattributableAllow {
		t.Errorf("expected mechanism %q so the checker bug is alertable, got %q",
			MechanismUnattributableAllow, got.Mechanism)
	}
}

// A denial with no mechanism stays a denial — already the safe direction — but
// is named rather than left blank so it can be alerted on.
func TestEvaluateActAs_DenialWithoutMechanismIsNamed(t *testing.T) {
	checker := &malformedChecker{result: ActAsResult{Outcome: ActAsDenied}} // no mechanism

	got, _ := EvaluateActAs(context.Background(), checker, evalUser(), evalTarget())
	if got.Outcome != ActAsDenied {
		t.Errorf("a denial must remain a denial; got %s", got.Outcome)
	}
	if got.Mechanism != MechanismUnspecified {
		t.Errorf("expected mechanism %q, got %q", MechanismUnspecified, got.Mechanism)
	}
}

// Mechanism is mandatory on every emitted result, whatever the checker did.
func TestEvaluateActAs_MechanismIsNeverEmpty(t *testing.T) {
	cases := []ActAsResult{
		{Outcome: ActAsAllowed},
		{Outcome: ActAsDenied},
		{Outcome: ActAsIndeterminate},
	}
	for _, c := range cases {
		got, _ := EvaluateActAs(context.Background(), &malformedChecker{result: c}, evalUser(), evalTarget())
		if got.Mechanism == "" {
			t.Errorf("mechanism must never be empty; outcome %s produced a blank", c.Outcome)
		}
	}
}

// malformedChecker returns whatever it is told to, including combinations the
// CallerPermissionChecker contract forbids, so that EvaluateActAs's defences
// against a non-conforming implementation can be exercised.
type malformedChecker struct {
	result ActAsResult
	err    error
}

func (m *malformedChecker) CanActAs(_ context.Context, _ Principal, _ *GCPServiceAccount) (ActAsResult, error) {
	return m.result, m.err
}
