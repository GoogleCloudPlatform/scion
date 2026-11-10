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

// Package authctx carries request-scoped values produced by hub
// authentication to consumers outside the auth middleware.
//
// It imports only the standard library so that pkg/hub and its
// subsystems can both depend on it without an import cycle. It must not
// import pkg/hub.
package authctx

import "context"

// sessionGenerationKey is the context key for the session generation.
// It is unexported so that only this package can set or read the value.
type sessionGenerationKey struct{}

// WithSessionGeneration returns a copy of ctx carrying gen as the session
// generation of the authenticated user.
//
// It is called only by pkg/hub's hub-JWT authentication arm (auth.go),
// with the users-row value it compared against the token's claim in the
// same request.
func WithSessionGeneration(ctx context.Context, gen int64) context.Context {
	return context.WithValue(ctx, sessionGenerationKey{}, gen)
}

// SessionGeneration returns the session generation recorded by the hub-JWT
// authentication arm. ok is false when the request was not authenticated
// by that arm; a zero value with ok true is a real generation.
func SessionGeneration(ctx context.Context) (gen int64, ok bool) {
	if ctx == nil {
		return 0, false
	}
	gen, ok = ctx.Value(sessionGenerationKey{}).(int64)
	return gen, ok
}
