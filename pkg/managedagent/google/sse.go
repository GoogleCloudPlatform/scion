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

package google

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
)

// SSE event type constants from the Google Managed Agents API.
const (
	EventInteractionCreated      = "interaction.created"
	EventInteractionCompleted    = "interaction.completed"
	EventInteractionStatusUpdate = "interaction.status_update"
	EventStepStart               = "step.start"
	EventStepDelta               = "step.delta"
	EventStepStop                = "step.stop"
	EventError                   = "error"
	EventDone                    = "done"
)

// SSEEvent represents a parsed Server-Sent Event.
type SSEEvent struct {
	Type string
	ID   string
	Data json.RawMessage
}

// SSEReader reads and parses Server-Sent Events from an io.Reader.
// It uses bufio.Reader internally so that Buffered() can be called to
// retrieve any data read-ahead from the underlying stream.
type SSEReader struct {
	br *bufio.Reader
}

// NewSSEReader creates a new SSE parser that reads from r.
func NewSSEReader(r io.Reader) *SSEReader {
	return &SSEReader{
		br: bufio.NewReader(r),
	}
}

// Buffered returns the number of bytes buffered but not yet consumed.
func (r *SSEReader) Buffered() int {
	return r.br.Buffered()
}

// BufferedReader returns the underlying bufio.Reader. Use this to
// recover any read-ahead data after consuming specific events.
func (r *SSEReader) BufferedReader() *bufio.Reader {
	return r.br
}

func (r *SSEReader) readLine() (string, error) {
	line, err := r.br.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	return line, err
}

// Next reads the next SSE event from the stream. Returns io.EOF when
// the stream ends or a "done" event is received.
func (r *SSEReader) Next() (*SSEEvent, error) {
	var event SSEEvent
	var dataLines []string
	hasFields := false

	for {
		line, err := r.readLine()

		if line == "" && err != nil {
			if hasFields {
				if len(dataLines) > 0 {
					event.Data = json.RawMessage(strings.Join(dataLines, "\n"))
				}
				return &event, nil
			}
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, fmt.Errorf("reading SSE stream: %w", err)
		}

		if line == "" {
			if !hasFields {
				continue
			}
			if len(dataLines) > 0 {
				event.Data = json.RawMessage(strings.Join(dataLines, "\n"))
			}
			if event.Type == EventDone {
				return nil, io.EOF
			}
			return &event, nil
		}

		if strings.HasPrefix(line, ":") {
			continue
		}

		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		hasFields = true

		switch field {
		case "event":
			event.Type = value
		case "data":
			dataLines = append(dataLines, value)
		case "id":
			event.ID = value
		case "retry":
			log.Printf("SSE: server sent retry: %s (ignored)", value)
		}

		if err != nil {
			if len(dataLines) > 0 {
				event.Data = json.RawMessage(strings.Join(dataLines, "\n"))
			}
			return &event, nil
		}
	}
}

// ParseStepStart parses the data payload of a step.start event.
func ParseStepStart(data json.RawMessage) (*StepStartEvent, error) {
	var e StepStartEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("parsing step.start: %w", err)
	}
	return &e, nil
}

// ParseStepDelta parses the data payload of a step.delta event.
func ParseStepDelta(data json.RawMessage) (*StepDeltaEvent, error) {
	var e StepDeltaEvent
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, fmt.Errorf("parsing step.delta: %w", err)
	}
	return &e, nil
}

// ParseInteraction parses the data payload of interaction.created or
// interaction.completed events.
func ParseInteraction(data json.RawMessage) (*Interaction, error) {
	var i Interaction
	if err := json.Unmarshal(data, &i); err != nil {
		return nil, fmt.Errorf("parsing interaction: %w", err)
	}
	return &i, nil
}
