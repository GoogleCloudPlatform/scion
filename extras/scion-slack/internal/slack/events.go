package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	slackapi "github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
)

var botMentionRe = regexp.MustCompile(`<@[A-Z0-9]+>\s*`)

// eventServer handles inbound Slack events and delivers them to the hub.
type eventServer struct {
	mu            sync.Mutex
	client        *slackapi.Client
	socketClient  *socketmode.Client
	signingSecret string
	httpServer    *http.Server
	socketCancel  context.CancelFunc
	log           *slog.Logger
	botUserID     string
	onBotUserID   func(string)

	store          Store
	hubClient      HubClient
	registration   *RegistrationHandler
	deliverInbound func(topic string, msg *messages.StructuredMessage) *hubError
}

// startHTTP begins listening for Slack events via HTTP webhooks.
func (s *eventServer) startHTTP(listenAddr string) error {
	if err := s.resolveBotUserID(); err != nil {
		s.log.Warn("failed to resolve bot user ID via auth.test", "error", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /slack/events", s.handleEvents)
	mux.HandleFunc("POST /slack/commands", s.handleCommands)
	mux.HandleFunc("POST /slack/interactions", s.handleInteractions)
	mux.HandleFunc("GET /slack/healthz", s.handleHealthz)

	srv := &http.Server{Addr: listenAddr, Handler: mux}
	s.mu.Lock()
	s.httpServer = srv
	s.mu.Unlock()
	return srv.ListenAndServe()
}

// startSocketMode begins listening via WebSocket (Socket Mode).
func (s *eventServer) startSocketMode() error {
	if s.socketClient == nil {
		return fmt.Errorf("socket mode not configured: app_token is required")
	}

	if err := s.resolveBotUserID(); err != nil {
		s.log.Warn("failed to resolve bot user ID via auth.test", "error", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.socketCancel = cancel
	s.mu.Unlock()

	go func() {
		for evt := range s.socketClient.Events {
			switch evt.Type {
			case socketmode.EventTypeEventsAPI:
				s.handleSocketEvent(evt)
			case socketmode.EventTypeSlashCommand:
				s.handleSocketCommand(evt)
			case socketmode.EventTypeInteractive:
				s.handleSocketInteraction(evt)
			}
		}
	}()

	return s.socketClient.RunContext(ctx)
}

func (s *eventServer) stop(ctx context.Context) error {
	s.mu.Lock()
	srv := s.httpServer
	cancel := s.socketCancel
	s.mu.Unlock()
	if srv != nil {
		return srv.Shutdown(ctx)
	}
	if cancel != nil {
		cancel()
	}
	return nil
}

func (s *eventServer) resolveBotUserID() error {
	resp, err := s.client.AuthTest()
	if err != nil {
		return err
	}
	s.botUserID = resp.UserID
	s.log.Info("resolved bot user ID", "bot_user_id", s.botUserID)
	if s.onBotUserID != nil {
		s.onBotUserID(s.botUserID)
	}
	return nil
}

// --- HTTP handlers ---

func (s *eventServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if s.signingSecret != "" {
		if err := verifyRequest(r, body, s.signingSecret); err != nil {
			s.log.Warn("event signature verification failed", "error", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	eventsAPIEvent, err := slackevents.ParseEvent(json.RawMessage(body), slackevents.OptionNoVerifyToken())
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if eventsAPIEvent.Type == slackevents.URLVerification {
		var challenge slackevents.ChallengeResponse
		if err := json.Unmarshal(body, &challenge); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(challenge.Challenge))
		return
	}

	if eventsAPIEvent.Type == slackevents.CallbackEvent {
		w.WriteHeader(http.StatusOK)
		go s.dispatchCallbackEvent(eventsAPIEvent)
	}
}

func (s *eventServer) handleCommands(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if s.signingSecret != "" {
		if err := verifyRequest(r, body, s.signingSecret); err != nil {
			s.log.Warn("command signature verification failed", "error", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(body))

	cmd, err := slackapi.SlashCommandParse(r)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"response_type": "ephemeral",
		"text":          "Processing...",
	})

	go s.handleSlashCommand(cmd)
}

func (s *eventServer) handleInteractions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1024*1024))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if s.signingSecret != "" {
		if err := verifyRequest(r, body, s.signingSecret); err != nil {
			s.log.Warn("interaction signature verification failed", "error", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	payload := r.FormValue("payload")
	if payload == "" {
		http.Error(w, "missing payload", http.StatusBadRequest)
		return
	}

	var callback slackapi.InteractionCallback
	if err := json.Unmarshal([]byte(payload), &callback); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusOK)
	go s.handleInteractionCallback(callback)
}

func (s *eventServer) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// --- Socket Mode handlers ---

func (s *eventServer) handleSocketEvent(evt socketmode.Event) {
	data, ok := evt.Data.(slackevents.EventsAPIEvent)
	if !ok {
		return
	}
	s.socketClient.Ack(*evt.Request)
	go s.dispatchCallbackEvent(data)
}

func (s *eventServer) handleSocketCommand(evt socketmode.Event) {
	cmd, ok := evt.Data.(slackapi.SlashCommand)
	if !ok {
		return
	}
	s.socketClient.Ack(*evt.Request)
	go s.handleSlashCommand(cmd)
}

func (s *eventServer) handleSocketInteraction(evt socketmode.Event) {
	callback, ok := evt.Data.(slackapi.InteractionCallback)
	if !ok {
		return
	}
	s.socketClient.Ack(*evt.Request)
	go s.handleInteractionCallback(callback)
}

// --- Event dispatch ---

func (s *eventServer) dispatchCallbackEvent(apiEvent slackevents.EventsAPIEvent) {
	innerEvent := apiEvent.InnerEvent

	switch ev := innerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		s.handleAppMention(ev)
	case *slackevents.MessageEvent:
		if ev.BotID != "" || ev.SubType != "" {
			return
		}
		s.handleDirectMessage(ev)
	default:
		s.log.Debug("unhandled event type", "type", innerEvent.Type)
	}
}

func (s *eventServer) handleAppMention(ev *slackevents.AppMentionEvent) {
	text := stripBotMention(ev.Text)
	if text == "" {
		return
	}

	threadID := ev.ThreadTimeStamp
	if threadID == "" {
		threadID = ev.TimeStamp
	}

	s.deliverUserMessage(ev.Channel, threadID, ev.User, text)
}

func (s *eventServer) handleDirectMessage(ev *slackevents.MessageEvent) {
	if ev.Text == "" {
		return
	}

	threadID := ev.ThreadTimeStamp
	if threadID == "" {
		threadID = ev.TimeStamp
	}

	s.deliverUserMessage(ev.Channel, threadID, ev.User, ev.Text)
}

func (s *eventServer) handleSlashCommand(cmd slackapi.SlashCommand) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	command := strings.TrimPrefix(cmd.Command, "/")
	args := cmd.Text

	s.log.Info("slash command received",
		"command", command,
		"args", args,
		"channel", cmd.ChannelID,
		"user", cmd.UserID,
	)

	HandleCommand(ctx, s.client, s.store, s.hubClient, s.registration, s.deliverInbound, cmd, s.log)
}

func (s *eventServer) handleInteractionCallback(callback slackapi.InteractionCallback) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch callback.Type {
	case slackapi.InteractionTypeBlockActions:
		if len(callback.ActionCallback.BlockActions) == 0 {
			return
		}
		action := callback.ActionCallback.BlockActions[0]
		s.log.Debug("block action received", "action_id", action.ActionID)
		HandleBlockAction(ctx, s.client, s.store, s.deliverInbound, callback, action, s.log)

	case slackapi.InteractionTypeViewSubmission:
		s.log.Debug("view submission received", "callback_id", callback.View.CallbackID)
		HandleViewSubmission(ctx, s.client, s.store, s.deliverInbound, callback, s.log)
	}
}

// --- Inbound message delivery ---

func (s *eventServer) deliverUserMessage(channelID, threadID, userID, text string) {
	if s.store == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	link, err := s.store.GetChannelLink(ctx, channelID)
	if err != nil {
		s.log.Error("Failed to get channel link", "channel_id", channelID, "error", err)
		return
	}
	if link == nil || !link.Active {
		return
	}

	mapping, err := s.store.GetUserMapping(ctx, userID)
	if err != nil {
		s.log.Error("Failed to get user mapping", "user_id", userID, "error", err)
		return
	}
	if mapping == nil {
		s.client.PostEphemeral(channelID, userID,
			slackapi.MsgOptionText("Please use `/scion register` first to interact with agents.", false))
		return
	}

	sender := "user:" + mapping.ScionEmail
	if mapping.ScionEmail == "" {
		sender = "slack:" + mapping.SlackUsername
	}

	agentSlug := link.DefaultAgent
	if agentSlug == "" {
		return
	}

	cc := &ConversationContext{
		SlackUserID:   userID,
		ProjectID:     link.ProjectID,
		AgentSlug:     agentSlug,
		LastChannelID: channelID,
		LastThreadTS:  threadID,
		LastMessageAt: time.Now(),
	}
	if err := s.store.SetConversationContext(ctx, cc); err != nil {
		s.log.Warn("Failed to save conversation context", "error", err)
	}

	topic := projectcompat.AgentTopic(link.ProjectID, agentSlug)
	recipient := "agent:" + agentSlug

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Channel:   "slack",
		ThreadID:  threadID,
		Sender:    sender,
		SenderID:  userID,
		Recipient: recipient,
		Msg:       text,
		Type:      messages.TypeInstruction,
		Metadata: map[string]string{
			"slack_channel_id": channelID,
			"slack_thread_ts":  threadID,
			"project_id":       link.ProjectID,
		},
	}

	if he := s.deliverInbound(topic, msg); he != nil {
		s.client.PostEphemeral(channelID, userID,
			slackapi.MsgOptionText(he.userFacingMessage(), false))
	}
}

func stripBotMention(text string) string {
	return strings.TrimSpace(botMentionRe.ReplaceAllString(text, ""))
}
