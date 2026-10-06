package discord

import (
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
)

// replySlugBroker returns a broker whose webhook manager lists channelWebhooks
// for channel C1 from a stub that records every request.
func replySlugBroker(t *testing.T, channelWebhooks ...*discordgo.Webhook) (*DiscordBroker, *webhookListStub) {
	t.Helper()
	stub := &webhookListStub{webhooks: map[string][]*discordgo.Webhook{"C1": channelWebhooks}}
	b := &DiscordBroker{
		webhooks:      newWebhookListManager(t, stub),
		threadParents: map[string]string{"C1": ""},
	}
	return b, stub
}

// replySlug resolves the agent for a reply in C1 the way the broker does.
func replySlug(b *DiscordBroker, ref *discordgo.Message) string {
	return agentFromReply(ref, func(webhookID string) bool {
		return b.ownsWebhook("C1", webhookID)
	})
}

// assertOnlyWebhookReads checks that every recorded request is a read.
func assertOnlyWebhookReads(t *testing.T, stub *webhookListStub) {
	t.Helper()
	for _, r := range stub.snapshot() {
		assert.True(t, strings.HasPrefix(r, http.MethodGet+" "), "only reads are made, got %s", r)
	}
}

func TestReplySlug_OwnWebhookResolvesAgent(t *testing.T) {
	ours := &discordgo.Webhook{ID: "wh-ours", Name: webhookName, User: &discordgo.User{ID: "BOT123"}}
	b, stub := replySlugBroker(t, ours)

	ref := &discordgo.Message{WebhookID: "wh-ours", Author: &discordgo.User{ID: "wh-ours", Username: "coder"}}
	assert.Equal(t, "coder", replySlug(b, ref))
	assertOnlyWebhookReads(t, stub)
}

func TestReplySlug_OnlyOwnWebhookResolvesAgent(t *testing.T) {
	ours := &discordgo.Webhook{ID: "wh-ours", Name: webhookName, User: &discordgo.User{ID: "BOT123"}}
	for name, other := range map[string]*discordgo.Webhook{
		"webhook with our name and another creator": {ID: "wh-other", Name: webhookName, User: &discordgo.User{ID: "OTHER"}},
		"bot-created webhook with another name":     {ID: "wh-other", Name: "Something Else", User: &discordgo.User{ID: "BOT123"}},
	} {
		t.Run(name, func(t *testing.T) {
			for label, listed := range map[string][]*discordgo.Webhook{
				"channel also has our webhook": {other, ours},
				"channel has only this one":    {other},
			} {
				t.Run(label, func(t *testing.T) {
					b, stub := replySlugBroker(t, listed...)
					ref := &discordgo.Message{WebhookID: "wh-other", Author: &discordgo.User{ID: "wh-other", Username: "coder"}}
					assert.Equal(t, "", replySlug(b, ref))
					assertOnlyWebhookReads(t, stub)
				})
			}
		})
	}
}

func TestReplySlug_BotMessageResolvesNoAgent(t *testing.T) {
	ours := &discordgo.Webhook{ID: "wh-ours", Name: webhookName, User: &discordgo.User{ID: "BOT123"}}
	b, stub := replySlugBroker(t, ours)

	ref := &discordgo.Message{Author: &discordgo.User{ID: "BOT123", Username: "coder"}}
	assert.Equal(t, "", replySlug(b, ref))
	assert.Empty(t, stub.snapshot(), "a message without a webhook needs no lookup")
}

func TestReplySlug_UnknownBotUserResolvesNoAgent(t *testing.T) {
	unnamedCreator := &discordgo.Webhook{ID: "wh-ours", Name: webhookName, User: &discordgo.User{}}
	b, stub := replySlugBroker(t, unnamedCreator)
	b.webhooks.session.State.User = &discordgo.User{}

	ref := &discordgo.Message{WebhookID: "wh-ours", Author: &discordgo.User{ID: "wh-ours", Username: "coder"}}
	assert.Equal(t, "", replySlug(b, ref))
	assertOnlyWebhookReads(t, stub)
}
