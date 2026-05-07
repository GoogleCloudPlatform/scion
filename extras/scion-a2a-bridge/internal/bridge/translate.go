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

package bridge

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/google/uuid"
)

// A2A task states (matching the A2A protocol spec).
const (
	TaskStateSubmitted     = "submitted"
	TaskStateWorking       = "working"
	TaskStateCompleted     = "completed"
	TaskStateFailed        = "failed"
	TaskStateCanceled      = "canceled"
	TaskStateInputRequired = "input-required"
	TaskStateRejected      = "rejected"
)

// A2A message roles.
const (
	RoleUser  = "user"
	RoleAgent = "agent"
)

// Part represents a typed content part in an A2A message.
type Part struct {
	Kind      string      `json:"kind,omitempty"`
	Text      string      `json:"text,omitempty"`
	MediaType string      `json:"mediaType,omitempty"`
	URL       string      `json:"url,omitempty"`
	Data      interface{} `json:"data,omitempty"`
}

// Message represents an A2A protocol message.
type Message struct {
	MessageID string `json:"messageId"`
	Role      string `json:"role"`
	Parts     []Part `json:"parts"`
}

// Artifact represents an A2A output artifact.
type Artifact struct {
	ArtifactID string `json:"artifactId"`
	Name       string `json:"name,omitempty"`
	Parts      []Part `json:"parts"`
	LastChunk  bool   `json:"lastChunk"`
}

// TaskStatus represents the status of an A2A task.
type TaskStatus struct {
	State   string   `json:"state"`
	Message *Message `json:"message,omitempty"`
}

// TaskResult represents an A2A task with its current state.
type TaskResult struct {
	ID        string     `json:"id"`
	ContextID string     `json:"contextId"`
	Status    TaskStatus `json:"status"`
	Artifacts []Artifact `json:"artifacts,omitempty"`
}

// MapActivityToTaskState maps a Scion agent activity string to an A2A task state.
func MapActivityToTaskState(activity string) string {
	switch strings.ToUpper(activity) {
	case "IDLE":
		return TaskStateCompleted
	case "THINKING", "EXECUTING":
		return TaskStateWorking
	case "WAITING_FOR_INPUT":
		return TaskStateInputRequired
	case "COMPLETED":
		return TaskStateCompleted
	case "ERROR":
		return TaskStateFailed
	case "STALLED", "LIMITS_EXCEEDED", "OFFLINE":
		return TaskStateFailed
	default:
		return TaskStateWorking
	}
}

// IsTerminalState returns true if the task state is a terminal state.
func IsTerminalState(state string) bool {
	switch state {
	case TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected:
		return true
	default:
		return false
	}
}

// TranslateA2AToScion converts A2A message parts into a Scion StructuredMessage.
func TranslateA2AToScion(parts []Part) *messages.StructuredMessage {
	var textContent strings.Builder
	var attachments []string

	for _, part := range parts {
		switch {
		case part.Text != "":
			if textContent.Len() > 0 {
				textContent.WriteString("\n")
			}
			textContent.WriteString(part.Text)
		case part.URL != "":
			attachments = append(attachments, part.URL)
		case part.Data != nil:
			jsonBytes, err := json.Marshal(part.Data)
			if err == nil {
				if textContent.Len() > 0 {
					textContent.WriteString("\n")
				}
				textContent.WriteString(string(jsonBytes))
			}
		}
	}

	msg := textContent.String()
	if msg == "" {
		if len(attachments) > 0 {
			msg = "[A2A request with attachments only]"
		} else {
			msg = "[empty A2A request]"
		}
	}

	return &messages.StructuredMessage{
		Version:     1,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
		Msg:         msg,
		Type:        messages.TypeInstruction,
		Attachments: attachments,
	}
}

// TranslateScionToA2A converts a Scion StructuredMessage into an A2A Message and optional Artifacts.
func TranslateScionToA2A(msg *messages.StructuredMessage) (Message, []Artifact) {
	parts := []Part{{Text: msg.Msg, MediaType: "text/plain"}}

	for _, att := range msg.Attachments {
		parts = append(parts, Part{URL: att})
	}

	message := Message{
		MessageID: uuid.New().String(),
		Role:      RoleAgent,
		Parts:     parts,
	}

	var artifacts []Artifact
	switch msg.Type {
	case "", messages.TypeInstruction, messages.TypeAssistantReply:
		artifacts = append(artifacts, Artifact{
			ArtifactID: uuid.New().String(),
			Parts:      parts,
			LastChunk:  true,
		})
	}

	return message, artifacts
}
