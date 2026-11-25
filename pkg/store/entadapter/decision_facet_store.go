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
	"sort"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionanswer"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/decisionfacetentry"
)

type facetEntryKey struct {
	ProjectID uuid.UUID
	ChatKey   string
	Kind      decisionfacetentry.Kind
	ActorID   string
}

type facetEntryDelta struct {
	ProjectID uuid.UUID
	ChatKey   string
	Kind      decisionfacetentry.Kind
	ActorID   string
	Name      string
	Email     string
	LatestAt  time.Time
	Delta     int
}

const facetUpsertChunkSize = 250

// facetDeltaNewer reports whether candidate (ts, name, email) wins over
// current (curTS, curName, curEmail). Matches decisionAgentFacet's tie-break:
// newer timestamp wins; equal timestamp keeps the greater name (then email).
func facetDeltaNewer(ts time.Time, name, email string, curTS time.Time, curName, curEmail string) bool {
	if ts.After(curTS) {
		return true
	}
	if ts.Before(curTS) {
		return false
	}
	if name != curName {
		return name > curName
	}
	return email > curEmail
}

// applyDecisionFacetDeltas merges positive (+k) and negative (-k) contributions
// per key into a single net signed delta and applies them in one pass in
// deterministic sorted (project_id, chat_key, kind, actor_id) order inside the
// caller's transaction. Applying all signed operations in a single sorted pass
// prevents Postgres row-lock-order deadlocks (40P01) between concurrent
// multi-key writers or indexers (e.g., crossed interpreted-sibling supersession).
func applyDecisionFacetDeltas(ctx context.Context, client *ent.Client, deltas []facetEntryDelta) error {
	if len(deltas) == 0 {
		return nil
	}

	type agg struct {
		key      facetEntryKey
		netDelta int
		hasPos   bool
		name     string
		email    string
		latestAt time.Time
	}

	byKey := make(map[facetEntryKey]*agg, len(deltas))
	for _, d := range deltas {
		if d.Delta == 0 || d.ChatKey == "" || d.ActorID == "" {
			continue
		}
		k := facetEntryKey{
			ProjectID: d.ProjectID,
			ChatKey:   d.ChatKey,
			Kind:      d.Kind,
			ActorID:   d.ActorID,
		}
		a := byKey[k]
		if a == nil {
			a = &agg{key: k}
			byKey[k] = a
		}
		a.netDelta += d.Delta
		if d.Delta > 0 {
			ts := d.LatestAt.UTC().Truncate(time.Microsecond)
			if !a.hasPos || facetDeltaNewer(ts, d.Name, d.Email, a.latestAt, a.name, a.email) {
				a.name = d.Name
				a.email = d.Email
				a.latestAt = ts
			}
			a.hasPos = true
		}
	}
	if len(byKey) == 0 {
		return nil
	}

	ordered := make([]*agg, 0, len(byKey))
	for _, a := range byKey {
		if a.netDelta == 0 && !a.hasPos {
			continue
		}
		ordered = append(ordered, a)
	}
	if len(ordered) == 0 {
		return nil
	}
	sort.Slice(ordered, func(i, j int) bool {
		ki, kj := ordered[i].key, ordered[j].key
		if ki.ProjectID != kj.ProjectID {
			return ki.ProjectID.String() < kj.ProjectID.String()
		}
		if ki.ChatKey != kj.ChatKey {
			return ki.ChatKey < kj.ChatKey
		}
		if ki.Kind != kj.Kind {
			return ki.Kind < kj.Kind
		}
		return ki.ActorID < kj.ActorID
	})

	var posCreates []*ent.DecisionFacetEntryCreate
	flushPos := func() error {
		if len(posCreates) == 0 {
			return nil
		}
		err := client.DecisionFacetEntry.CreateBulk(posCreates...).
			OnConflictColumns(
				decisionfacetentry.FieldProjectID,
				decisionfacetentry.FieldChatKey,
				decisionfacetentry.FieldKind,
				decisionfacetentry.FieldActorID,
			).
			Update(func(u *ent.DecisionFacetEntryUpsert) {
				tbl := decisionfacetentry.Table
				u.Set(decisionfacetentry.FieldRefCount, entsql.ExprFunc(func(b *entsql.Builder) {
					b.WriteString(tbl + ".ref_count + excluded.ref_count")
				}))
				u.Set(decisionfacetentry.FieldName, entsql.ExprFunc(func(b *entsql.Builder) {
					b.WriteString("CASE WHEN excluded.latest_at > " + tbl + ".latest_at OR (excluded.latest_at = " + tbl + ".latest_at AND excluded.name > " + tbl + ".name) THEN excluded.name ELSE " + tbl + ".name END")
				}))
				u.Set(decisionfacetentry.FieldEmail, entsql.ExprFunc(func(b *entsql.Builder) {
					b.WriteString("CASE WHEN excluded.latest_at > " + tbl + ".latest_at OR (excluded.latest_at = " + tbl + ".latest_at AND (excluded.name > " + tbl + ".name OR (excluded.name = " + tbl + ".name AND excluded.email > " + tbl + ".email))) THEN excluded.email ELSE " + tbl + ".email END")
				}))
				u.Set(decisionfacetentry.FieldLatestAt, entsql.ExprFunc(func(b *entsql.Builder) {
					b.WriteString("CASE WHEN excluded.latest_at > " + tbl + ".latest_at THEN excluded.latest_at ELSE " + tbl + ".latest_at END")
				}))
			}).
			Exec(ctx)
		posCreates = posCreates[:0]
		return mapError(err)
	}

	drv := client.Driver()
	d := entsql.Dialect(drv.Dialect())

	for _, a := range ordered {
		if a.netDelta > 0 {
			posCreates = append(posCreates, client.DecisionFacetEntry.Create().
				SetProjectID(a.key.ProjectID).
				SetChatKey(a.key.ChatKey).
				SetKind(a.key.Kind).
				SetActorID(a.key.ActorID).
				SetName(a.name).
				SetEmail(a.email).
				SetLatestAt(a.latestAt).
				SetRefCount(a.netDelta),
			)
			if len(posCreates) >= facetUpsertChunkSize {
				if err := flushPos(); err != nil {
					return err
				}
			}
			continue
		}

		// Flush any preceding positive upserts before executing this key's
		// UPDATE so row locks are acquired strictly in sorted key order.
		if err := flushPos(); err != nil {
			return err
		}

		ub := d.Update(decisionfacetentry.Table)
		if a.netDelta < 0 {
			// Decrementing ref_count on supersede does not recompute name,
			// email, or latest_at from remaining answers, so while ref_count > 0
			// the row's snapshot metadata may still come from the superseded
			// answer (acceptable per design-scale §3.2/§6).
			dec := -a.netDelta
			ub.Set(decisionfacetentry.FieldRefCount, entsql.ExprFunc(func(b *entsql.Builder) {
				b.WriteString("CASE WHEN ref_count > ")
				b.Arg(dec)
				b.WriteString(" THEN ref_count - ")
				b.Arg(dec)
				b.WriteString(" ELSE 0 END")
			}))
		}
		if a.hasPos {
			ts, name, email := a.latestAt, a.name, a.email
			ub.Set(decisionfacetentry.FieldName, entsql.ExprFunc(func(b *entsql.Builder) {
				b.WriteString("CASE WHEN ")
				b.Arg(ts)
				b.WriteString(" > latest_at OR (")
				b.Arg(ts)
				b.WriteString(" = latest_at AND ")
				b.Arg(name)
				b.WriteString(" > name) THEN ")
				b.Arg(name)
				b.WriteString(" ELSE name END")
			}))
			ub.Set(decisionfacetentry.FieldEmail, entsql.ExprFunc(func(b *entsql.Builder) {
				b.WriteString("CASE WHEN ")
				b.Arg(ts)
				b.WriteString(" > latest_at OR (")
				b.Arg(ts)
				b.WriteString(" = latest_at AND (")
				b.Arg(name)
				b.WriteString(" > name OR (")
				b.Arg(name)
				b.WriteString(" = name AND ")
				b.Arg(email)
				b.WriteString(" > email))) THEN ")
				b.Arg(email)
				b.WriteString(" ELSE email END")
			}))
			ub.Set(decisionfacetentry.FieldLatestAt, entsql.ExprFunc(func(b *entsql.Builder) {
				b.WriteString("CASE WHEN ")
				b.Arg(ts)
				b.WriteString(" > latest_at THEN ")
				b.Arg(ts)
				b.WriteString(" ELSE latest_at END")
			}))
		}
		ub.Where(entsql.And(
			entsql.EQ(decisionfacetentry.FieldProjectID, a.key.ProjectID),
			entsql.EQ(decisionfacetentry.FieldChatKey, a.key.ChatKey),
			entsql.EQ(decisionfacetentry.FieldKind, string(a.key.Kind)),
			entsql.EQ(decisionfacetentry.FieldActorID, a.key.ActorID),
		))
		query, args := ub.Query()
		if err := drv.Exec(ctx, query, args, nil); err != nil {
			return mapError(err)
		}
	}

	return flushPos()
}

// supersededInterpretedFacetDelta returns a -1 answerer delta when a direct
// answer supersedes an already-facet_indexed agent_interpreted sibling that
// had a non-empty answered_by_user_id on a decision with a non-empty chat_key.
func supersededInterpretedFacetDelta(
	projectID uuid.UUID,
	chatKey *string,
	existing []*ent.DecisionAnswer,
) (facetEntryDelta, bool) {
	if chatKey == nil || *chatKey == "" {
		return facetEntryDelta{}, false
	}
	for _, sib := range existing {
		if sib.Provenance == decisionanswer.ProvenanceAgentInterpreted &&
			sib.FacetIndexed &&
			sib.AnsweredByUserID != nil && *sib.AnsweredByUserID != "" {
			return facetEntryDelta{
				ProjectID: projectID,
				ChatKey:   *chatKey,
				Kind:      decisionfacetentry.KindAnswerer,
				ActorID:   *sib.AnsweredByUserID,
				Delta:     -1,
			}, true
		}
	}
	return facetEntryDelta{}, false
}
