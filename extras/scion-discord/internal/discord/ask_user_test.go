package discord

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// askUserTransport records outbound Discord REST calls with their bodies and
// answers channel message posts with a fixed message ID.
type askUserTransport struct {
	mu    sync.Mutex
	posts []askUserPost
}

type askUserPost struct {
	method string
	path   string
	body   []byte
}

const askUserTestMessageID = "msg-ask-1"

func (rt *askUserTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	rt.mu.Lock()
	rt.posts = append(rt.posts, askUserPost{method: req.Method, path: req.URL.Path, body: body})
	rt.mu.Unlock()

	resp := `{}`
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/messages") {
		resp = `{"id":"` + askUserTestMessageID + `"}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(resp)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Request:    req,
	}, nil
}

// postedCustomIDs returns the button custom_ids of the messages posted to
// the given channel.
func (rt *askUserTransport) postedCustomIDs(t *testing.T, channelID string) []string {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()

	var ids []string
	for _, p := range rt.posts {
		if p.method != http.MethodPost || p.path != "/api/v9/channels/"+channelID+"/messages" {
			continue
		}
		var sent struct {
			Components []struct {
				Components []struct {
					CustomID string `json:"custom_id"`
				} `json:"components"`
			} `json:"components"`
		}
		require.NoError(t, json.Unmarshal(p.body, &sent))
		for _, row := range sent.Components {
			for _, c := range row.Components {
				ids = append(ids, c.CustomID)
			}
		}
	}
	return ids
}

type deliveredAnswer struct {
	topic string
	msg   *messages.StructuredMessage
}

// newAskUserFixture returns a broker wired to a recording session and a
// SQLite store, plus a deliver function that records hub deliveries.
func newAskUserFixture(t *testing.T, channelID string) (*DiscordBroker, *askUserTransport, *[]deliveredAnswer, func(string, *messages.StructuredMessage) *hubError) {
	t.Helper()
	session, err := discordgo.New("Bot test-token")
	require.NoError(t, err)
	rt := &askUserTransport{}
	session.Client = &http.Client{Transport: rt}
	session.MaxRestRetries = 0
	session.ShouldRetryOnRateLimit = false
	_ = session.State.GuildAdd(&discordgo.Guild{ID: testGuildID})
	_ = session.State.ChannelAdd(&discordgo.Channel{ID: channelID, GuildID: testGuildID, Type: discordgo.ChannelTypeGuildText})

	b := testBroker(session)
	b.log = discardLogger()
	b.store = newTestBrokerStore(t)

	var mu sync.Mutex
	delivered := &[]deliveredAnswer{}
	deliver := func(topic string, msg *messages.StructuredMessage) *hubError {
		mu.Lock()
		defer mu.Unlock()
		*delivered = append(*delivered, deliveredAnswer{topic: topic, msg: msg})
		return nil
	}
	return b, rt, delivered, deliver
}

func askUserQuestion(text, choicesJSON string) *messages.StructuredMessage {
	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Channel:   "discord",
		Sender:    "agent:coder",
		Recipient: "user:alice@example.com",
		Msg:       text,
		Type:      messages.TypeInputNeeded,
		ThreadID:  "chan-ask",
	}
	if choicesJSON != "" {
		msg.Metadata = map[string]string{"choices": choicesJSON}
	}
	return msg
}

func askUserMember(userID string) *discordgo.Member {
	return &discordgo.Member{User: &discordgo.User{ID: userID}}
}

// TestAskUser_ButtonAnswerDelivered runs from a posted question with choices
// to the answer delivered to the asking agent through a choice button.
func TestAskUser_ButtonAnswerDelivered(t *testing.T) {
	ctx := context.Background()
	const channelID = "chan-ask"
	b, rt, delivered, deliver := newAskUserFixture(t, channelID)

	topic := projectkeys.UserTopic("proj-1", "alice")
	require.NoError(t, b.Publish(ctx, topic, askUserQuestion("Deploy now?", `["yes","no"]`)))

	ids := rt.postedCustomIDs(t, channelID)
	require.Len(t, ids, 2, "the question should be posted with one button per choice")
	require.True(t, strings.HasPrefix(ids[1], "ask:opt:"), "unexpected custom_id %q", ids[1])
	requestID := strings.Split(ids[1], ":")[2]

	pending, err := b.store.GetPendingAskUser(ctx, requestID)
	require.NoError(t, err)
	require.NotNil(t, pending, "posting the question should record a pending ask-user entry")
	assert.Equal(t, channelID, pending.ChannelID)
	assert.Equal(t, askUserTestMessageID, pending.MessageID)
	assert.Equal(t, "coder", pending.AgentSlug)
	assert.Equal(t, "proj-1", pending.ProjectID)
	assert.Equal(t, []string{"yes", "no"}, pending.Choices)

	h := NewCallbackHandler(b.store, b.session, nil, deliver, discardLogger())
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type:      discordgo.InteractionMessageComponent,
		ChannelID: channelID,
		Member:    askUserMember("u-1"),
		Data:      discordgo.MessageComponentInteractionData{CustomID: ids[1]},
	}}
	h.Dispatch(b.session, i, ids[1], nil)

	require.Len(t, *delivered, 1, "the button answer should be delivered to the hub")
	got := (*delivered)[0]
	assert.Equal(t, projectkeys.AgentTopic("proj-1", "coder"), got.topic)
	assert.Equal(t, "agent:coder", got.msg.Recipient)
	assert.Equal(t, "no", got.msg.Msg)
	assert.Equal(t, requestID, got.msg.Metadata["ask_request_id"])

	pending, err = b.store.GetPendingAskUser(ctx, requestID)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.True(t, pending.Responded, "the entry should be marked answered after delivery")

	// A second click on the answered question delivers nothing more.
	h.Dispatch(b.session, i, ids[0], nil)
	assert.Len(t, *delivered, 1)
}

// TestAskUser_ModalAnswerDelivered runs from a posted free-text question to
// the answer delivered through the Reply button and its modal.
func TestAskUser_ModalAnswerDelivered(t *testing.T) {
	ctx := context.Background()
	const channelID = "chan-ask"
	b, rt, delivered, deliver := newAskUserFixture(t, channelID)

	topic := projectkeys.UserTopic("proj-1", "alice")
	require.NoError(t, b.Publish(ctx, topic, askUserQuestion("Which branch?", "")))

	ids := rt.postedCustomIDs(t, channelID)
	require.Len(t, ids, 2, "the question should be posted with Reply and Dismiss buttons")
	require.True(t, strings.HasPrefix(ids[0], "ask:reply:"), "unexpected custom_id %q", ids[0])
	requestID := strings.TrimPrefix(ids[0], "ask:reply:")

	pending, err := b.store.GetPendingAskUser(ctx, requestID)
	require.NoError(t, err)
	require.NotNil(t, pending, "posting the question should record a pending ask-user entry")

	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type:      discordgo.InteractionModalSubmit,
		ChannelID: channelID,
		Member:    askUserMember("u-1"),
		Data: discordgo.ModalSubmitInteractionData{
			CustomID: "ask:modal:" + requestID,
			Components: []discordgo.MessageComponent{
				&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					&discordgo.TextInput{CustomID: "response", Value: "release/1.2"},
				}},
			},
		},
	}}
	HandleModalSubmit(b.session, i, b.store, deliver, discardLogger())

	require.Len(t, *delivered, 1, "the modal answer should be delivered to the hub")
	got := (*delivered)[0]
	assert.Equal(t, projectkeys.AgentTopic("proj-1", "coder"), got.topic)
	assert.Equal(t, "release/1.2", got.msg.Msg)
	assert.Equal(t, requestID, got.msg.Metadata["ask_request_id"])

	pending, err = b.store.GetPendingAskUser(ctx, requestID)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.True(t, pending.Responded, "the entry should be marked answered after delivery")
}
