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

package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// NonceCache stores HMAC request nonces for replay attack prevention.
// Each nonce is stored with an expiration time; expired entries are
// periodically purged by a cleanup goroutine. This replaces the
// process-local in-memory map to support multi-instance deployments.
type NonceCache struct {
	ent.Schema
}

// Fields of the NonceCache.
func (NonceCache) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("nonce").
			NotEmpty().
			Unique().
			Comment("The HMAC request nonce value"),
		field.Time("expires_at").
			Comment("When this nonce entry expires and can be purged"),
		field.Time("created_at").
			Default(time.Now).
			Immutable().
			Comment("When this nonce was first seen"),
	}
}

// Indexes of the NonceCache.
func (NonceCache) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("expires_at"), // for efficient TTL eviction
	}
}

// Annotations of the NonceCache.
func (NonceCache) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "nonce_cache"},
	}
}
