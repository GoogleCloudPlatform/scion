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

package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// CallbackHandler processes Adaptive Card Action.Execute invoke activities.
type CallbackHandler struct {
	broker *TeamsBroker
	log    *slog.Logger
}

// NewCallbackHandler creates a new CallbackHandler.
func NewCallbackHandler(broker *TeamsBroker, log *slog.Logger) *CallbackHandler {
	return &CallbackHandler{
		broker: broker,
		log:    log,
	}
}

// getStore returns the broker's store under the broker mutex.
func (h *CallbackHandler) getStore() Store {
	h.broker.mu.Lock()
	store := h.broker.store
	h.broker.mu.Unlock()
	return store
}

// HandleInvoke processes an invoke activity (Adaptive Card button click).
// It reads the action field from activity.Value to dispatch to the correct handler.
func (h *CallbackHandler) HandleInvoke(ctx context.Context, activity *Activity) (*InvokeResponse, error) {
	// Only handle adaptiveCard/action invokes.
	if activity.Name != "adaptiveCard/action" {
		h.log.Debug("Ignoring non-adaptive-card invoke", "name", activity.Name)
		return &InvokeResponse{Status: 200, Body: map[string]string{"status": "ok"}}, nil
	}

	// Parse the Value to extract action data.
	var data map[string]interface{}
	if activity.Value != nil {
		// Teams wraps Action.Execute data inside {"action": {"data": ...}}
		// but in practice the data is the Value itself or under a "data" wrapper.
		var raw json.RawMessage
		if err := json.Unmarshal(activity.Value, &raw); err != nil {
			h.log.Warn("Failed to parse invoke value", "error", err)
			return &InvokeResponse{Status: 200, Body: map[string]string{"status": "ok"}}, nil
		}

		// Try parsing as wrapped action data first: {"action": {"type": "Action.Execute", "data": {...}}}
		var wrapped struct {
			Action struct {
				Data json.RawMessage `json:"data"`
			} `json:"action"`
		}
		if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Action.Data != nil {
			if err := json.Unmarshal(wrapped.Action.Data, &data); err != nil {
				h.log.Warn("Failed to parse wrapped action data", "error", err)
				return &InvokeResponse{Status: 200, Body: map[string]string{"status": "ok"}}, nil
			}
		} else {
			// Fallback: data is the value itself.
			if err := json.Unmarshal(raw, &data); err != nil {
				h.log.Warn("Failed to parse invoke value as map", "error", err)
				return &InvokeResponse{Status: 200, Body: map[string]string{"status": "ok"}}, nil
			}
		}
	}

	if data == nil {
		h.log.Debug("Invoke activity with no action data")
		return &InvokeResponse{Status: 200, Body: map[string]string{"status": "ok"}}, nil
	}

	action, _ := data["action"].(string)
	h.log.Debug("Processing invoke callback", "action", action)

	switch action {
	case "ask_response":
		return h.handleAskResponse(ctx, activity, data)
	case "ask_input":
		return h.handleAskInput(ctx, activity, data)
	case "setup_confirm":
		return h.handleSetupConfirm(ctx, activity, data)
	default:
		h.log.Debug("Unknown invoke action, acknowledging", "action", action)
		return &InvokeResponse{Status: 200, Body: map[string]string{"status": "ok"}}, nil
	}
}

