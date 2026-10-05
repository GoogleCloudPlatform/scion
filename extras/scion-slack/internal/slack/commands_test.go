package slack

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSlack records the ephemeral messages a handler posts.
type fakeSlack struct {
	mu        sync.Mutex
	ephemeral []url.Values
	server    *httptest.Server
}

func newFakeSlack(t *testing.T) *fakeSlack {
	t.Helper()
	f := &fakeSlack{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if strings.HasSuffix(r.URL.Path, "/chat.postEphemeral") {
			f.mu.Lock()
			f.ephemeral = append(f.ephemeral, r.Form)
			f.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"message_ts":"1.0"}`))
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeSlack) client() *slackapi.Client {
	return slackapi.New("xoxb-test", slackapi.OptionAPIURL(f.server.URL+"/"))
}

// lastText returns the text of the last ephemeral message, or, for a block
// message, the raw blocks JSON.
func (f *fakeSlack) lastText(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.ephemeral, "expected an ephemeral message")
	last := f.ephemeral[len(f.ephemeral)-1]
	if text := last.Get("text"); text != "" {
		return text
	}
	return last.Get("blocks")
}

// commandFixture wires a store, fake Slack and fake hub for slash commands.
type commandFixture struct {
	store Store
	slack *fakeSlack
	hub   *fakeHub
}

func newCommandFixture(t *testing.T) *commandFixture {
	t.Helper()
	return &commandFixture{store: newTestStore(t), slack: newFakeSlack(t), hub: newFakeHub(t)}
}

func (f *commandFixture) linkChannel(t *testing.T) {
	t.Helper()
	require.NoError(t, f.store.CreateChannelLink(context.Background(), &ChannelLink{
		ChannelID:   "C1",
		TeamID:      "T1",
		ProjectID:   "proj-1",
		ProjectSlug: "proj-one",
		LinkedBy:    "U1",
		LinkedAt:    time.Now(),
		Active:      true,
	}))
}

func (f *commandFixture) linkUser(t *testing.T, email string) {
	t.Helper()
	require.NoError(t, f.store.CreateUserMapping(context.Background(), &SlackUserMapping{
		SlackUserID:   "U1",
		SlackUsername: "alice",
		ScionUserID:   "uid-alice",
		ScionEmail:    email,
		LinkedAt:      time.Now(),
	}))
}

func (f *commandFixture) run(t *testing.T, text string) {
	t.Helper()
	HandleCommand(context.Background(), f.slack.client(), f.store, f.hub.client(), nil, nil,
		slackapi.SlashCommand{ChannelID: "C1", UserID: "U1", UserName: "alice", Text: text}, slog.Default())
}

func TestHandleAgents_RequestWithLinkedUser(t *testing.T) {
	f := newCommandFixture(t)
	f.linkChannel(t)
	f.linkUser(t, "alice@example.com")
	f.hub.on("GET", "/api/v1/projects/proj-1/agents", http.StatusOK, `{"agents":[{"slug":"alpha"}]}`)

	f.run(t, "agents")

	reqs := f.hub.recorded()
	require.Len(t, reqs, 1)
	assert.Equal(t, "user:alice@example.com", reqs[0].LinkedUser)
	assert.Contains(t, f.slack.lastText(t), "alpha")
}

func TestHandleAgents_WithoutLinkedAccountAsksToRegister(t *testing.T) {
	f := newCommandFixture(t)
	f.linkChannel(t)

	f.run(t, "agents")

	assert.Empty(t, f.hub.recorded(), "no hub request without a linked account")
	assert.Contains(t, f.slack.lastText(t), "/scion register")
}

func TestHandleStatus_RequestWithLinkedUser(t *testing.T) {
	f := newCommandFixture(t)
	f.linkChannel(t)
	f.linkUser(t, "alice@example.com")
	f.hub.on("GET", "/api/v1/projects/proj-1/agents", http.StatusOK, `{"agents":[{"slug":"alpha","activity":"working"}]}`)

	f.run(t, "status alpha")

	reqs := f.hub.recorded()
	require.Len(t, reqs, 1)
	assert.Equal(t, "user:alice@example.com", reqs[0].LinkedUser)
	assert.Contains(t, f.slack.lastText(t), "working")
}

func TestHandleSetup_ProjectReadWithLinkedUser(t *testing.T) {
	f := newCommandFixture(t)
	f.linkUser(t, "alice@example.com")
	f.hub.on("GET", "/api/v1/projects", http.StatusOK, `{"projects":[{"id":"p1","name":"Project One"}]}`)

	f.run(t, "setup")

	reqs := f.hub.recorded()
	require.NotEmpty(t, reqs)
	assert.Equal(t, "/api/v1/projects", reqs[0].Path)
	assert.Equal(t, "user:alice@example.com", reqs[0].LinkedUser)
	assert.Contains(t, f.slack.lastText(t), "Project One")
}
