package chatapp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// --- Stub implementations for hubclient interfaces ---

// stubAgentService implements hubclient.AgentService using function fields.
// Unimplemented methods panic; tests only call the methods they configure.
type stubAgentService struct {
	hubclient.AgentService // embed for compile-time interface satisfaction

	getFunc                   func(ctx context.Context, id string) (*hubclient.Agent, error)
	createFunc                func(ctx context.Context, req *hubclient.CreateAgentRequest) (*hubclient.CreateAgentResponse, error)
	startFunc                 func(ctx context.Context, id string) error
	sendStructuredMessageFunc func(ctx context.Context, id string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error)
}

func (s *stubAgentService) Get(ctx context.Context, id string) (*hubclient.Agent, error) {
	if s.getFunc != nil {
		return s.getFunc(ctx, id)
	}
	return nil, fmt.Errorf("agent not found")
}

func (s *stubAgentService) Create(ctx context.Context, req *hubclient.CreateAgentRequest) (*hubclient.CreateAgentResponse, error) {
	if s.createFunc != nil {
		return s.createFunc(ctx, req)
	}
	return nil, fmt.Errorf("not implemented")
}

func (s *stubAgentService) Start(ctx context.Context, id string) error {
	if s.startFunc != nil {
		return s.startFunc(ctx, id)
	}
	return nil
}

func (s *stubAgentService) SendStructuredMessage(ctx context.Context, id string, msg *messages.StructuredMessage, interrupt, notify, wake bool) (*hubclient.MessageResponse, error) {
	if s.sendStructuredMessageFunc != nil {
		return s.sendStructuredMessageFunc(ctx, id, msg, interrupt, notify, wake)
	}
	return &hubclient.MessageResponse{}, nil
}

// stubSecretService implements hubclient.SecretService using function fields.
type stubSecretService struct {
	hubclient.SecretService // embed for compile-time interface satisfaction

	listFunc   func(ctx context.Context, opts *hubclient.ListSecretOptions) (*hubclient.ListSecretResponse, error)
	getFunc    func(ctx context.Context, key string, opts *hubclient.SecretScopeOptions) (*hubclient.Secret, error)
	setFunc    func(ctx context.Context, key string, req *hubclient.SetSecretRequest) (*hubclient.SetSecretResponse, error)
	deleteFunc func(ctx context.Context, key string, opts *hubclient.SecretScopeOptions) error
}

func (s *stubSecretService) List(ctx context.Context, opts *hubclient.ListSecretOptions) (*hubclient.ListSecretResponse, error) {
	if s.listFunc != nil {
		return s.listFunc(ctx, opts)
	}
	return &hubclient.ListSecretResponse{}, nil
}

func (s *stubSecretService) Get(ctx context.Context, key string, opts *hubclient.SecretScopeOptions) (*hubclient.Secret, error) {
	if s.getFunc != nil {
		return s.getFunc(ctx, key, opts)
	}
	return nil, fmt.Errorf("secret not found")
}

func (s *stubSecretService) Set(ctx context.Context, key string, req *hubclient.SetSecretRequest) (*hubclient.SetSecretResponse, error) {
	if s.setFunc != nil {
		return s.setFunc(ctx, key, req)
	}
	return &hubclient.SetSecretResponse{}, nil
}

func (s *stubSecretService) Delete(ctx context.Context, key string, opts *hubclient.SecretScopeOptions) error {
	if s.deleteFunc != nil {
		return s.deleteFunc(ctx, key, opts)
	}
	return nil
}

// stubClient implements hubclient.Client, delegating to stub services.
type stubClient struct {
	hubclient.Client // embed for compile-time interface satisfaction

	agents  *stubAgentService
	secrets *stubSecretService
}

func (c *stubClient) ProjectAgents(projectID string) hubclient.AgentService {
	return c.agents
}

func (c *stubClient) Secrets() hubclient.SecretService {
	return c.secrets
}

// newStubClient creates a stubClient with default (empty) services.
func newStubClient() *stubClient {
	return &stubClient{
		agents:  &stubAgentService{},
		secrets: &stubSecretService{},
	}
}