// handleAskResponse processes a user clicking approve/reject on an ask-user card.
func (h *CallbackHandler) handleAskResponse(ctx context.Context, activity *Activity, data map[string]interface{}) (*InvokeResponse, error) {
	requestID, _ := data["request_id"].(string)
	choice, _ := data["choice"].(string)

	if requestID == "" {
		h.log.Warn("ask_response with no request_id")
		return h.respondWithUpdatedCard(activity, "Invalid response — missing request ID."), nil
	}

	store := h.getStore()
	if store == nil {
		return h.respondWithMessage("Store not initialized."), nil
	}

	pending, err := store.GetPendingAskUser(ctx, requestID)
	if err != nil {
		h.log.Error("Failed to look up pending ask-user", "request_id", requestID, "error", err)
		return h.respondWithMessage("An error occurred processing your response. Please try again."), nil
	}

	if pending == nil {
		return h.respondWithUpdatedCard(activity, "This request has expired or was not found."), nil
	}

	if pending.Responded {
		return h.respondWithUpdatedCard(activity, "This request has already been responded to."), nil
	}

	if time.Now().After(pending.ExpiresAt) {
		return h.respondWithUpdatedCard(activity, "This request has expired."), nil
	}

	// When the choice is "custom", use the text typed into the Input.Text
	// field. Other choices must be one of the request's choices. Invalid input
	// keeps the card so the user can answer again.
	responseText := choice
	if choice == "custom" {
		replyText, _ := data["reply_text"].(string)
		if strings.TrimSpace(replyText) == "" {
			return h.respondWithMessage("Please type a reply before sending."), nil
		}
		responseText = replyText
	} else if !containsChoice(pending.Choices, choice) {
		return h.respondWithMessage("That choice isn't available for this question. Please use one of the buttons."), nil
	}

	// Replies go back to the conversation the answer came from.
	conversationID := stripThreadSuffix(activity.Conversation.ID)

	// Answers are sent as the linked user. Without a usable link, show what to
	// do next and keep the card so the request can still be answered.
	teamsUserID := teamsUserIDOf(activity)
	mapping, err := linkedUserByTeamsID(ctx, store, teamsUserID)
	if problem := linkProblem(mapping, err, registerHint); problem != "" {
		if err != nil {
			h.log.Warn("Error looking up user mapping", "error", err, "teams_user_id", teamsUserID)
		}
		return h.respondWithMessage(problem), nil
	}

	// Claim the request so only one click is delivered. A click that loses
	// the claim is told the request was already answered.
	claimed, err := store.MarkAskUserResponded(ctx, requestID)
	if err != nil {
		h.log.Error("Failed to mark ask-user as responded", "request_id", requestID, "error", err)
		return h.respondWithMessage("An error occurred processing your response. Please try again."), nil
	}
	if !claimed {
		return h.respondWithUpdatedCard(activity, "This request has already been responded to."), nil
	}

	// Deliver the response to the hub. On failure release the claim and keep
	// the card so the answer can be retried.
	if err := h.deliverAskUserResponse(ctx, activity, pending, mapping, conversationID, responseText); err != nil {
		h.log.Error("Failed to deliver ask-user response to hub", "error", err)
		// Reopen even if the click's context has been cancelled.
		resetCtx, resetCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		resetErr := store.ResetAskUserResponded(resetCtx, requestID)
		resetCancel()
		if resetErr != nil {
			h.log.Error("Failed to reopen ask-user request", "request_id", requestID, "error", resetErr)
		}
		return h.respondWithMessage(
			hubErrorText(err, mapping, h.projectSlugFor(ctx, conversationID, pending.ProjectID), "Failed to deliver your response. Please try again.")), nil
	}

	// Build updated card showing the response.
	responder := activity.From.Name
	if responder == "" {
		responder = "User"
	}

	return h.respondWithUpdatedCard(activity,
		fmt.Sprintf("Responded: **%s** (by %s)", responseText, responder)), nil
}

// handleAskInput processes a user clicking "Custom Reply..." on an ask-user card.
// Returns an Adaptive Card with an Input.Text field so the user can type a
// custom reply that is submitted back through the invoke flow.
func (h *CallbackHandler) handleAskInput(ctx context.Context, activity *Activity, data map[string]interface{}) (*InvokeResponse, error) {
	requestID, _ := data["request_id"].(string)

	if requestID == "" {
		return h.respondWithUpdatedCard(activity, "Invalid request — missing request ID."), nil
	}

	store := h.getStore()
	if store == nil {
		return h.respondWithMessage("Store not initialized."), nil
	}

	pending, err := store.GetPendingAskUser(ctx, requestID)
	if err != nil {
		// Keep the card so the user can try again.
		h.log.Error("Failed to look up pending ask-user", "request_id", requestID, "error", err)
		return h.respondWithMessage("An error occurred loading this request. Please try again."), nil
	}
	if pending == nil {
		return h.respondWithUpdatedCard(activity, "This request has expired or was not found."), nil
	}

	if pending.Responded {
		return h.respondWithUpdatedCard(activity, "This request has already been responded to."), nil
	}

	if time.Now().After(pending.ExpiresAt) {
		return h.respondWithUpdatedCard(activity, "This request has expired."), nil
	}

	// Return an Adaptive Card with an Input.Text field and Submit button
	// so the reply stays within the invoke flow.
	question := "Please provide your reply:"
	card := &AdaptiveCard{
		Type:    "AdaptiveCard",
		Schema:  "http://adaptivecards.io/schemas/adaptive-card.json",
		Version: "1.5",
		Body: []CardElement{
			TextBlock{Type: "TextBlock", Text: fmt.Sprintf("%s needs your input", pending.AgentSlug), Weight: "Bolder"},
			TextBlock{Type: "TextBlock", Text: question, Wrap: true},
			InputText{Type: "Input.Text", ID: "reply_text", IsMultiline: true, Placeholder: "Type your reply..."},
		},
		Actions: []CardAction{
			ActionExecute{Type: "Action.Execute", Title: "Send Reply", Style: "positive", Verb: "ask_response",
				Data: map[string]interface{}{"action": "ask_response", "request_id": requestID, "choice": "custom"}},
		},
	}

	return h.respondWithInputCard(activity, card), nil
}

