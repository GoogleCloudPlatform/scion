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

package hubclient

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

const (
	triggerPresetsBasePath    = "/api/v1/trigger-presets"
	triggerPresetsCompilePath = "/api/v1/trigger-presets/compile"
)

// TriggerPresetService provides read and server-side compilation operations for
// trigger rule presets.
type TriggerPresetService interface {
	// List returns trigger preset schemas, optionally filtered by source (for example "github").
	List(ctx context.Context, source string) ([]TriggerPreset, error)

	// Compile validates preset form fields and compiles them into match types,
	// ordered CEL rules, subject policy, canonicalized form state, and a
	// suggested action.
	Compile(ctx context.Context, req *CompileTriggerPresetRequest) (*CompiledTriggerPreset, error)
}

type triggerPresetService struct {
	c *client
}

// TriggerPreset describes one server-side trigger rule preset schema.
type TriggerPreset struct {
	ID          string               `json:"id"`
	Source      string               `json:"source"`
	Version     int                  `json:"version"`
	Title       string               `json:"title"`
	Description string               `json:"description"`
	Fields      []TriggerPresetField `json:"fields"`
	ActionKinds []string             `json:"action_kinds"`
}

// TriggerPresetField describes one configurable field in a TriggerPreset.
type TriggerPresetField struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Enum     []string `json:"enum,omitempty"`
	Default  any      `json:"default,omitempty"`
	Required bool     `json:"required"`
	Help     string   `json:"help"`
}

// CompileTriggerPresetRequest is the request body for TriggerPresetService.Compile.
type CompileTriggerPresetRequest struct {
	Preset string         `json:"preset"`
	Fields map[string]any `json:"fields,omitempty"`
}

// CompiledTriggerPreset is the compiled output returned by TriggerPresetService.Compile.
type CompiledTriggerPreset struct {
	Match            CompiledTriggerMatch     `json:"match"`
	Subject          CompiledTriggerSubject   `json:"subject"`
	Form             CompiledTriggerForm      `json:"form"`
	SuggestedAction  *SuggestedTriggerAction  `json:"suggested_action,omitempty"`
	SuggestedActions []SuggestedTriggerAction `json:"suggested_actions,omitempty"`
}

// CompiledTriggerMatch holds the event-type filter and ordered match rule list.
type CompiledTriggerMatch struct {
	Types []string              `json:"types"`
	Rules []CompiledTriggerRule `json:"rules"`
}

// CompiledTriggerRule is one dialect-tagged rule element in CompiledTriggerMatch.Rules.
type CompiledTriggerRule struct {
	Dialect string `json:"dialect"`
	Expr    string `json:"expr"`
}

// CompiledTriggerSubject holds the compiled subject policy.
type CompiledTriggerSubject struct {
	Mode           string `json:"mode"`
	CoalesceWindow string `json:"coalesce_window,omitempty"`
	Expr           string `json:"expr,omitempty"`
}

// CompiledTriggerForm is the canonicalized form state for Match.Form.
type CompiledTriggerForm struct {
	Preset  string         `json:"preset"`
	Version int            `json:"version"`
	Fields  map[string]any `json:"fields"`
}

// SuggestedTriggerAckComment configures the post-fire GitHub acknowledgement comment.
type SuggestedTriggerAckComment struct {
	Body string `json:"body"`
}

// SuggestedTriggerAction holds the suggested dispatch_agent action emitted by a preset.
type SuggestedTriggerAction struct {
	Type          string                      `json:"type"`
	AgentName     string                      `json:"agentName,omitempty"`
	Template      string                      `json:"template"`
	Task          string                      `json:"task"`
	Branch        string                      `json:"branch,omitempty"`
	Labels        map[string]string           `json:"labels,omitempty"`
	Ephemeral     bool                        `json:"ephemeral"`
	GitHubToken   string                      `json:"githubToken"`
	ReuseMessage  string                      `json:"reuseMessage,omitempty"`
	AckComment    *SuggestedTriggerAckComment `json:"ackComment,omitempty"`
	UntrustedVars []string                    `json:"untrustedVars"`
}

func (s *triggerPresetService) List(ctx context.Context, source string) ([]TriggerPreset, error) {
	query := url.Values{}
	if trimmed := strings.TrimSpace(source); trimmed != "" {
		query.Set("source", trimmed)
	}
	resp, err := s.c.getWithQuery(ctx, triggerPresetsBasePath, query, nil)
	if err != nil {
		return nil, err
	}
	presets, err := apiclient.DecodeRequired[[]TriggerPreset](resp)
	if err != nil {
		return nil, err
	}
	return *presets, nil
}

func (s *triggerPresetService) Compile(ctx context.Context, req *CompileTriggerPresetRequest) (*CompiledTriggerPreset, error) {
	if req == nil {
		return nil, errors.New("compile trigger preset request is required")
	}
	resp, err := s.c.post(ctx, triggerPresetsCompilePath, req, nil)
	if err != nil {
		return nil, err
	}
	return apiclient.DecodeRequired[CompiledTriggerPreset](resp)
}
