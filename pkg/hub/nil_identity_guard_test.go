package hub

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestScopedCursorBinding_NilPointerIdentityBindsAsNoIdentity checks that a
// non-nil Identity interface holding a nil pointer binds exactly like a nil
// identity, for the scoped UAT, agent, and default cases, without calling a
// method on the nil receiver.
func TestScopedCursorBinding_NilPointerIdentityBindsAsNoIdentity(t *testing.T) {
	filter := map[string]string{"scope": "user"}
	want := scopedCursorBinding("templates", filter, nil)

	cases := map[string]Identity{
		"scoped user":        (*ScopedUserIdentity)(nil),
		"agent":              (*agentIdentityWrapper)(nil),
		"authenticated user": (*AuthenticatedUser)(nil),
	}
	for name, identity := range cases {
		t.Run(name, func(t *testing.T) {
			var got string
			require.NotPanics(t, func() {
				got = scopedCursorBinding("templates", filter, identity)
			})
			assert.Equal(t, want, got)
		})
	}
}

// TestUATMessageGate_NilScopedIdentityDenies checks that the agent.message
// bearer gate denies a nil scoped identity at entry.
func TestUATMessageGate_NilScopedIdentityDenies(t *testing.T) {
	authz := &AuthzService{}
	var decision *Decision
	require.NotPanics(t, func() {
		decision = authz.uatMessageGate(context.Background(), (*ScopedUserIdentity)(nil), Resource{Type: "agent", ID: "agent-1", ParentType: "project", ParentID: "project-1"})
	})
	require.NotNil(t, decision)
	assert.False(t, decision.Allowed)
	assert.Equal(t, bearerReasonProjectAccessDenied, decision.Reason)
}
