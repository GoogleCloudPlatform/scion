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

package agent

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// PreDelivery is work one message needs done in the agent's container
// immediately before its text is delivered (ptone/scion#4596: writing the
// message's attachments into the agent's home). The manager runs it once
// per message, in message order, right before the text is injected and
// outside any retry of the injection.
type PreDelivery interface {
	// BeforeText does the work in the container containerID of runtime rt
	// and returns text to append after the message's own text ("" for
	// none). rt is nil when the container could not be found; the work
	// then fails and the returned text says so.
	BeforeText(ctx context.Context, rt runtime.Runtime, containerID string) string

	// Discard releases what the work holds when the message is dropped
	// without being delivered (for example sent after the buffer closed).
	Discard()
}

type preDeliveryKey struct{}

// WithPreDelivery returns a context carrying pd. When passed to
// AgentManager.Message, pd runs right before the message's text is
// delivered. Only the value is read from ctx.
func WithPreDelivery(ctx context.Context, pd PreDelivery) context.Context {
	if pd == nil {
		return ctx
	}
	return context.WithValue(ctx, preDeliveryKey{}, pd)
}

// PreDeliveryFromContext returns the PreDelivery attached with
// WithPreDelivery, or nil.
func PreDeliveryFromContext(ctx context.Context) PreDelivery {
	if ctx == nil {
		return nil
	}
	pd, _ := ctx.Value(preDeliveryKey{}).(PreDelivery)
	return pd
}

// runPreDelivery runs pd for the message to agentID in projectID, against
// the agent's resolved container, and returns the text to append.
func (m *AgentManager) runPreDelivery(ctx context.Context, agentID, projectID string, pd PreDelivery) string {
	if pd == nil {
		return ""
	}
	// The same target resolution and ambiguity refusal as the delivery
	// itself: an ambiguous name on an owned manager writes nothing.
	target, matches, err := m.findMessageTargetCounted(ctx, agentID, projectID)
	if err != nil || target == nil || m.Runtime == nil || m.refuseAmbiguousOwnedTarget(agentID, projectID, matches) != nil {
		return pd.BeforeText(ctx, nil, "")
	}
	return pd.BeforeText(ctx, m.Runtime, target.ContainerID)
}
