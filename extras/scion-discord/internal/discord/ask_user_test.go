package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

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

type askUserButton struct {
	customID string
	label    string
}

// postedButtons returns the buttons of the messages posted to the given
// channel, in order.
func (rt *askUserTransport) postedButtons(t *testing.T, channelID string) []askUserButton {
	t.Helper()
	rt.mu.Lock()
	defer rt.mu.Unlock()

	var buttons []askUserButton
	for _, p := range rt.posts {
		if p.method != http.MethodPost || p.path != "/api/v9/channels/"+channelID+"/messages" {
			continue
		}
		var sent struct {
			Components []struct {
				Components []struct {
					CustomID string `json:"custom_id"`
					Label    string `json:"label"`
				} `json:"components"`
			} `json:"components"`
		}
		require.NoError(t, json.Unmarshal(p.body, &sent))
		for _, row := range sent.Components {
			for _, c := range row.Components {
				buttons = append(buttons, askUserButton{customID: c.CustomID, label: c.Label})
			}
		}
	}
	return buttons
}

// channelPosts returns the number of messages posted to the given channel.
func (rt *askUserTransport) channelPosts(channelID string) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	n := 0
	for _, p := range rt.posts {
		if p.method == http.MethodPost && p.path == "/api/v9/channels/"+channelID+"/messages" {
			n++
		}
	}
	return n
}

// interactionCalls returns the interaction responses sent for the test
// interaction token, split into edits of the original message and
// follow-up messages.
func (rt *askUserTransport) interactionCalls() (edits, followups []askUserPost) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	for _, p := range rt.posts {
		if !strings.Contains(p.path, "/webhooks/app-1/tok-1") {
			continue
		}
		switch {
		case p.method == http.MethodPatch && strings.HasSuffix(p.path, "/messages/@original"):
			edits = append(edits, p)
		case p.method == http.MethodPost:
			followups = append(followups, p)
		}
	}
	return edits, followups
}

func askUserClick(channelID, customID string) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		AppID:     "app-1",
		Token:     "tok-1",
		Type:      discordgo.InteractionMessageComponent,
		ChannelID: channelID,
		Member:    askUserMember("u-1"),
		Data:      discordgo.MessageComponentInteractionData{CustomID: customID},
	}}
}

// TestAskUser_FailedDeliveryKeepsQuestion checks that a failed delivery of
// a choice answer leaves the question, its buttons and its pending entry in
// place so the user can answer again.
func TestAskUser_FailedDeliveryKeepsQuestion(t *testing.T) {
	ctx := context.Background()
	const channelID = "chan-ask"
	b, rt, _, _ := newAskUserFixture(t, channelID)

	require.NoError(t, b.Publish(ctx, projectkeys.UserTopic("proj-1", "alice"),
		askUserQuestion("Deploy now?", `["yes","no"]`)))
	ids := rt.postedCustomIDs(t, channelID)
	require.Len(t, ids, 2)
	requestID := strings.Split(ids[0], ":")[2]

	var attempts []*messages.StructuredMessage
	deliver := func(_ string, msg *messages.StructuredMessage) *hubError {
		attempts = append(attempts, msg)
		return &hubError{StatusCode: http.StatusServiceUnavailable, Code: "unavailable", Message: "try later"}
	}
	h := NewCallbackHandler(b.store, b.session, nil, deliver, discardLogger())
	h.Dispatch(b.session, askUserClick(channelID, ids[0]), ids[0], nil)

	require.Len(t, attempts, 1, "the answer should be sent to the hub once")
	assert.Equal(t, "discord:u-1", attempts[0].Sender, "the answer should carry the clicking user")
	assert.Equal(t, "u-1", attempts[0].SenderID)

	pending, err := b.store.GetPendingAskUser(ctx, requestID)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.False(t, pending.Responded, "a failed delivery should leave the entry unanswered")

	edits, followups := rt.interactionCalls()
	assert.Empty(t, edits, "the question and its buttons should not be edited")
	require.Len(t, followups, 1, "the error should be shown as an ephemeral follow-up")
	assertEphemeralFollowup(t, followups[0],
		"Failed to deliver message. Please try again or contact an administrator.")
}

