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

package hub

import (
	"context"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// fanOutAgentMentions resolves and delivers @-mentions for direct unit tests.
// Production send paths call resolveMentionCopyTargets once before the policy
// gate and then deliverResolvedAgentMentions with that exact resolved plan.
func (s *Server) fanOutAgentMentions(ctx context.Context, in agentMentionFanoutInput) []messages.MentionResult {
	resolved := s.resolveMentionCopyTargets(ctx, in)
	return s.deliverResolvedAgentMentions(ctx, in, resolved)
}
