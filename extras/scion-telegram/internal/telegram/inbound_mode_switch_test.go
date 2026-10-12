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

package telegram

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for switching the inbound mode (poll / webhook) at runtime.

// pollState returns whether a poll loop is running and the channel that is
// closed when the most recent poll loop has exited.
func pollState(b *TelegramBrokerV2) (running bool, done <-chan struct{}) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.pollCancel != nil, b.pollDone
}

// getUpdatesCount returns how many getUpdates requests the fake received.
func (f *fakeTGServerV2) getUpdatesCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getUpdatesCalls
}

// waitPolling waits until the broker has polled the fake at least once.
func waitPolling(t *testing.T, tgSrv *fakeTGServerV2) {
	t.Helper()
	require.Eventually(t, func() bool { return tgSrv.getUpdatesCount() > 0 },
		5*time.Second, 10*time.Millisecond, "the broker must poll for updates")
}

// assertClosed checks that done is closed now, without waiting.
func assertClosed(t *testing.T, done <-chan struct{}, msg string) {
	t.Helper()
	require.NotNil(t, done, "a poll loop must have been started")
	select {
	case <-done:
	default:
		assert.Fail(t, msg)
	}
}

// Switching from webhook mode to poll mode removes the webhook registration
// and starts polling for the subscriptions made in webhook mode.
func TestV2_Reconfigure_WebhookToPollDeletesWebhookAndPolls(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.Default())
	defer b.Close()

	require.NoError(t, b.Configure(validWebhookConfig(t, tgSrv)))
	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))
	running, _ := pollState(b)
	require.False(t, running, "webhook mode must not poll")

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))

	tgSrv.mu.Lock()
	deleteCalls, registeredURL := tgSrv.deleteWebhookCalls, tgSrv.webhookURL
	tgSrv.mu.Unlock()
	assert.Equal(t, 1, deleteCalls, "leaving webhook mode must call deleteWebhook")
	assert.Empty(t, registeredURL, "the webhook must no longer be registered")

	running, _ = pollState(b)
	assert.True(t, running, "poll mode must poll for the existing subscriptions")
	waitPolling(t, tgSrv)
}

// Switching from poll mode to webhook mode stops the poll loop; it has
// exited by the time Configure returns.
func TestV2_Reconfigure_PollToWebhookStopsPollLoop(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.Default())
	defer b.Close()

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))
	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))
	waitPolling(t, tgSrv)
	_, done := pollState(b)

	require.NoError(t, b.Configure(validWebhookConfig(t, tgSrv)))

	assertClosed(t, done, "the poll loop must have exited when the switch to webhook mode returns")
	running, _ := pollState(b)
	assert.False(t, running, "webhook mode must not keep a poll loop")
}

// Close stops the poll loop; it has exited by the time Close returns.
func TestV2_Close_StopsPollLoop(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.Default())

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))
	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))
	waitPolling(t, tgSrv)
	_, done := pollState(b)

	require.NoError(t, b.Close())

	assertClosed(t, done, "the poll loop must have exited when Close returns")
	running, _ := pollState(b)
	assert.False(t, running, "no poll loop may be left after Close")
}