// TestAskUser_LookupErrorKeepsQuestion checks that a store error while
// looking up the request leaves the question and its buttons in place.
func TestAskUser_LookupErrorKeepsQuestion(t *testing.T) {
	ctx := context.Background()
	const channelID = "chan-ask"
	b, rt, delivered, deliver := newAskUserFixture(t, channelID)

	require.NoError(t, b.Publish(ctx, projectkeys.UserTopic("proj-1", "alice"),
		askUserQuestion("Deploy now?", `["yes","no"]`)))
	ids := rt.postedCustomIDs(t, channelID)
	require.Len(t, ids, 2)

	require.NoError(t, b.store.Close())
	h := NewCallbackHandler(b.store, b.session, nil, deliver, discardLogger())
	h.Dispatch(b.session, askUserClick(channelID, ids[0]), ids[0], nil)

	assert.Empty(t, *delivered)
	edits, followups := rt.interactionCalls()
	assert.Empty(t, edits, "the question and its buttons should not be edited")
	require.Len(t, followups, 1, "the error should be shown as an ephemeral follow-up")
	assertEphemeralFollowup(t, followups[0], "Error looking up request. Please try again.")
}

// assertEphemeralFollowup checks that a follow-up is ephemeral and shows
// the given text.
func assertEphemeralFollowup(t *testing.T, p askUserPost, want string) {
	t.Helper()
	var sent struct {
		Content string `json:"content"`
		Flags   int    `json:"flags"`
	}
	require.NoError(t, json.Unmarshal(p.body, &sent))
	assert.Equal(t, int(discordgo.MessageFlagsEphemeral), sent.Flags)
	assert.Equal(t, 64, sent.Flags)
	assert.Equal(t, want, sent.Content)
}

// TestAskUser_ChoiceButtonLimits checks how choices that do not fit
// Discord's button limits are posted and stored.
func TestAskUser_ChoiceButtonLimits(t *testing.T) {
	long := strings.Repeat("é", 100)
	many := make([]string, 26)
	for i := range many {
		many[i] = fmt.Sprintf("c%d", i)
	}
	manyJSON, err := json.Marshal(many)
	require.NoError(t, err)

	tests := []struct {
		name        string
		choices     string
		wantLabels  []string
		wantChoices []string
	}{
		{
			name:        "long label is truncated, full text stored",
			choices:     `["short","` + long + `"]`,
			wantLabels:  []string{"short", strings.Repeat("é", 79) + "…"},
			wantChoices: []string{"short", long},
		},
		{
			name:       "blank label gives Reply and Dismiss",
			choices:    `["yes","  "]`,
			wantLabels: []string{"Reply", "Dismiss"},
		},
		{
			name:       "26 choices give Reply and Dismiss",
			choices:    string(manyJSON),
			wantLabels: []string{"Reply", "Dismiss"},
		},
		{
			name:        "25 choices keep one button each",
			choices:     `["a","b","c","d","e","f","g","h","i","j","k","l","m","n","o","p","q","r","s","t","u","v","w","x","y"]`,
			wantLabels:  strings.Split("a b c d e f g h i j k l m n o p q r s t u v w x y", " "),
			wantChoices: strings.Split("a b c d e f g h i j k l m n o p q r s t u v w x y", " "),
		},
		{
			name:       "unparseable choices give Reply and Dismiss",
			choices:    `not json`,
			wantLabels: []string{"Reply", "Dismiss"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			const channelID = "chan-ask"
			b, rt, _, _ := newAskUserFixture(t, channelID)

			require.NoError(t, b.Publish(ctx, projectkeys.UserTopic("proj-1", "alice"),
				askUserQuestion("Pick one", tt.choices)))

			buttons := rt.postedButtons(t, channelID)
			var labels []string
			for _, btn := range buttons {
				labels = append(labels, btn.label)
				assert.LessOrEqual(t, utf8.RuneCountInString(btn.label), 80)
			}
			assert.Equal(t, tt.wantLabels, labels)

			requestID := strings.Split(buttons[0].customID, ":")[2]
			pending, err := b.store.GetPendingAskUser(ctx, requestID)
			require.NoError(t, err)
			require.NotNil(t, pending)
			assert.Equal(t, tt.wantChoices, pending.Choices)

			// Clicking a truncated button delivers the full choice.
			for idx, want := range tt.wantChoices {
				if want == tt.wantLabels[idx] {
					continue
				}
				var got []*messages.StructuredMessage
				deliver := func(_ string, msg *messages.StructuredMessage) *hubError {
					got = append(got, msg)
					return nil
				}
				h := NewCallbackHandler(b.store, b.session, nil, deliver, discardLogger())
				id := buttons[idx].customID
				h.Dispatch(b.session, askUserClick(channelID, id), id, nil)
				require.Len(t, got, 1)
				assert.Equal(t, want, got[0].Msg)
				assert.Equal(t, 100, utf8.RuneCountInString(got[0].Msg))
			}
		})
	}
}