// newTestRouterWithHub creates a CommandRouter with a stub hub client and a
// pre-linked space, suitable for testing commands that require hub API access.
func newTestRouterWithHub(t *testing.T, client *stubClient) (*CommandRouter, *fakeMessenger, *state.Store) {
	t.Helper()
	store := newTestStore(t)
	fm := &fakeMessenger{}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Pre-link a space to a project.
	if err := store.SetSpaceLink(&state.SpaceLink{
		SpaceID:     "spaces/test",
		Platform:    "googlechat",
		ProjectID:   "proj-123",
		ProjectSlug: "test-project",
	}); err != nil {
		t.Fatalf("setting space link: %v", err)
	}

	router := &CommandRouter{
		hubURL:         "https://hub.example.com",
		store:          store,
		messenger:      fm,
		log:            log,
		pendingAuth:    make(map[string]*pendingDeviceAuth),
		pendingDeletes: make(map[string]string),
		testClient:     client,
	}
	return router, fm, store
}

func testEvent() *ChatEvent {
	return &ChatEvent{
		Type:      EventCommand,
		Platform:  "googlechat",
		SpaceID:   "spaces/test",
		UserID:    "user-1",
		UserEmail: "user@example.com",
		Command:   "scionAdmin",
	}
}

// --- cmdTerminal tests ---