// respondWithInputCard creates an InvokeResponse that replaces the original
// card with the provided Adaptive Card (e.g., one containing Input.Text).
func (h *CallbackHandler) respondWithInputCard(activity *Activity, card *AdaptiveCard) *InvokeResponse {
	cardJSON, err := json.Marshal(card)
	if err != nil {
		h.log.Error("Failed to marshal input card", "error", err)
		return &InvokeResponse{Status: 200, Body: map[string]string{"status": "ok"}}
	}

	updatedAttachment := map[string]interface{}{
		"statusCode": 200,
		"type":       "application/vnd.microsoft.card.adaptive",
		"value":      json.RawMessage(cardJSON),
	}

	_ = activity // available for future enhancements

	return &InvokeResponse{
		Status: 200,
		Body:   updatedAttachment,
	}
}

// handleSetupConfirm processes a user confirming project setup from a card.
func (h *CallbackHandler) handleSetupConfirm(ctx context.Context, activity *Activity, data map[string]interface{}) (*InvokeResponse, error) {
	projectSlug, _ := data["project_slug"].(string)
	projectID, _ := data["project_id"].(string)

	if projectSlug == "" && projectID == "" {
		return h.respondWithUpdatedCard(activity, "Invalid setup — missing project information."), nil
	}

	store := h.getStore()
	if store == nil {
		return h.respondWithMessage("Store not initialized."), nil
	}

	// Setup requires a linked user, and the project must be one of theirs.
	// Retryable failures show a message and keep the card; final outcomes
	// replace it.
	mapping, err := linkedUserByTeamsID(ctx, store, teamsUserIDOf(activity))
	if problem := linkProblem(mapping, err, registerHint); problem != "" {
		if err != nil {
			h.log.Warn("Error looking up user mapping", "error", err)
		}
		return h.respondWithMessage(problem), nil
	}

	// Normalize conversation ID — strip thread suffix for consistent lookups.
	convID := stripThreadSuffix(activity.Conversation.ID)

	// Check if already linked.
	existing, err := store.GetChannelLink(ctx, convID)
	if err != nil {
		h.log.Error("Failed to check existing channel link", "error", err)
		return h.respondWithMessage("An error occurred while checking the existing link. Please try again."), nil
	}
	if existing != nil {
		return h.respondWithUpdatedCard(activity,
			fmt.Sprintf("This conversation is already linked to project **%s**.", existing.ProjectSlug)), nil
	}

	hubClient := h.broker.hubClient
	if hubClient == nil {
		return h.respondWithMessage("Hub client not configured."), nil
	}
	lookup := projectSlug
	if lookup == "" {
		lookup = projectID
	}
	project, err := findUserProject(ctx, hubClient, mapping, lookup)
	if err != nil {
		h.log.Warn("Failed to resolve project for setup", "error", err, "project", lookup)
		return h.respondWithMessage(
			hubErrorText(err, mapping, projectSlug, "Failed to look up the project. Please try again.")), nil
	}
	if project == nil || (projectID != "" && project.ID != projectID) {
		return h.respondWithUpdatedCard(activity,
			fmt.Sprintf("Project **%s** was not found among your Scion projects.", lookup)), nil
	}
	projectID = project.ID
	projectSlug = project.Slug

	// Extract team info.
	teamID := ""
	if activity.ChannelData != nil {
		if activity.ChannelData.TeamsTeamID != "" {
			teamID = activity.ChannelData.TeamsTeamID
		} else if activity.ChannelData.Team != nil {
			teamID = activity.ChannelData.Team.ID
		}
	}

	linkedBy := activity.From.AadObjectID
	if linkedBy == "" {
		linkedBy = activity.From.ID
	}

	link := &ChannelLink{
		ConversationID: convID,
		TeamID:         teamID,
		ProjectID:      projectID,
		ProjectSlug:    projectSlug,
		LinkedBy:       linkedBy,
		LinkedAt:       time.Now(),
		Active:         true,
	}

	if err := store.CreateChannelLink(ctx, link); err != nil {
		h.log.Error("Failed to create channel link from card", "error", err)
		return h.respondWithMessage("Failed to link conversation. Please try again."), nil
	}

	return h.respondWithUpdatedCard(activity,
		fmt.Sprintf("✅ Conversation linked to project **%s**.", projectSlug)), nil
}

