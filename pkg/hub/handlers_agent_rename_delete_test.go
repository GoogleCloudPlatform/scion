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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// TestApplyAgentUpdate_RenameThenDelete_DoesNotCancelOtherAgentsScheduledEvents
// verifies the delete-time cleanup contract: cancelScheduledEventsForAgent
// (run on agent DELETE) selects the pending scheduled events belonging to the
// agent being deleted via eventTargetsAgent, which keys on ID/Slug only. Name
// is a mutable display field, not an identifier, so an event targeting one
// agent's Slug stays pending when a different agent is deleted, even if that
// agent's Name equals the first agent's Slug.
func TestApplyAgentUpdate_RenameThenDelete_DoesNotCancelOtherAgentsScheduledEvents(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()

	// A pending scheduled event targeting agent B (f.caller) by slug, the
	// same way the scheduler's own dispatch resolution (server.go's
	// GetAgentBySlug call) treats the payload's AgentName field.
	payload, err := json.Marshal(map[string]string{"agentName": f.caller.Slug})
	require.NoError(t, err)
	evt := &store.ScheduledEvent{
		ID:        tid("rename-delete-evt"),
		ProjectID: f.project.ID,
		EventType: "message",
		FireAt:    time.Now().Add(time.Hour),
		Payload:   string(payload),
		Status:    store.ScheduledEventPending,
	}
	require.NoError(t, f.store.CreateScheduledEvent(ctx, evt))

	// Set A's display Name to a value equal to B's Slug (allowed: Name is free-form).
	rec := doRequestAsUser(t, f.srv, f.member, http.MethodPatch, f.targetPath(),
		map[string]interface{}{"name": f.caller.Slug})
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body: %s", rec.Body.String())

	renamed, err := f.store.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)
	require.Equal(t, f.caller.Slug, renamed.Name, "rename must have taken effect")
	require.NotEqual(t, f.caller.Slug, renamed.Slug, "Slug must still be A's own, unchanged")

	// Delete agent A.
	delRec := doRequestAsUser(t, f.srv, f.member, http.MethodDelete, f.targetPath(), nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, delRec.Code,
		"DELETE body: %s", delRec.Body.String())

	// B's scheduled event targets B by slug, not A. It must still be pending
	// after A is deleted (delete cleanup keys on ID/Slug, not Name).
	after, err := f.store.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	require.Equal(t, store.ScheduledEventPending, after.Status,
		"B's scheduled event must still be pending after A is deleted; delete cleanup keys on ID/Slug, not the display Name")
}
