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
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
)

// Review notices (ptone/scion#3229, design §8.2). When a review version of
// an artifact is finalized, the hub tells the artifact's owning agent with a
// system message (category artifact-review) that names the version and how
// to fetch it. The notice is the hub's own message: it is sent as "system",
// never as the reviewer, and it carries the reference in its body only (not
// in the artifacts metadata key, which is admitted only under a sender's
// credential). Whether the owner may read the version is decided when it
// runs the command, as for every reference. A user owner has no inbound
// channel for it; the artifact page shows the review-pending state instead.

// artifactReviewNoticeTimeout bounds the background delivery of one notice.
const artifactReviewNoticeTimeout = 30 * time.Second

// notifyArtifactReview is the artifact service's review notifier. It
// returns at once; delivery runs in the background so the finalize response
// does not wait on a broker.
func (s *Server) notifyArtifactReview(ctx context.Context, n artifacts.ReviewNotice) {
	if n.OwnerKind != artifacts.PrincipalKindAgent || n.OwnerRef == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), artifactReviewNoticeTimeout)
		defer cancel()
		if err := s.deliverArtifactReview(ctx, n); err != nil {
			slog.WarnContext(ctx, "artifacts: review notice not delivered",
				"artifact_id", n.ArtifactID, "seq", n.Seq, "agent_id", n.OwnerRef, "error", err)
		}
	}()
}

// deliverArtifactReview dispatches the review notice to the owning agent.
func (s *Server) deliverArtifactReview(ctx context.Context, n artifacts.ReviewNotice) error {
	agent, err := s.store.GetAgent(ctx, n.OwnerRef)
	if err != nil {
		return fmt.Errorf("owner agent: %w", err)
	}
	if reincarnationInFlight(agent) {
		return fmt.Errorf("owner agent is reincarnating")
	}
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return fmt.Errorf("no dispatcher available")
	}
	text := artifactReviewNoticeText(n)
	msg := messages.NewSystemMessage("system", "agent:"+agent.Slug, text, messages.SystemCategoryArtifactReview)
	msg.RecipientID = agent.ID
	if s.writeDenyEnabled() {
		var ts time.Time
		if t, err := time.Parse(time.RFC3339, msg.Timestamp); err == nil {
			ts = t
		}
		msg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{Msg: msg, CreatedAt: ts})
	}
	return dispatchWithBrokerRetry(ctx, dispatcher, agent, text, false, msg)
}

// artifactReviewNoticeText is the body of a review notice. It is built from
// the notice alone, with the same fetch-hint line as a message reference.
func artifactReviewNoticeText(n artifacts.ReviewNotice) string {
	ref := n.Ref()
	var b strings.Builder
	fmt.Fprintf(&b, "A review (v%d, by a %s) was published on your artifact; it is now the current version.\n", n.Seq, n.ReviewerKind)
	fmt.Fprintf(&b, "Artifact: v%d - scion artifact get %s\n", n.Seq, ref)
	fmt.Fprintf(&b, "Without the marks: scion artifact get %s --clean\n", ref)
	fmt.Fprintf(&b, "Apply or answer the marks, then publish the result as the next version: scion artifact publish <file> --version-of %s",
		artifacts.FormatRef(n.ArtifactID, 0))
	return b.String()
}
