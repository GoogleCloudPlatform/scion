// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package entadapter

import (
	"context"
	"fmt"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/conversation"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/message"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/predicate"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/user"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// batchLookupSize caps how many keys one batched lookup binds into a single
// IN (...) list, keeping every statement well inside the bind-parameter
// limits of SQLite and Postgres. Larger key sets are split across several
// statements. A var so tests can exercise the split with small inputs.
var batchLookupSize = 500

// chunkKeys splits keys into slices of at most size, preserving order.
func chunkKeys[T any](keys []T, size int) [][]T {
	if len(keys) == 0 {
		return nil
	}
	if size <= 0 {
		return [][]T{keys}
	}
	chunks := make([][]T, 0, (len(keys)+size-1)/size)
	for i := 0; i < len(keys); i += size {
		end := min(i+size, len(keys))
		chunks = append(chunks, keys[i:end])
	}
	return chunks
}

// GetUsersByIDs retrieves users by a list of IDs. Missing and malformed IDs
// are skipped, as GetUser reports them as not found.
func (s *UserStore) GetUsersByIDs(ctx context.Context, ids []string) (map[string]*store.User, error) {
	uids := parseUUIDList(ids)
	result := make(map[string]*store.User, len(uids))
	for _, batch := range chunkKeys(uids, batchLookupSize) {
		users, err := s.client.User.Query().Where(user.IDIn(batch...)).All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, u := range users {
			su := entUserToStore(u)
			result[su.ID] = su
		}
	}
	return result, nil
}

// GetAgentsByIDsIncludingDeleted retrieves agents by a list of IDs, including
// soft-deleted agents, matching what GetAgent returns for each ID.
func (s *AgentStore) GetAgentsByIDsIncludingDeleted(ctx context.Context, ids []string) (map[string]*store.Agent, error) {
	uids := parseUUIDList(ids)
	result := make(map[string]*store.Agent, len(uids))
	for _, batch := range chunkKeys(uids, batchLookupSize) {
		agents, err := s.client.Agent.Query().Where(agent.IDIn(batch...)).All(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, a := range agents {
			sa := entAgentToStore(a)
			result[sa.ID] = sa
		}
	}
	return result, nil
}

// GetConversationsByExternalRefs looks up active conversations on one surface
// by external ref. An empty surface means "native", as in
// GetConversationByExternalRef.
func (s *ConversationStore) GetConversationsByExternalRefs(ctx context.Context, surface string, externalRefs []string) (map[string]*store.Conversation, error) {
	if surface == "" {
		surface = "native"
	}
	refs := make([]string, 0, len(externalRefs))
	for _, ref := range externalRefs {
		if ref != "" {
			refs = append(refs, ref)
		}
	}
	result := make(map[string]*store.Conversation, len(refs))
	for _, batch := range chunkKeys(refs, batchLookupSize) {
		convs, err := s.client.Conversation.Query().
			Where(
				conversation.SurfaceEQ(conversation.Surface(surface)),
				conversation.ExternalRefIn(batch...),
				conversation.DeletedAtIsNil(),
			).
			All(ctx)
		if err != nil {
			return nil, err
		}
		for _, c := range convs {
			sc := entConversationToStore(c)
			result[sc.ExternalRef] = sc
		}
	}
	return result, nil
}

// LatestMessagesByThreadIDs returns the newest matching message per thread.
func (s *MessageStore) LatestMessagesByThreadIDs(ctx context.Context, threadIDs []string, opts store.LatestMessageOptions) (map[string]*store.Message, error) {
	keys := make([]string, 0, len(threadIDs))
	for _, id := range threadIDs {
		if id != "" {
			keys = append(keys, id)
		}
	}
	result := make(map[string]*store.Message, len(keys))
	for _, batch := range chunkKeys(keys, batchLookupSize) {
		msgs, err := s.latestMessagesPerGroup(ctx, message.FieldThreadID, message.ThreadIDIn(batch...), opts)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			result[m.ThreadID] = m
		}
	}
	return result, nil
}

// LatestMessagesByConversationIDs returns the newest matching message per
// conversation.
func (s *MessageStore) LatestMessagesByConversationIDs(ctx context.Context, conversationIDs []string, opts store.LatestMessageOptions) (map[string]*store.Message, error) {
	keys := make([]uuid.UUID, 0, len(conversationIDs))
	for _, id := range conversationIDs {
		cid, err := parseUUID(id)
		if err != nil {
			return nil, fmt.Errorf("invalid conversation_id filter: %w", err)
		}
		keys = append(keys, cid)
	}
	result := make(map[string]*store.Message, len(keys))
	for _, batch := range chunkKeys(keys, batchLookupSize) {
		msgs, err := s.latestMessagesPerGroup(ctx, message.FieldConversationID, message.ConversationIDIn(batch...), opts)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			result[m.ConversationID] = m
		}
	}
	return result, nil
}

// latestMessagesPerGroup selects, in one statement, every message in the key
// set (inKeys) that matches opts and has no newer matching message in the
// same group (groupCol). "Newer" is the ListMessages order: a later created,
// or an equal created and a greater id. Exactly one row per group survives,
// because (created, id) is unique. The correlated NOT EXISTS is served by
// the (thread_id|conversation_id, channel, created, id) indexes.
func (s *MessageStore) latestMessagesPerGroup(
	ctx context.Context, groupCol string, inKeys predicate.Message, opts store.LatestMessageOptions,
) ([]*store.Message, error) {
	query := s.client.Message.Query().Where(inKeys)
	if opts.Channel != "" {
		query.Where(message.ChannelEQ(opts.Channel))
	}
	if opts.ExcludeType != "" {
		query.Where(message.TypeNEQ(opts.ExcludeType))
	}
	query.Where(func(sel *entsql.Selector) {
		newer := entsql.Table(message.Table).As("newer_message")
		conds := []*entsql.Predicate{
			entsql.ColumnsEQ(newer.C(groupCol), sel.C(groupCol)),
			entsql.Or(
				entsql.ColumnsGT(newer.C(message.FieldCreated), sel.C(message.FieldCreated)),
				entsql.And(
					entsql.ColumnsEQ(newer.C(message.FieldCreated), sel.C(message.FieldCreated)),
					entsql.ColumnsGT(newer.C(message.FieldID), sel.C(message.FieldID)),
				),
			),
		}
		if opts.Channel != "" {
			conds = append(conds, entsql.EQ(newer.C(message.FieldChannel), opts.Channel))
		}
		if opts.ExcludeType != "" {
			conds = append(conds, entsql.NEQ(newer.C(message.FieldType), opts.ExcludeType))
		}
		sel.Where(entsql.NotExists(
			entsql.Select(newer.C(message.FieldID)).From(newer).Where(entsql.And(conds...)),
		))
	})
	entities, err := query.All(ctx)
	if err != nil {
		return nil, err
	}
	msgs := make([]*store.Message, 0, len(entities))
	for _, e := range entities {
		msgs = append(msgs, entMessageToStore(e))
	}
	return msgs, nil
}
