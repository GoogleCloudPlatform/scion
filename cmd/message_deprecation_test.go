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

package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetMessageFlags resets all message-related global flags to their defaults
// and returns a restore function.
func resetMessageFlags() func() {
	orig := struct {
		interrupt  bool
		broadcast  bool
		all        bool
		in         string
		at         string
		plain      bool
		raw        bool
		attach     []string
		notify     bool
		wake       bool
		channel    string
		threadID   string
		cc         []string
		visibility string
	}{
		msgInterrupt, msgBroadcast, msgAll, msgIn, msgAt, msgPlain,
		msgRaw, msgAttach, msgNotify, msgWake, msgChannel, msgThreadID,
		msgCC, msgVisibility,
	}
	// Reset all
	msgInterrupt = false
	msgBroadcast = false
	msgAll = false
	msgIn = ""
	msgAt = ""
	msgPlain = false
	msgRaw = false
	msgAttach = nil
	msgNotify = false
	msgWake = false
	msgChannel = ""
	msgThreadID = ""
	msgCC = nil
	msgVisibility = ""

	return func() {
		msgInterrupt = orig.interrupt
		msgBroadcast = orig.broadcast
		msgAll = orig.all
		msgIn = orig.in
		msgAt = orig.at
		msgPlain = orig.plain
		msgRaw = orig.raw
		msgAttach = orig.attach
		msgNotify = orig.notify
		msgWake = orig.wake
		msgChannel = orig.channel
		msgThreadID = orig.threadID
		msgCC = orig.cc
		msgVisibility = orig.visibility
	}
}

// newDeprecationTestServer creates a Hub mock server that accepts all
// standard message, broadcast, and scheduling operations.
func newDeprecationTestServer(t *testing.T, projectID string) (*httptest.Server, *[]sentMessage) {
	t.Helper()
	var sent []sentMessage
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/agents"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"agents": []hubclient.Agent{
					{Name: "test-agent", Status: "running"},
				},
			})

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "channels"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"channels": []hubclient.MessageChannel{
					{Name: "test-channel", Status: "active"},
				},
			})

		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/broadcast"):
			var body struct {
				StructuredMessage *messages.StructuredMessage `json:"structured_message"`
				Interrupt         bool                        `json:"interrupt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			sm := sentMessage{
				StructuredMsg: body.StructuredMessage,
				Interrupt:     body.Interrupt,
			}
			if body.StructuredMessage != nil {
				sm.Message = body.StructuredMessage.Msg
			}
			mu.Lock()
			sent = append(sent, sm)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":   "accepted",
				"targeted": 1,
				"skipped":  0,
			})

		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/scheduled-events"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id":      "evt-123",
				"fire_at": "2030-01-01T00:00:00Z",
			})

		case r.Method == http.MethodPost:
			var body struct {
				Message           string                      `json:"message"`
				StructuredMessage *messages.StructuredMessage `json:"structured_message"`
				Interrupt         bool                        `json:"interrupt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			sm := sentMessage{
				Interrupt:     body.Interrupt,
				StructuredMsg: body.StructuredMessage,
			}
			if body.StructuredMessage != nil {
				sm.Message = body.StructuredMessage.Msg
			} else {
				sm.Message = body.Message
			}
			mu.Lock()
			sent = append(sent, sm)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return server, &sent
}

// TestDeprecatedFlag_Broadcast tests that --broadcast emits a deprecation
// warning on stderr and still succeeds identically.
func TestDeprecatedFlag_Broadcast(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	projectID := "proj-depr-bcast"
	server, sent := newDeprecationTestServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	// Simulate the flag being set via cobra
	msgBroadcast = true
	require.NoError(t, messageCmd.Flags().Set("broadcast", "true"))

	stderr := captureStderr(t, func() {
		err = sendMessageViaHub(hubCtx, "", "broadcast test", false, true, false, false, false)
	})

	// Verify the warning was emitted
	// (emitDeprecationWarnings is called in RunE, but sendMessageViaHub doesn't call it;
	// we test it separately via the command execution path below)
	_ = stderr

	// Verify the command still succeeded
	require.NoError(t, err)
	require.Len(t, *sent, 1)
	assert.Equal(t, "broadcast test", (*sent)[0].Message)

	// Now test through the RunE to verify deprecation warnings
	*sent = nil
	stderr = captureStderr(t, func() {
		emitDeprecationWarnings(messageCmd)
	})
	assert.Contains(t, stderr, "Warning: --broadcast is deprecated")
	assert.Contains(t, stderr, "scion broadcast")
}