// --- Helpers ---

// deliverAskUserResponse sends the user's choice to the hub via inbound
// delivery as the linked user. conversationID is the conversation the answer
// came from; the agent's follow-up is routed there. pending.ConversationID is
// where the card was first posted and is informational only.
func (h *CallbackHandler) deliverAskUserResponse(ctx context.Context, activity *Activity, pending *PendingAskUser, mapping *TeamsUserMapping, conversationID, responseText string) error {
	hubClient := h.broker.hubClient
	if hubClient == nil {
		return fmt.Errorf("hub client not configured")
	}

	// Sender is the linked Scion principal, SenderID the Teams user ID.
	recipient := "agent:" + pending.AgentSlug

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    onBehalfOfUser(mapping),
		SenderID:  teamsUserIDOf(activity),
		Recipient: recipient,
		Msg:       responseText,
		Type:      messages.TypeInstruction,
		Channel:   "teams",
		ThreadID:  conversationID,
		Metadata: map[string]string{
			"teams_conversation_id": conversationID,
			"project_id":            pending.ProjectID,
			"ask_request_id":        pending.RequestID,
		},
	}

	topic := projectkeys.AgentTopic(pending.ProjectID, pending.AgentSlug)

	return hubClient.DeliverInbound(ctx, topic, msg)
}

// containsChoice reports whether choice is one of choices.
func containsChoice(choices []string, choice string) bool {
	for _, c := range choices {
		if c == choice {
			return true
		}
	}
	return false
}

// respondWithMessage creates an InvokeResponse that shows text to the user
// and leaves the original card in place.
func (h *CallbackHandler) respondWithMessage(text string) *InvokeResponse {
	return &InvokeResponse{
		Status: 200,
		Body: map[string]interface{}{
			"statusCode": 200,
			"type":       invokeMessageResponseType,
			"value":      text,
		},
	}
}

// invokeMessageResponseType is the invoke response type that shows a message
// without replacing the card.
const invokeMessageResponseType = "application/vnd.microsoft.activity.message"

// respondWithUpdatedCard creates an InvokeResponse that replaces the original
// card with a simple text card (buttons removed).
func (h *CallbackHandler) respondWithUpdatedCard(activity *Activity, text string) *InvokeResponse {
	card := NewAdaptiveCard()
	card.Body = append(card.Body, TextBlock{
		Type: "TextBlock",
		Text: text,
		Wrap: true,
	})

	cardJSON, err := json.Marshal(card)
	if err != nil {
		h.log.Error("Failed to marshal updated card", "error", err)
		return &InvokeResponse{Status: 200, Body: map[string]string{"status": "ok"}}
	}

	// The invoke response body for adaptiveCard/action should be an adaptive card
	// attachment to replace the original card.
	updatedAttachment := map[string]interface{}{
		"statusCode": 200,
		"type":       "application/vnd.microsoft.card.adaptive",
		"value":      json.RawMessage(cardJSON),
	}

	_ = activity // available for future enhancements (e.g., logging conversation ID)

	return &InvokeResponse{
		Status: 200,
		Body:   updatedAttachment,
	}
}

// projectSlugFor returns the slug of the project linked to conversationID
// when that is projectID, or "" otherwise.
func (h *CallbackHandler) projectSlugFor(ctx context.Context, conversationID, projectID string) string {
	store := h.getStore()
	if store == nil {
		return ""
	}
	link, err := store.GetChannelLink(ctx, stripThreadSuffix(conversationID))
	if err != nil || link == nil || link.ProjectID != projectID {
		return ""
	}
	return link.ProjectSlug
}
