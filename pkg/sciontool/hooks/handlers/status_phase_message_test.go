//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStatusHandlerUpdatePhaseAndMessage checks the merged phase+message
// write used by sciontool init's failure paths.
func TestStatusHandlerUpdatePhaseAndMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-info.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"template":"tpl","activity":"working","detail":{"other":1}}`), 0o644))
	h := &StatusHandler{StatusPath: path}

	require.NoError(t, h.UpdatePhaseAndMessage(state.PhaseStopped, state.ActivityLimitsExceeded, "limits exceeded"))
	info := readInfo(t, path)
	assert.Equal(t, string(state.PhaseStopped), info["phase"])
	assert.Equal(t, string(state.ActivityLimitsExceeded), info["activity"])
	assert.Equal(t, map[string]any{"other": float64(1), "message": "limits exceeded"}, info["detail"])

	require.NoError(t, h.UpdatePhaseAndMessage(state.PhaseError, "", ""))
	info = readInfo(t, path)
	assert.Equal(t, string(state.PhaseError), info["phase"])
	assert.NotContains(t, info, "activity")
	assert.Equal(t, map[string]any{"other": float64(1)}, info["detail"])
}
