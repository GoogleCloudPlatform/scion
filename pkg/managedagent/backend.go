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

package managedagent

import (
	"context"
	"io"
)

// ManagedAgentBackend is the cloud-provider abstraction for managed agent services.
// Each backend (Google, etc.) implements this interface.
type ManagedAgentBackend interface {
	// Name returns the backend identifier (e.g., "google").
	Name() string

	// CreateAgent creates a persistent agent configuration on the cloud service.
	// Returns the cloud-assigned agent ID.
	CreateAgent(ctx context.Context, cfg CreateAgentConfig) (string, error)

	// DeleteAgent removes the agent configuration from the cloud service.
	DeleteAgent(ctx context.Context, cloudAgentID string) error

	// CreateInteraction starts a new interaction (turn) with the agent.
	CreateInteraction(ctx context.Context, req InteractionRequest) (*InteractionHandle, error)

	// GetInteraction retrieves the current state of an interaction.
	GetInteraction(ctx context.Context, interactionID string) (*InteractionState, error)

	// CancelInteraction cancels a running interaction.
	CancelInteraction(ctx context.Context, interactionID string) error

	// StreamInteraction opens an SSE stream for a running or completed interaction.
	// If lastEventID is non-empty, resumes from that point.
	StreamInteraction(ctx context.Context, interactionID string, lastEventID string) (io.ReadCloser, error)
}