// TestDeprecatedFlag_All tests that --all emits a deprecation warning
// and still succeeds.
func TestDeprecatedFlag_All(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	msgAll = true
	require.NoError(t, messageCmd.Flags().Set("all", "true"))

	stderr := captureStderr(t, func() {
		emitDeprecationWarnings(messageCmd)
	})
	assert.Contains(t, stderr, "Warning: --all is deprecated")
	assert.Contains(t, stderr, "scion broadcast --all")
}

// TestDeprecatedFlag_Raw tests that --raw emits a deprecation warning
// and still succeeds.
func TestDeprecatedFlag_Raw(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	msgRaw = true
	require.NoError(t, messageCmd.Flags().Set("raw", "true"))

	stderr := captureStderr(t, func() {
		emitDeprecationWarnings(messageCmd)
	})
	assert.Contains(t, stderr, "Warning: --raw is deprecated")
	assert.Contains(t, stderr, "scion keys")
}

// TestDeprecatedFlag_Plain tests that --plain emits a deprecation warning
// and still succeeds.
func TestDeprecatedFlag_Plain(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	msgPlain = true
	require.NoError(t, messageCmd.Flags().Set("plain", "true"))

	stderr := captureStderr(t, func() {
		emitDeprecationWarnings(messageCmd)
	})
	assert.Contains(t, stderr, "Warning: --plain is deprecated")
	assert.Contains(t, stderr, "will be removed")
}

// TestDeprecatedFlag_Notify tests that --notify emits a deprecation warning
// and still succeeds.
func TestDeprecatedFlag_Notify(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	msgNotify = true
	require.NoError(t, messageCmd.Flags().Set("notify", "true"))

	stderr := captureStderr(t, func() {
		emitDeprecationWarnings(messageCmd)
	})
	assert.Contains(t, stderr, "Warning: --notify is deprecated")
	assert.Contains(t, stderr, "scion notifications subscribe")
}

// TestDeprecatedFlag_In tests that --in emits a deprecation warning
// and still succeeds.
func TestDeprecatedFlag_In(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	projectID := "proj-depr-in"
	server, _ := newDeprecationTestServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	msgIn = "30m"
	require.NoError(t, messageCmd.Flags().Set("in", "30m"))

	// Verify the warning
	stderr := captureStderr(t, func() {
		emitDeprecationWarnings(messageCmd)
	})
	assert.Contains(t, stderr, "Warning: --in is deprecated")
	assert.Contains(t, stderr, "scion schedule create")

	// Verify the command still works (schedule via Hub)
	err = scheduleMessageViaHub(hubCtx, "test-agent", "scheduled msg", false, false)
	require.NoError(t, err)
}

// TestDeprecatedFlag_At tests that --at emits a deprecation warning
// and still succeeds.
func TestDeprecatedFlag_At(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	projectID := "proj-depr-at"
	server, _ := newDeprecationTestServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	msgAt = "2030-01-01T00:00:00Z"
	require.NoError(t, messageCmd.Flags().Set("at", "2030-01-01T00:00:00Z"))

	// Verify the warning
	stderr := captureStderr(t, func() {
		emitDeprecationWarnings(messageCmd)
	})
	assert.Contains(t, stderr, "Warning: --at is deprecated")
	assert.Contains(t, stderr, "scion schedule create")

	// Verify the command still works
	err = scheduleMessageViaHub(hubCtx, "test-agent", "scheduled msg", false, false)
	require.NoError(t, err)
}

// TestDeprecatedFlag_Channel tests that --channel emits a deprecation warning
// and still succeeds.
func TestDeprecatedFlag_Channel(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	msgChannel = "test-channel"
	require.NoError(t, messageCmd.Flags().Set("channel", "test-channel"))

	stderr := captureStderr(t, func() {
		emitDeprecationWarnings(messageCmd)
	})
	assert.Contains(t, stderr, "Warning: --channel is deprecated")
	assert.Contains(t, stderr, "@<agent-name>")
}

