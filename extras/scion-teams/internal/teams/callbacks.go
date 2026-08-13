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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// CallbackHandler processes Adaptive Card Action.Submit invoke activities.
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
		// The Teams SDK wraps the Action.Submit data inside {"action": {"data": ...}}
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
		return h.respondWithUpdatedCard(activity, "Store not initialized."), nil
	}

	pending, err := store.GetPendingAskUser(ctx, requestID)
	if err != nil {
		h.log.Error("Failed to look up pending ask-user", "request_id", requestID, "error", err)
		return h.respondWithUpdatedCard(activity, "An error occurred processing your response."), nil
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

	// When the choice is "custom", use the text typed into the Input.Text field.
	responseText := choice
	if choice == "custom" {
		if replyText, ok := data["reply_text"].(string); ok && replyText != "" {
			responseText = replyText
		}
	}

	// Deliver the response to the hub.
	if err := h.deliverAskUserResponse(ctx, activity, pending, responseText); err != nil {
		h.log.Error("Failed to deliver ask-user response to hub", "error", err)
		return h.respondWithUpdatedCard(activity, "Failed to deliver your response. Please try again."), nil
	}

	// Mark as responded.
	if err := store.MarkAskUserResponded(ctx, requestID); err != nil {
		h.log.Warn("Failed to mark ask-user as responded", "error", err)
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
		return h.respondWithUpdatedCard(activity, "Store not initialized."), nil
	}

	pending, err := store.GetPendingAskUser(ctx, requestID)
	if err != nil || pending == nil {
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
			ActionExecute{Type: "Action.Execute", Title: "Send Reply", Style: "positive",
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
		return h.respondWithUpdatedCard(activity, "Store not initialized."), nil
	}

	// Normalize conversation ID — strip thread suffix for consistent lookups.
	convID := stripThreadSuffix(activity.Conversation.ID)

	// Check if already linked.
	existing, err := store.GetChannelLink(ctx, convID)
	if err != nil {
		h.log.Error("Failed to check existing channel link", "error", err)
		return h.respondWithUpdatedCard(activity, "An error occurred while checking existing link."), nil
	}
	if existing != nil {
		return h.respondWithUpdatedCard(activity,
			fmt.Sprintf("This conversation is already linked to project **%s**.", existing.ProjectSlug)), nil
	}

	if projectID == "" {
		projectID = projectSlug
	}

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
		ConversationID:     convID,
		TeamID:             teamID,
		ProjectID:          projectID,
		ProjectSlug:        projectSlug,
		LinkedBy:           linkedBy,
		LinkedAt:           time.Now(),
		Active:             true,
		ShowAssistantReply: true,
	}

	if err := store.CreateChannelLink(ctx, link); err != nil {
		h.log.Error("Failed to create channel link from card", "error", err)
		return h.respondWithUpdatedCard(activity, "Failed to link conversation. Please try again."), nil
	}

	return h.respondWithUpdatedCard(activity,
		fmt.Sprintf("✅ Conversation linked to project **%s**.", projectSlug)), nil
}

// --- Helpers ---

// deliverAskUserResponse sends the user's choice to the hub via inbound delivery.
func (h *CallbackHandler) deliverAskUserResponse(ctx context.Context, activity *Activity, pending *PendingAskUser, responseText string) error {
	hubClient := h.broker.hubClient
	if hubClient == nil {
		return fmt.Errorf("hub client not configured")
	}

	// Resolve user identity.
	senderID := activity.From.AadObjectID
	if senderID == "" {
		senderID = activity.From.ID
	}
	senderName := activity.From.Name

	// Try to resolve Teams user to Scion identity.
	store := h.getStore()
	if store != nil {
		mapping, err := store.GetUserMapping(ctx, senderID)
		if err == nil && mapping != nil && mapping.ScionEmail != "" {
			senderID = "user:" + mapping.ScionEmail
		} else {
			senderID = "teams:" + senderID
		}
	} else {
		senderID = "teams:" + senderID
	}

	recipient := "agent:" + pending.AgentSlug

	msg := &messages.StructuredMessage{
		Version:   messages.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Sender:    senderName,
		SenderID:  senderID,
		Recipient: recipient,
		Msg:       responseText,
		Type:      messages.TypeInstruction,
		Channel:   "teams",
		ThreadID:  pending.ConversationID,
		Metadata: map[string]string{
			"teams_conversation_id": pending.ConversationID,
			"project_id":            pending.ProjectID,
			"ask_request_id":        pending.RequestID,
		},
	}

	// Build the topic from project and agent.
	topic := pending.ProjectID
	if pending.AgentSlug != "" {
		topic = topic + "." + pending.AgentSlug
	}

	return hubClient.DeliverInbound(ctx, topic, msg)
}

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
