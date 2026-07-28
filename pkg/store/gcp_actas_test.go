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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The zero values are the security property of this file. Every assertion
// below exists because some future refactor could reorder a const block or add
// a field and quietly turn "not populated" into "allowed".

func TestActAsZeroValueIsIndeterminate(t *testing.T) {
	var outcome ActAsOutcome
	assert.Equal(t, ActAsIndeterminate, outcome,
		"the zero ActAsOutcome must be indeterminate; if a reordered const block makes it ActAsAllowed, every unpopulated result silently becomes an allow")

	var result ActAsResult
	assert.Equal(t, ActAsIndeterminate, result.Outcome)
	assert.False(t, result.Allowed(),
		"an unpopulated ActAsResult must not permit assignment")
}

func TestActAsResultAllowedOnlyForAllowed(t *testing.T) {
	assert.True(t, ActAsResult{Outcome: ActAsAllowed}.Allowed())
	assert.False(t, ActAsResult{Outcome: ActAsDenied}.Allowed())
	assert.False(t, ActAsResult{Outcome: ActAsIndeterminate}.Allowed(),
		"indeterminate is not an allow; callers wanting different treatment must test Outcome explicitly")
}

func TestActAsOutcomeString(t *testing.T) {
	assert.Equal(t, "allowed", ActAsAllowed.String())
	assert.Equal(t, "denied", ActAsDenied.String())
	assert.Equal(t, "indeterminate", ActAsIndeterminate.String())
	assert.Equal(t, "indeterminate", ActAsOutcome(99).String(),
		"an unrecognised outcome must render as indeterminate, not as an allow")
}

func TestPrincipalZeroValueHasNoGCPIdentity(t *testing.T) {
	var p Principal
	assert.Equal(t, PrincipalUnknown, p.Kind,
		"the zero PrincipalKind must be unknown so an unpopulated caller cannot be mistaken for an authorized one")
	assert.False(t, p.HasGCPIdentity())
	assert.Empty(t, p.GCPPrincipalID())
}

func TestPrincipalHasGCPIdentity(t *testing.T) {
	tests := []struct {
		name  string
		p     Principal
		want  bool
		wantI string
	}{
		{
			name:  "agent with SA",
			p:     Principal{Kind: PrincipalAgent, ServiceAccountEmail: "a@p.iam.gserviceaccount.com"},
			want:  true,
			wantI: "serviceAccount:a@p.iam.gserviceaccount.com",
		},
		{
			// This is the block-mode agent. It has no GCP identity by
			// configuration, so it cannot assign any SA.
			name:  "block-mode agent has no SA email",
			p:     Principal{Kind: PrincipalAgent},
			want:  false,
			wantI: "",
		},
		{
			name:  "user with email",
			p:     Principal{Kind: PrincipalUser, Email: "someone@example.com"},
			want:  true,
			wantI: "user:someone@example.com",
		},
		{
			name:  "user without email",
			p:     Principal{Kind: PrincipalUser},
			want:  false,
			wantI: "",
		},
		{
			// Broker and unidentified callers.
			name:  "unknown kind even with an SA email set",
			p:     Principal{Kind: PrincipalUnknown, ServiceAccountEmail: "a@p.iam.gserviceaccount.com"},
			want:  false,
			wantI: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.p.HasGCPIdentity())
			assert.Equal(t, tc.wantI, tc.p.GCPPrincipalID())
		})
	}
}

func TestPermissionActAsIsActAsNotTokenCreator(t *testing.T) {
	// Q3 ruled the caller-side permission is actAs. tokenCreator is the Hub's
	// own grant that enables the probe, not the thing checked on the caller.
	// Pinned as a test because the two are easy to confuse at a call site.
	assert.Equal(t, "iam.serviceAccounts.actAs", PermissionActAs)
	assert.NotContains(t, PermissionActAs, "tokenCreator")
	assert.NotContains(t, PermissionActAs, "getAccessToken")
}