// TestDeprecatedFlag_ThreadID tests that --thread-id emits a deprecation
// warning and still succeeds.
func TestDeprecatedFlag_ThreadID(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	msgThreadID = "thread-123"
	require.NoError(t, messageCmd.Flags().Set("thread-id", "thread-123"))

	stderr := captureStderr(t, func() {
		emitDeprecationWarnings(messageCmd)
	})
	assert.Contains(t, stderr, "Warning: --thread-id is deprecated")
	assert.Contains(t, stderr, "@<agent-name>")
}

// TestDeprecatedFlag_CC tests that --cc emits a deprecation warning
// and still succeeds.
func TestDeprecatedFlag_CC(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	msgCC = []string{"agent-a"}
	require.NoError(t, messageCmd.Flags().Set("cc", "agent-a"))

	stderr := captureStderr(t, func() {
		emitDeprecationWarnings(messageCmd)
	})
	assert.Contains(t, stderr, "Warning: --cc is deprecated")
	assert.Contains(t, stderr, "deprecated and will be removed")
}

// TestDeprecatedFlag_BroadcastStillSucceeds verifies that using the
// deprecated --broadcast flag on `scion message` still delivers the
// message successfully (AC-15 requirement: warn AND succeed).
func TestDeprecatedFlag_BroadcastStillSucceeds(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	projectID := "proj-depr-bcast-works"
	server, sent := newDeprecationTestServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	msgBroadcast = true

	err = sendMessageViaHub(hubCtx, "", "broadcast still works", false, true, false, false, false)
	require.NoError(t, err, "deprecated --broadcast must still succeed")
	require.True(t, len(*sent) > 0, "message must be delivered")
	assert.Equal(t, "broadcast still works", (*sent)[0].Message)
}

