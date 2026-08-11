// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package googlechat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/extras/scion-chat-app/internal/chatapp"
)

const (
	PlatformName  = "google_chat"
	chatAPIBase   = "https://chat.googleapis.com/v1"
	uploadAPIBase = "https://chat.googleapis.com/upload/v1"
)

// EventHandler processes normalized chat events and returns an optional synchronous response.
type EventHandler func(ctx context.Context, event *chatapp.ChatEvent) (*chatapp.EventResponse, error)

// Adapter implements the chatapp.Messenger interface for Google Chat.
type Adapter struct {
	config       Config
	projectID    string
	externalURL  string
	commandIDs   map[string]string // command ID → command name
	httpServer   *http.Server
	eventHandler EventHandler
	httpClient   *http.Client // authenticated client for Chat API calls
	log          *slog.Logger

	pubsubIngress *PubSubIngress
	pubsubCancel  context.CancelFunc

	mu     sync.RWMutex
	spaces map[string]bool // tracked spaces
}

// Config holds Google Chat adapter configuration.
type Config struct {
	ProjectID           string
	ExternalURL         string            // Public endpoint URL for action functions
	ServiceAccountEmail string            // Per-project SA for token verification
	CommandIDMap        map[string]string // Console command ID → command name
	ListenAddress       string
	Credentials         string // Path to service account key
	IngressMode         string // "http" (default) or "pubsub"
	PubSubSubscription  string // Pub/Sub subscription resource name
}

// NewAdapter creates a new Google Chat adapter.
func NewAdapter(cfg Config, handler EventHandler, httpClient *http.Client, log *slog.Logger) *Adapter {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	cmdIDs := cfg.CommandIDMap
	if cmdIDs == nil {
		cmdIDs = make(map[string]string)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Adapter{
		config:       cfg,
		projectID:    cfg.ProjectID,
		externalURL:  cfg.ExternalURL,
		commandIDs:   cmdIDs,
		eventHandler: handler,
		httpClient:   httpClient,
		log:          log,
		spaces:       make(map[string]bool),
	}
}

// Start begins serving events. When ingress_mode is "pubsub", events arrive
// via Cloud Pub/Sub. Otherwise the default HTTP webhook endpoint is used.
func (a *Adapter) Start(listenAddr string) error {
	mode := a.config.IngressMode
	switch mode {
	case "pubsub":
		return a.startPubSub(listenAddr)
	case "http", "":
		if a.config.PubSubSubscription != "" {
			a.log.Warn("pubsub_subscription is set but ingress_mode is not \"pubsub\"; subscription will be ignored")
		}
		return a.startHTTP(listenAddr)
	default:
		return fmt.Errorf("unknown ingress_mode %q: must be \"http\" or \"pubsub\"", mode)
	}
}

// startHTTP serves the HTTP webhook endpoint for Google Chat events.
func (a *Adapter) startHTTP(listenAddr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat/events", a.handleEvent)
	mux.HandleFunc("GET /chat/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	a.httpServer = &http.Server{
		Addr:    listenAddr,
		Handler: mux,
	}

	a.log.Info("google chat webhook server starting", "address", listenAddr)
	return a.httpServer.ListenAndServe()
}

// startPubSub creates a Pub/Sub client and starts the PubSubIngress.
// A minimal HTTP health server is started alongside the subscriber so that
// Kubernetes liveness/readiness probes have an endpoint to hit.
func (a *Adapter) startPubSub(listenAddr string) error {
	sub := a.config.PubSubSubscription
	if sub == "" {
		return fmt.Errorf("pubsub ingress mode requires pubsub_subscription to be set")
	}
	ingress, err := NewPubSubIngress(sub, a, a.config.Credentials, a.log)
	if err != nil {
		return fmt.Errorf("creating pubsub ingress: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	a.pubsubCancel = cancel
	a.pubsubIngress = ingress

	// Bind to the port synchronously to catch startup errors.
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		cancel()
		return fmt.Errorf("starting health server listener: %w", err)
	}

	// Start a minimal health endpoint for probes.
	a.startHealthServer(ln)

	a.log.Info("starting pubsub ingress", "subscription", sub)
	return ingress.Start(ctx)
}

// Stop gracefully shuts down the adapter, covering both HTTP and Pub/Sub modes.
func (a *Adapter) Stop(ctx context.Context) error {
	if a.pubsubCancel != nil {
		a.pubsubCancel()
	}
	if a.pubsubIngress != nil {
		if err := a.pubsubIngress.Stop(); err != nil {
			a.log.Error("stopping pubsub ingress", "error", err)
		}
	}
	if a.httpServer != nil {
		return a.httpServer.Shutdown(ctx)
	}
	return nil
}

// startHealthServer starts a minimal HTTP server exposing /healthz for
// liveness/readiness probes. It is used in Pub/Sub mode where no other
// HTTP server is running. The listener is pre-bound so that port conflicts
// are caught synchronously during startup.
func (a *Adapter) startHealthServer(ln net.Listener) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	a.httpServer = &http.Server{
		Handler: mux,
	}

	go func() {
		a.log.Info("health server starting (pubsub mode)", "address", ln.Addr().String())
		if err := a.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			a.log.Error("health server error", "error", err)
		}
	}()
}