func TestFakeDeniesUnscriptedTarget(t *testing.T) {
	f := NewFakeCallerPermissionChecker()
	sa := &GCPServiceAccount{ID: "sa-1", Email: "target@p.iam.gserviceaccount.com"}

	got, err := f.CanActAs(context.Background(), Principal{Kind: PrincipalAgent, ServiceAccountEmail: "caller@p.iam.gserviceaccount.com"}, sa)

	require.NoError(t, err)
	assert.Equal(t, ActAsDenied, got.Outcome,
		"an unscripted target must deny, so a test that forgets to script does not pass on an accidental allow")
	assert.False(t, got.Allowed())
}

func TestFakeScriptedOutcomes(t *testing.T) {
	const target = "target@p.iam.gserviceaccount.com"
	sa := &GCPServiceAccount{ID: "sa-1", Email: target}
	caller := Principal{Kind: PrincipalAgent, ServiceAccountEmail: "caller@p.iam.gserviceaccount.com"}

	t.Run("allow", func(t *testing.T) {
		f := NewFakeCallerPermissionChecker().AllowTarget(target)
		got, err := f.CanActAs(context.Background(), caller, sa)
		require.NoError(t, err)
		assert.True(t, got.Allowed())
	})

	t.Run("deny", func(t *testing.T) {
		f := NewFakeCallerPermissionChecker().DenyTarget(target, "no actAs grant")
		got, err := f.CanActAs(context.Background(), caller, sa)
		require.NoError(t, err)
		assert.Equal(t, ActAsDenied, got.Outcome)
		assert.Equal(t, "no actAs grant", got.Reason)
	})

	t.Run("indeterminate carries no error", func(t *testing.T) {
		f := NewFakeCallerPermissionChecker().IndeterminateTarget(target, "troubleshooter returned UNKNOWN_INFO")
		got, err := f.CanActAs(context.Background(), caller, sa)
		require.NoError(t, err,
			"indeterminate is a legitimate answer, not a failure, so it must not be reported through error")
		assert.Equal(t, ActAsIndeterminate, got.Outcome)
		assert.False(t, got.Allowed())
	})

	t.Run("transport failure pairs an error with indeterminate", func(t *testing.T) {
		boom := errors.New("iam api timeout")
		f := NewFakeCallerPermissionChecker().FailTarget(target, boom)
		got, err := f.CanActAs(context.Background(), caller, sa)
		require.ErrorIs(t, err, boom)
		assert.Equal(t, ActAsIndeterminate, got.Outcome,
			"the outcome accompanying an error must be indeterminate, never denied and never allowed, so a caller that ignores err still fails closed")
		assert.False(t, got.Allowed())
	})
}

func TestFakeRecordsCalls(t *testing.T) {
	const target = "target@p.iam.gserviceaccount.com"
	f := NewFakeCallerPermissionChecker().AllowTarget(target)
	sa := &GCPServiceAccount{ID: "sa-1", Email: target}
	caller := Principal{Kind: PrincipalAgent, ID: "agent-7", ServiceAccountEmail: "caller@p.iam.gserviceaccount.com"}

	assert.Equal(t, 0, f.CallCount(), "a fresh fake has made no calls")

	_, err := f.CanActAs(context.Background(), caller, sa)
	require.NoError(t, err)

	require.Equal(t, 1, f.CallCount())
	calls := f.Calls()
	require.Len(t, calls, 1)
	assert.Equal(t, "agent-7", calls[0].Caller.ID)
	assert.Equal(t, "sa-1", calls[0].TargetSAID)
	assert.Equal(t, target, calls[0].TargetSAEmail)

	// Reset clears history but keeps the script, so a test can assert on a
	// second phase without rebuilding the fake.
	f.Reset()
	assert.Equal(t, 0, f.CallCount())
	got, err := f.CanActAs(context.Background(), caller, sa)
	require.NoError(t, err)
	assert.True(t, got.Allowed(), "Reset must not clear scripted targets")
}

func TestFakeHandlesNilTarget(t *testing.T) {
	// A nil target is a programming error at the call site, but the fake must
	// not panic — a panicking fake turns a clear test failure into a stack
	// trace in an unrelated place.
	f := NewFakeCallerPermissionChecker()
	got, err := f.CanActAs(context.Background(), Principal{Kind: PrincipalAgent}, nil)
	require.NoError(t, err)
	assert.False(t, got.Allowed())
	assert.Equal(t, 1, f.CallCount())
}
