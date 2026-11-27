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
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func p7aProbeTrigger(t *testing.T, f *triggersTestFixture, auth Identity, name string, notify *TriggerNotifySpec) *store.Trigger {
	t.Helper()
	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name: name + "-ep", SignatureMode: store.WebhookSignatureModeScionV1,
	})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))
	trRec := f.doTrigger(t, auth, "", http.MethodPost, CreateTriggerRequest{
		Name:   name,
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: store.TriggerActionMessage, AgentID: f.targetAgent.ID, Message: "hi"},
		Notify: notify,
	})
	require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
	var resp TriggerResponse
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &resp))
	tr, err := f.store.GetTrigger(context.Background(), resp.ID)
	require.NoError(t, err)
	return tr
}

func p7aFire(f *triggersTestFixture, tr *store.Trigger) *store.TriggerFire {
	return &store.TriggerFire{
		ID: api.NewUUID(), ProjectID: f.projectID, TriggerID: tr.ID, EventID: api.NewUUID(),
		ActionType: store.TriggerActionMessage, ActionIndex: 0,
	}
}

// Two hub replicas (two Servers over one store) each finish a different failed
// fire of the same trigger within the cooldown. Acceptance 12 / §7.4: exactly
// one user notice within the cooldown.
func TestP7aProbe_CooldownAcrossReplicas_DefectRepro(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()
	fixedNow := time.Date(2026, 11, 27, 15, 10, 0, 0, time.UTC)
	f.srv.setFireNotifyClockForTest(func() time.Time { return fixedNow })

	srvB, _ := testServerOnMigratedStore(t, f.store)
	enableTriggersExperimentForTest(t, srvB, true)
	srvB.setFireNotifyClockForTest(func() time.Time { return fixedNow.Add(5 * time.Minute) })

	tr := p7aProbeTrigger(t, f, f.ownerAuth, "p7a-replicas", nil)
	out := hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "message failed"}
	f.srv.notifyTriggerFire(ctx, tr, p7aFire(f, tr), out)
	srvB.notifyTriggerFire(ctx, tr, p7aFire(f, tr), out)

	notifs, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.owner.ID, false)
	require.NoError(t, err)
	t.Logf("user notices after 2 failures on 2 replicas within cooldown: %d", len(notifs))
	assert.Len(t, notifs, 1, "exactly one user notice per trigger within the cooldown, across replicas")
}

// Same for a restart: a fresh Server must still honor the durable cooldown
// within the hour bucket.
func TestP7aProbe_CooldownAcrossRestart_DefectRepro(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()
	fixedNow := time.Date(2026, 11, 27, 15, 10, 0, 0, time.UTC)
	f.srv.setFireNotifyClockForTest(func() time.Time { return fixedNow })

	tr := p7aProbeTrigger(t, f, f.ownerAuth, "p7a-restart", nil)
	out := hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "message failed"}
	f.srv.notifyTriggerFire(ctx, tr, p7aFire(f, tr), out)
	restarted, _ := testServerOnMigratedStore(t, f.store)
	enableTriggersExperimentForTest(t, restarted, true)
	restarted.setFireNotifyClockForTest(func() time.Time { return fixedNow.Add(5 * time.Minute) })
	restarted.notifyTriggerFire(ctx, tr, p7aFire(f, tr), out)

	notifs, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.owner.ID, false)
	require.NoError(t, err)
	t.Logf("user notices after restart within cooldown: %d", len(notifs))
	assert.Len(t, notifs, 1)
}

// Design §10: cooldown is per (definition, sink, outcome, reason). A
// project-cap skip notice must not suppress a later, different refused outcome
// within the hour, while a repeated refused with the same reason is suppressed.
func TestP7aProbe_CooldownKeyPerOutcomeReason_DefectRepro(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()
	fixedNow := time.Date(2026, 11, 27, 15, 10, 0, 0, time.UTC)
	f.srv.setFireNotifyClockForTest(func() time.Time { return fixedNow })

	tr := p7aProbeTrigger(t, f, f.ownerAuth, "p7a-key", nil)
	f.srv.notifyTriggerFire(ctx, tr, p7aFire(f, tr), hubfire.FireOutcome{Status: hubfire.FireStatusSkipped, Error: trigger.ErrFireProjectTriggerAgentCap})
	f.srv.notifyTriggerFire(ctx, tr, p7aFire(f, tr), hubfire.FireOutcome{Status: hubfire.FireStatusRefused, Error: "message refused"})
	notifs, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.owner.ID, false)
	require.NoError(t, err)
	t.Logf("user notices after cap-skip then refused within 1h: %d", len(notifs))
	assert.Len(t, notifs, 2, "different (outcome, reason) must not share a cooldown per §10")

	// Second refused with the same reason within the same hour bucket -> suppressed (stays 2).
	f.srv.notifyTriggerFire(ctx, tr, p7aFire(f, tr), hubfire.FireOutcome{Status: hubfire.FireStatusRefused, Error: "message refused"})
	notifsAfterDup, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.owner.ID, false)
	require.NoError(t, err)
	t.Logf("user notices after second identical refused within 1h: %d", len(notifsAfterDup))
	assert.Len(t, notifsAfterDup, 2, "identical (outcome, reason) within the hour bucket must be suppressed")
}