// TestAskUser_LongChoiceEditFits checks that answering a very long choice
// keeps the edit within Discord's message limit while the delivered
// answer carries the full choice.
func TestAskUser_LongChoiceEditFits(t *testing.T) {
	ctx := context.Background()
	const channelID = "chan-ask"
	b, rt, delivered, deliver := newAskUserFixture(t, channelID)

	long := strings.Repeat("é", 3000)
	require.NoError(t, b.Publish(ctx, projectkeys.UserTopic("proj-1", "alice"),
		askUserQuestion("Pick one", `["short","`+long+`"]`)))
	ids := rt.postedCustomIDs(t, channelID)
	require.Len(t, ids, 2)

	h := NewCallbackHandler(b.store, b.session, nil, deliver, discardLogger())
	h.Dispatch(b.session, askUserClick(channelID, ids[1]), ids[1], nil)

	require.Len(t, *delivered, 1)
	assert.Equal(t, long, (*delivered)[0].msg.Msg, "the full choice should be delivered")

	edits, _ := rt.interactionCalls()
	require.Len(t, edits, 1)
	var sent struct {
		Content string `json:"content"`
	}
	require.NoError(t, json.Unmarshal(edits[0].body, &sent))
	assert.LessOrEqual(t, utf8.RuneCountInString(sent.Content), maxDiscordMessageLength)
	assert.True(t, strings.HasSuffix(sent.Content, "é…**"))
}

// TestFormatAskResponded checks that the edit shown after a choice is
// answered fits Discord's message limit.
func TestFormatAskResponded(t *testing.T) {
	assert.Equal(t, "✅ Responded: **yes**", formatAskResponded("yes"))

	got := formatAskResponded(strings.Repeat("é", 3000))
	assert.Equal(t, maxDiscordMessageLength, utf8.RuneCountInString(got))
	assert.True(t, strings.HasPrefix(got, "✅ Responded: **é"))
	assert.True(t, strings.HasSuffix(got, "é…**"))

	fits := strings.Repeat("a", maxDiscordMessageLength-utf8.RuneCountInString("✅ Responded: ****"))
	assert.Equal(t, "✅ Responded: **"+fits+"**", formatAskResponded(fits))
}

// TestAskUser_FallbackPostsPlainText checks that a question with no store
// or no sender slug is posted as plain text without buttons.
func TestAskUser_FallbackPostsPlainText(t *testing.T) {
	tests := []struct {
		name    string
		noStore bool
		sender  string
	}{
		{name: "no store", noStore: true, sender: "agent:coder"},
		{name: "no sender slug", sender: "user:bob@example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			const channelID = "chan-ask"
			b, rt, _, _ := newAskUserFixture(t, channelID)
			if tt.noStore {
				b.store = nil
			}
			msg := askUserQuestion("Deploy now?", `["yes","no"]`)
			msg.Sender = tt.sender

			require.NoError(t, b.Publish(ctx, projectkeys.UserTopic("proj-1", "alice"), msg))

			assert.Equal(t, 1, rt.channelPosts(channelID), "the question should be posted")
			assert.Empty(t, rt.postedButtons(t, channelID), "the question should have no buttons")
		})
	}
}
