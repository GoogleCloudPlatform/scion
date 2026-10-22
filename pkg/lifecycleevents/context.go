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

package lifecycleevents

import "context"

// CreateContext is the creating principal of an agent create. It is not on
// the agent row; the hub attaches it to the create transaction's context
// (WP1 design 2.1) and the observer records it on created and in the
// cursor, from which later snapshots copy it.
type CreateContext struct {
	CreatorType string // "user" | "agent" | ...
	CreatorID   string
}

type createContextKey struct{}

// WithCreateContext attaches the create's principal to ctx.
func WithCreateContext(ctx context.Context, c CreateContext) context.Context {
	return context.WithValue(ctx, createContextKey{}, c)
}

// CreateContextFrom returns the create principal attached to ctx.
func CreateContextFrom(ctx context.Context) (CreateContext, bool) {
	c, ok := ctx.Value(createContextKey{}).(CreateContext)
	return c, ok
}

// Create-failure causes (WP1 design 2.3, 2.4a steps 1 and 3).
const (
	// CauseCreateRolledBack: a failed create removes its row; the removal
	// emits launch_failed(create_rolled_back) and deleted(create_rolled_back).
	CauseCreateRolledBack = "create_rolled_back"
	// CauseCreateFailed: a failed create that keeps its row.
	CauseCreateFailed = "create_failed"
)

// Cause annotates a write with why it happened, for writes whose row
// change alone does not say (a create rollback looks like a hard delete).
type Cause struct {
	Kind  string // CauseCreateRolledBack | CauseCreateFailed
	Stage string // the failed create stage, emitted as error_code
}

type causeKey struct{}

// WithCause attaches a create-failure cause to ctx.
func WithCause(ctx context.Context, kind, stage string) context.Context {
	return context.WithValue(ctx, causeKey{}, Cause{Kind: kind, Stage: stage})
}

// CauseFrom returns the cause attached to ctx.
func CauseFrom(ctx context.Context) (Cause, bool) {
	c, ok := ctx.Value(causeKey{}).(Cause)
	return c, ok && c.Kind != ""
}
