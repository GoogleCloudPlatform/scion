package discord

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestStructuredMessage() *messages.StructuredMessage {
	return &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Channel:   "discord",
		Sender:    "user:alice@example.com",
		Recipient: "agent:coder",
		Msg:       "hello",
		Type:      messages.TypeInstruction,
	}
}

func TestParseHubError(t *testing.T) {
	t.Run("valid error response", func(t *testing.T) {
		body := `{"error":{"code":"agent_not_found","message":"Agent \"coder\" not found in project"}}`
		resp := &http.Response{
			StatusCode: 404,
			Body:       io.NopCloser(strings.NewReader(body)),
		}
		he := parseHubError(resp)
		require.NotNil(t, he)
		assert.Equal(t, 404, he.StatusCode)
		assert.Equal(t, "agent_not_found", he.Code)
		assert.Equal(t, `Agent "coder" not found in project`, he.Message)
	})

	t.Run("empty body", func(t *testing.T) {
		resp := &http.Response{
			StatusCode: 500,
			Body:       io.NopCloser(strings.NewReader("")),
		}
		he := parseHubError(resp)
		assert.Equal(t, "unknown", he.Code)
		assert.Equal(t, "Internal Server Error", he.Message)
	})

	t.Run("invalid JSON", func(t *testing.T) {
		resp := &http.Response{
			StatusCode: 403,
			Body:       io.NopCloser(strings.NewReader("not json")),
		}
		he := parseHubError(resp)
		assert.Equal(t, "unknown", he.Code)
		assert.Equal(t, "Forbidden", he.Message)
	})
}

func TestHubError_UserFacingMessage(t *testing.T) {
	tests := []struct {
		name     string
		err      hubError
		contains string
	}{
		{
			name:     "agent not found",
			err:      hubError{StatusCode: 404, Code: "agent_not_found", Message: "Agent not found"},
			contains: "Target agent not found",
		},
		{
			name:     "forbidden",
			err:      hubError{StatusCode: 403, Code: "forbidden", Message: "no permission"},
			contains: "permission",
		},
		{
			name:     "unauthorized",
			err:      hubError{StatusCode: 401, Code: "unauthorized", Message: "bad auth"},
			contains: "Authentication error",
		},
		{
			name:     "broker auth failed",
			err:      hubError{StatusCode: 401, Code: "broker_auth_failed", Message: "bad hmac"},
			contains: "Authentication error",
		},
		{
			name:     "server error",
			err:      hubError{StatusCode: 502, Code: "runtime_error", Message: "agent unreachable"},
			contains: "try again or contact",
		},
		{
			name:     "other client error",
			err:      hubError{StatusCode: 400, Code: "invalid_request", Message: "bad topic format"},
			contains: "try again or contact",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := tt.err.userFacingMessage()
			assert.Contains(t, msg, tt.contains)
		})
	}
}

func TestDeliverInbound_ReturnsHubError(t *testing.T) {
	t.Run("404 agent not found", func(t *testing.T) {
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "agent_not_found",
					"message": "Agent not found",
				},
			})
		}))
		defer hub.Close()

		b := &DiscordBroker{
			log:        discardLogger(),
			hubURL:     hub.URL,
			httpClient: http.DefaultClient,
		}

		he := b.deliverInbound("scion.project.p1.agent.coder.messages", newTestStructuredMessage())
		require.NotNil(t, he)
		assert.Equal(t, 404, he.StatusCode)
		assert.Equal(t, "agent_not_found", he.Code)
	})

	t.Run("403 forbidden", func(t *testing.T) {
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "forbidden",
					"message": "user does not have permission",
				},
			})
		}))
		defer hub.Close()

		b := &DiscordBroker{
			log:        discardLogger(),
			hubURL:     hub.URL,
			httpClient: http.DefaultClient,
		}

		he := b.deliverInbound("scion.project.p1.agent.coder.messages", newTestStructuredMessage())
		require.NotNil(t, he)
		assert.Equal(t, 403, he.StatusCode)
		assert.Equal(t, "forbidden", he.Code)
	})

	t.Run("200 success returns nil", func(t *testing.T) {
		hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"delivered": true,
				"agentId":   "agent-123",
			})
		}))
		defer hub.Close()

		b := &DiscordBroker{
			log:        discardLogger(),
			hubURL:     hub.URL,
			httpClient: http.DefaultClient,
		}

		he := b.deliverInbound("scion.project.p1.agent.coder.messages", newTestStructuredMessage())
		assert.Nil(t, he)
	})

	t.Run("in-process handler returns nil", func(t *testing.T) {
		b := &DiscordBroker{
			log: discardLogger(),
			InboundHandler: func(_ string, _ *messages.StructuredMessage) {
			},
		}

		he := b.deliverInbound("test.topic", newTestStructuredMessage())
		assert.Nil(t, he)
	})
}
