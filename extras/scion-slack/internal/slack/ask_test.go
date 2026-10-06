package slack

import (
	"context"
	"log/slog"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// askDelivery records what an ask-user answer handler sends to the hub.
type askDelivery struct {
	msgs   []*messages.StructuredMessage
	result *hubError
}

func (d *askDelivery) deliver(topic string, msg *messages.StructuredMessage) *hubError {
	d.msgs = append(d.msgs, msg)
	return d.result
}

func (f *commandFixture) createPendingAsk(t *testing.T) {
	t.Helper()
	require.NoError(t, f.store.CreatePendingAskUser(context.Background(), &PendingAskUser{
		RequestID: "req-1",
		MessageTS: "1.0",
		ChannelID: "C1",
		AgentSlug: "alpha",
		ProjectID: "proj-1",
		Choices:   []string{"yes", "no"},
		ExpiresAt: time.Now().Add(time.Hour),
	}))
}

func (f *commandFixture) askResponded(t *testing.T) bool {
	t.Helper()
	pending, err := f.store.GetPendingAskUser(context.Background(), "req-1")
	require.NoError(t, err)
	require.NotNil(t, pending)
	return pending.Responded
}

// askAnswerPaths drives each way a Slack user answers an agent question.
var askAnswerPaths = []struct {
	name   string
	answer func(f *commandFixture, d *askDelivery)
	text   string
}{
	{
		name: "option button",
		text: "yes",
		answer: func(f *commandFixture, d *askDelivery) {
			// Called directly: no current question message renders option
			// buttons that dispatch here.
			action := &slackapi.BlockAction{ActionID: "ask:opt:req-1", Value: "yes"}
			var cb slackapi.InteractionCallback
			cb.Channel.ID = "C1"
			cb.User.ID = "U1"
			cb.ActionCallback.BlockActions = []*slackapi.BlockAction{action}
			handleAskOption(context.Background(), f.slack.client(), f.store, d.deliver, cb, "req-1", slog.Default())
		},
	},
	{
		name: "reply form",
		text: "free text answer",
		answer: func(f *commandFixture, d *askDelivery) {
			var cb slackapi.InteractionCallback
			cb.User.ID = "U1"
			cb.View.CallbackID = "ask:modal:req-1"
			cb.View.State = &slackapi.ViewState{Values: map[string]map[string]slackapi.BlockAction{
				"response_block": {"response": {Value: "free text answer"}},
			}}
			HandleViewSubmission(context.Background(), f.slack.client(), f.store, d.deliver, cb, slog.Default())
		},
	},
}

func TestAskAnswer_SentAsTheLinkedUser(t *testing.T) {
	for _, p := range askAnswerPaths {
		t.Run(p.name, func(t *testing.T) {
			f := newCommandFixture(t)
			f.linkUser(t, "alice@example.com")
			f.createPendingAsk(t)
			d := &askDelivery{}

			p.answer(f, d)

			require.Len(t, d.msgs, 1)
			assert.Equal(t, "user:alice@example.com", d.msgs[0].Sender)
			assert.Equal(t, p.text, d.msgs[0].Msg)
			assert.True(t, f.askResponded(t))
		})
	}
}

func TestAskAnswer_LinkWithoutEmailAsksToReRegister(t *testing.T) {
	for _, p := range askAnswerPaths {
		t.Run(p.name, func(t *testing.T) {
			f := newCommandFixture(t)
			f.linkUser(t, "")
			f.createPendingAsk(t)
			d := &askDelivery{}

			p.answer(f, d)

			assert.Empty(t, d.msgs, "no answer is sent without a linked Scion email")
			assert.Equal(t, missingEmailLinkText, f.slack.lastText(t))
			assert.False(t, f.askResponded(t), "the question stays open")
		})
	}
}

func TestAskAnswer_WithoutLinkedAccountAsksToRegister(t *testing.T) {
	for _, p := range askAnswerPaths {
		t.Run(p.name, func(t *testing.T) {
			f := newCommandFixture(t)
			f.createPendingAsk(t)
			d := &askDelivery{}

			p.answer(f, d)

			assert.Empty(t, d.msgs, "no answer is sent without a linked account")
			assert.Contains(t, f.slack.lastText(t), "/scion register")
			assert.False(t, f.askResponded(t), "the question stays open")
		})
	}
}

func TestAskAnswer_DeliveryErrorIsShownAndQuestionStaysOpen(t *testing.T) {
	for _, p := range askAnswerPaths {
		t.Run(p.name, func(t *testing.T) {
			f := newCommandFixture(t)
			f.linkUser(t, "alice@example.com")
			f.createPendingAsk(t)
			d := &askDelivery{result: &hubError{StatusCode: 403, Code: "message_denied", Message: "Message delivery denied"}}

			p.answer(f, d)

			require.Len(t, d.msgs, 1)
			assert.Equal(t, "Your Scion account (alice@example.com) doesn't have permission to message this agent. Ask a project owner.", f.slack.lastText(t))
			assert.False(t, f.askResponded(t), "the user can answer again")
		})
	}
}
