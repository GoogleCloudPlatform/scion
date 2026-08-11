package chatapp

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/state"
)

// newTestRouter creates a CommandRouter backed by an ephemeral store and a
// fakeMessenger. The idMapper is nil because these tests exercise code paths
// that check the space link before reaching identity resolution.
func newTestRouter(t *testing.T) (*CommandRouter, *fakeMessenger) {
	t.Helper()
	store := newTestStore(t)
	fm := &fakeMessenger{}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	router := &CommandRouter{
		store:          store,
		messenger:      fm,
		log:            log,
		pendingAuth:    make(map[string]*pendingDeviceAuth),
		pendingDeletes: make(map[string]string),
	}
	return router, fm
}

// TestHandleEvent_CommandRouting verifies that /scion routes to messaging
// and /scionAdmin routes to admin command handling.
func TestHandleEvent_CommandRouting(t *testing.T) {
	router, _ := newTestRouter(t)

	tests := []struct {
		name        string
		command     string
		args        string
		wantContain string
	}{
		{
			name:        "scion with no args shows messaging help",
			command:     "scion",
			args:        "",
			wantContain: "Message Agents",
		},
		{
			name:        "scion help shows messaging help",
			command:     "scion",
			args:        "help",
			wantContain: "Message Agents",
		},
		{
			name:        "scionAdmin with no args shows admin help",
			command:     "scionAdmin",
			args:        "",
			wantContain: "Admin Commands",
		},
		{
			name:        "scionAdmin help shows admin help",
			command:     "scionAdmin",
			args:        "help",
			wantContain: "Admin Commands",
		},
		{
			name:        "scionAdmin unknown command",
			command:     "scionAdmin",
			args:        "bogus",
			wantContain: "Unknown command",
		},
		{
			name:        "scion help with extra args falls through to messaging",
			command:     "scion",
			args:        "help me understand X",
			wantContain: "not linked",
		},
		{
			name:        "scionAdmin help with extra args returns unknown command",
			command:     "scionAdmin",
			args:        "help something",
			wantContain: "Unknown command",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := &ChatEvent{
				Type:     EventCommand,
				Platform: "googlechat",
				SpaceID:  "spaces/test",
				UserID:   "user-1",
				Command:  tt.command,
				Args:     tt.args,
			}
			resp, err := router.HandleEvent(context.Background(), event)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp == nil || resp.Message == nil {
				t.Fatal("expected a response")
			}
			if !strings.Contains(resp.Message.Text, tt.wantContain) {
				t.Errorf("expected response to contain %q, got: %s", tt.wantContain, resp.Message.Text)
			}
		})
	}
}