// handleEvent processes incoming Google Chat Workspace Add-on events.
func (a *Adapter) handleEvent(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		a.log.Error("reading event body", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	a.log.Debug("raw event received", "body_len", len(body))

	var raw rawEvent
	if err := json.Unmarshal(body, &raw); err != nil {
		a.log.Error("parsing event", "error", err)
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	event := a.normalizeEvent(&raw)
	if event == nil {
		a.log.Debug("event normalized to nil, ignoring")
		w.WriteHeader(http.StatusOK)
		return
	}

	a.log.Info("event received",
		"type", event.Type,
		"space", event.SpaceID,
		"user", event.UserID,
		"command", event.Command,
		"args", event.Args,
		"action_id", event.ActionID,
		"is_dialog", event.IsDialogEvent,
	)

	resp, err := a.eventHandler(r.Context(), event)
	if err != nil {
		a.log.Error("handler error", "type", event.Type, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Serialize synchronous response if handler returned one.
	if resp != nil {
		payload := a.buildSyncResponse(resp)
		if payload != nil {
			a.log.Info("sending sync response", "type", event.Type, "has_message", resp.Message != nil, "has_dialog", resp.Dialog != nil, "close_dialog", resp.CloseDialog)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if err := json.NewEncoder(w).Encode(payload); err != nil {
				a.log.Error("encoding sync response", "error", err)
			}
			return
		}
	}

	a.log.Debug("sending empty 200 response", "type", event.Type)
	w.WriteHeader(http.StatusOK)
}

// buildSyncResponse converts an EventResponse to the Workspace Add-on JSON format.
func (a *Adapter) buildSyncResponse(resp *chatapp.EventResponse) map[string]any {
	if resp.Dialog != nil {
		return a.buildDialogResponse(resp.Dialog)
	}
	if resp.CloseDialog {
		result := map[string]any{
			"action": map[string]any{
				"navigations": []any{
					map[string]any{"endNavigation": "CLOSE_DIALOG"},
				},
			},
		}
		if resp.Notification != "" {
			result["action"].(map[string]any)["notification"] = map[string]any{
				"text": resp.Notification,
			}
		}
		return result
	}
	if resp.Message != nil {
		return a.buildMessageResponse("createMessageAction", resp.Message)
	}
	if resp.UpdateMessage != nil {
		return a.buildMessageResponse("updateMessageAction", resp.UpdateMessage)
	}
	return nil
}

// buildDialogResponse creates a pushCard navigation response for opening a dialog.
func (a *Adapter) buildDialogResponse(dialog *chatapp.Dialog) map[string]any {
	widgets := make([]any, 0, len(dialog.Fields))
	for _, f := range dialog.Fields {
		switch f.Type {
		case "text", "textarea":
			w := map[string]any{
				"label": f.Label,
				"name":  f.ID,
				"type":  "SINGLE_LINE",
			}
			if f.Type == "textarea" {
				w["type"] = "MULTIPLE_LINE"
			}
			if f.Placeholder != "" {
				w["hintText"] = f.Placeholder
			}
			widgets = append(widgets, map[string]any{"textInput": w})
		case "select", "checkbox":
			items := make([]any, 0, len(f.Options))
			for _, opt := range f.Options {
				items = append(items, map[string]any{
					"text":     opt.Label,
					"value":    opt.Value,
					"selected": false,
				})
			}
			selType := "CHECK_BOX"
			if f.Type == "select" {
				selType = "DROPDOWN"
			}
			widgets = append(widgets, map[string]any{
				"selectionInput": map[string]any{
					"name":  f.ID,
					"label": f.Label,
					"type":  selType,
					"items": items,
				},
			})
		}
	}

	card := map[string]any{
		"header":   map[string]any{"title": dialog.Title},
		"sections": []any{map[string]any{"widgets": widgets}},
	}

	// Footer buttons
	footer := map[string]any{}
	if dialog.Submit.Label != "" {
		footer["primaryButton"] = map[string]any{
			"text": dialog.Submit.Label,
			"onClick": map[string]any{
				"action": map[string]any{
					"function":   a.externalURL,
					"parameters": []any{map[string]any{"key": "action", "value": dialog.Submit.ActionID}},
				},
			},
		}
	}
	if dialog.Cancel.Label != "" {
		footer["secondaryButton"] = map[string]any{
			"text": dialog.Cancel.Label,
			"onClick": map[string]any{
				"action": map[string]any{
					"function":   a.externalURL,
					"parameters": []any{map[string]any{"key": "action", "value": dialog.Cancel.ActionID}},
				},
			},
		}
	}
	if len(footer) > 0 {
		card["fixedFooter"] = footer
	}

	return map[string]any{
		"action": map[string]any{
			"navigations": []any{
				map[string]any{"pushCard": card},
			},
		},
	}
}

// buildMessageResponse wraps a message in the hostAppDataAction envelope.
func (a *Adapter) buildMessageResponse(actionKey string, req *chatapp.SendMessageRequest) map[string]any {
	msg := map[string]any{}
	if req.Text != "" {
		msg["text"] = req.Text
	}
	if req.Card != nil {
		msg["cardsV2"] = []map[string]any{
			{
				"cardId": "scion_card",
				"card":   a.renderCardV2(req.Card),
			},
		}
	}
	return map[string]any{
		"hostAppDataAction": map[string]any{
			"chatDataAction": map[string]any{
				actionKey: map[string]any{
					"message": msg,
				},
			},
		},
	}
}

// rawEvent represents the Workspace Add-on event envelope.
type rawEvent struct {
	CommonEventObject *rawCommonEventObject `json:"commonEventObject,omitempty"`
	Chat              *rawChatPayload       `json:"chat,omitempty"`
}

type rawCommonEventObject struct {
	Platform   string                         `json:"platform"`
	HostApp    string                         `json:"hostApp"`
	UserLocale string                         `json:"userLocale"`
	TimeZone   *rawTimeZone                   `json:"timeZone,omitempty"`
	Parameters map[string]string              `json:"parameters,omitempty"`
	FormInputs map[string]rawFormInputWrapper `json:"formInputs,omitempty"`
}

type rawTimeZone struct {
	ID     string      `json:"id"`
	Offset json.Number `json:"offset"`
}

type rawFormInputWrapper struct {
	StringInputs  *rawStringInputs  `json:"stringInputs,omitempty"`
	DateTimeInput *rawDateTimeInput `json:"dateTimeInput,omitempty"`
}

type rawStringInputs struct {
	Value []string `json:"value"`
}

type rawDateTimeInput struct {
	Milliseconds int64 `json:"msSinceEpoch"`
	HasDate      bool  `json:"hasDate"`
	HasTime      bool  `json:"hasTime"`
}

type rawChatPayload struct {
	User                    *rawUser                    `json:"user,omitempty"`
	Space                   *rawSpace                   `json:"space,omitempty"`
	EventTime               string                      `json:"eventTime"`
	MessagePayload          *rawMessagePayload          `json:"messagePayload,omitempty"`
	AddedToSpacePayload     *rawAddedToSpacePayload     `json:"addedToSpacePayload,omitempty"`
	RemovedFromSpacePayload *rawRemovedFromSpacePayload `json:"removedFromSpacePayload,omitempty"`
	ButtonClickedPayload    *rawButtonClickedPayload    `json:"buttonClickedPayload,omitempty"`
	AppCommandPayload       *rawAppCommandPayload       `json:"appCommandPayload,omitempty"`
	WidgetUpdatedPayload    *rawWidgetUpdatedPayload    `json:"widgetUpdatedPayload,omitempty"`
}

type rawMessagePayload struct {
	Message                   *rawMessage `json:"message,omitempty"`
	Space                     *rawSpace   `json:"space,omitempty"`
	ConfigCompleteRedirectUri string      `json:"configCompleteRedirectUri,omitempty"`
}

type rawAddedToSpacePayload struct {
	Space                     *rawSpace `json:"space,omitempty"`
	InteractionAdd            bool      `json:"interactionAdd"`
	ConfigCompleteRedirectUri string    `json:"configCompleteRedirectUri,omitempty"`
}

type rawRemovedFromSpacePayload struct {
	Space *rawSpace `json:"space,omitempty"`
}

type rawButtonClickedPayload struct {
	Message         *rawMessage `json:"message,omitempty"`
	Space           *rawSpace   `json:"space,omitempty"`
	IsDialogEvent   bool        `json:"isDialogEvent"`
	DialogEventType string      `json:"dialogEventType"`
}

type rawAppCommandPayload struct {
	AppCommandMetadata *rawAppCommandMetadata `json:"appCommandMetadata,omitempty"`
	Space              *rawSpace              `json:"space,omitempty"`
	Thread             *rawThread             `json:"thread,omitempty"`
	Message            *rawMessage            `json:"message,omitempty"`
	IsDialogEvent      bool                   `json:"isDialogEvent"`
	DialogEventType    string                 `json:"dialogEventType"`
}

type rawAppCommandMetadata struct {
	AppCommandId   json.Number `json:"appCommandId"`
	AppCommandType string      `json:"appCommandType"`
}

type rawWidgetUpdatedPayload struct {
	Space   *rawSpace   `json:"space,omitempty"`
	Message *rawMessage `json:"message,omitempty"`
}

type rawSpace struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Type        string `json:"type"`
}

type rawMessage struct {
	Name         string           `json:"name"`
	Text         string           `json:"text"`
	ArgumentText string           `json:"argumentText"`
	Thread       *rawThread       `json:"thread,omitempty"`
	Annotations  []rawAnnotation  `json:"annotations,omitempty"`
	SlashCommand *rawSlashCommand `json:"slashCommand,omitempty"`
	Attachment   []rawAttachment  `json:"attachment,omitempty"`
}

type rawAttachment struct {
	Name              string                `json:"name"`
	ContentName       string                `json:"contentName"`
	ContentType       string                `json:"contentType"`
	DownloadURI       string                `json:"downloadUri"`
	AttachmentDataRef *rawAttachmentDataRef `json:"attachmentDataRef,omitempty"`
}

type rawAttachmentDataRef struct {
	ResourceName string `json:"resourceName"`
}

type rawSlashCommand struct {
	CommandId json.Number `json:"commandId"`
}

type rawThread struct {
	Name      string `json:"name"`
	ThreadKey string `json:"threadKey,omitempty"`
}

type rawAnnotation struct {
	Type       string  `json:"type"`
	StartIndex float64 `json:"startIndex"`
	Length     float64 `json:"length"`
}

type rawUser struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
	Type        string `json:"type"`
}

// normalizeEvent converts a Workspace Add-on event to a ChatEvent.
// Event type is inferred from which chat payload is present (no top-level type field).
func (a *Adapter) normalizeEvent(raw *rawEvent) *chatapp.ChatEvent {
	if raw.Chat == nil {
		a.log.Debug("event has no chat payload, ignoring")
		return nil
	}

	event := &chatapp.ChatEvent{
		Platform: PlatformName,
	}

	// User is at the top level of the chat object.
	if raw.Chat.User != nil {
		event.UserID = raw.Chat.User.Name
		event.UserEmail = raw.Chat.User.Email
		event.UserDisplayName = raw.Chat.User.DisplayName
	}

	// Extract parameters from commonEventObject.
	params := a.getParameters(raw)

	switch {
	case raw.Chat.AddedToSpacePayload != nil:
		p := raw.Chat.AddedToSpacePayload
		event.Type = chatapp.EventSpaceJoin
		event.InteractionAdd = p.InteractionAdd
		if p.Space != nil {
			event.SpaceID = p.Space.Name
		}
		a.mu.Lock()
		a.spaces[event.SpaceID] = true
		a.mu.Unlock()
		return event

	case raw.Chat.RemovedFromSpacePayload != nil:
		p := raw.Chat.RemovedFromSpacePayload
		event.Type = chatapp.EventSpaceRemove
		if p.Space != nil {
			event.SpaceID = p.Space.Name
		}
		a.mu.Lock()
		delete(a.spaces, event.SpaceID)
		a.mu.Unlock()
		return event

	case raw.Chat.AppCommandPayload != nil:
		p := raw.Chat.AppCommandPayload
		event.Type = chatapp.EventCommand
		if p.Space != nil {
			event.SpaceID = p.Space.Name
		}
		if p.Thread != nil {
			event.ThreadID = p.Thread.Name
		}
		// Map numeric command ID to command name.
		if p.AppCommandMetadata != nil {
			cmdID := p.AppCommandMetadata.AppCommandId.String()
			if name, ok := a.commandIDs[cmdID]; ok {
				event.Command = name
			} else {
				event.Command = commandNameFromText(p.Message)
			}
		} else if p.Message != nil && p.Message.SlashCommand != nil {
			cmdID := p.Message.SlashCommand.CommandId.String()
			if name, ok := a.commandIDs[cmdID]; ok {
				event.Command = name
			} else {
				event.Command = commandNameFromText(p.Message)
			}
		}
		if p.Message != nil {
			event.Args = strings.TrimSpace(p.Message.ArgumentText)
			event.MessageName = p.Message.Name
			if p.Message.Thread != nil {
				event.ThreadID = p.Message.Thread.Name
			}
		}
		return event

	case raw.Chat.MessagePayload != nil:
		p := raw.Chat.MessagePayload
		if p.Message == nil {
			return nil
		}
		if p.Space != nil {
			event.SpaceID = p.Space.Name
		}
		event.MessageName = p.Message.Name
		if p.Message.Thread != nil {
			event.ThreadID = p.Message.Thread.Name
		}
		// Extract attachments from message.
		event.Attachments = extractAttachments(p.Message)
		if p.Message.SlashCommand != nil {
			event.Type = chatapp.EventCommand
			cmdID := p.Message.SlashCommand.CommandId.String()
			if name, ok := a.commandIDs[cmdID]; ok {
				event.Command = name
			} else {
				event.Command = commandNameFromText(p.Message)
			}
			event.Args = strings.TrimSpace(p.Message.ArgumentText)
			return event
		}
		event.Type = chatapp.EventMessage
		text := p.Message.ArgumentText
		if text == "" {
			text = p.Message.Text
		}
		event.Text = strings.TrimSpace(text)
		return event

	case raw.Chat.ButtonClickedPayload != nil:
		p := raw.Chat.ButtonClickedPayload
		if p.Space != nil {
			event.SpaceID = p.Space.Name
		}
		if p.Message != nil {
			event.MessageName = p.Message.Name
			if p.Message.Thread != nil {
				event.ThreadID = p.Message.Thread.Name
			}
		}
		event.IsDialogEvent = p.IsDialogEvent

		// Read action ID from parameters.
		actionID := params["action"]
		if actionID == "" {
			// Backward compat: pre-migration cards pass old function name here.
			actionID = params["__action_method_name__"]
		}
		event.ActionID = actionID

		// Collect remaining parameters as action data.
		for k, v := range params {
			if k == "action" || k == "__action_method_name__" {
				continue
			}
			if event.ActionData != "" {
				event.ActionData += ","
			}
			event.ActionData += k + "=" + v
		}

		// Check for form inputs (dialog submission).
		formInputs := a.getFormInputs(raw)
		if len(formInputs) > 0 {
			event.Type = chatapp.EventDialogSubmit
			event.DialogData = formInputs
		} else {
			event.Type = chatapp.EventAction
		}
		return event

	default:
		a.log.Debug("no recognized chat payload present")
		return nil
	}
}

// commandNameFromText extracts the slash command name from the message text,
// falling back to "scion" if the text doesn't contain a recognizable command.
func commandNameFromText(msg *rawMessage) string {
	if msg == nil {
		return "scion"
	}
	f := strings.Fields(msg.Text)
	if len(f) > 0 && strings.HasPrefix(f[0], "/") {
		return strings.TrimPrefix(f[0], "/")
	}
	return "scion"
}

// extractAttachments converts raw message attachments to normalized EventAttachment structs.
func extractAttachments(msg *rawMessage) []chatapp.EventAttachment {
	if msg == nil || len(msg.Attachment) == 0 {
		return nil
	}
	var attachments []chatapp.EventAttachment
	for _, a := range msg.Attachment {
		if a.DownloadURI == "" {
			continue
		}
		name := a.ContentName
		if name == "" {
			name = filepath.Base(a.Name)
		}
		attachments = append(attachments, chatapp.EventAttachment{
			Name:        name,
			ContentType: a.ContentType,
			DownloadURI: a.DownloadURI,
		})
	}
	return attachments
}

// getParameters extracts action parameters from commonEventObject.
func (a *Adapter) getParameters(raw *rawEvent) map[string]string {
	if raw.CommonEventObject != nil && raw.CommonEventObject.Parameters != nil {
		return raw.CommonEventObject.Parameters
	}
	return map[string]string{}
}

// getFormInputs extracts form input values from commonEventObject.
// Values are arrays in the Add-on format; we take the first value for single inputs
// and join with commas for multi-value inputs.
func (a *Adapter) getFormInputs(raw *rawEvent) map[string]string {
	if raw.CommonEventObject == nil || raw.CommonEventObject.FormInputs == nil {
		return nil
	}
	result := make(map[string]string)
	for k, v := range raw.CommonEventObject.FormInputs {
		if v.StringInputs != nil && len(v.StringInputs.Value) > 0 {
			if len(v.StringInputs.Value) == 1 {
				result[k] = v.StringInputs.Value[0]
			} else {
				result[k] = strings.Join(v.StringInputs.Value, ",")
			}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// SendMessage sends a text or card message to a Google Chat space.
// If the request includes attachments, each file is uploaded via UploadMedia
// and attached to the message.
func (a *Adapter) SendMessage(ctx context.Context, req chatapp.SendMessageRequest) (string, error) {
	payload := map[string]any{}

	hasText := req.Text != ""
	hasCard := req.Card != nil

	if hasText {
		payload["text"] = req.Text
	}

	if hasCard {
		payload["cardsV2"] = []map[string]any{
			{
				"cardId": "scion_card",
				"card":   a.renderCardV2(req.Card),
			},
		}
	}

	// Upload attachments and include references in the message.
	if len(req.Attachments) > 0 {
		var attachmentRefs []map[string]any
		var failedAttachments []string
		for _, att := range req.Attachments {
			resourceName, uploadErr := a.uploadAttachment(ctx, req.SpaceID, att)
			if uploadErr != nil {
				a.log.Error("failed to upload attachment",
					"filename", att.Filename,
					"error", uploadErr,
				)
				failedAttachments = append(failedAttachments, att.Filename)
				continue
			}
			attachmentRefs = append(attachmentRefs, map[string]any{
				"attachmentDataRef": map[string]string{
					"resourceName": resourceName,
				},
			})
		}
		if len(attachmentRefs) > 0 {
			payload["attachment"] = attachmentRefs
		}
		// Surface upload failures in the message text so recipients know
		// an attachment was intended but could not be delivered.
		if len(failedAttachments) > 0 {
			errNote := "\n\n_[Failed to upload: " + strings.Join(failedAttachments, ", ") + "]_"
			if existing, ok := payload["text"].(string); ok {
				payload["text"] = existing + errNote
			} else {
				payload["text"] = errNote
			}
		}
	}

	if req.ThreadID != "" {
		payload["thread"] = map[string]string{
			"name": req.ThreadID,
		}
	} else if req.ThreadKey != "" {
		payload["thread"] = map[string]string{
			"threadKey": req.ThreadKey,
		}
	}

	url := fmt.Sprintf("%s/%s/messages", chatAPIBase, req.SpaceID)
	if req.ThreadID != "" || req.ThreadKey != "" {
		url += "?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD"
	}

	a.log.Info("sending async message",
		"space", req.SpaceID,
		"thread", req.ThreadID,
		"has_text", hasText,
		"has_card", hasCard,
		"has_attachments", len(req.Attachments) > 0,
		"text_len", len(req.Text),
	)

	respBody, err := a.doPost(ctx, url, payload)
	if err != nil {
		a.log.Error("async message failed", "space", req.SpaceID, "error", err)
		return "", fmt.Errorf("sending message: %w", err)
	}

	var result struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parsing response: %w", err)
	}
	a.log.Info("async message sent", "message_id", result.Name, "space", req.SpaceID)
	return result.Name, nil
}

// uploadAttachment opens a local file (or reads from URL) and uploads it via UploadMedia.
// Returns the attachment resource name. Enforces the 25 MB size limit.
func (a *Adapter) uploadAttachment(ctx context.Context, spaceID string, att chatapp.Attachment) (string, error) {
	if att.Path == "" {
		return "", fmt.Errorf("attachment has no path")
	}

	fi, err := os.Stat(att.Path)
	if err != nil {
		return "", fmt.Errorf("stat attachment %q: %w", att.Path, err)
	}
	if fi.Size() > chatapp.MaxAttachmentSize {
		return "", fmt.Errorf("attachment %q too large (%d bytes, max %d)", att.Filename, fi.Size(), chatapp.MaxAttachmentSize)
	}

	f, err := os.Open(att.Path)
	if err != nil {
		return "", fmt.Errorf("open attachment %q: %w", att.Path, err)
	}
	defer f.Close()

	filename := att.Filename
	if filename == "" {
		filename = filepath.Base(att.Path)
	}

	return a.UploadMedia(ctx, spaceID, filename, f)
}

// SendCard sends a card-only message to a Google Chat space.
func (a *Adapter) SendCard(ctx context.Context, spaceID string, card chatapp.Card) (string, error) {
	return a.SendMessage(ctx, chatapp.SendMessageRequest{
		SpaceID: spaceID,
		Card:    &card,
	})
}

// UpdateMessage updates an existing message.
func (a *Adapter) UpdateMessage(ctx context.Context, messageID string, req chatapp.SendMessageRequest) error {
	payload := map[string]any{}
	if req.Text != "" {
		payload["text"] = req.Text
	}
	if req.Card != nil {
		payload["cardsV2"] = []map[string]any{
			{
				"cardId": "scion_card",
				"card":   a.renderCardV2(req.Card),
			},
		}
	}

	url := fmt.Sprintf("%s/%s", chatAPIBase, messageID)
	_, err := a.doPatch(ctx, url, payload)
	return err
}

// OpenDialog presents a dialog in Google Chat.
func (a *Adapter) OpenDialog(ctx context.Context, triggerID string, dialog chatapp.Dialog) error {
	// Google Chat dialogs are returned as part of the webhook response,
	// not as separate API calls. This is handled in the event handler.
	a.log.Debug("dialog open requested (handled via webhook response)", "trigger", triggerID)
	return nil
}

// UpdateDialog updates an existing dialog.
func (a *Adapter) UpdateDialog(ctx context.Context, triggerID string, dialog chatapp.Dialog) error {
	a.log.Debug("dialog update requested", "trigger", triggerID)
	return nil
}

// GetUser retrieves a Google Chat user's information.
func (a *Adapter) GetUser(ctx context.Context, userID string) (*chatapp.ChatUser, error) {
	// Google Chat provides user info in webhook events;
	// for standalone lookups, we use the People API or cached data.
	// For MVP, return a placeholder with the user ID.
	return &chatapp.ChatUser{
		PlatformID: userID,
	}, nil
}

// SetAgentIdentity configures how agent messages appear.
// In Google Chat, the bot identity is fixed; we use card headers instead.
func (a *Adapter) SetAgentIdentity(ctx context.Context, agent chatapp.AgentIdentity) error {
	a.log.Debug("agent identity set (used in card headers)", "slug", agent.Slug)
	return nil
}

// UploadMedia uploads a file to a Google Chat space and returns the attachment
// resource name for inclusion in messages.
//
// The upload uses the media.upload endpoint with multipart/related containing:
//   - Part 1: JSON metadata with the filename (Content-Type: application/json)
//   - Part 2: File content (Content-Type: application/octet-stream)
//
// The multipart body is streamed via io.Pipe to avoid buffering the entire file
// in memory.
//
// See https://developers.google.com/workspace/chat/api/reference/rest/v1/media/upload
func (a *Adapter) UploadMedia(ctx context.Context, spaceID, filename string, content io.Reader) (string, error) {
	url := fmt.Sprintf("%s/%s/attachments:upload", uploadAPIBase, spaceID)
	return a.uploadMediaToURL(ctx, url, filename, content)
}

// uploadMediaToURL performs the actual multipart/related upload to the given URL.
// Extracted from UploadMedia to allow tests to provide a test-server URL.
func (a *Adapter) uploadMediaToURL(ctx context.Context, url, filename string, content io.Reader) (string, error) {
	// Use io.Pipe to stream the multipart body without buffering the entire
	// file in memory.
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	contentType := fmt.Sprintf("multipart/related; boundary=%s", writer.Boundary())

	// Write multipart parts in a goroutine so the HTTP request can read from
	// the pipe reader concurrently.
	errChan := make(chan error, 1)
	go func() {
		var writeErr error
		defer func() {
			if writeErr != nil {
				pw.CloseWithError(writeErr)
			} else {
				pw.Close()
			}
			errChan <- writeErr
		}()

		// Part 1: JSON metadata.
		metaHeader := make(textproto.MIMEHeader)
		metaHeader.Set("Content-Type", "application/json")
		metaPart, err := writer.CreatePart(metaHeader)
		if err != nil {
			writeErr = fmt.Errorf("creating metadata part: %w", err)
			return
		}
		metadata := map[string]string{"filename": filename}
		if err := json.NewEncoder(metaPart).Encode(metadata); err != nil {
			writeErr = fmt.Errorf("encoding metadata: %w", err)
			return
		}

		// Part 2: file content with explicit Content-Type header.
		fileHeader := make(textproto.MIMEHeader)
		fileHeader.Set("Content-Type", "application/octet-stream")
		filePart, err := writer.CreatePart(fileHeader)
		if err != nil {
			writeErr = fmt.Errorf("creating file part: %w", err)
			return
		}
		if _, err := io.Copy(filePart, content); err != nil {
			writeErr = fmt.Errorf("copying file content: %w", err)
			return
		}

		if err := writer.Close(); err != nil {
			writeErr = fmt.Errorf("closing multipart writer: %w", err)
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, pr)
	if err != nil {
		return "", fmt.Errorf("creating upload request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("uploading media: %w", err)
	}
	defer resp.Body.Close()

	// Check for errors from the writer goroutine.
	if writeErr := <-errChan; writeErr != nil {
		return "", writeErr
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading upload response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("upload failed (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		AttachmentDataRef struct {
			ResourceName string `json:"resourceName"`
		} `json:"attachmentDataRef"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parsing upload response: %w", err)
	}

	a.log.Info("uploaded media to Google Chat",
		"url", url,
		"filename", filename,
		"resource_name", result.AttachmentDataRef.ResourceName,
	)
	return result.AttachmentDataRef.ResourceName, nil
}

// DownloadAttachment downloads a file from a Google Chat attachment download URI.
// The download requires authentication, which is handled by the adapter's httpClient.
func (a *Adapter) DownloadAttachment(ctx context.Context, downloadURI string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURI, nil)
	if err != nil {
		return nil, fmt.Errorf("creating download request: %w", err)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading attachment: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download failed (HTTP %d)", resp.StatusCode)
	}

	return resp.Body, nil
}

// renderCardV2 converts a platform-agnostic Card to Google Chat Cards V2 format.
// Action IDs are moved into parameters and the function is set to the external URL.
func (a *Adapter) renderCardV2(card *chatapp.Card) map[string]any {
	c := map[string]any{}

	// Header
	if card.Header.Title != "" {
		header := map[string]any{
			"title": card.Header.Title,
		}
		if card.Header.Subtitle != "" {
			header["subtitle"] = card.Header.Subtitle
		}
		if card.Header.IconURL != "" {
			header["imageUrl"] = card.Header.IconURL
			header["imageType"] = "CIRCLE"
		}
		c["header"] = header
	}

	// Sections
	sections := make([]map[string]any, 0)
	for _, s := range card.Sections {
		section := map[string]any{}
		if s.Header != "" {
			section["header"] = s.Header
		}
		if len(s.Widgets) > 0 {
			widgets := make([]map[string]any, 0, len(s.Widgets))
			for _, w := range s.Widgets {
				widget := a.renderWidget(&w)
				if widget != nil {
					widgets = append(widgets, widget)
				}
			}
			section["widgets"] = widgets
		}
		sections = append(sections, section)
	}

	// Render card-level actions as a button list in a footer section
	if len(card.Actions) > 0 {
		buttons := make([]any, 0, len(card.Actions))
		for _, act := range card.Actions {
			btn := map[string]any{
				"text":    act.Label,
				"onClick": a.resolveOnClick(act.ActionID),
			}
			if act.Style == "danger" {
				btn["color"] = map[string]any{
					"red": 0.9, "green": 0.2, "blue": 0.2, "alpha": 1,
				}
			} else if act.Style == "primary" {
				btn["color"] = map[string]any{
					"red": 0.1, "green": 0.5, "blue": 0.9, "alpha": 1,
				}
			}
			buttons = append(buttons, btn)
		}
		sections = append(sections, map[string]any{
			"widgets": []any{
				map[string]any{
					"buttonList": map[string]any{
						"buttons": buttons,
					},
				},
			},
		})
	}

	if len(sections) > 0 {
		c["sections"] = sections
	}

	return c
}

// resolveOnClick returns an openLink onClick when the actionID starts with
// "link." (the remainder is the URL), or a callback action otherwise.
func (a *Adapter) resolveOnClick(actionID string) map[string]any {
	if strings.HasPrefix(actionID, "link.") {
		return a.openLinkOnClick(strings.TrimPrefix(actionID, "link."))
	}
	return a.actionOnClick(actionID)
}

// actionOnClick builds an onClick action with the full external URL and action ID in parameters.
func (a *Adapter) actionOnClick(actionID string) map[string]any {
	return map[string]any{
		"action": map[string]any{
			"function": a.externalURL,
			"parameters": []any{
				map[string]any{"key": "action", "value": actionID},
			},
		},
	}
}

// openLinkOnClick builds an onClick that opens a URL in a new tab.
func (a *Adapter) openLinkOnClick(url string) map[string]any {
	return map[string]any{
		"openLink": map[string]any{
			"url": url,
		},
	}
}

// renderWidget converts a Widget to Google Chat widget format.
func (a *Adapter) renderWidget(w *chatapp.Widget) map[string]any {
	switch w.Type {
	case chatapp.WidgetText:
		return map[string]any{
			"textParagraph": map[string]any{
				"text": strings.ReplaceAll(w.Content, "\n", "<br>"),
			},
		}
	case chatapp.WidgetKeyValue:
		decorated := map[string]any{
			"topLabel": w.Label,
			"text":     strings.ReplaceAll(w.Content, "\n", "<br>"),
		}
		return map[string]any{
			"decoratedText": decorated,
		}
	case chatapp.WidgetButton:
		btn := map[string]any{
			"text":    w.Label,
			"onClick": a.resolveOnClick(w.ActionID),
		}
		return map[string]any{
			"buttonList": map[string]any{
				"buttons": []any{btn},
			},
		}
	case chatapp.WidgetDivider:
		return map[string]any{
			"divider": map[string]any{},
		}
	case chatapp.WidgetImage:
		return map[string]any{
			"image": map[string]any{
				"imageUrl": w.Content,
			},
		}
	case chatapp.WidgetInput:
		input := map[string]any{
			"label": w.Label,
			"name":  w.ActionID,
			"type":  "SINGLE_LINE",
		}
		if w.ActionID != "" {
			input["onChangeAction"] = a.actionOnClick(w.ActionID)
		}
		return map[string]any{
			"textInput": input,
		}
	case chatapp.WidgetCheckbox:
		items := make([]any, 0, len(w.Options))
		for _, opt := range w.Options {
			items = append(items, map[string]any{
				"text":     opt.Label,
				"value":    opt.Value,
				"selected": false,
			})
		}
		return map[string]any{
			"selectionInput": map[string]any{
				"name":  w.ActionID,
				"label": w.Label,
				"type":  "CHECK_BOX",
				"items": items,
			},
		}
	default:
		return nil
	}
}

// doPost performs an authenticated POST request.
func (a *Adapter) doPost(ctx context.Context, url string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	a.log.Debug("chat API POST", "url", url, "payload_len", len(data))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		a.log.Error("chat API POST failed", "url", url, "error", err)
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		a.log.Error("chat API error response", "url", url, "status", resp.StatusCode, "body", string(body))
		return nil, fmt.Errorf("chat API error %d: %s", resp.StatusCode, string(body))
	}
	a.log.Debug("chat API POST success", "url", url, "status", resp.StatusCode)
	return body, nil
}

// doPatch performs an authenticated PATCH request.
func (a *Adapter) doPatch(ctx context.Context, url string, payload any) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, strings.NewReader(string(data)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("chat API error %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}
