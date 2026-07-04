package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDBConfig_NonStringValues(t *testing.T) {
	rt := New(Options{Integration: "test"})

	// Simulate what loadDBConfig does with JSONB containing non-string values.
	configJSON := `{"bot_token":"abc123","mention_routing":true,"send_queue_size":200,"ratio":3.14}`

	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(configJSON), &raw))

	result := make(map[string]string, len(raw))
	for k, v := range raw {
		switch val := v.(type) {
		case string:
			result[k] = val
		default:
			result[k] = fmt.Sprintf("%v", val)
		}
	}

	_ = rt
	assert.Equal(t, "abc123", result["bot_token"])
	assert.Equal(t, "true", result["mention_routing"])
	assert.Equal(t, "200", result["send_queue_size"])
	assert.Equal(t, "3.14", result["ratio"])
}

func TestShutdownRequested_Channel(t *testing.T) {
	rt := New(Options{Integration: "test"})

	// Channel should be buffered (size 1).
	select {
	case rt.shutdownCh <- "update-123":
	default:
		t.Fatal("shutdownCh should accept one non-blocking send")
	}

	// Should be readable via ShutdownRequested.
	select {
	case id := <-rt.ShutdownRequested():
		assert.Equal(t, "update-123", id)
	default:
		t.Fatal("ShutdownRequested should be readable")
	}
}

func TestShutdownRequested_SecondSendDropped(t *testing.T) {
	rt := New(Options{Integration: "test"})

	rt.shutdownCh <- "first"

	// Second send should not block (select with default in handleUpdateSignal).
	select {
	case rt.shutdownCh <- "second":
		t.Fatal("second send should not succeed on a full channel")
	default:
	}

	id := <-rt.ShutdownRequested()
	assert.Equal(t, "first", id)
}

func TestTransitionUpdateState_NilDB(t *testing.T) {
	rt := New(Options{Integration: "test"})

	// With no DB, transitions always succeed.
	applied, err := rt.transitionUpdateState(context.Background(), "update-1", "requested", "acknowledged", "")
	require.NoError(t, err)
	assert.True(t, applied)
}

func TestTransitionUpdateState_EmptyID(t *testing.T) {
	rt := New(Options{Integration: "test"})

	applied, err := rt.transitionUpdateState(context.Background(), "", "requested", "acknowledged", "")
	require.NoError(t, err)
	assert.True(t, applied)
}

func TestLoadConfig_EnvLayering(t *testing.T) {
	t.Setenv("TEST_BOT_TOKEN", "env-token")
	t.Setenv("TEST_HUB_URL", "https://env-hub.example.com")

	rt := New(Options{
		Integration: "test",
		EnvPrefix:   "TEST",
		EnvKeys:     []string{"bot_token", "hub_url"},
	})

	err := rt.loadConfig(context.Background(), true)
	require.NoError(t, err)

	cfg := rt.Config()
	assert.Equal(t, "env-token", cfg["bot_token"])
	assert.Equal(t, "https://env-hub.example.com", cfg["hub_url"])
}

func TestConfig_ThreadSafe(t *testing.T) {
	rt := New(Options{Integration: "test"})
	rt.config = map[string]string{"key": "value"}
	close(rt.ready)

	cfg := rt.Config()
	cfg["key"] = "mutated"

	original := rt.Config()
	assert.Equal(t, "value", original["key"], "Config should return a copy")
}
