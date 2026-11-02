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
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oldBrokerProjectStorageText is an older broker's project storage refusal,
// which named the host mount path in the message.
const oldBrokerProjectStorageText = "project storage share is not mounted on this broker (/mnt/nfs/hub-a; mount it or set nfs.auto_mount)"

// projectStorageRefusalCases are every known path-free project storage
// refusal text, kept as sent, and an older broker's text, replaced by the
// hub's fixed text.
func projectStorageRefusalCases() []struct{ name, sent, want string } {
	var cases []struct{ name, sent, want string }
	for i, text := range scionrt.ProjectShareClientTexts() {
		cases = append(cases, struct{ name, sent, want string }{fmt.Sprintf("known %d", i), text, text})
	}
	return append(cases, struct{ name, sent, want string }{"old broker", oldBrokerProjectStorageText, projectStorageRefusalFallback})
}

func projectStorageBrokerErr(message string) error {
	return brokerHarnessConfigErr(http.StatusServiceUnavailable, "project_storage_unavailable", message)
}

func TestBrokerProjectStorageRefusal(t *testing.T) {
	for _, tc := range projectStorageRefusalCases() {
		t.Run(tc.name, func(t *testing.T) {
			for _, err := range []error{
				projectStorageBrokerErr(tc.sent),
				fmt.Errorf("dispatch start failed: %w", projectStorageBrokerErr(tc.sent)),
			} {
				status, code, msg, ok := brokerProjectStorageRefusal(err)
				require.True(t, ok)
				assert.Equal(t, http.StatusServiceUnavailable, status)
				assert.Equal(t, "project_storage_unavailable", code)
				assert.Equal(t, tc.want, msg)
				assert.NotContains(t, msg, "/mnt/nfs")
			}
		})
	}
	var typedNil *brokerStatusError
	for name, err := range map[string]error{
		"nil":                  nil,
		"plain error":          errors.New("dial tcp: connection refused"),
		"typed nil":            typedNil,
		"other 503 code":       brokerHarnessConfigErr(http.StatusServiceUnavailable, "broker_draining", "draining"),
		"code on other status": brokerHarnessConfigErr(http.StatusBadGateway, "project_storage_unavailable", oldBrokerProjectStorageText),
	} {
		_, _, _, ok := brokerProjectStorageRefusal(err)
		assert.False(t, ok, name)
	}
}

// The provision-only create warning and the reincarnation record carry the
// code and the filtered message of a broker 503 project_storage_unavailable,
// never the raw body.
func TestDispatchFailureText_ProjectStorageRefusalFiltered(t *testing.T) {
	for _, tc := range projectStorageRefusalCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := dispatchFailureText(projectStorageBrokerErr(tc.sent))
			assert.Equal(t, "project_storage_unavailable: "+tc.want, got)
			assert.NotContains(t, got, "/mnt/nfs", "an older broker's mount path must not be recorded")
		})
	}
}

// A wake whose resume the broker refuses because it does not see the
// project storage share answers with the broker's status and code, and only
// a known path-free message.
func TestWakeAgentForDM_ProjectStorageRefusal(t *testing.T) {
	for _, tc := range projectStorageRefusalCases() {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
			srv.SetDispatcher(&wakeTrackingDispatcher{startReturnErr: projectStorageBrokerErr(tc.sent)})

			result, dmErr := srv.wakeAgentForDM(context.Background(), target)
			assert.Nil(t, result)
			require.NotNil(t, dmErr)
			assert.Equal(t, http.StatusServiceUnavailable, dmErr.HTTPStatus)
			assert.Equal(t, "project_storage_unavailable", dmErr.Code)
			assert.Equal(t, tc.want, dmErr.Message)
			assert.Empty(t, dmErr.Details, "broker start markers are not relayed")
		})
	}
}

// wakeProjectStorageEntryCases are the old-broker and one known text, for
// the three wake entry points.
var wakeProjectStorageEntryCases = []struct{ name, sent, want string }{
	{"known", projectStorageMessage, projectStorageMessage},
	{"old broker", oldBrokerProjectStorageText, projectStorageRefusalFallback},
}

// Message send with wake.
func TestMessageWake_ProjectStorageRefusal(t *testing.T) {
	for i, tc := range wakeProjectStorageEntryCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
			disp := &wakeTrackingDispatcher{startReturnErr: projectStorageBrokerErr(tc.sent)}
			srv.SetDispatcher(disp)
			u := wakeLifecycleUser(t, s, fmt.Sprintf("wake-share-%d", i), target.ProjectID, "agent.message", "agent.read", "agent.lifecycle")

			rec := requestAsIdentity(t, srv, authUser(u), http.MethodPost, "/api/v1/agents/"+target.ID+"/message",
				map[string]interface{}{"message": "please wake", "wake": true})

			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			apiErr := decodeTargetAPIError(t, rec)
			assert.Equal(t, "project_storage_unavailable", apiErr.Code)
			assert.Equal(t, tc.want, apiErr.Message)
			assert.NotContains(t, rec.Body.String(), "/mnt/nfs")
			assert.Empty(t, disp.getMessageCalls(), "no message may be delivered")
		})
	}
}

// Agent DM with wake.
func TestExecuteAgentDM_Wake_ProjectStorageRefusal(t *testing.T) {
	for _, tc := range wakeProjectStorageEntryCases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, sender, target := createWakeDMFixtures(t, string(state.PhaseSuspended))
			disp := &wakeTrackingDispatcher{startReturnErr: projectStorageBrokerErr(tc.sent)}
			srv.SetDispatcher(disp)

			result, dmErr := srv.ExecuteAgentDM(context.Background(), &AgentDMInput{
				SenderAgent:    sender,
				SenderIdentity: wakeDMSenderIdentity(sender, ScopeProjectRead, ScopeAgentLifecycle),
				TargetAgent:    target,
				Msg:            "hello after wake",
				Type:           "instruction",
				ProjectID:      sender.ProjectID,
				Wake:           true,
			})

			assert.Nil(t, result)
			require.NotNil(t, dmErr)
			assert.Equal(t, http.StatusServiceUnavailable, dmErr.HTTPStatus)
			assert.Equal(t, "project_storage_unavailable", dmErr.Code)
			assert.Equal(t, tc.want, dmErr.Message)
			assert.Empty(t, disp.getMessageCalls(), "no message may be delivered")
		})
	}
}

// Chat send with wake.
func TestChatV2Wake_ProjectStorageRefusal(t *testing.T) {
	for _, tc := range wakeProjectStorageEntryCases {
		t.Run(tc.name, func(t *testing.T) {
			f := chatWakeSetup(t, string(state.PhaseSuspended))
			f.disp.startReturnErr = projectStorageBrokerErr(tc.sent)

			rec := doRequest(t, f.srv, http.MethodPost, f.path(),
				map[string]any{"content": "hello", "wake": true})

			require.Equal(t, http.StatusServiceUnavailable, rec.Code, "body=%s", rec.Body.String())
			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
			assert.Equal(t, "project_storage_unavailable", resp.Error.Code)
			assert.Equal(t, tc.want, resp.Error.Message)
			assert.NotContains(t, rec.Body.String(), "/mnt/nfs")
			assert.Empty(t, f.disp.getMessageCalls())
			assert.Equal(t, 0, f.countThreadMessages(t))
		})
	}
}
