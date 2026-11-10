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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setUserTimezone stores tz as the user's profile time zone.
func setUserTimezone(t *testing.T, st store.Store, user *store.User, tz string) {
	t.Helper()
	ctx := context.Background()
	u, err := st.GetUser(ctx, user.ID)
	require.NoError(t, err)
	if u.Preferences == nil {
		u.Preferences = &store.UserPreferences{}
	}
	u.Preferences.Timezone = tz
	require.NoError(t, st.UpdateUser(ctx, u))
}

// spoofedTimezoneBody is a structured message to the agent whose metadata
// carries a client-supplied sender time zone.
func spoofedTimezoneBody(agentSlug string) map[string]interface{} {
	return map[string]interface{}{
		"structured_message": map[string]interface{}{
			"version":   messages.Version,
			"recipient": "agent:" + agentSlug,
			"msg":       "hello",
			"type":      "instruction",
			"metadata":  map[string]string{messages.MetadataSenderTimezone: "Pacific/Fake"},
		},
	}
}

// A human sender's profile time zone is in the delivered message's metadata,
// under both envelope renderers; a client cannot supply the key itself.
func TestSenderTimezone_HumanSenderWithProfileZone(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	setUserTimezone(t, f.st, f.owner, "Europe/Berlin")

	body := spoofedTimezoneBody(f.agent.Slug)
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, f.projectMessagePath(f.agent.Slug), body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	calls := f.dispatcher.getCalls()
	require.Len(t, calls, 1)
	sm := calls[0].StructuredMessage
	require.NotNil(t, sm)
	assert.Equal(t, "Europe/Berlin", sm.Metadata[messages.MetadataSenderTimezone])
	require.NotEmpty(t, sm.DeliveryText, "the envelope switch is on")
	assert.Contains(t, sm.DeliveryText, `"sender_timezone": "Europe/Berlin"`)
	assert.NotContains(t, sm.DeliveryText, "Pacific/Fake")
	legacy := messages.FormatForDelivery(sm)
	assert.Contains(t, legacy, `"sender_timezone": "Europe/Berlin"`)
}

// Without a profile time zone the key is absent, and a client-supplied value
// does not reach the agent.
func TestSenderTimezone_HumanSenderWithoutProfileZone(t *testing.T) {
	f := newVisitorMessagingFixture(t)

	body := spoofedTimezoneBody(f.agent.Slug)
	rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, f.projectMessagePath(f.agent.Slug), body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	calls := f.dispatcher.getCalls()
	require.Len(t, calls, 1)
	sm := calls[0].StructuredMessage
	require.NotNil(t, sm)
	assert.NotContains(t, sm.Metadata, messages.MetadataSenderTimezone)
	assert.NotContains(t, sm.DeliveryText, "sender_timezone")
}

// Only a human user sender gets the key: an agent sender never does, even
// when its ID happens to name a user with a profile time zone, and a value
// already in the metadata is removed.
func TestSenderTimezone_StampRules(t *testing.T) {
	f := newVisitorMessagingFixture(t)
	setUserTimezone(t, f.st, f.owner, "America/New_York")
	ctx := context.Background()
	spoofed := map[string]string{"k": "v", messages.MetadataSenderTimezone: "Pacific/Fake"}

	got := f.srv.stampSenderTimezone(ctx, spoofed, "user:"+f.owner.Email, f.owner.ID)
	assert.Equal(t, map[string]string{"k": "v", messages.MetadataSenderTimezone: "America/New_York"}, got)

	got = f.srv.stampSenderTimezone(ctx, spoofed, "agent:"+f.agent.Slug, f.owner.ID)
	assert.Equal(t, map[string]string{"k": "v"}, got, "an agent sender gets no time zone")

	got = f.srv.stampSenderTimezone(ctx, spoofed, "user:"+f.owner.Email, "")
	assert.Equal(t, map[string]string{"k": "v"}, got, "no sender ID, no time zone")

	got = f.srv.stampSenderTimezone(ctx, spoofed, "user:nobody@example.com", "no-such-user")
	assert.Equal(t, map[string]string{"k": "v"}, got, "an unknown user gets no time zone")

	assert.Equal(t, "Pacific/Fake", spoofed[messages.MetadataSenderTimezone], "the input map is not mutated")

	plain := map[string]string{"k": "v"}
	setUserTimezone(t, f.st, f.owner, "")
	got = f.srv.stampSenderTimezone(ctx, plain, "user:"+f.owner.Email, f.owner.ID)
	assert.Equal(t, plain, got)
}
