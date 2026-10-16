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
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ProjectVisitorRule holds the schema definition for a project visitor rule:
// visitor access to a project for every user of a trusted issuer whose
// verified email is in a domain. A rule is not itself a grant: on a
// matching user's request the hub creates a project-visitor role binding
// for that user tagged with the rule's ID (role_bindings.source_rule_id),
// and removing the rule deletes those bindings.
type ProjectVisitorRule struct {
	ent.Schema
}

// Fields of the ProjectVisitorRule.
func (ProjectVisitorRule) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.String("project_id").
			NotEmpty().
			Immutable().
			Comment("The project the rule grants visitor access to"),
		field.String("issuer").
			NotEmpty().
			Immutable().
			Comment("The normalized URL of the trusted issuer"),
		field.String("domain").
			NotEmpty().
			Immutable().
			Comment("The lower-cased email domain; matched exactly, with no wildcard or subdomain match"),
		field.Time("not_before").
			Optional().
			Nillable(),
		field.Time("expires_at").
			Optional().
			Nillable(),
		field.String("created_by").
			Optional(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
	}
}

// Indexes of the ProjectVisitorRule.
func (ProjectVisitorRule) Indexes() []ent.Index {
	return []ent.Index{
		// One rule per (project, issuer, domain).
		index.Fields("project_id", "issuer", "domain").Unique(),
		// The request path finds the rules matching a verified email.
		index.Fields("issuer", "domain"),
	}
}