// Live authz at notification: admin authors a trigger with a notify agent,
// then loses project membership. The agent sink must not deliver.
func TestP7aProbe_AgentSinkLiveAuthzAfterRevocation(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()
	disp := &notifyTestDispatcher{}
	f.srv.SetDispatcher(disp)
	tr := p7aProbeTrigger(t, f, f.adminAuth, "p7a-revoke", &TriggerNotifySpec{AgentID: f.agentA.ID, UserNotice: boolPtr(false)})

	_, delErr := f.store.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.admin.ID)
	require.NoError(t, delErr)

	f.srv.notifyTriggerFire(ctx, tr, p7aFire(f, tr), hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "message failed"})
	for _, c := range disp.getCalls() {
		assert.NotEqual(t, f.agentA.ID, c.Agent.ID, "revoked author's notify message must not be delivered")
	}
	fires, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: f.projectID, TriggerID: tr.ID}, store.ListOptions{Limit: 20})
	require.NoError(t, err)
	for _, fr := range fires.Items {
		if fr.ActionType == store.TriggerActionNotify {
			t.Logf("notify row status=%s error=%q", fr.Status, fr.Error)
			assert.Equal(t, store.TriggerFireStatusRefused, fr.Status)
		}
	}
}

// O4: user notices must not be delivered to a creator who no longer holds
// trigger.read on the project.
func TestTriggerNotify_UserNoticeSkippedForRemovedProjectMember(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()
	fixedNow := time.Date(2026, 11, 27, 15, 10, 0, 0, time.UTC)
	f.srv.setFireNotifyClockForTest(func() time.Time { return fixedNow })

	tr := p7aProbeTrigger(t, f, f.adminAuth, "p7a-removed-member", &TriggerNotifySpec{UserNotice: boolPtr(true)})

	// Remove admin's project role bindings so they no longer hold trigger.read.
	_, err := f.store.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.admin.ID)
	require.NoError(t, err)

	f.srv.notifyTriggerFire(ctx, tr, p7aFire(f, tr), hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "message failed"})

	notifs, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.admin.ID, false)
	require.NoError(t, err)
	assert.Empty(t, notifs, "removed project member must not receive trigger failure user notices")

	fires, err := f.store.ListTriggerFires(ctx, store.TriggerFireFilter{ProjectID: f.projectID, TriggerID: tr.ID}, store.ListOptions{Limit: 20})
	require.NoError(t, err)
	foundSkipped := false
	for _, fr := range fires.Items {
		if fr.ActionType == store.TriggerActionNotify && fr.ActionIndex == store.TriggerFireActionIndexNotifyUser {
			assert.Equal(t, store.TriggerFireStatusSkipped, fr.Status)
			foundSkipped = true
		}
	}
	assert.True(t, foundSkipped, "expected user-notice notify row recorded as skipped")
}

// N3: sanitizeFireNotifyReason must truncate on a UTF-8 rune boundary.
func TestTriggerNotify_SanitizeReasonUTF8Boundary(t *testing.T) {
	// 45 four-byte runes = 180 bytes (> maxFireNotifyReasonBytes=120).
	raw := strings.Repeat("🔥", 45)
	got := sanitizeFireNotifyReason(hubfire.FireStatusFailed, raw)
	assert.True(t, utf8.ValidString(got), "truncated reason must remain valid UTF-8")
	assert.LessOrEqual(t, len(got), maxFireNotifyReasonBytes)
	assert.Equal(t, strings.Repeat("🔥", 30), got)

	// Offset by 1 ASCII byte so byte 120 falls inside a 4-byte rune.
	rawOffset := "a" + strings.Repeat("🔥", 45)
	gotOffset := sanitizeFireNotifyReason(hubfire.FireStatusFailed, rawOffset)
	assert.True(t, utf8.ValidString(gotOffset), "offset truncated reason must remain valid UTF-8")
	assert.LessOrEqual(t, len(gotOffset), maxFireNotifyReasonBytes)
	assert.Equal(t, "a"+strings.Repeat("🔥", 29), gotOffset)
}
