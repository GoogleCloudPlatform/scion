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
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestCallbackHandler(t *testing.T) (*CallbackHandler, *fakeTGServerV2, *fakeHubClient, Store) {
	t.Helper()
	tgSrv := newFakeTGServerV2(t)
	hub := newFakeHubClient()
	store := newTestStore(t)
	api := NewAPIClient("test-token", tgSrv.srv.URL)
	return NewCallbackHandler(store, api, hub, slog.Default()), tgSrv, hub, store
}

func setupCallback(data string, fromID int64) *CallbackQuery {
	return &CallbackQuery{
		ID:      "cb-1",
		From:    &TGUser{ID: fromID},
		Message: &TGMessage{MessageID: 7, Chat: TGChat{ID: -100, Type: "group"}},
		Data:    data,
	}
}

func TestCallbackHandler_SetupProject_ListsAgentsAsLinkedUser(t *testing.T) {
	h, _, hub, store := newTestCallbackHandler(t)
	hub.agents["p1"] = []AgentInfo{{Slug: "coder"}}
	principal := linkTestUser(t, store, 42, "alice@example.com")

	_, err := h.HandleCallback(context.Background(), setupCallback("setup:proj:p1", 42))
	require.NoError(t, err)

	calls := hub.agentCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, fakeListAgentsCall{ProjectID: "p1", OnBehalfOf: principal}, calls[0])
}

func TestCallbackHandler_SetupProject_UnlinkedUserGetsRegisterHint(t *testing.T) {
	h, tgSrv, hub, _ := newTestCallbackHandler(t)
	hub.agents["p1"] = []AgentInfo{{Slug: "coder"}}

	_, err := h.HandleCallback(context.Background(), setupCallback("setup:proj:p1", 42))
	require.NoError(t, err)

	assert.Empty(t, hub.agentCalls(), "no hub read without a linked user")
	answered := tgSrv.getAnsweredCallbacks()
	require.Len(t, answered, 1)
	assert.Contains(t, answered[0].Text, "/register")
}
