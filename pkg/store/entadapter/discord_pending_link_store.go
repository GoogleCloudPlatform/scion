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

package entadapter

import (
	"context"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/discordpendinglink"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// DiscordPendingLinkStore implements store.DiscordPendingLinkStore using the
// Ent ORM.
type DiscordPendingLinkStore struct {
	client *ent.Client
}

// NewDiscordPendingLinkStore creates a new Ent-backed DiscordPendingLinkStore.
func NewDiscordPendingLinkStore(client *ent.Client) *DiscordPendingLinkStore {
	return &DiscordPendingLinkStore{client: client}
}

func entDiscordPendingLinkToStore(e *ent.DiscordPendingLink) *store.DiscordPendingLink {
	return &store.DiscordPendingLink{
		ID:            e.ID.String(),
		Code:          e.Code,
		DiscordUserID: e.DiscordUserID,
		Status:        e.Status,
		UserID:        e.UserID,
		UserEmail:     e.UserEmail,
		ExpiresAt:     e.ExpiresAt,
		CreatedAt:     e.CreatedAt,
	}
}

func (s *DiscordPendingLinkStore) CreateDiscordPendingLink(ctx context.Context, link *store.DiscordPendingLink) error {
	id := uuid.New()
	if link.ID != "" {
		var err error
		id, err = parseUUID(link.ID)
		if err != nil {
			return err
		}
	}
	if link.CreatedAt.IsZero() {
		link.CreatedAt = time.Now()
	}

	if err := s.client.DiscordPendingLink.Create().
		SetID(id).
		SetCode(link.Code).
		SetDiscordUserID(link.DiscordUserID).
		SetStatus(link.Status).
		SetUserID(link.UserID).
		SetUserEmail(link.UserEmail).
		SetExpiresAt(link.ExpiresAt).
		SetCreatedAt(link.CreatedAt).
		Exec(ctx); err != nil {
		return mapError(err)
	}
	link.ID = id.String()
	return nil
}

func (s *DiscordPendingLinkStore) GetDiscordPendingLinkByCode(ctx context.Context, code string) (*store.DiscordPendingLink, error) {
	e, err := s.client.DiscordPendingLink.Query().
		Where(discordpendinglink.CodeEQ(code)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entDiscordPendingLinkToStore(e), nil
}

func (s *DiscordPendingLinkStore) GetDiscordPendingLinkByDiscordUser(ctx context.Context, discordUserID string) (*store.DiscordPendingLink, error) {
	e, err := s.client.DiscordPendingLink.Query().
		Where(discordpendinglink.DiscordUserIDEQ(discordUserID)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entDiscordPendingLinkToStore(e), nil
}

func (s *DiscordPendingLinkStore) UpdateDiscordPendingLink(ctx context.Context, link *store.DiscordPendingLink) error {
	uid, err := parseUUID(link.ID)
	if err != nil {
		return err
	}
	n, err := s.client.DiscordPendingLink.Update().
		Where(
			discordpendinglink.IDEQ(uid),
			discordpendinglink.StatusEQ("pending"),
		).
		SetStatus(link.Status).
		SetUserID(link.UserID).
		SetUserEmail(link.UserEmail).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	if n == 0 {
		return store.ErrVersionConflict
	}
	return nil
}

func (s *DiscordPendingLinkStore) DeleteDiscordPendingLink(ctx context.Context, code string) error {
	_, err := s.client.DiscordPendingLink.Delete().
		Where(discordpendinglink.CodeEQ(code)).
		Exec(ctx)
	return err
}

func (s *DiscordPendingLinkStore) DeleteDiscordPendingLinksByDiscordUser(ctx context.Context, discordUserID string) error {
	_, err := s.client.DiscordPendingLink.Delete().
		Where(discordpendinglink.DiscordUserIDEQ(discordUserID)).
		Exec(ctx)
	return err
}

func (s *DiscordPendingLinkStore) DeleteExpiredDiscordPendingLinks(ctx context.Context) (int, error) {
	n, err := s.client.DiscordPendingLink.Delete().
		Where(discordpendinglink.ExpiresAtLT(time.Now())).
		Exec(ctx)
	return n, err
}