// TestCmdStart_RequiresSpaceLink verifies that /scion start now requires a
// space link (grove context) before attempting to start an agent.
func TestCmdStart_RequiresSpaceLink(t *testing.T) {
	router, _ := newTestRouter(t)
	event := &ChatEvent{
		Type:     EventCommand,
		Platform: "googlechat",
		SpaceID:  "spaces/unlinked",
		UserID:   "user-1",
	}

	resp, err := router.cmdStart(context.Background(), event, []string{"deploy"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.Message == nil {
		t.Fatal("expected a response")
	}
	if !strings.Contains(resp.Message.Text, "not linked") {
		t.Errorf("expected 'not linked' message, got: %s", resp.Message.Text)
	}
}

// TestCmdStop_RequiresSpaceLink verifies that /scion stop now requires a
// space link (grove context) before attempting to stop an agent.
func TestCmdStop_RequiresSpaceLink(t *testing.T) {
	router, _ := newTestRouter(t)
	event := &ChatEvent{
		Type:     EventCommand,
		Platform: "googlechat",
		SpaceID:  "spaces/unlinked",
		UserID:   "user-1",
	}

	resp, err := router.cmdStop(context.Background(), event, []string{"deploy"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.Message == nil {
		t.Fatal("expected a response")
	}
	if !strings.Contains(resp.Message.Text, "not linked") {
		t.Errorf("expected 'not linked' message, got: %s", resp.Message.Text)
	}
}

// TestCmdUnsubscribe_RequiresSpaceLink verifies that /scion unsubscribe now
// requires a space link to scope the deletion to the correct grove.
func TestCmdUnsubscribe_RequiresSpaceLink(t *testing.T) {
	router, _ := newTestRouter(t)
	event := &ChatEvent{
		Type:     EventCommand,
		Platform: "googlechat",
		SpaceID:  "spaces/unlinked",
		UserID:   "user-1",
	}

	resp, err := router.cmdUnsubscribe(context.Background(), event, []string{"deploy"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp == nil || resp.Message == nil {
		t.Fatal("expected a response")
	}
	if !strings.Contains(resp.Message.Text, "not linked") {
		t.Errorf("expected 'not linked' message, got: %s", resp.Message.Text)
	}
}

// TestHandleAgentAction_RequiresSpaceLink verifies that agent button actions
// (start, stop, logs) now require a space link for grove scoping.
func TestHandleAgentAction_RequiresSpaceLink(t *testing.T) {
	router, fm := newTestRouter(t)
	event := &ChatEvent{
		Type:     EventAction,
		Platform: "googlechat",
		SpaceID:  "spaces/unlinked",
		UserID:   "user-1",
	}

	for _, verb := range []string{"start", "stop", "logs"} {
		t.Run(verb, func(t *testing.T) {
			fm.messages = nil
			err := router.handleAgentAction(context.Background(), event, verb, "agent-123")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(fm.messages) == 0 {
				t.Fatal("expected a reply message")
			}
			if !strings.Contains(fm.messages[0].Text, "not linked") {
				t.Errorf("expected 'not linked' reply, got: %s", fm.messages[0].Text)
			}
		})
	}
}

// TestExecuteDelete_RequiresSpaceLink verifies that the delete confirmation
// handler requires a space link for grove scoping.
func TestExecuteDelete_RequiresSpaceLink(t *testing.T) {
	router, fm := newTestRouter(t)
	event := &ChatEvent{
		Type:     EventAction,
		Platform: "googlechat",
		SpaceID:  "spaces/unlinked",
		UserID:   "user-1",
	}

	err := router.executeDelete(context.Background(), event, "agent-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fm.messages) == 0 {
		t.Fatal("expected a reply message")
	}
	if !strings.Contains(fm.messages[0].Text, "not linked") {
		t.Errorf("expected 'not linked' reply, got: %s", fm.messages[0].Text)
	}
}

// TestDialogSubmitRespond_RequiresSpaceLink verifies that the agent.respond
// dialog handler requires a space link for grove scoping.
func TestDialogSubmitRespond_RequiresSpaceLink(t *testing.T) {
	router, fm := newTestRouter(t)
	event := &ChatEvent{
		Type:     EventDialogSubmit,
		Platform: "googlechat",
		SpaceID:  "spaces/unlinked",
		UserID:   "user-1",
		ActionID: "agent.respond.agent-123",
		DialogData: map[string]string{
			"response": "yes, proceed",
		},
	}

	err := router.handleDialogSubmit(context.Background(), event)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fm.messages) == 0 {
		t.Fatal("expected a reply message")
	}
	if !strings.Contains(fm.messages[0].Text, "not linked") {
		t.Errorf("expected 'not linked' reply, got: %s", fm.messages[0].Text)
	}
}

// --- Thread Default Routing Tests ---

func TestResolveDefaultAgent_ThreadDefaultTakesPrecedence(t *testing.T) {
	router, _ := newTestRouter(t)

	// Set up a space link with a space-level default.
	router.store.SetSpaceLink(&state.SpaceLink{
		SpaceID:      "spaces/test",
		Platform:     "googlechat",
		ProjectID:    "proj-1",
		ProjectSlug:  "my-project",
		LinkedBy:     "user-1",
		DefaultAgent: "space-agent",
	})
	// Set a thread-level default.
	router.store.SetThreadDefault("spaces/test", "thread-1", "googlechat", "thread-agent", "user@example.com")

	agent, err := router.resolveDefaultAgent("spaces/test", "thread-1", "googlechat", "space-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agent != "thread-agent" {
		t.Errorf("expected thread-agent, got %q", agent)
	}
}

func TestResolveDefaultAgent_FallsBackToSpaceDefault(t *testing.T) {
	router, _ := newTestRouter(t)

	// Space link with default, no thread default set.
	router.store.SetSpaceLink(&state.SpaceLink{
		SpaceID:      "spaces/test",
		Platform:     "googlechat",
		ProjectID:    "proj-1",
		ProjectSlug:  "my-project",
		LinkedBy:     "user-1",
		DefaultAgent: "space-agent",
	})

	agent, err := router.resolveDefaultAgent("spaces/test", "thread-1", "googlechat", "space-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agent != "space-agent" {
		t.Errorf("expected space-agent, got %q", agent)
	}
}

func TestResolveDefaultAgent_EmptyWhenNeitherSet(t *testing.T) {
	router, _ := newTestRouter(t)

	agent, err := router.resolveDefaultAgent("spaces/test", "thread-1", "googlechat", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agent != "" {
		t.Errorf("expected empty, got %q", agent)
	}
}

func TestResolveDefaultAgent_EmptyThreadIDSkipsLookup(t *testing.T) {
	router, _ := newTestRouter(t)

	// Even with a thread default set, passing empty threadID should skip it.
	router.store.SetThreadDefault("spaces/test", "thread-1", "googlechat", "thread-agent", "u")

	agent, err := router.resolveDefaultAgent("spaces/test", "", "googlechat", "space-agent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if agent != "space-agent" {
		t.Errorf("expected space-agent when threadID is empty, got %q", agent)
	}
}

func TestCmdSetDefault_ThreadFlag(t *testing.T) {
	router, _ := newTestRouter(t)

	// Link the space first.
	router.store.SetSpaceLink(&state.SpaceLink{
		SpaceID:     "spaces/test",
		Platform:    "googlechat",
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		LinkedBy:    "user-1",
	})

	tests := []struct {
		name        string
		args        []string
		threadID    string
		wantContain string
	}{
		{
			name:        "thread flag without thread context",
			args:        []string{"my-agent", "--thread"},
			threadID:    "",
			wantContain: "only be used inside a thread",
		},
		{
			name:        "thread flag with clear",
			args:        []string{"clear", "--thread"},
			threadID:    "thread-1",
			wantContain: "Thread-level default agent cleared",
		},
		{
			name:        "query thread default when none set",
			args:        []string{"--thread"},
			threadID:    "thread-1",
			wantContain: "No thread-level default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := &ChatEvent{
				Type:     EventCommand,
				Platform: "googlechat",
				SpaceID:  "spaces/test",
				UserID:   "user-1",
				ThreadID: tt.threadID,
			}

			resp, err := router.cmdSetDefault(context.Background(), event, tt.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp == nil || resp.Message == nil {
				t.Fatal("expected a response")
			}
			if !strings.Contains(resp.Message.Text, tt.wantContain) {
				t.Errorf("expected response to contain %q, got: %s", tt.wantContain, resp.Message.Text)
			}
		})
	}
}

func TestCmdSetDefault_ThreadQueryShowsCurrentDefault(t *testing.T) {
	router, _ := newTestRouter(t)

	router.store.SetSpaceLink(&state.SpaceLink{
		SpaceID:     "spaces/test",
		Platform:    "googlechat",
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		LinkedBy:    "user-1",
	})
	router.store.SetThreadDefault("spaces/test", "thread-1", "googlechat", "my-agent", "u")

	event := &ChatEvent{
		Type:     EventCommand,
		Platform: "googlechat",
		SpaceID:  "spaces/test",
		UserID:   "user-1",
		ThreadID: "thread-1",
	}

	resp, err := router.cmdSetDefault(context.Background(), event, []string{"--thread"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Message.Text, "my-agent") {
		t.Errorf("expected response to mention my-agent, got: %s", resp.Message.Text)
	}
}

func TestHandleSpaceRemove_CleansUpThreadDefaults(t *testing.T) {
	router, _ := newTestRouter(t)

	// Set up space link and thread defaults.
	router.store.SetSpaceLink(&state.SpaceLink{
		SpaceID:     "spaces/test",
		Platform:    "googlechat",
		ProjectID:   "proj-1",
		ProjectSlug: "my-project",
		LinkedBy:    "user-1",
	})
	router.store.SetThreadDefault("spaces/test", "thread-1", "googlechat", "agent-a", "u")
	router.store.SetThreadDefault("spaces/test", "thread-2", "googlechat", "agent-b", "u")

	event := &ChatEvent{
		Type:     EventSpaceRemove,
		Platform: "googlechat",
		SpaceID:  "spaces/test",
		UserID:   "user-1",
	}

	if err := router.handleSpaceRemove(context.Background(), event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify thread defaults are cleaned up.
	got1, _ := router.store.GetThreadDefault("spaces/test", "thread-1", "googlechat")
	got2, _ := router.store.GetThreadDefault("spaces/test", "thread-2", "googlechat")
	if got1 != "" || got2 != "" {
		t.Errorf("thread defaults should be cleaned up after space removal, got thread-1=%q, thread-2=%q", got1, got2)
	}
}
