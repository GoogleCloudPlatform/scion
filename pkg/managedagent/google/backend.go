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
	"context"
	"fmt"
	"io"

	"github.com/GoogleCloudPlatform/scion/pkg/managedagent"
)

// BackendConfig holds configuration for the Google managed agent backend.
type BackendConfig struct {
	APIKey    string
	BaseAgent string
}

// Backend implements managedagent.ManagedAgentBackend using the Google API.
type Backend struct {
	client    *Client
	baseAgent string
}

// NewBackend creates a new Google managed agent backend.
func NewBackend(cfg BackendConfig) *Backend {
	return &Backend{
		client:    NewClient(cfg.APIKey),
		baseAgent: cfg.BaseAgent,
	}
}

func (b *Backend) Name() string {
	return "google"
}

func (b *Backend) CreateAgent(ctx context.Context, cfg managedagent.CreateAgentConfig) (string, error) {
	req := &CreateAgentRequest{
		ID:                cfg.ID,
		BaseAgent:         cfg.BaseAgent,
		SystemInstruction: cfg.SystemInstruction,
		Description:       cfg.Description,
		Tools:             convertToolConfigs(cfg.Tools),
		BaseEnvironment:   convertEnvironmentConfig(cfg.Environment),
	}
	if req.BaseAgent == "" {
		req.BaseAgent = b.baseAgent
	}

	agent, err := b.client.CreateAgent(ctx, req)
	if err != nil {
		return "", fmt.Errorf("google backend: %w", err)
	}
	return agent.ID, nil
}

func (b *Backend) DeleteAgent(ctx context.Context, cloudAgentID string) error {
	if err := b.client.DeleteAgent(ctx, cloudAgentID); err != nil {
		return fmt.Errorf("google backend: %w", err)
	}
	return nil
}

func (b *Backend) CreateInteraction(ctx context.Context, req managedagent.InteractionRequest) (*managedagent.InteractionHandle, error) {
	apiReq := &CreateInteractionRequest{
		Agent:                 req.CloudAgentID,
		Input:                 req.Input,
		PreviousInteractionID: req.PreviousInteractionID,
		Stream:                req.Stream,
		Background:            req.Background,
		SystemInstruction:     req.SystemInstruction,
		Tools:                 convertToolConfigs(req.Tools),
	}

	if req.EnvironmentID != "" {
		apiReq.Environment = &Environment{Type: req.EnvironmentID}
	} else if req.Environment != nil {
		apiReq.Environment = convertEnvironmentConfig(req.Environment)
	}

	if req.Stream {
		stream, err := b.client.CreateInteractionStream(ctx, apiReq)
		if err != nil {
			return nil, fmt.Errorf("google backend: %w", err)
		}
		return &managedagent.InteractionHandle{
			Status:      managedagent.StatusInProgress,
			EventStream: stream,
		}, nil
	}

	interaction, err := b.client.CreateInteraction(ctx, apiReq)
	if err != nil {
		return nil, fmt.Errorf("google backend: %w", err)
	}

	return convertInteractionToHandle(interaction), nil
}

func (b *Backend) GetInteraction(ctx context.Context, interactionID string) (*managedagent.InteractionState, error) {
	interaction, err := b.client.GetInteraction(ctx, interactionID)
	if err != nil {
		return nil, fmt.Errorf("google backend: %w", err)
	}

	return &managedagent.InteractionState{
		InteractionID: interaction.ID,
		Status:        managedagent.InteractionStatus(interaction.Status),
		Steps:         convertSteps(interaction.Steps),
		OutputText:    interaction.OutputText,
		EnvironmentID: interaction.EnvironmentID,
		Usage:         convertUsage(interaction.Usage),
	}, nil
}

func (b *Backend) CancelInteraction(ctx context.Context, interactionID string) error {
	if err := b.client.CancelInteraction(ctx, interactionID); err != nil {
		return fmt.Errorf("google backend: %w", err)
	}
	return nil
}

func (b *Backend) StreamInteraction(ctx context.Context, interactionID string, lastEventID string) (io.ReadCloser, error) {
	stream, err := b.client.GetInteractionStream(ctx, interactionID, lastEventID)
	if err != nil {
		return nil, fmt.Errorf("google backend: %w", err)
	}
	return stream, nil
}

func convertInteractionToHandle(i *Interaction) *managedagent.InteractionHandle {
	return &managedagent.InteractionHandle{
		InteractionID: i.ID,
		EnvironmentID: i.EnvironmentID,
		Status:        managedagent.InteractionStatus(i.Status),
		Steps:         convertSteps(i.Steps),
		OutputText:    i.OutputText,
		Usage:         convertUsage(i.Usage),
	}
}

func convertSteps(steps []InteractionStep) []managedagent.Step {
	if len(steps) == 0 {
		return nil
	}
	result := make([]managedagent.Step, len(steps))
	for i, s := range steps {
		result[i] = managedagent.Step{
			Type:      s.Type,
			Text:      s.Text,
			Arguments: s.ArgumentsDelta,
			ToolName:  s.ToolName,
		}
	}
	return result
}

func convertUsage(u *InteractionUsage) *managedagent.UsageInfo {
	if u == nil {
		return nil
	}
	return &managedagent.UsageInfo{
		TotalInputTokens:  u.TotalInputTokens,
		TotalOutputTokens: u.TotalOutputTokens,
		TotalTokens:       u.TotalTokens,
	}
}

func convertToolConfigs(tools []managedagent.ToolConfig) []AgentTool {
	if len(tools) == 0 {
		return nil
	}
	result := make([]AgentTool, len(tools))
	for i, t := range tools {
		result[i] = AgentTool{
			Type:       t.Type,
			Name:       t.Name,
			Parameters: t.Parameters,
		}
	}
	return result
}

func convertEnvironmentConfig(env *managedagent.EnvironmentConfig) *Environment {
	if env == nil {
		return nil
	}
	e := &Environment{
		Type: env.Type,
	}
	if len(env.Sources) > 0 {
		e.Sources = make([]SourceConfig, len(env.Sources))
		for i, s := range env.Sources {
			e.Sources[i] = SourceConfig{
				Type:   s.Type,
				URI:    s.URI,
				Branch: s.Branch,
				Path:   s.Path,
			}
		}
	}
	if env.Network != nil {
		e.Network = &NetworkConfig{
			Disabled: env.Network.Disabled,
		}
		if len(env.Network.Allowlist) > 0 {
			e.Network.Allowlist = make([]AllowlistEntry, len(env.Network.Allowlist))
			for i, a := range env.Network.Allowlist {
				e.Network.Allowlist[i] = AllowlistEntry{
					Domain:  a.Domain,
					Headers: a.Headers,
				}
			}
		}
	}
	return e
}