func TestCmdTerminal_NoArgs(t *testing.T) {
	client := newStubClient()
	router, _, _ := newTestRouterWithHub(t, client)
	resp, err := router.cmdTerminal(context.Background(), testEvent(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Message.Text, "Usage") {
		t.Errorf("expected usage message, got: %s", resp.Message.Text)
	}
}

func TestCmdTerminal_AgentNotFound(t *testing.T) {
	client := newStubClient()
	client.agents.getFunc = func(_ context.Context, id string) (*hubclient.Agent, error) {
		return nil, fmt.Errorf("not found")
	}
	router, _, _ := newTestRouterWithHub(t, client)

	resp, err := router.cmdTerminal(context.Background(), testEvent(), []string{"missing-agent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Message.Text, "not found") {
		t.Errorf("expected 'not found' message, got: %s", resp.Message.Text)
	}
}

func TestCmdTerminal_AgentNotRunning(t *testing.T) {
	client := newStubClient()
	client.agents.getFunc = func(_ context.Context, id string) (*hubclient.Agent, error) {
		return &hubclient.Agent{
			ID:    "agent-id-1",
			Slug:  "my-agent",
			Phase: "stopped",
		}, nil
	}
	router, _, _ := newTestRouterWithHub(t, client)

	resp, err := router.cmdTerminal(context.Background(), testEvent(), []string{"my-agent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Message.Text, "not running") {
		t.Errorf("expected 'not running' message, got: %s", resp.Message.Text)
	}
}

func TestCmdTerminal_AgentRunning(t *testing.T) {
	client := newStubClient()
	client.agents.getFunc = func(_ context.Context, id string) (*hubclient.Agent, error) {
		return &hubclient.Agent{
			ID:    "agent-id-1",
			Slug:  "my-agent",
			Phase: "running",
		}, nil
	}
	router, _, _ := newTestRouterWithHub(t, client)

	resp, err := router.cmdTerminal(context.Background(), testEvent(), []string{"my-agent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Message.Card == nil {
		t.Fatal("expected card response")
	}
	if resp.Message.Card.Header.Title != "Web Terminal" {
		t.Errorf("expected 'Web Terminal' card title, got: %s", resp.Message.Card.Header.Title)
	}
	// Verify the action uses link. prefix for openLink rendering.
	if len(resp.Message.Card.Actions) == 0 {
		t.Fatal("expected card actions")
	}
	action := resp.Message.Card.Actions[0]
	if !strings.HasPrefix(action.ActionID, "link.") {
		t.Errorf("expected ActionID to start with 'link.', got: %s", action.ActionID)
	}
	if !strings.Contains(action.ActionID, "agent-id-1/terminal") {
		t.Errorf("expected terminal URL in ActionID, got: %s", action.ActionID)
	}
}

// --- cmdThread tests ---

func TestCmdThread_NoArgs(t *testing.T) {
	client := newStubClient()
	router, _, _ := newTestRouterWithHub(t, client)
	resp, err := router.cmdThread(context.Background(), testEvent(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Message.Text, "Usage") {
		t.Errorf("expected usage message, got: %s", resp.Message.Text)
	}
}

func TestCmdThread_SuccessWithoutInstruction(t *testing.T) {
	client := newStubClient()
	client.agents.createFunc = func(_ context.Context, req *hubclient.CreateAgentRequest) (*hubclient.CreateAgentResponse, error) {
		return &hubclient.CreateAgentResponse{
			Agent: &hubclient.Agent{
				ID:   "agent-id-new",
				Slug: req.Name,
			},
		}, nil
	}
	router, fm, _ := newTestRouterWithHub(t, client)

	resp, err := router.cmdThread(context.Background(), testEvent(), []string{"my-new-agent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Message.Card == nil {
		t.Fatal("expected card response")
	}
	if resp.Message.Card.Header.Title != "Thread Created" {
		t.Errorf("expected 'Thread Created' card title, got: %s", resp.Message.Card.Header.Title)
	}
	// Verify the kickoff message was sent with a threadKey.
	if len(fm.messages) == 0 {
		t.Fatal("expected messenger to have sent a kickoff message")
	}
	kickoff := fm.messages[0]
	if kickoff.ThreadKey == "" {
		t.Error("expected kickoff message to have a ThreadKey for new thread creation")
	}
	if !strings.Contains(kickoff.ThreadKey, "my-new-agent") {
		t.Errorf("expected threadKey to contain agent slug, got: %s", kickoff.ThreadKey)
	}
}

func TestCmdThread_SuccessWithInstruction(t *testing.T) {
	client := newStubClient()
	var sentMessage *messages.StructuredMessage
	client.agents.createFunc = func(_ context.Context, req *hubclient.CreateAgentRequest) (*hubclient.CreateAgentResponse, error) {
		return &hubclient.CreateAgentResponse{
			Agent: &hubclient.Agent{
				ID:   "agent-id-new",
				Slug: req.Name,
			},
		}, nil
	}
	client.agents.sendStructuredMessageFunc = func(_ context.Context, _ string, msg *messages.StructuredMessage, _, _, _ bool) (*hubclient.MessageResponse, error) {
		sentMessage = msg
		return &hubclient.MessageResponse{}, nil
	}
	router, fm, _ := newTestRouterWithHub(t, client)

	resp, err := router.cmdThread(context.Background(), testEvent(), []string{"my-agent", "start", "task", "X"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Message.Card == nil {
		t.Fatal("expected card response")
	}
	// Verify instruction was sent.
	if sentMessage == nil {
		t.Fatal("expected instruction to be sent to agent")
	}
	if sentMessage.Msg != "start task X" {
		t.Errorf("expected instruction 'start task X', got: %s", sentMessage.Msg)
	}
	// Verify the kickoff message includes the instruction.
	if len(fm.messages) == 0 {
		t.Fatal("expected messenger to have sent a kickoff message")
	}
	if !strings.Contains(fm.messages[0].Text, "start task X") {
		t.Errorf("expected kickoff message to include instruction, got: %s", fm.messages[0].Text)
	}
	// Verify card includes instruction widget.
	found := false
	for _, w := range resp.Message.Card.Sections[0].Widgets {
		if w.Label == "Instruction" && w.Content == "start task X" {
			found = true
		}
	}
	if !found {
		t.Error("expected card to include Instruction widget")
	}
}

// --- cmdSend tests ---

func TestCmdSend_NoArgs(t *testing.T) {
	client := newStubClient()
	router, _, _ := newTestRouterWithHub(t, client)
	resp, err := router.cmdSend(context.Background(), testEvent(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Message.Text, "Usage") {
		t.Errorf("expected usage message, got: %s", resp.Message.Text)
	}
}

func TestCmdSend_AgentNotFound(t *testing.T) {
	client := newStubClient()
	client.agents.getFunc = func(_ context.Context, id string) (*hubclient.Agent, error) {
		return nil, fmt.Errorf("not found")
	}
	router, _, _ := newTestRouterWithHub(t, client)

	resp, err := router.cmdSend(context.Background(), testEvent(), []string{"missing-agent", "/path/to/file"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Message.Text, "not found") {
		t.Errorf("expected 'not found' message, got: %s", resp.Message.Text)
	}
}

func TestCmdSend_AgentFoundShowsFileInfo(t *testing.T) {
	client := newStubClient()
	client.agents.getFunc = func(_ context.Context, id string) (*hubclient.Agent, error) {
		return &hubclient.Agent{
			ID:   "agent-id-1",
			Slug: "my-agent",
		}, nil
	}
	router, _, _ := newTestRouterWithHub(t, client)

	resp, err := router.cmdSend(context.Background(), testEvent(), []string{"my-agent", "/path/to/file.txt"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Message.Card == nil {
		t.Fatal("expected card response")
	}
	if resp.Message.Card.Header.Title != "Send File" {
		t.Errorf("expected 'Send File' card title, got: %s", resp.Message.Card.Header.Title)
	}
	// Verify file path is shown.
	found := false
	for _, w := range resp.Message.Card.Sections[0].Widgets {
		if w.Label == "File Path" && w.Content == "/path/to/file.txt" {
			found = true
		}
	}
	if !found {
		t.Error("expected card to show file path")
	}
}

// --- cmdSecret tests ---

func TestCmdSecret_NoArgs(t *testing.T) {
	client := newStubClient()
	router, _, _ := newTestRouterWithHub(t, client)
	resp, err := router.cmdSecret(context.Background(), testEvent(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Message.Text, "Usage") {
		t.Errorf("expected usage message, got: %s", resp.Message.Text)
	}
}

func TestCmdSecretList_Empty(t *testing.T) {
	client := newStubClient()
	client.secrets.listFunc = func(_ context.Context, opts *hubclient.ListSecretOptions) (*hubclient.ListSecretResponse, error) {
		return &hubclient.ListSecretResponse{Secrets: nil}, nil
	}
	router, _, _ := newTestRouterWithHub(t, client)

	link := &state.SpaceLink{ProjectID: "proj-123", ProjectSlug: "test-project"}
	resp, err := router.cmdSecretList(context.Background(), testEvent(), link, client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Message.Text, "No secrets found") {
		t.Errorf("expected 'No secrets found' message, got: %s", resp.Message.Text)
	}
}

func TestCmdSecretList_WithSecrets(t *testing.T) {
	client := newStubClient()
	client.secrets.listFunc = func(_ context.Context, opts *hubclient.ListSecretOptions) (*hubclient.ListSecretResponse, error) {
		return &hubclient.ListSecretResponse{
			Secrets: []hubclient.Secret{
				{Key: "API_KEY", SecretType: "environment"},
				{Key: "DB_PASS", Description: "Database password"},
			},
		}, nil
	}
	router, _, _ := newTestRouterWithHub(t, client)

	link := &state.SpaceLink{ProjectID: "proj-123", ProjectSlug: "test-project"}
	resp, err := router.cmdSecretList(context.Background(), testEvent(), link, client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Message.Text, "API_KEY") {
		t.Errorf("expected response to contain 'API_KEY', got: %s", resp.Message.Text)
	}
	if !strings.Contains(resp.Message.Text, "DB_PASS") {
		t.Errorf("expected response to contain 'DB_PASS', got: %s", resp.Message.Text)
	}
	if !strings.Contains(resp.Message.Text, "(2)") {
		t.Errorf("expected response to show count (2), got: %s", resp.Message.Text)
	}
}

func TestCmdSecretSet_ReturnsCardInput(t *testing.T) {
	client := newStubClient()
	router, _, _ := newTestRouterWithHub(t, client)

	link := &state.SpaceLink{ProjectID: "proj-123", ProjectSlug: "test-project"}
	resp, err := router.cmdSecretSet(context.Background(), testEvent(), link, client, []string{"MY_SECRET"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Message.Card == nil {
		t.Fatal("expected card response with input widget")
	}
	if resp.Message.Card.Header.Title != "Set Secret" {
		t.Errorf("expected 'Set Secret' card title, got: %s", resp.Message.Card.Header.Title)
	}
	// Even if extra args are provided, should still return card (no inline value).
	resp2, err := router.cmdSecretSet(context.Background(), testEvent(), link, client, []string{"MY_SECRET", "should-be-ignored"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp2.Message.Card == nil {
		t.Fatal("expected card response even when value args are provided (inline values are not supported)")
	}
}

func TestCmdSecretGet_Found(t *testing.T) {
	client := newStubClient()
	client.secrets.getFunc = func(_ context.Context, key string, _ *hubclient.SecretScopeOptions) (*hubclient.Secret, error) {
		return &hubclient.Secret{
			Key:        key,
			Scope:      "project",
			SecretType: "environment",
			Version:    3,
		}, nil
	}
	router, _, _ := newTestRouterWithHub(t, client)

	link := &state.SpaceLink{ProjectID: "proj-123", ProjectSlug: "test-project"}
	resp, err := router.cmdSecretGet(context.Background(), testEvent(), link, client, []string{"API_KEY"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Message.Card == nil {
		t.Fatal("expected card response")
	}
	if resp.Message.Card.Header.Title != "Secret Details" {
		t.Errorf("expected 'Secret Details' card title, got: %s", resp.Message.Card.Header.Title)
	}
}

func TestCmdSecretGet_NotFound(t *testing.T) {
	client := newStubClient()
	client.secrets.getFunc = func(_ context.Context, key string, _ *hubclient.SecretScopeOptions) (*hubclient.Secret, error) {
		return nil, fmt.Errorf("secret not found")
	}
	router, _, _ := newTestRouterWithHub(t, client)

	link := &state.SpaceLink{ProjectID: "proj-123", ProjectSlug: "test-project"}
	resp, err := router.cmdSecretGet(context.Background(), testEvent(), link, client, []string{"MISSING"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Message.Text, "Failed to get secret") {
		t.Errorf("expected error message, got: %s", resp.Message.Text)
	}
}

func TestCmdSecretDelete_Success(t *testing.T) {
	deleted := false
	client := newStubClient()
	client.secrets.deleteFunc = func(_ context.Context, key string, _ *hubclient.SecretScopeOptions) error {
		deleted = true
		return nil
	}
	router, _, _ := newTestRouterWithHub(t, client)

	link := &state.SpaceLink{ProjectID: "proj-123", ProjectSlug: "test-project"}
	resp, err := router.cmdSecretDelete(context.Background(), testEvent(), link, client, []string{"OLD_SECRET"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !deleted {
		t.Error("expected delete to have been called")
	}
	if !strings.Contains(resp.Message.Text, "has been deleted") {
		t.Errorf("expected success message, got: %s", resp.Message.Text)
	}
}

func TestCmdSecretDelete_InvalidKey(t *testing.T) {
	client := newStubClient()
	router, _, _ := newTestRouterWithHub(t, client)

	link := &state.SpaceLink{ProjectID: "proj-123", ProjectSlug: "test-project"}
	resp, err := router.cmdSecretDelete(context.Background(), testEvent(), link, client, []string{"bad key"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Message.Text, "Invalid key") {
		t.Errorf("expected invalid key error, got: %s", resp.Message.Text)
	}
}

// --- handleSecretAction tests ---

func TestHandleSecretAction_SpecificKeyLookup(t *testing.T) {
	setKey := ""
	setValue := ""
	client := newStubClient()
	client.secrets.setFunc = func(_ context.Context, key string, req *hubclient.SetSecretRequest) (*hubclient.SetSecretResponse, error) {
		setKey = key
		setValue = req.Value
		return &hubclient.SetSecretResponse{}, nil
	}
	router, fm, _ := newTestRouterWithHub(t, client)

	event := &ChatEvent{
		Type:     EventAction,
		Platform: "googlechat",
		SpaceID:  "spaces/test",
		UserID:   "user-1",
		ActionID: "secret.set.MY_KEY",
		DialogData: map[string]string{
			"secret.set.MY_KEY": "my-secret-value",
			"unrelated_field":   "noise",
		},
	}

	err := router.handleSecretAction(context.Background(), event, "set", "MY_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if setKey != "MY_KEY" {
		t.Errorf("expected key 'MY_KEY', got: %s", setKey)
	}
	if setValue != "my-secret-value" {
		t.Errorf("expected value 'my-secret-value', got: %s", setValue)
	}
	// Verify success reply was sent.
	if len(fm.messages) == 0 {
		t.Fatal("expected a reply message")
	}
	if !strings.Contains(fm.messages[0].Text, "has been set") {
		t.Errorf("expected success reply, got: %s", fm.messages[0].Text)
	}
}

func TestHandleSecretAction_NoValue(t *testing.T) {
	client := newStubClient()
	router, fm, _ := newTestRouterWithHub(t, client)

	event := &ChatEvent{
		Type:       EventAction,
		Platform:   "googlechat",
		SpaceID:    "spaces/test",
		UserID:     "user-1",
		ActionID:   "secret.set.MY_KEY",
		DialogData: map[string]string{},
	}

	err := router.handleSecretAction(context.Background(), event, "set", "MY_KEY")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fm.messages) == 0 {
		t.Fatal("expected a reply message")
	}
	if !strings.Contains(fm.messages[0].Text, "No secret value") {
		t.Errorf("expected 'No secret value' reply, got: %s", fm.messages[0].Text)
	}
}

func TestCmdSecretList_TruncatedAt50(t *testing.T) {
	client := newStubClient()
	// Create 60 secrets — only first 50 should be shown.
	secrets := make([]hubclient.Secret, 60)
	for i := range secrets {
		secrets[i] = hubclient.Secret{Key: fmt.Sprintf("SECRET_%03d", i)}
	}
	client.secrets.listFunc = func(_ context.Context, opts *hubclient.ListSecretOptions) (*hubclient.ListSecretResponse, error) {
		return &hubclient.ListSecretResponse{Secrets: secrets}, nil
	}
	router, _, _ := newTestRouterWithHub(t, client)

	link := &state.SpaceLink{ProjectID: "proj-123", ProjectSlug: "test-project"}
	resp, err := router.cmdSecretList(context.Background(), testEvent(), link, client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	text := resp.Message.Text
	// Should contain the total count.
	if !strings.Contains(text, "(60)") {
		t.Errorf("expected total count (60) in output, got: %s", text)
	}
	// Should show SECRET_049 (last of the 50).
	if !strings.Contains(text, "SECRET_049") {
		t.Errorf("expected SECRET_049 in output (50th entry), got: %s", text)
	}
	// Should NOT show SECRET_050 (beyond cap).
	if strings.Contains(text, "SECRET_050") {
		t.Errorf("expected SECRET_050 to be truncated, but found it in output")
	}
	// Should show truncation notice.
	if !strings.Contains(text, "Showing 50 of 60") {
		t.Errorf("expected truncation notice 'Showing 50 of 60', got: %s", text)
	}
}

// --- validateSecretKey tests ---

func TestValidateSecretKey(t *testing.T) {
	tests := []struct {
		key     string
		wantErr bool
	}{
		{"API_KEY", false},
		{"MY-SECRET-123", false},
		{"", true},
		{"has space", true},
		{"has=equals", true},
		{"has:colon", true},
		{"has\ttab", true},
		{"has\nnewline", true},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			err := validateSecretKey(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSecretKey(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
			}
		})
	}
}

// --- Help text tests ---

func TestAdminHelp_IncludesNewCommands(t *testing.T) {
	client := newStubClient()
	router, _, _ := newTestRouterWithHub(t, client)
	resp, err := router.cmdAdminHelp(context.Background(), testEvent())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, cmd := range []string{"terminal", "thread", "send", "secret list", "secret set", "secret get", "secret delete"} {
		if !strings.Contains(resp.Message.Text, cmd) {
			t.Errorf("expected help text to contain %q", cmd)
		}
	}
}

// --- Command dispatch tests ---

func TestHandleAdminCommand_DispatchesNewCommands(t *testing.T) {
	client := newStubClient()
	router, _, _ := newTestRouterWithHub(t, client)

	tests := []struct {
		args        string
		wantContain string
	}{
		{"terminal", "Usage"},
		{"thread", "Usage"},
		{"send", "Usage"},
		{"secret", "Usage"},
		{"secret bogus", "Unknown secret subcommand"},
	}

	for _, tt := range tests {
		t.Run(tt.args, func(t *testing.T) {
			event := testEvent()
			event.Args = tt.args
			resp, err := router.handleAdminCommand(context.Background(), event)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp == nil || resp.Message == nil {
				t.Fatal("expected a response")
			}
			text := resp.Message.Text
			if resp.Message.Card != nil {
				text = resp.Message.Card.Header.Title
			}
			if !strings.Contains(text, tt.wantContain) {
				t.Errorf("expected response to contain %q, got text=%q", tt.wantContain, text)
			}
		})
	}
}
