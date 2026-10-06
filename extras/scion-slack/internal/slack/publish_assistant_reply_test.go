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

package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// The retired assistant-reply mirror is discarded before any Slack API
// call, even if an older hub still forwards it. The same message as an
// instruction is the control: it reaches chat.postMessage.
func TestPublish_DiscardsRetiredAssistantReply(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			posts.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"channel":"C1","ts":"1.1"}`))
	}))
	t.Cleanup(srv.Close)

	b := NewBroker(nil)
	b.client = slackapi.New("xoxb-test", slackapi.OptionAPIURL(srv.URL+"/"))
	msg := &messages.StructuredMessage{
		Version:  messages.Version,
		Sender:   "agent:coder",
		Msg:      "turn text",
		Type:     messages.TypeAssistantReply,
		Metadata: map[string]string{"slack_channel_id": "C1"},
	}
	require.NoError(t, b.Publish(context.Background(), "scion.project.p1.agent.coder.messages", msg))
	assert.Zero(t, posts.Load(), "assistant-reply must not be posted")

	ctrl := *msg
	ctrl.Type = messages.TypeInstruction
	ctrl.Msg = "deliberate"
	require.NoError(t, b.Publish(context.Background(), "scion.project.p1.agent.coder.messages", &ctrl))
	assert.NotZero(t, posts.Load(), "control: an instruction is posted")
}
