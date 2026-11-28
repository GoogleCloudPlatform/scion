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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCreateTriggerRequest_NotifyFlags(t *testing.T) {
	reset := func() {
		triggerName = "notify-cli"
		triggerDescription = ""
		triggerWebhookRef = "deploys"
		triggerGitHubSource = false
		triggerMatchTypes = nil
		triggerWhereRules = nil
		triggerWhereFile = ""
		triggerAction = "message"
		triggerAgent = "lead"
		triggerMessage = "hello"
		triggerInterrupt = false
		triggerWake = false
		triggerPlain = false
		triggerOffline = ""
		triggerQueueTTL = ""
		triggerDispatch = dispatchAgentFlagValues{}
		triggerSubjectExpr = ""
		triggerOnSubject = ""
		triggerCoalesceWindow = ""
		triggerMaxFiresPerMinute = 0
		triggerMaxConcurrent = 0
		triggerNotifyUser = true
		triggerNotifyAgent = ""
		triggerNotifyOn = nil
	}
	t.Cleanup(reset)

	t.Run("omits notify when no notify flags are set", func(t *testing.T) {
		reset()
		req, _, err := buildCreateTriggerRequestWithAllFlags(false, false, false, false, false)
		require.NoError(t, err)
		assert.Nil(t, req.Notify)
	})

	t.Run("sets --notify-user=false, --notify-agent name, and --notify-on", func(t *testing.T) {
		reset()
		triggerNotifyUser = false
		triggerNotifyAgent = "ops-agent"
		triggerNotifyOn = []string{"failed,refused,expired,skipped"}

		req, _, err := buildCreateTriggerRequestWithAllFlags(false, false, false, false, true)
		require.NoError(t, err)
		require.NotNil(t, req.Notify)
		require.NotNil(t, req.Notify.UserNotice)
		assert.False(t, *req.Notify.UserNotice)
		assert.Equal(t, "ops-agent", req.Notify.AgentName)
		assert.Empty(t, req.Notify.AgentID)
		assert.Equal(t, []string{"failed", "refused", "expired", "skipped"}, req.Notify.OnOutcomes)
	})

	t.Run("sets --notify-agent UUID as AgentID", func(t *testing.T) {
		reset()
		agentUUID := "11111111-2222-3333-4444-555555555555"
		triggerNotifyAgent = agentUUID

		req, _, err := buildCreateTriggerRequestWithAllFlags(false, false, false, false, false)
		require.NoError(t, err)
		require.NotNil(t, req.Notify)
		assert.Equal(t, agentUUID, req.Notify.AgentID)
		assert.Empty(t, req.Notify.AgentName)
		assert.Nil(t, req.Notify.UserNotice)
	})

	t.Run("rejects invalid --notify-on outcome", func(t *testing.T) {
		reset()
		triggerNotifyOn = []string{"failed,fired"}

		_, _, err := buildCreateTriggerRequestWithAllFlags(false, false, false, false, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid --notify-on value")
	})
}

func TestTriggerUpdate_NotifyFlags(t *testing.T) {
	trigID := "22222222-3333-4444-5555-666666666666"
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
			userFalse := false
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(hubclient.Trigger{
				ID:                    trigID,
				ProjectID:             "proj-1",
				Name:                  "notify-upd",
				Status:                "active",
				AuthorizationRevision: 2,
				Notify: &hubclient.TriggerNotifySpec{
					UserNotice: &userFalse,
					AgentName:  "ops",
					OnOutcomes: []string{"failed", "skipped"},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	origCtxFn := triggerHubContextFn
	triggerHubContextFn = func() (*HubContext, string, context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		return &HubContext{Client: client, Endpoint: srv.URL}, "proj-1", ctx, cancel, nil
	}
	t.Cleanup(func() { triggerHubContextFn = origCtxFn })

	require.NoError(t, triggerUpdateCmd.Flags().Set("notify-user", "false"))
	require.NoError(t, triggerUpdateCmd.Flags().Set("notify-agent", "ops"))
	require.NoError(t, triggerUpdateCmd.Flags().Set("notify-on", "failed,skipped"))
	t.Cleanup(func() {
		_ = triggerUpdateCmd.Flags().Set("notify-user", "true")
		_ = triggerUpdateCmd.Flags().Set("notify-agent", "")
		_ = triggerUpdateCmd.Flags().Set("notify-on", "")
		triggerUpdateCmd.Flags().Lookup("notify-user").Changed = false
		triggerUpdateCmd.Flags().Lookup("notify-agent").Changed = false
		triggerUpdateCmd.Flags().Lookup("notify-on").Changed = false
		triggerUpdateNotifyOn = nil
	})

	require.NoError(t, runTriggerUpdate(triggerUpdateCmd, []string{trigID}))
	notifyMap, ok := gotBody["notify"].(map[string]any)
	require.True(t, ok, "expected notify block in PATCH request: %#v", gotBody)
	assert.Equal(t, false, notifyMap["userNotice"])
	assert.Equal(t, "ops", notifyMap["agentName"])
	assert.Equal(t, []any{"failed", "skipped"}, notifyMap["onOutcomes"])
}

func TestPrintTriggerDetail_ShowsNotify(t *testing.T) {
	userOn := true
	tr := &hubclient.Trigger{
		ID:                    "tr-1",
		ProjectID:             "proj-1",
		Name:                  "notify-get",
		Status:                "active",
		AuthorizationRevision: 1,
		Source:                hubclient.TriggerSourceSpec{Kind: "webhook", ID: "ep-1"},
		Match:                 hubclient.TriggerMatchSpec{Types: []string{"webhook.received"}},
		Actions:               []hubclient.TriggerActionSpec{{Type: "message", AgentName: "lead", Message: "hi"}},
		Notify: &hubclient.TriggerNotifySpec{
			UserNotice: &userOn,
			AgentID:    "ag-ops",
			AgentName:  "ops",
			OnOutcomes: []string{"failed", "refused", "expired"},
		},
	}

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	printTriggerDetail(tr)
	_ = w.Close()
	os.Stdout = origStdout

	var buf bytes.Buffer
	_, err = io.Copy(&buf, r)
	require.NoError(t, err)
	out := buf.String()
	assert.Contains(t, out, "Notify:      user=on agent=ops on=failed,refused,expired")
}
