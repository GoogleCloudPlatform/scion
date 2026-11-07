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
	"errors"
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// The hub-authored user notification writer (ptone/scion#5145): one row
// addressed to one user, about one resource, with a message the hub
// composes from a fixed template. The Ask-for-review notice
// (artifacts_grant_notify.go) writes through it, and so will the review
// notices of ptone/scion#7135. The pattern is schedule_blocked.go's: a
// user-subscriber row with no subscription, published on the user's own
// subject only.

// userNoticeWriteTimeout bounds one notice write. It runs on a context
// detached from the request, so a client that goes away does not cut a
// notice short after its grant committed.
const userNoticeWriteTimeout = 5 * time.Second

// userNoticeKinds is the closed set of (Kind, ResourceType) pairs the
// writer accepts. A new hub-authored notice adds its pkg/store constants
// here; no request field ever reaches these columns.
var userNoticeKinds = map[string]string{
	store.NotificationKindArtifactGrant: store.NotificationResourceArtifact,
}

// userNotice is a hub-authored notification to one user. Kind, ResourceType
// and Status must be pkg/store constants. Message comes from the kind's
// fixed template (for artifact grants, grantNoticeMessage) filled with ids,
// enums and an already sanitised label only, so no request text reaches
// the row (K14/K19).
type userNotice struct {
	Kind, ResourceType, ResourceID, Status string
	ProjectID, SubscriberUserID, AgentID   string // ProjectID or AgentID "" -> nil UUID
	Message                                string // from the per-kind template only
}

// errUserNoticeInvalid refuses a notice outside the writer's closed set.
var errUserNoticeInvalid = errors.New("hub: user notice outside the writer's closed set")

// writeUserNotification stores n as a notification to its subscriber and,
// only once it is stored, publishes it on that user's own subject. It never
// publishes the broadcast notification event (O3). It runs synchronously
// on a context detached from ctx with a 5 s timeout, and returns the store
// error, so the caller can say the notice was not sent.
func (s *Server) writeUserNotification(ctx context.Context, n userNotice) error {
	if rt, ok := userNoticeKinds[n.Kind]; !ok || rt != n.ResourceType || n.Status == "" || n.Message == "" ||
		n.SubscriberUserID == "" || !canonicalUUID(n.ResourceID) {
		return fmt.Errorf("%w: kind %q resource %q", errUserNoticeInvalid, n.Kind, n.ResourceType)
	}
	if s.store == nil {
		return errors.New("hub: no store for user notices")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), userNoticeWriteTimeout)
	defer cancel()
	notif := &store.Notification{
		ID:             api.NewUUID(),
		SubscriptionID: uuid.Nil.String(),
		AgentID:        uuidOrNil(n.AgentID),
		ProjectID:      uuidOrNil(n.ProjectID),
		SubscriberType: store.SubscriberTypeUser,
		SubscriberID:   n.SubscriberUserID,
		Status:         n.Status,
		Message:        n.Message,
		CreatedAt:      time.Now(),
		Kind:           n.Kind,
		ResourceType:   n.ResourceType,
		ResourceID:     n.ResourceID,
	}
	if err := s.store.CreateNotification(ctx, notif); err != nil {
		return err
	}
	if s.events != nil {
		s.events.PublishUserNotification(ctx, notif)
	}
	return nil
}

// canonicalUUID reports whether id is a UUID in its canonical lower-case
// 36-character form.
func canonicalUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	u, err := uuid.Parse(id)
	return err == nil && u.String() == id
}

// uuidOrNil returns id when it is a canonical UUID and the nil UUID
// otherwise.
func uuidOrNil(id string) string {
	if canonicalUUID(id) {
		return id
	}
	return uuid.Nil.String()
}
