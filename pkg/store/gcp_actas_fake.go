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
	"sync"
)

// FakeCallerPermissionChecker is a scriptable CallerPermissionChecker for
// tests. No test of the SA-assignment gate may require live GCP, so every
// consumer gates on this.
//
// It lives in a non-test file, unlike the mockGCPTokenGenerator pattern in
// pkg/hub, because two packages consume the interface — pkg/hub and
// pkg/lifecyclehooks — and a fake in a _test.go file is not importable across
// package boundaries. Duplicating it per package would let the copies drift,
// which matters here: the surfaces are supposed to reach the same decision,
// and tests that disagree about what the checker does cannot demonstrate that.
//
// It records every call, which is load-bearing rather than convenient. Two
// required behaviours are defined by the absence of a GCP round-trip —
// same-SA propagation and the disabled toggle — and the only way to assert
// "no call was made" is to be able to count calls.
type FakeCallerPermissionChecker struct {
	mu sync.Mutex

	// defaultResult applies when no per-target rule matches. It starts as a
	// denial so that a test which forgets to script a target fails closed and
	// is visible, rather than silently passing on an accidental allow.
	defaultResult ActAsResult
	defaultErr    error

	byTarget map[string]fakeActAsResponse

	calls []ActAsCall
}

type fakeActAsResponse struct {
	result ActAsResult
	err    error
}

// ActAsCall records one invocation of CanActAs.
type ActAsCall struct {
	Caller        Principal
	TargetSAID    string
	TargetSAEmail string
}

// NewFakeCallerPermissionChecker returns a fake that denies every caller until
// a target is scripted. Denial is the default deliberately: an unscripted
// target in a test should not read as permission.
func NewFakeCallerPermissionChecker() *FakeCallerPermissionChecker {
	return &FakeCallerPermissionChecker{
		defaultResult: ActAsResult{
			Outcome:   ActAsDenied,
			Mechanism: "fake",
			Reason:    "fake checker: target not scripted",
		},
		byTarget: make(map[string]fakeActAsResponse),
	}
}

// AllowTarget scripts an allow for the given target service account email.
func (f *FakeCallerPermissionChecker) AllowTarget(email string) *FakeCallerPermissionChecker {
	return f.setTarget(email, ActAsResult{
		Outcome:   ActAsAllowed,
		Mechanism: "fake",
		Reason:    "fake checker: scripted allow",
	}, nil)
}

// DenyTarget scripts a denial for the given target service account email.
func (f *FakeCallerPermissionChecker) DenyTarget(email, reason string) *FakeCallerPermissionChecker {
	return f.setTarget(email, ActAsResult{
		Outcome:   ActAsDenied,
		Mechanism: "fake",
		Reason:    reason,
	}, nil)
}

// IndeterminateTarget scripts an indeterminate outcome — the state a real
// checker reports when it cannot reach an answer, such as Policy
// Troubleshooter returning an UNKNOWN variant. Returns no error, because
// indeterminate is a legitimate answer rather than a failure.
func (f *FakeCallerPermissionChecker) IndeterminateTarget(email, reason string) *FakeCallerPermissionChecker {
	return f.setTarget(email, ActAsResult{
		Outcome:   ActAsIndeterminate,
		Mechanism: "fake",
		Reason:    reason,
	}, nil)
}

// FailTarget scripts a transport or programming failure. Per the interface
// contract the outcome accompanying an error is ActAsIndeterminate, never a
// denial — this models an API timeout, which is exactly the case where a
// caller that ignores err must still fail closed.
func (f *FakeCallerPermissionChecker) FailTarget(email string, err error) *FakeCallerPermissionChecker {
	return f.setTarget(email, ActAsResult{
		Outcome:   ActAsIndeterminate,
		Mechanism: "fake",
		Reason:    "fake checker: scripted failure",
	}, err)
}

func (f *FakeCallerPermissionChecker) setTarget(email string, result ActAsResult, err error) *FakeCallerPermissionChecker {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byTarget == nil {
		f.byTarget = make(map[string]fakeActAsResponse)
	}
	f.byTarget[email] = fakeActAsResponse{result: result, err: err}
	return f
}

// CanActAs implements CallerPermissionChecker.
func (f *FakeCallerPermissionChecker) CanActAs(_ context.Context, caller Principal, targetSA *GCPServiceAccount) (ActAsResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	call := ActAsCall{Caller: caller}
	if targetSA != nil {
		call.TargetSAID = targetSA.ID
		call.TargetSAEmail = targetSA.Email
	}
	f.calls = append(f.calls, call)

	if targetSA != nil {
		if resp, ok := f.byTarget[targetSA.Email]; ok {
			return resp.result, resp.err
		}
	}
	return f.defaultResult, f.defaultErr
}

// Calls returns a copy of the recorded invocations, in order.
func (f *FakeCallerPermissionChecker) Calls() []ActAsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ActAsCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// CallCount returns the number of times CanActAs was invoked. A count of zero
// is the assertion that proves a short-circuit fired without a GCP round-trip.
func (f *FakeCallerPermissionChecker) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// Reset clears recorded calls, leaving scripted targets in place.
func (f *FakeCallerPermissionChecker) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = nil
}

// Compile-time assertion that the fake satisfies the interface.
var _ CallerPermissionChecker = (*FakeCallerPermissionChecker)(nil)
