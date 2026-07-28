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

// GitHubResolutionCache stores GitHub skill resolution metadata (commit SHA,
// file list, bundle hash) to avoid redundant GitHub REST API calls. The cache
// is keyed by (URI + token_scope) and expires after a configurable TTL.
type GitHubResolutionCache struct {
	ent.Schema
}

// Fields of the GitHubResolutionCache.
func (GitHubResolutionCache) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("cache_key").
			NotEmpty().
			Unique().
			Comment("sha256(normalized_uri + \":\" + token_scope_id)"),
		field.String("original_uri").
			NotEmpty().
			Comment("Original gh:// URI for debugging"),
		field.String("commit_sha").
			NotEmpty().
			Comment("Resolved 40-char commit SHA"),
		field.JSON("file_entries", []GitHubFileEntry{}).
			Comment("List of file metadata: path, download URL, git blob SHA, size"),
		field.String("bundle_hash").
			NotEmpty().
			Comment("Content-addressed bundle hash computed from file_entries"),
		field.String("token_scope").
			Default("public").
			Comment("GitHub App installation ID or 'public' for unauthenticated"),
		field.Time("expires_at").
			Comment("Cache entry expiration time"),
		field.Time("create_time").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the GitHubResolutionCache.
func (GitHubResolutionCache) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("cache_key").Unique(),
		index.Fields("expires_at"), // for efficient TTL eviction
	}
}

// Annotations of the GitHubResolutionCache.
func (GitHubResolutionCache) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "github_resolution_cache"},
	}
}

// GitHubFileEntry represents a single file in a GitHub skill resolution result.
// This is a plain Go struct (not an ent entity) stored as JSON in the file_entries field.
type GitHubFileEntry struct {
	Path string `json:"path"` // Relative path within the skill directory
	URL  string `json:"url"`  // GitHub CDN download URL (raw.githubusercontent.com)
	Hash string `json:"hash"` // Git blob SHA for content-addressed caching
	Size int64  `json:"size"` // File size in bytes
}
