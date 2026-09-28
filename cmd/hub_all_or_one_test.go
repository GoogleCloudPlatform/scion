// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHubAllOrOneActions(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		output     string
		args       []string
		all        bool
		run        func(*cobra.Command, []string) error
		setAllFlag func(bool)
	}{
		{
			name:       "message",
			path:       "/api/v1/messages/msg-1/read",
			output:     "Message msg-1 marked as read.\n",
			args:       []string{"msg-1"},
			run:        runMessagesRead,
			setAllFlag: func(all bool) { messagesReadAll = all },
		},
		{
			name:       "all messages",
			path:       "/api/v1/messages/read-all",
			output:     "All messages marked as read.\n",
			all:        true,
			run:        runMessagesRead,
			setAllFlag: func(all bool) { messagesReadAll = all },
		},
		{
			name:       "notification",
			path:       "/api/v1/notifications/notif-1/ack",
			output:     "Notification notif-1 acknowledged.\n",
			args:       []string{"notif-1"},
			run:        runNotificationsAck,
			setAllFlag: func(all bool) { notificationsAckAll = all },
		},
		{
			name:       "all notifications",
			path:       "/api/v1/notifications/ack-all",
			output:     "All notifications acknowledged.\n",
			all:        true,
			run:        runNotificationsAck,
			setAllFlag: func(all bool) { notificationsAckAll = all },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requestCount atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount.Add(1)
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, tt.path, r.URL.Path)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			oldProjectPath := projectPath
			oldMessagesReadAll := messagesReadAll
			oldNotificationsAckAll := notificationsAckAll
			defer func() {
				projectPath = oldProjectPath
				messagesReadAll = oldMessagesReadAll
				notificationsAckAll = oldNotificationsAckAll
			}()

			t.Setenv("HOME", t.TempDir())
			projectPath = setupSecretProject(t, t.TempDir(), server.URL)
			tt.setAllFlag(tt.all)

			var runErr error
			output := captureStdout(t, func() {
				runErr = tt.run(nil, tt.args)
			})
			require.NoError(t, runErr)
			assert.Equal(t, tt.output, output)
			assert.EqualValues(t, 1, requestCount.Load())
		})
	}
}

func TestHubAllOrOneActionValidation(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		all        bool
		wantError  string
		run        func(*cobra.Command, []string) error
		setAllFlag func(bool)
	}{
		{
			name:       "message target missing",
			wantError:  "provide a message ID or use --all to mark all messages as read",
			run:        runMessagesRead,
			setAllFlag: func(all bool) { messagesReadAll = all },
		},
		{
			name:       "message target conflicts with all",
			args:       []string{"msg-1"},
			all:        true,
			wantError:  "provide either a message ID or --all, not both",
			run:        runMessagesRead,
			setAllFlag: func(all bool) { messagesReadAll = all },
		},
		{
			name:       "notification target missing",
			wantError:  "provide a notification ID or use --all to acknowledge all notifications",
			run:        runNotificationsAck,
			setAllFlag: func(all bool) { notificationsAckAll = all },
		},
		{
			name:       "notification target conflicts with all",
			args:       []string{"notif-1"},
			all:        true,
			wantError:  "provide either a notification ID or --all, not both",
			run:        runNotificationsAck,
			setAllFlag: func(all bool) { notificationsAckAll = all },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldMessagesReadAll := messagesReadAll
			oldNotificationsAckAll := notificationsAckAll
			defer func() {
				messagesReadAll = oldMessagesReadAll
				notificationsAckAll = oldNotificationsAckAll
			}()

			tt.setAllFlag(tt.all)
			assert.EqualError(t, tt.run(nil, tt.args), tt.wantError)
		})
	}
}