// TestDeprecatedFlag_NotifyStillSucceeds verifies that using the
// deprecated --notify flag still delivers the message with notify=true.
func TestDeprecatedFlag_NotifyStillSucceeds(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	projectID := "proj-depr-notify-works"

	var notifyReceived bool
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.Method == http.MethodPost:
			var body struct {
				Notify bool `json:"notify"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			notifyReceived = body.Notify
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	msgNotify = true

	err = sendMessageViaHub(hubCtx, "test-agent", "hello", false, false, false, true, false)
	require.NoError(t, err, "deprecated --notify must still succeed")

	mu.Lock()
	assert.True(t, notifyReceived, "notify should be passed through")
	mu.Unlock()
}

// TestDeprecatedFlag_PlainStillSucceeds verifies that the --plain flag
// still sets the Plain field on the structured message.
func TestDeprecatedFlag_PlainStillSucceeds(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	projectID := "proj-depr-plain-works"
	server, sent := newDeprecationTestServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	msgPlain = true

	err = sendMessageViaHub(hubCtx, "test-agent", "plain message", false, false, false, false, false)
	require.NoError(t, err, "deprecated --plain must still succeed")

	require.Len(t, *sent, 1)
	require.NotNil(t, (*sent)[0].StructuredMsg)
	assert.True(t, (*sent)[0].StructuredMsg.Plain, "Plain flag must be set in structured message")
}

// TestDeprecatedFlag_ChannelStillSucceeds verifies that the --channel flag
// is still passed through to the structured message.
func TestDeprecatedFlag_ChannelStillSucceeds(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	projectID := "proj-depr-channel-works"
	server, sent := newDeprecationTestServer(t, projectID)
	defer server.Close()

	client, err := hubclient.New(server.URL)
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  server.URL,
		ProjectID: projectID,
	}

	msgChannel = "test-channel"

	err = sendMessageViaHub(hubCtx, "test-agent", "channeled message", false, false, false, false, false)
	require.NoError(t, err, "deprecated --channel must still succeed")

	require.Len(t, *sent, 1)
	require.NotNil(t, (*sent)[0].StructuredMsg)
	assert.Equal(t, "test-channel", (*sent)[0].StructuredMsg.Channel, "Channel must be set in structured message")
}

// TestDeprecatedFlags_NoWarningForRetainedFlags verifies that retained
// flags (--interrupt, --wake, --attach, --visibility) do NOT emit
// deprecation warnings.
func TestDeprecatedFlags_NoWarningForRetainedFlags(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	// Reset the Changed state for all deprecated flags that may have been
	// set by earlier tests in this package (cobra flags are process-level
	// singletons and their Changed bit persists across tests).
	deprecatedNames := []string{
		"broadcast", "all", "raw", "plain", "notify",
		"in", "at", "channel", "thread-id", "cc",
	}
	for _, name := range deprecatedNames {
		f := messageCmd.Flags().Lookup(name)
		if f != nil {
			f.Changed = false
		}
	}

	// Now set only retained flags
	msgInterrupt = true
	require.NoError(t, messageCmd.Flags().Set("interrupt", "true"))

	stderr := captureStderr(t, func() {
		emitDeprecationWarnings(messageCmd)
	})
	assert.Empty(t, stderr, "retained flags should not produce deprecation warnings")

	// Reset interrupt's Changed state for other tests
	f := messageCmd.Flags().Lookup("interrupt")
	if f != nil {
		f.Changed = false
	}
}

// TestDeprecatedFlags_MultipleWarnings verifies that using multiple deprecated
// flags produces multiple warnings.
func TestDeprecatedFlags_MultipleWarnings(t *testing.T) {
	orig := saveMessageTestState()
	defer orig.restore()
	restore := resetMessageFlags()
	defer restore()

	require.NoError(t, messageCmd.Flags().Set("broadcast", "true"))
	require.NoError(t, messageCmd.Flags().Set("plain", "true"))
	msgBroadcast = true
	msgPlain = true

	stderr := captureStderr(t, func() {
		emitDeprecationWarnings(messageCmd)
	})
	assert.Contains(t, stderr, "Warning: --broadcast is deprecated")
	assert.Contains(t, stderr, "Warning: --plain is deprecated")
}

// TestDeprecatedFlags_Hidden verifies that deprecated flags are hidden
// from help output.
func TestDeprecatedFlags_Hidden(t *testing.T) {
	deprecatedFlags := []string{
		"broadcast", "all", "in", "at", "plain", "raw",
		"notify", "channel", "thread-id", "cc",
	}
	for _, name := range deprecatedFlags {
		f := messageCmd.Flags().Lookup(name)
		require.NotNil(t, f, "flag --%s should exist", name)
		assert.True(t, f.Hidden, "flag --%s should be hidden", name)
	}
}

// TestRetainedFlags_NotHidden verifies that retained flags are NOT hidden.
func TestRetainedFlags_NotHidden(t *testing.T) {
	retainedFlags := []string{
		"interrupt", "wake", "attach", "visibility",
	}
	for _, name := range retainedFlags {
		f := messageCmd.Flags().Lookup(name)
		require.NotNil(t, f, "flag --%s should exist", name)
		assert.False(t, f.Hidden, "flag --%s should NOT be hidden", name)
	}
}

// TestBroadcastCmd_IsRegistered verifies the broadcast command is available
// on the root command.
func TestBroadcastCmd_IsRegistered(t *testing.T) {
	found := false
	for _, cmd := range rootCmd.Commands() {
		if cmd.Name() == "broadcast" {
			found = true
			break
		}
	}
	assert.True(t, found, "broadcast command should be registered on rootCmd")
}

// findReplacementProblems scans deprecation warning output for replacement
// command references ("scion <subcommand>") and validates each resolves
// against rootCmd. Returns problems found and the count of replacements examined.
func findReplacementProblems(stderr string) (problems []string, checked int) {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Quote-agnostic: find the token "scion " anywhere in the line,
		// then take following words up to the first flag, quote, comma, or end.
		idx := strings.Index(line, "scion ")
		if idx < 0 {
			continue
		}
		// Extract from "scion" onward
		rest := line[idx:]
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			continue
		}
		// Collect subcommand tokens (skip "scion" itself), stop at flags, quotes, commas
		var subArgs []string
		for _, f := range fields[1:] {
			// Stop at flags
			if strings.HasPrefix(f, "--") || strings.HasPrefix(f, "-") {
				break
			}
			// Stop at tokens that are just punctuation/quotes
			cleaned := strings.Trim(f, "\"'`,;)")
			if cleaned == "" {
				break
			}
			// Strip trailing punctuation from the token itself
			cleaned2 := strings.TrimRight(f, "\"'`,;)")
			if cleaned2 == "" {
				break
			}
			subArgs = append(subArgs, cleaned2)
			// If the original token had trailing punctuation (closing quote,
			// comma, etc.), we've reached the end of the command reference.
			if cleaned2 != f {
				break
			}
		}
		if len(subArgs) == 0 {
			continue
		}
		checked++
		cmd, _, err := rootCmd.Find(subArgs)
		if err != nil {
			problems = append(problems,
				fmt.Sprintf("replacement command not found: scion %s (from: %s)",
					strings.Join(subArgs, " "), line))
			continue
		}
		// Verify the command was fully consumed
		if cmd.Name() != subArgs[len(subArgs)-1] {
			problems = append(problems,
				fmt.Sprintf("replacement resolves to wrong command: wanted %s, got %s (from: %s)",
					subArgs[len(subArgs)-1], cmd.Name(), line))
		}
	}
	return problems, checked
}

