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

package bridge

import "context"

// CallerIdentity holds the per-request caller identity extracted from a
// validated hub credential. Absent for legacy apiKey/bearer/none modes.
type CallerIdentity struct {
	UserID    string
	Email     string
	Role      string
	RawToken  string // The original bearer token for UAT passthrough
	TokenType string // "uat" or "jwt"
}

type callerContextKey struct{}

// withCallerIdentity injects a CallerIdentity into the context.
func withCallerIdentity(ctx context.Context, id *CallerIdentity) context.Context {
	return context.WithValue(ctx, callerContextKey{}, id)
}

// callerIdentityFromContext retrieves the CallerIdentity from the context.
// Returns nil for legacy auth modes (apiKey/bearer/none).
func callerIdentityFromContext(ctx context.Context) *CallerIdentity {
	v, _ := ctx.Value(callerContextKey{}).(*CallerIdentity)
	return v
}
