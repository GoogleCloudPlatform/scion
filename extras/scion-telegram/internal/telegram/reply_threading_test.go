package telegram

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireThreadedPlainReply asserts that exactly one message was sent, that
// it carries no parse mode, and that it replies to the sender's message.
func requireThreadedPlainReply(t *testing.T, tgSrv *fakeTGServerV2, wantText string, replyTo int64) {
	t.Helper()
	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, wantText)
	assert.Empty(t, sent[0].ParseMode, "error replies are plain text")
	assert.Equal(t, replyTo, sent[0].ReplyToMessageID, "error reply threads to the sender's message")
}

func TestV2_ErrorReplies_ThreadToSenderWithoutParseMode(t *testing.T) {
	t.Run("default agent no longer available", func(t *testing.T) {
		b, tgSrv, hub := newRoutingTestBroker(t)
		hub.agents["proj-1"] = []AgentInfo{{Slug: "other"}}
		linkTestUser(t, b.store, 456, "alice@example.com")

		msg := plainGroupMessage(456, "hello")
		b.handleGroupMessage(b.gen, msg)

		requireThreadedPlainReply(t, tgSrv, "no longer available", msg.MessageID)
	})

	t.Run("agent list unavailable", func(t *testing.T) {
		b, tgSrv, hub := newRoutingTestBroker(t)
		hub.listAgentsErr = errors.New("list agents returned status 500")
		linkTestUser(t, b.store, 456, "alice@example.com")

		msg := plainGroupMessage(456, "hello")
		b.handleGroupMessage(b.gen, msg)

		requireThreadedPlainReply(t, tgSrv, "Couldn't fetch the agent list", msg.MessageID)
	})

	t.Run("sender denied by the hub", func(t *testing.T) {
		b, tgSrv, hub := newRoutingTestBroker(t)
		hub.listAgentsErr = forbiddenListAgents()
		linkTestUser(t, b.store, 456, "alice@example.com")

		// Addresses the bot without naming an agent, so the message
		// resolves to no target and the denial is reported.
		msg := botMentionMessage(456, "hello")
		b.handleGroupMessage(b.gen, msg)

		requireThreadedPlainReply(t, tgSrv, "doesn't have permission to list agents", msg.MessageID)
	})

	t.Run("delivery failed", func(t *testing.T) {
		hubSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": "not_found", "message": "agent not found"},
			})
		}))
		t.Cleanup(hubSrv.Close)

		b, tgSrv, hub := newRoutingTestBroker(t)
		hub.agents["proj-1"] = []AgentInfo{{Slug: "coder"}}
		linkTestUser(t, b.store, 456, "alice@example.com")
		b.mu.Lock()
		b.InboundHandler = nil
		b.hubURL = hubSrv.URL
		b.mu.Unlock()

		msg := plainGroupMessage(456, "hello")
		b.handleGroupMessage(b.gen, msg)

		requireThreadedPlainReply(t, tgSrv, "Message delivery failed: agent not found", msg.MessageID)
	})
}