// TestDeprecationWarnings_ReplacementsExist validates AC-15a: every
// deprecation warning must name a replacement that exists in the binary.
// It triggers all deprecated flags, captures the warnings, extracts any
// 'scion <subcommand>' references, and verifies each resolves via
// rootCmd.Find().
func TestDeprecationWarnings_ReplacementsExist(t *testing.T) {
	restore := resetMessageFlags()
	defer restore()

	deprecatedFlags := []string{"broadcast", "all", "raw", "plain", "notify", "in", "at", "channel", "thread-id", "cc"}
	for _, name := range deprecatedFlags {
		f := messageCmd.Flags().Lookup(name)
		require.NotNil(t, f, "deprecated flag --%s must be registered", name)
		f.Changed = true
	}
	defer func() {
		for _, name := range deprecatedFlags {
			f := messageCmd.Flags().Lookup(name)
			if f != nil {
				f.Changed = false
			}
		}
	}()

	stderr := captureStderr(t, func() {
		emitDeprecationWarnings(messageCmd)
	})

	problems, checked := findReplacementProblems(stderr)
	for _, p := range problems {
		t.Error(p)
	}
	// Six of the ten warnings name a 'scion ...' command; assert a floor.
	// Raise this floor when adding replacement references; never lower it.
	require.GreaterOrEqual(t, checked, 6,
		"expected at least 6 replacement references in deprecation warnings; got %d — "+
			"the extractor may be broken or warnings were removed", checked)

	// Rule 10: prove findReplacementProblems catches bad replacements.
	// These call the same function used by the main body (Rule 13).
	t.Run("catches_deepest_match_blind_spot", func(t *testing.T) {
		problems, _ := findReplacementProblems(
			"Warning: --x is deprecated, use 'scion schedule message' instead")
		assert.NotEmpty(t, problems, "should catch nonexistent subcommand via deepest-match blind spot")
	})
	t.Run("catches_backtick_quoted_unknown", func(t *testing.T) {
		problems, _ := findReplacementProblems(
			"Warning: --x is deprecated, use `scion agent poke` instead")
		assert.NotEmpty(t, problems, "should catch unknown command in backtick-quoted reference")
	})
	t.Run("catches_unquoted_unknown", func(t *testing.T) {
		problems, _ := findReplacementProblems(
			"Warning: --x is deprecated, use scion nonexistent-thing instead")
		assert.NotEmpty(t, problems, "should catch unknown command in unquoted reference")
	})
	t.Run("accepts_valid_replacement", func(t *testing.T) {
		problems, checked := findReplacementProblems(
			"Warning: --x is deprecated, use 'scion broadcast' instead")
		assert.Empty(t, problems, "should accept valid replacement command")
		assert.Equal(t, 1, checked, "should have checked exactly one reference")
	})
}
