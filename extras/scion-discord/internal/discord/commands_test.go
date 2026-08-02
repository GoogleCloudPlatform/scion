package discord

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// ---------------------------------------------------------------------------
// agentStatusEmoji
// ---------------------------------------------------------------------------

func TestAgentStatusEmoji_ActivityIcons(t *testing.T) {
	tests := []struct {
		activity string
		want     string
	}{
		{"working", "⚙️"},
		{"thinking", "\U0001f4ad"},
		{"executing", "⚙️"},
		{"waiting_for_input", "\U0001f514"},
		{"blocked", "\U0001f6a7"},
		{"completed", "✅"},
		{"limits_exceeded", "\U0001f6ab"},
		{"stalled", "⏳"},
		{"offline", "\U0001f4e1"},
		{"crashed", "\U0001f4a5"},
	}
	for _, tt := range tests {
		t.Run(tt.activity, func(t *testing.T) {
			got := agentStatusEmoji(tt.activity, "")
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAgentStatusEmoji_PhaseIcons(t *testing.T) {
	tests := []struct {
		phase string
		want  string
	}{
		{"created", "\U0001f4e6"},
		{"provisioning", "\U0001f504"},
		{"cloning", "\U0001f4e5"},
		{"starting", "\U0001f680"},
		{"running", "▶️"},
		{"suspended", "⏸️"},
		{"stopping", "\U0001f6d1"},
		{"stopped", "⏹️"},
		{"error", "❌"},
	}
	for _, tt := range tests {
		t.Run(tt.phase, func(t *testing.T) {
			got := agentStatusEmoji("", tt.phase)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAgentStatusEmoji_ActivityPriorityOverPhase(t *testing.T) {
	// When both activity and phase are set, activity takes priority.
	got := agentStatusEmoji("crashed", "stopped")
	assert.Equal(t, "\U0001f4a5", got, "crashed activity should take priority over stopped phase")

	got = agentStatusEmoji("limits_exceeded", "stopped")
	assert.Equal(t, "\U0001f6ab", got, "limits_exceeded activity should take priority over stopped phase")

	got = agentStatusEmoji("completed", "running")
	assert.Equal(t, "✅", got, "completed activity should take priority over running phase")
}

func TestAgentStatusEmoji_MixedCaseInput(t *testing.T) {
	// Activity matching should be case-insensitive.
	got := agentStatusEmoji("Working", "")
	assert.Equal(t, "⚙️", got, "mixed-case activity 'Working' should return gear icon")

	got = agentStatusEmoji("BLOCKED", "")
	assert.Equal(t, "\U0001f6a7", got, "upper-case activity 'BLOCKED' should return construction icon")

	// Phase matching should be case-insensitive.
	got = agentStatusEmoji("", "Running")
	assert.Equal(t, "▶️", got, "mixed-case phase 'Running' should return play icon")

	got = agentStatusEmoji("", "STOPPED")
	assert.Equal(t, "⏹️", got, "upper-case phase 'STOPPED' should return stop button icon")
}

func TestAgentStatusEmoji_EmptyBothDefaultsToPlay(t *testing.T) {
	got := agentStatusEmoji("", "")
	assert.Equal(t, "▶️", got, "empty activity and phase should return default play icon")
}

func TestAgentStatusEmoji_UnknownActivityFallsToPhase(t *testing.T) {
	// Unknown activity should fall through to phase icon.
	got := agentStatusEmoji("unknown_activity", "stopped")
	assert.Equal(t, "⏹️", got, "unknown activity should fall through to phase icon")

	got = agentStatusEmoji("unknown_activity", "error")
	assert.Equal(t, "❌", got, "unknown activity should fall through to error phase icon")
}
